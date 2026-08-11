package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"syna/internal/server/db"
	"syna/internal/server/objectstore"
)

func Stats(database *db.DB) error {
	counts, err := database.Counts()
	if err != nil {
		return err
	}
	version, err := database.CurrentSchemaVersion()
	if err != nil {
		return err
	}
	fmt.Printf("schema_version: %d\n", version)
	fmt.Printf("latest_schema_version: %d\n", db.LatestSchemaVersion)
	for _, key := range []string{"workspaces", "devices", "roots", "events", "objects", "sessions"} {
		fmt.Printf("%s: %d\n", key, counts[key])
	}
	transferredBytes, err := database.TransferredBytes()
	if err != nil {
		return err
	}
	fmt.Printf("transferred_bytes: %d\n", transferredBytes)
	return nil
}

func Doctor(database *db.DB, dataDir string) error {
	if err := database.SQL.Ping(); err != nil {
		return err
	}
	for _, p := range []string{
		dataDir,
		filepath.Join(dataDir, "objects"),
		filepath.Join(dataDir, "tmp"),
		filepath.Join(dataDir, "state.db"),
	} {
		if _, err := os.Stat(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	version, err := database.CurrentSchemaVersion()
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	fmt.Printf("schema_version: %d\n", version)
	fmt.Printf("latest_schema_version: %d\n", db.LatestSchemaVersion)
	return nil
}

func GC(database *db.DB, store *objectstore.Store, now time.Time, eventRetention, zeroRefRetention time.Duration) error {
	deletedEvents, deletedSnapshots, objectIDs, err := database.Prune(now, eventRetention, zeroRefRetention)
	if err != nil {
		return err
	}
	for _, objectID := range objectIDs {
		_ = os.Remove(store.ObjectPath(objectID))
	}
	fmt.Printf("deleted_events: %d\n", deletedEvents)
	fmt.Printf("deleted_snapshots: %d\n", deletedSnapshots)
	fmt.Printf("deleted_objects: %d\n", len(objectIDs))
	return nil
}

func PurgeWorkspace(database *db.DB, store *objectstore.Store, dataDir, workspaceID string, output io.Writer) error {
	if !validWorkspaceID(workspaceID) {
		return fmt.Errorf("invalid workspace ID: expected 32 lowercase hexadecimal characters")
	}
	if err := RecoverPurgeStaging(database, store, dataDir); err != nil {
		return err
	}
	candidates, err := database.WorkspacePurgeObjects(workspaceID)
	if err != nil {
		return err
	}
	stageDir, staged, err := stagePurgeObjects(store, dataDir, candidates)
	if err != nil {
		return err
	}
	result, err := database.PurgeWorkspace(workspaceID, candidates, time.Now().UTC())
	if err != nil {
		_ = restoreStagedObjects(store, stageDir, staged)
		return err
	}

	deleted := make(map[string]struct{}, len(result.Objects))
	var deletedBytes int64
	for _, object := range result.Objects {
		deleted[object.ObjectID] = struct{}{}
		deletedBytes += object.SizeBytes
	}
	for objectID := range staged {
		if _, ok := deleted[objectID]; !ok {
			if err := restoreStagedObject(store, stageDir, objectID); err != nil {
				return err
			}
		}
	}
	if stageDir != "" {
		if err := os.RemoveAll(stageDir); err != nil {
			return fmt.Errorf("remove purge staging directory: %w", err)
		}
		if err := syncDir(filepath.Join(dataDir, "tmp")); err != nil {
			return fmt.Errorf("sync purge staging parent: %w", err)
		}
	}
	if err := database.CompactAfterPurge(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "workspace_id: %s\ndeleted_devices: %d\ndeleted_sessions: %d\ndeleted_roots: %d\ndeleted_events: %d\ndeleted_snapshots: %d\ndeleted_objects: %d\ndeleted_bytes: %d\n",
		workspaceID, result.Devices, result.Sessions, result.Roots, result.Events, result.Snapshots, len(result.Objects), deletedBytes); err != nil {
		return err
	}
	return nil
}

func RecoverPurgeStaging(database *db.DB, store *objectstore.Store, dataDir string) error {
	tmpDir := filepath.Join(dataDir, "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "purge-") {
			continue
		}
		stageDir := filepath.Join(tmpDir, entry.Name())
		files, err := os.ReadDir(stageDir)
		if err != nil {
			return err
		}
		for _, file := range files {
			objectID := strings.TrimSuffix(file.Name(), ".bin")
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".bin") || !objectstore.ValidObjectID(objectID) {
				return fmt.Errorf("invalid purge staging entry %s", filepath.Join(stageDir, file.Name()))
			}
			var exists int
			err := database.SQL.QueryRow(`SELECT 1 FROM objects WHERE object_id = ?`, objectID).Scan(&exists)
			switch {
			case err == nil:
				if err := restoreStagedObject(store, stageDir, objectID); err != nil {
					return err
				}
			case errors.Is(err, sql.ErrNoRows):
				if err := os.Remove(filepath.Join(stageDir, file.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			default:
				return err
			}
		}
		if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncDir(tmpDir); err != nil {
			return err
		}
	}
	return nil
}

func stagePurgeObjects(store *objectstore.Store, dataDir string, objects []db.PurgeObject) (string, map[string]struct{}, error) {
	staged := make(map[string]struct{})
	if len(objects) == 0 {
		return "", staged, nil
	}
	stageDir, err := os.MkdirTemp(filepath.Join(dataDir, "tmp"), "purge-")
	if err != nil {
		return "", nil, err
	}
	if err := syncDir(filepath.Join(dataDir, "tmp")); err != nil {
		_ = os.Remove(stageDir)
		return "", nil, err
	}
	sourceDirs := make(map[string]struct{})
	for _, object := range objects {
		if !objectstore.ValidObjectID(object.ObjectID) {
			_ = restoreStagedObjects(store, stageDir, staged)
			return "", nil, fmt.Errorf("invalid object ID %q in workspace ownership data", object.ObjectID)
		}
		source := store.ObjectPath(object.ObjectID)
		target := filepath.Join(stageDir, object.ObjectID+".bin")
		if err := os.Rename(source, target); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			_ = restoreStagedObjects(store, stageDir, staged)
			return "", nil, fmt.Errorf("stage object %s: %w", object.ObjectID, err)
		}
		staged[object.ObjectID] = struct{}{}
		sourceDirs[filepath.Dir(source)] = struct{}{}
	}
	for sourceDir := range sourceDirs {
		if err := syncDir(sourceDir); err != nil {
			_ = restoreStagedObjects(store, stageDir, staged)
			return "", nil, fmt.Errorf("sync object directory: %w", err)
		}
	}
	if err := syncDir(stageDir); err != nil {
		_ = restoreStagedObjects(store, stageDir, staged)
		return "", nil, fmt.Errorf("sync purge staging directory: %w", err)
	}
	return stageDir, staged, nil
}

func restoreStagedObjects(store *objectstore.Store, stageDir string, staged map[string]struct{}) error {
	var firstErr error
	for objectID := range staged {
		if err := restoreStagedObject(store, stageDir, objectID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if stageDir != "" {
		if err := os.Remove(stageDir); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
		if err := syncDir(filepath.Dir(stageDir)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func restoreStagedObject(store *objectstore.Store, stageDir, objectID string) error {
	source := filepath.Join(stageDir, objectID+".bin")
	target := store.ObjectPath(objectID)
	if _, err := os.Stat(target); err == nil {
		if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDir(stageDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore staged object %s: %w", objectID, err)
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	if err := syncDir(stageDir); err != nil {
		return err
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func validWorkspaceID(workspaceID string) bool {
	if len(workspaceID) != 32 {
		return false
	}
	for _, char := range workspaceID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

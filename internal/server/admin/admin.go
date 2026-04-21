package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

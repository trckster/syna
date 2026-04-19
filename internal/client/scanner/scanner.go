package scanner

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"syna/internal/common/protocol"
)

type Entry struct {
	RelPath       string
	AbsPath       string
	Kind          protocol.RootKind
	Mode          int64
	MTimeNS       int64
	SizeBytes     int64
	ContentSHA256 string
}

type Result struct {
	Entries  []Entry
	Warnings []string
	RootKind protocol.RootKind
}

func ScanRoot(rootPath string) (*Result, error) {
	return scan(rootPath, "")
}

func ScanSubtree(rootPath, relPath string) (*Result, error) {
	return scan(rootPath, filepath.Clean(filepath.FromSlash(relPath)))
}

func scan(rootPath, subtree string) (*Result, error) {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fs.ErrInvalid
	}
	result := &Result{}
	if info.IsDir() {
		result.RootKind = protocol.RootKindDir
		scanRoot := rootPath
		if subtree != "" && subtree != "." {
			scanRoot = filepath.Join(rootPath, subtree)
			info, err = os.Lstat(scanRoot)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fs.ErrInvalid
			}
		}
		if !info.IsDir() {
			sum, err := hashFile(scanRoot)
			if err != nil {
				return nil, err
			}
			result.Entries = append(result.Entries, statEntry(toRelPath(subtree), scanRoot, info, protocol.RootKindFile, sum))
			return result, nil
		}
		result.Entries = append(result.Entries, statEntry(toRelPath(subtree), scanRoot, info, protocol.RootKindDir, ""))
		err = filepath.WalkDir(scanRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == scanRoot {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(rootPath, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			mode := info.Mode()
			switch {
			case mode&os.ModeSymlink != 0:
				result.Warnings = append(result.Warnings, "ignored symlink: "+rel)
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			case mode.IsDir():
				result.Entries = append(result.Entries, statEntry(rel, path, info, protocol.RootKindDir, ""))
			case mode.IsRegular():
				sum, err := hashFile(path)
				if err != nil {
					return err
				}
				result.Entries = append(result.Entries, statEntry(rel, path, info, protocol.RootKindFile, sum))
			default:
				result.Warnings = append(result.Warnings, "ignored unsupported file: "+rel)
			}
			return nil
		})
	} else {
		result.RootKind = protocol.RootKindFile
		sum, err := hashFile(rootPath)
		if err != nil {
			return nil, err
		}
		result.Entries = append(result.Entries, statEntry("", rootPath, info, protocol.RootKindFile, sum))
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		if a.RelPath == "" || b.RelPath == "" {
			return a.RelPath == ""
		}
		if a.Kind != b.Kind {
			return a.Kind == protocol.RootKindDir
		}
		return a.RelPath < b.RelPath
	})
	return result, nil
}

func toRelPath(path string) string {
	if path == "" || path == "." {
		return ""
	}
	return filepath.ToSlash(path)
}

func statEntry(relPath, absPath string, info os.FileInfo, kind protocol.RootKind, sha string) Entry {
	return Entry{
		RelPath:       relPath,
		AbsPath:       absPath,
		Kind:          kind,
		Mode:          int64(info.Mode().Perm()),
		MTimeNS:       info.ModTime().UnixNano(),
		SizeBytes:     info.Size(),
		ContentSHA256: sha,
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

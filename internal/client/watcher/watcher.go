package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type Change struct {
	RootID      string
	RelPathHint string
}

type Manager struct {
	watcher  *fsnotify.Watcher
	onChange func(Change)
	mu       sync.Mutex
	rootDirs map[string]string
	dirRoots map[string]string
}

func New(onChange func(Change)) (*Manager, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		watcher:  w,
		onChange: onChange,
		rootDirs: make(map[string]string),
		dirRoots: make(map[string]string),
	}
	go m.loop()
	return m, nil
}

func (m *Manager) Close() error {
	return m.watcher.Close()
}

func (m *Manager) AddRoot(rootID, absPath string) error {
	m.mu.Lock()
	m.rootDirs[rootID] = absPath
	m.mu.Unlock()
	info, err := os.Stat(absPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		parent := filepath.Dir(absPath)
		if err := m.addDir(rootID, parent); err != nil {
			return err
		}
		return nil
	}
	return m.addDirTree(rootID, absPath)
}

func (m *Manager) addDirTree(rootID, absPath string) error {
	return filepath.WalkDir(absPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := m.addDir(rootID, path); err != nil {
			return err
		}
		return nil
	})
}

func (m *Manager) addDir(rootID, dir string) error {
	m.mu.Lock()
	if currentRootID, ok := m.dirRoots[dir]; ok && currentRootID == rootID {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := m.watcher.Add(dir); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		if !isAlreadyWatched(err) {
			return err
		}
	}
	m.mu.Lock()
	m.dirRoots[dir] = rootID
	m.mu.Unlock()
	return nil
}

func (m *Manager) RemoveRoot(rootID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rootDirs, rootID)
	for dir, currentRootID := range m.dirRoots {
		if currentRootID == rootID {
			_ = m.watcher.Remove(dir)
			delete(m.dirRoots, dir)
		}
	}
}

func (m *Manager) loop() {
	for {
		select {
		case ev, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			rootID := m.rootForEvent(ev.Name)
			if rootID != "" && ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = m.addDirTree(rootID, ev.Name)
				}
			}
			hint := ""
			if rootID != "" {
				hint = m.relHint(rootID, ev.Name)
			}
			if rootID == "" && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				rootID, hint = m.rootForMissingPath(ev.Name)
			}
			if rootID != "" {
				m.onChange(Change{RootID: rootID, RelPathHint: hint})
			}
		case _, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (m *Manager) rootForEvent(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := path
	for {
		if rootID := m.dirRoots[current]; rootID != "" {
			if m.pathMatchesRootLocked(rootID, path) {
				return rootID
			}
			return ""
		}
		next := filepath.Dir(current)
		if next == current {
			return ""
		}
		current = next
	}
}

func (m *Manager) rootForMissingPath(path string) (string, string) {
	current := filepath.Dir(path)
	for {
		if current == "." || current == "/" || current == "" {
			rootID := m.rootForEvent(filepath.Dir(path))
			if rootID == "" {
				return "", ""
			}
			return rootID, m.nearestExistingAncestorHint(rootID, path)
		}
		if _, err := os.Stat(current); err == nil {
			rootID := m.rootForEvent(current)
			if rootID == "" {
				return "", ""
			}
			return rootID, m.nearestExistingAncestorHint(rootID, path)
		}
		next := filepath.Dir(current)
		if next == current {
			return "", ""
		}
		current = next
	}
}

func (m *Manager) relHint(rootID, path string) string {
	m.mu.Lock()
	rootPath := m.rootDirs[rootID]
	m.mu.Unlock()
	if rootPath == "" {
		return ""
	}
	return relHint(rootPath, path)
}

func (m *Manager) nearestExistingAncestorHint(rootID, path string) string {
	m.mu.Lock()
	rootPath := m.rootDirs[rootID]
	m.mu.Unlock()
	if rootPath == "" {
		return ""
	}
	current := path
	for {
		if samePath(current, rootPath) {
			return ""
		}
		if _, err := os.Stat(current); err == nil {
			return relHint(rootPath, current)
		}
		next := filepath.Dir(current)
		if next == current {
			return ""
		}
		current = next
	}
}

func (m *Manager) pathMatchesRootLocked(rootID, path string) bool {
	rootPath := m.rootDirs[rootID]
	if rootPath == "" {
		return false
	}
	if samePath(path, rootPath) {
		return true
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		return false
	}
	if rootInfo.IsDir() {
		return withinPath(rootPath, path)
	}
	return samePath(path, rootPath)
}

func relHint(rootPath, path string) string {
	if samePath(path, rootPath) {
		return ""
	}
	rootInfo, err := os.Stat(rootPath)
	if err == nil && !rootInfo.IsDir() {
		return ""
	}
	rel, err := filepath.Rel(rootPath, path)
	if err != nil {
		return ""
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func withinPath(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if root == path {
		return true
	}
	prefix := root + string(os.PathSeparator)
	return strings.HasPrefix(path, prefix)
}

func isAlreadyWatched(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrExist) || err.Error() == "file already exists"
}

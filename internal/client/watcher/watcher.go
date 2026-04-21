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
	dirRoots map[string]map[string]struct{}
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
		dirRoots: make(map[string]map[string]struct{}),
	}
	go m.loop()
	return m, nil
}

func (m *Manager) Close() error {
	return m.watcher.Close()
}

func (m *Manager) AddRoot(rootID, absPath string) error {
	absPath = filepath.Clean(absPath)
	info, err := os.Stat(absPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.rootDirs[rootID] = absPath
	m.mu.Unlock()
	if !info.IsDir() {
		parent := filepath.Dir(absPath)
		if err := m.addDir(rootID, parent); err != nil {
			m.RemoveRoot(rootID)
			return err
		}
		return nil
	}
	if err := m.addDirTree(rootID, absPath); err != nil {
		m.RemoveRoot(rootID)
		return err
	}
	return nil
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
	dir = filepath.Clean(dir)
	m.mu.Lock()
	if roots := m.dirRoots[dir]; roots != nil {
		if _, ok := roots[rootID]; ok {
			m.mu.Unlock()
			return nil
		}
		roots[rootID] = struct{}{}
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
	if m.dirRoots[dir] == nil {
		m.dirRoots[dir] = make(map[string]struct{})
	}
	m.dirRoots[dir][rootID] = struct{}{}
	m.mu.Unlock()
	return nil
}

func (m *Manager) RemoveRoot(rootID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rootDirs, rootID)
	for dir, roots := range m.dirRoots {
		if _, ok := roots[rootID]; ok {
			delete(roots, rootID)
			if len(roots) == 0 {
				_ = m.watcher.Remove(dir)
				delete(m.dirRoots, dir)
			}
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
			rootIDs := m.rootsForEvent(ev.Name)
			if len(rootIDs) > 0 && ev.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					for _, rootID := range rootIDs {
						_ = m.addDirTree(rootID, ev.Name)
					}
				}
			}
			var changes []Change
			for _, rootID := range rootIDs {
				changes = append(changes, Change{RootID: rootID, RelPathHint: m.relHint(rootID, ev.Name)})
			}
			if len(changes) == 0 && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				changes = m.changesForMissingPath(ev.Name)
			}
			for _, change := range changes {
				m.onChange(change)
			}
		case _, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (m *Manager) rootsForEvent(path string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootsForEventLocked(path)
}

func (m *Manager) rootsForEventLocked(path string) []string {
	path = filepath.Clean(path)
	current := path
	for {
		if candidates := m.dirRoots[current]; len(candidates) > 0 {
			var roots []string
			for rootID := range candidates {
				if m.pathMatchesRootLocked(rootID, path) {
					roots = append(roots, rootID)
				}
			}
			return roots
		}
		next := filepath.Dir(current)
		if next == current {
			return nil
		}
		current = next
	}
}

func (m *Manager) changesForMissingPath(path string) []Change {
	current := filepath.Dir(path)
	for {
		if current == "." || current == "/" || current == "" {
			return m.changesForExistingAncestor(filepath.Dir(path), path)
		}
		if _, err := os.Stat(current); err == nil {
			return m.changesForExistingAncestor(current, path)
		}
		next := filepath.Dir(current)
		if next == current {
			return nil
		}
		current = next
	}
}

func (m *Manager) changesForExistingAncestor(ancestor, path string) []Change {
	rootIDs := m.rootsForEvent(ancestor)
	changes := make([]Change, 0, len(rootIDs))
	for _, rootID := range rootIDs {
		changes = append(changes, Change{RootID: rootID, RelPathHint: m.nearestExistingAncestorHint(rootID, path)})
	}
	return changes
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

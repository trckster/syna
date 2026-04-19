package paths

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
)

func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" {
		return "", errors.New("home directory is empty")
	}
	return filepath.Clean(home), nil
}

func ExpandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := HomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func CanonicalizeRootPath(input string, existing []string) (string, string, error) {
	expanded, err := ExpandHome(input)
	if err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)

	home, err := HomeDir()
	if err != nil {
		return "", "", err
	}
	if abs != home && !strings.HasPrefix(abs, home+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q is outside %s", abs, home)
	}

	rel, err := filepath.Rel(home, abs)
	if err != nil {
		return "", "", err
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return "", "", errors.New("home directory itself cannot be tracked")
	}
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimSuffix(rel, "/")

	if err := CheckOverlap(rel, existing); err != nil {
		return "", "", err
	}
	return abs, rel, nil
}

func CheckOverlap(candidate string, existing []string) error {
	candidate = cleanStored(candidate)
	for _, root := range existing {
		root = cleanStored(root)
		if overlaps(candidate, root) {
			return fmt.Errorf("root %q overlaps existing root %q", candidate, root)
		}
	}
	return nil
}

func SortStoredRoots(roots []string) {
	sort.Slice(roots, func(i, j int) bool {
		return cleanStored(roots[i]) < cleanStored(roots[j])
	})
}

func TargetForHomeRel(homeRel string) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(cleanStored(homeRel))), nil
}

func ValidateRemoteRelPath(raw string, allowRoot bool) (string, error) {
	return validateSlashRelPath(raw, allowRoot, "remote path")
}

func ValidateHomeRelPath(raw string) (string, error) {
	return validateSlashRelPath(raw, false, "home_rel_path")
}

func ResolveRemoteTarget(rootAbs, raw string, allowRoot bool) (string, string, error) {
	rel, err := ValidateRemoteRelPath(raw, allowRoot)
	if err != nil {
		return "", "", err
	}
	target := filepath.Clean(rootAbs)
	if rel != "" {
		target = filepath.Join(target, filepath.FromSlash(rel))
	}
	if err := ensureWithinRoot(rootAbs, target); err != nil {
		return "", "", err
	}
	return rel, target, nil
}

func ResolveHomeRelTarget(raw string) (string, string, error) {
	homeRel, err := ValidateHomeRelPath(raw)
	if err != nil {
		return "", "", err
	}
	target, err := TargetForHomeRel(homeRel)
	if err != nil {
		return "", "", err
	}
	home, err := HomeDir()
	if err != nil {
		return "", "", err
	}
	if err := ensureWithinRoot(home, target); err != nil {
		return "", "", err
	}
	return homeRel, target, nil
}

func EnsureSafeDir(rootAbs, dirAbs string, perm os.FileMode) error {
	rootAbs = filepath.Clean(rootAbs)
	dirAbs = filepath.Clean(dirAbs)
	rel, err := RelWithinRoot(rootAbs, dirAbs)
	if err != nil {
		return err
	}
	current := rootAbs
	if rel == "" {
		return ensureDirComponent(current, perm)
	}
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		if err := ensureDirComponent(current, perm); err != nil {
			return err
		}
	}
	return nil
}

func EnsureSafeParents(rootAbs, targetAbs string, perm os.FileMode) error {
	return EnsureSafeDir(rootAbs, filepath.Dir(filepath.Clean(targetAbs)), perm)
}

func RejectSymlinkParents(rootAbs, targetAbs string) error {
	rootAbs = filepath.Clean(rootAbs)
	targetAbs = filepath.Clean(targetAbs)
	if _, err := RelWithinRoot(rootAbs, targetAbs); err != nil {
		return err
	}
	current := rootAbs
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return rejectSymlink(current)
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		if err := rejectSymlink(current); err != nil {
			return err
		}
	}
	return nil
}

func RelWithinRoot(rootAbs, targetAbs string) (string, error) {
	rootAbs = filepath.Clean(rootAbs)
	targetAbs = filepath.Clean(targetAbs)
	if targetAbs == rootAbs {
		return "", nil
	}
	if !strings.HasPrefix(targetAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside root %q", targetAbs, rootAbs)
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

func cleanStored(p string) string {
	p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if p == "." {
		return ""
	}
	return p
}

func overlaps(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func validateSlashRelPath(raw string, allowEmpty bool, label string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%s contains NUL", label)
	}
	if raw == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("%s cannot be empty", label)
	}
	if strings.Contains(raw, "\\") {
		return "", fmt.Errorf("%s must use '/' separators", label)
	}
	if pathpkg.IsAbs(raw) {
		return "", fmt.Errorf("%s must be relative", label)
	}
	clean := pathpkg.Clean(raw)
	if clean == "." {
		return "", fmt.Errorf("%s cannot be empty", label)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s cannot traverse upward", label)
	}
	if clean != raw {
		return "", fmt.Errorf("%s must be canonical", label)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "." || part == ".." || part == "" {
			return "", fmt.Errorf("%s is malformed", label)
		}
	}
	return clean, nil
}

func ensureWithinRoot(rootAbs, targetAbs string) error {
	_, err := RelWithinRoot(rootAbs, targetAbs)
	return err
}

func ensureDirComponent(dir string, perm os.FileMode) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is a symlink", dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return rejectSymlink(dir)
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink", path)
	}
	return nil
}

package fsext

import (
	"os"
	"path/filepath"
)

// ResolvedPath is path with every symlink resolved, the part of it that
// does not exist yet (a file to be written, its new directories) taken as
// it is under its nearest existing directory. ok is false for a path that
// ends in a symlink that leads nowhere: writing it would make a file
// wherever the link points, and reading it reads nothing.
func ResolvedPath(path string) (string, bool) {
	var rest []string
	for dir := path; ; dir = filepath.Dir(dir) {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{real}, rest...)...), true
		}
		if info, lerr := os.Lstat(dir); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		rest = append([]string{filepath.Base(dir)}, rest...)
	}
}

// LeadsOut reports whether reading path reaches outside dir: path as
// written lies outside it, or a symlink on the way to it leads outside it,
// as a link that a downloaded repository holds can lead to ~/.ssh. real is
// where a read of path goes, its symlinks resolved; path, made absolute,
// when it ends in a link that leads nowhere, which reads nothing.
func LeadsOut(dir, path string) (outside bool, real string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true, path
	}
	real, ok := ResolvedPath(abs)
	if !ok {
		real = abs
	}
	absDir, err := filepath.Abs(dir)
	if err != nil || !HasPrefix(abs, absDir) {
		return true, real
	}
	realDir, ok := ResolvedPath(absDir)
	if !ok {
		return true, real
	}
	return !HasPrefix(real, realDir), real
}

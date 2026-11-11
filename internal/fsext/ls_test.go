package fsext

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/stretchr/testify/require"
)

func TestListDirectory(t *testing.T) {
	tmp := t.TempDir()

	testFiles := map[string]string{
		"regular.txt":     "content",
		".hidden":         "hidden content",
		".gitignore":      ".*\n*.log\n",
		"subdir/file.go":  "package main",
		"subdir/.another": "more hidden",
		"build.log":       "build output",
	}

	for name, content := range testFiles {
		fp := filepath.Join(tmp, name)
		dir := filepath.Dir(fp)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(fp, []byte(content), 0o644))
	}

	t.Run("no limit", func(t *testing.T) {
		files, truncated, err := ListDirectory(tmp, nil, -1, -1)
		require.NoError(t, err)
		require.False(t, truncated)
		// The .gitignore has ".*" pattern which ignores hidden files anywhere
		// (like real git does), so subdir/.another is ignored.
		require.Len(t, files, 3)
		require.ElementsMatch(t, []string{
			"regular.txt",
			"subdir",
			"subdir/file.go",
		}, relPaths(t, files, tmp))
	})
	t.Run("limit", func(t *testing.T) {
		files, truncated, err := ListDirectory(tmp, nil, -1, 2)
		require.NoError(t, err)
		require.True(t, truncated)
		require.Len(t, files, 2)
	})
}

// A walk cut at exactly the limit is still a truncated one: fastwalk hands
// the SkipAll that stops it back to the callback as the directory's error.
func TestListDirectoryTruncatedAtLimit(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	for i := range 10 {
		name := filepath.Join(tmp, fmt.Sprintf("file%02d.txt", i))
		require.NoError(t, os.WriteFile(name, nil, 0o644))
	}

	files, truncated, err := ListDirectory(tmp, nil, 0, 3)
	require.NoError(t, err)
	require.Len(t, files, 3)
	require.True(t, truncated)
}

func relPaths(tb testing.TB, in []string, base string) []string {
	tb.Helper()
	out := make([]string, 0, len(in))
	for _, p := range in {
		rel, err := filepath.Rel(base, p)
		require.NoError(tb, err)
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// The global ignore file is read under both names, and the patterns of
// both count: one started under the new name drops none of the old one's.
func TestGlobalIgnorePatternsReadsBothNames(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	for app, pattern := range map[string]string{"gnosh": "*.log", "crush": "secret/"} {
		require.NoError(t, os.MkdirAll(filepath.Join(configDir, app), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, app, "ignore"), []byte(pattern+"\n"), 0o644))
	}

	patterns := globalIgnorePatterns(configDir)
	require.Len(t, patterns, 2)
	matches := func(path string, isDir bool) bool {
		for _, p := range patterns {
			if p.Match([]string{path}, isDir) == gitignore.Exclude {
				return true
			}
		}
		return false
	}
	require.True(t, matches("debug.log", false), "the gnosh ignore file counts")
	require.True(t, matches("secret", true), "and so does the crush one")
	require.False(t, matches("main.go", false))

	require.Empty(t, globalIgnorePatterns(t.TempDir()), "no ignore file, no patterns")
}

package fsext

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// LeadsOut takes a path as outside a directory when it is written outside
// it or a link on the way leads outside it, and as inside when it is in
// it, a link in it leads within it, or it is not there yet.
func TestLeadsOut(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	project, outside := filepath.Join(base, "project"), filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(project, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	key := filepath.Join(outside, "key")
	require.NoError(t, os.WriteFile(key, []byte("k"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(project, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.Symlink(key, filepath.Join(project, "notes.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "sub", "out")))
	require.NoError(t, os.Symlink("../a.txt", filepath.Join(project, "sub", "in.txt")))
	require.NoError(t, os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(project, "dangling")))
	// The project as reached through a link of its own, as the temporary
	// directory is on macOS.
	viaLink := filepath.Join(base, "via")
	require.NoError(t, os.Symlink(project, viaLink))

	for _, tc := range []struct {
		dir, path string
		outside   bool
		real      string
	}{
		{project, filepath.Join(project, "a.txt"), false, filepath.Join(project, "a.txt")},
		{project, filepath.Join(project, "missing", "new.txt"), false, filepath.Join(project, "missing", "new.txt")},
		{project, filepath.Join(project, "sub", "in.txt"), false, filepath.Join(project, "a.txt")},
		{project, filepath.Join(project, "notes.txt"), true, key},
		{project, filepath.Join(project, "sub", "out", "key"), true, key},
		{project, filepath.Join(project, "sub", "out"), true, outside},
		{project, key, true, key},
		{project, filepath.Join(project, "dangling"), false, filepath.Join(project, "dangling")},
		{viaLink, filepath.Join(viaLink, "a.txt"), false, filepath.Join(project, "a.txt")},
		{viaLink, filepath.Join(viaLink, "notes.txt"), true, key},
		{project, filepath.Join(viaLink, "a.txt"), true, filepath.Join(project, "a.txt")},
	} {
		outside, real := LeadsOut(tc.dir, tc.path)
		require.Equal(t, tc.outside, outside, "%s in %s", tc.path, tc.dir)
		require.Equal(t, tc.real, real, "%s in %s", tc.path, tc.dir)
	}
}

// A listing follows a link to a directory only when it leads within the
// directory listed.
func TestListDirectoryDoesNotFollowLinksOut(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	project, outside := filepath.Join(base, "project"), filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(project, "pkg"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "id_ed25519"), []byte("k"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(project, "pkg", "lib.go"), []byte("package pkg"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "keys")))
	require.NoError(t, os.Symlink("pkg", filepath.Join(project, "linked")))

	files, _, err := ListDirectory(project, nil, 0, 0)
	require.NoError(t, err)
	names := strings.Join(files, "\n")
	require.Contains(t, names, filepath.Join(project, "keys"))
	require.NotContains(t, names, "id_ed25519")
	require.True(t, slices.Contains(files, filepath.Join(project, "linked", "lib.go")), "a link within the project is followed: %v", files)
}

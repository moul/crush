package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A value's quotes hold the blanks inside them, as git's do: git reads
// " true " as the six characters between the quotes, which is not one of
// its words for true.
func TestGitValueKeepsBlanksInQuotes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ raw, want string }{
		{`" true "`, " true "},
		{`"  pad  "`, "  pad  "},
		{`"  "`, "  "},
		{`a`, "a"},
		{`  a  `, "a"},
		{`"a"  `, "a"},
		{`  "a"`, "a"},
		{"a\tb", "a b"},
		{`a # b`, "a"},
		{`"a # b"`, "a # b"},
		{``, ""},
	} {
		require.Equal(t, c.want, gitValue(c.raw), "gitValue(%q)", c.raw)
	}
}

// A repository whose settings give core.fsmonitor a value that is not one
// of git's words for true names a program: git runs it when the
// repository is merely looked at, so the trust question must show it.
func TestGitProgramsSeesQuotedFsmonitor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	config := "[core]\n\tfsmonitor = \" true \"\n"
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644))

	held := GitPrograms(Folder{GitDir: gitDir, CommonDir: gitDir})
	require.Len(t, held, 1)
	require.True(t, strings.HasPrefix(held[0].Text, "git core.fsmonitor = "), "%q", held[0].Text)
}

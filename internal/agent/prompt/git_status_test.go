package prompt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Where the repository's own git config would have git run programs and
// the folder is not trusted, the prompt says its status was not read, and
// why, naming the settings without their values.
func TestUntrustedGitStatusSaysWhy(t *testing.T) {
	t.Parallel()
	got := untrustedGitStatus([]string{"core.fsmonitor=./x.sh", "hook post-index-change"})
	require.Equal(t, "Status: not read. This folder is not trusted, and the repository's own git config would have git run programs "+
		"(core.fsmonitor, hook post-index-change), so git commands ask the user first.\n", got)
}

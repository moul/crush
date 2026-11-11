package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// Outside a git repository the ls tool is limited to depth 2 and 100
// items. A listing cut at 100 items must still say so when the depth note
// is printed too, or the model takes the first 100 entries for the whole
// directory.
func TestListDirectoryTreeKeepsTruncationNoteWithDepth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 5 {
		name := filepath.Join(dir, fmt.Sprintf("file%d.txt", i))
		require.NoError(t, os.WriteFile(name, []byte("x"), 0o644))
	}

	depth, items := 2, 3
	output, metadata, err := ListDirectoryTree(dir, LSParams{}, config.ToolLs{
		MaxDepth: &depth,
		MaxItems: &items,
	})
	require.NoError(t, err)
	require.True(t, metadata.Truncated)
	require.Contains(t, output, "There are more than 3 files in the directory.")
	require.Contains(t, output, "The directory tree is shown up to a depth of 2.")
}

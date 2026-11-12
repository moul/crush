package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// A configured max_depth adds its own note, and that must not replace the
// note saying the listing was cut at max_items.
func TestListDirectoryTreeKeepsTruncationNoteWithMaxDepth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 10 {
		name := filepath.Join(dir, fmt.Sprintf("file%02d.txt", i))
		require.NoError(t, os.WriteFile(name, nil, 0o644))
	}

	depth, items := 2, 5
	output, metadata, err := ListDirectoryTree(dir, LSParams{}, config.ToolLs{MaxDepth: &depth, MaxItems: &items})
	require.NoError(t, err)
	require.True(t, metadata.Truncated)
	require.Contains(t, output, "There are more than 5 files in the directory.")
	require.Contains(t, output, "The directory tree is shown up to a depth of 2.")
}

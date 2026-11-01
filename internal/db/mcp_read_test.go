package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The MCP servers a sessions database turns on can be read without
// changing it, and a data directory with no database turns none on and
// gets none written.
func TestMCPServersEnabledIn(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := Connect(t.Context(), dataDir)
	require.NoError(t, err)
	require.NoError(t, New(conn).InsertMCPEnabledServer(t.Context(), "helper"))
	require.NoError(t, Release(dataDir))

	names, err := MCPServersEnabledIn(t.Context(), dataDir)
	require.NoError(t, err)
	require.Equal(t, []string{"helper"}, names)

	empty := t.TempDir()
	names, err = MCPServersEnabledIn(t.Context(), empty)
	require.NoError(t, err)
	require.Empty(t, names)
	require.NoFileExists(t, FileName(empty), "reading creates no database")
}

package db

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// MCPServersEnabledIn reads, opening it read-only and changing nothing,
// the MCP servers the sessions database in dataDir turns on (see
// ListMCPEnabledServers); none when dataDir holds no database. It is for
// a database gnosh may not have written: a downloaded repository's .gnosh
// can carry one.
func MCPServersEnabledIn(ctx context.Context, dataDir string) ([]string, error) {
	path := FileName(dataDir)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	conn, err := ConnectReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return New(conn).ListMCPEnabledServers(ctx)
}

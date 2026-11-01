package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// refusingPermissions records each permission request and says no to it.
type refusingPermissions struct {
	mockViewPermissionService
	mu       sync.Mutex
	requests []permission.CreatePermissionRequest
}

func (p *refusingPermissions) Request(_ context.Context, req permission.CreatePermissionRequest) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return false, nil
}

func (p *refusingPermissions) asked() []permission.CreatePermissionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]permission.CreatePermissionRequest(nil), p.requests...)
}

// linkedProject is a project that holds what a downloaded repository can:
// notes.txt, a link to a key outside it, docs, a link to the directory
// the key is in, and alias.txt, a link to a file of its own.
func linkedProject(t *testing.T) (project, outside, key string) {
	t.Helper()
	project, outside = realTempDir(t), realTempDir(t)
	key = filepath.Join(outside, "id_ed25519")
	require.NoError(t, os.WriteFile(key, []byte("SECRET-KEY-LINE\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(project, "a.txt"), []byte("plain line\n"), 0o644))
	require.NoError(t, os.Symlink(key, filepath.Join(project, "notes.txt")))
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "docs")))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(project, "alias.txt")))
	return project, outside, key
}

// realTempDir is a temporary directory by its real path (on macOS the
// temporary directory is under a link, /var to /private/var).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func runTool(t *testing.T, tool fantasy.AgentTool, input any) fantasy.ToolResponse {
	t.Helper()
	b, err := json.Marshal(input)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "s1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: tool.Info().Name + "-call", Name: tool.Info().Name, Input: string(b)})
	require.NoError(t, err)
	return resp
}

// A link in the project that leads out of it is read, listed and searched
// only once the user says yes, and is asked about as the place it leads
// to: a downloaded repository's notes.txt, a link to ~/.ssh/id_ed25519,
// does not hand the agent the key without a word.
func TestLinksOutOfTheProjectAreAskedAbout(t *testing.T) {
	t.Parallel()
	project, outside, key := linkedProject(t)

	for _, tc := range []struct {
		name  string
		tool  func(permission.Service) fantasy.AgentTool
		input any
		path  string
	}{
		{"view", func(p permission.Service) fantasy.AgentTool {
			return NewViewTool(nil, p, mockFileTracker{}, nil, nil, nil, project)
		}, ViewParams{FilePath: "notes.txt"}, key},
		{"ls", func(p permission.Service) fantasy.AgentTool {
			return NewLsTool(p, project, config.ToolLs{})
		}, LSParams{Path: "docs"}, outside},
		{"grep a link to a file", func(p permission.Service) fantasy.AgentTool {
			return NewGrepTool(p, project, config.ToolGrep{})
		}, GrepParams{Pattern: "SECRET", Path: "notes.txt"}, key},
		{"grep a link to a directory", func(p permission.Service) fantasy.AgentTool {
			return NewGrepTool(p, project, config.ToolGrep{})
		}, GrepParams{Pattern: "SECRET", Path: "docs"}, outside},
		{"glob", func(p permission.Service) fantasy.AgentTool {
			return NewGlobTool(p, project, config.ToolGlob{})
		}, GlobParams{Pattern: "*", Path: "docs"}, outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			perms := &refusingPermissions{}
			resp := runTool(t, tc.tool(perms), tc.input)
			require.NotContains(t, resp.Content, "SECRET")
			require.NotContains(t, resp.Content, "id_ed25519")
			asked := perms.asked()
			require.Len(t, asked, 1)
			require.Equal(t, tc.path, asked[0].Path, "asked about where the link leads")
			require.Contains(t, asked[0].Description, "outside working directory: "+tc.path)
			require.Contains(t, asked[0].Description, "(the link "+project+string(filepath.Separator))
			// The read and list questions show the path asked for, and
			// beside it where the link leads.
			switch params := asked[0].Params.(type) {
			case ViewPermissionsParams:
				require.Equal(t, tc.path, params.LeadsTo)
			case LSPermissionsParams:
				require.Equal(t, tc.path, params.LeadsTo)
			}
		})
	}
}

// Nothing is asked for a file of the project, a link that stays in it, or
// a search or listing of the whole project; and those never go through
// the links that lead out: the listing names docs and notes.txt but not
// what they lead to, and a search of the project does not find the key.
func TestLinksOutOfTheProjectAreNotFollowedOnTheWay(t *testing.T) {
	t.Parallel()
	project, _, _ := linkedProject(t)
	perms := &refusingPermissions{}

	view := NewViewTool(nil, perms, mockFileTracker{}, nil, nil, nil, project)
	require.Contains(t, runTool(t, view, ViewParams{FilePath: "a.txt"}).Content, "plain line")
	require.Contains(t, runTool(t, view, ViewParams{FilePath: "alias.txt"}).Content, "plain line")

	listing := runTool(t, NewLsTool(perms, project, config.ToolLs{}), LSParams{}).Content
	require.Contains(t, listing, "docs")
	require.Contains(t, listing, "notes.txt")
	require.NotContains(t, listing, "id_ed25519")

	found := runTool(t, NewGrepTool(perms, project, config.ToolGrep{}), GrepParams{Pattern: "SECRET|plain"}).Content
	require.Contains(t, found, "a.txt")
	require.NotContains(t, found, "SECRET")
	require.NotContains(t, found, "notes.txt")

	matches, err := searchFilesWithRegex("SECRET|plain", project, grepOptions{}, 100)
	require.NoError(t, err)
	for _, m := range matches {
		require.NotContains(t, m.lineText, "SECRET", "the Go search, used without ripgrep, skips links too")
	}
	require.NotEmpty(t, matches)

	require.Empty(t, perms.asked())
}

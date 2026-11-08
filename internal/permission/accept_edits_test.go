package permission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runsUnasked reports whether s lets tool's edit of path run without
// asking. Nobody answers here, so a request that is asked ends with the
// cancelled wait; one that is let run returns at once.
func runsUnasked(t *testing.T, ctx context.Context, s Service, tool, path string) bool {
	t.Helper()
	return runsUnaskedAt(t, ctx, s, tool, path, filepath.Dir(path))
}

// runsUnaskedAt is runsUnasked for a request whose Path is requestPath:
// the edit tools give the working directory for any file in it.
func runsUnaskedAt(t *testing.T, ctx context.Context, s Service, tool, path, requestPath string) bool {
	t.Helper()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	granted, err := s.Request(cancelled, CreatePermissionRequest{
		SessionID: "s1", ToolCallID: "call-1", ToolName: tool, Action: "write",
		Path: requestPath, FilePath: path,
	})
	if granted {
		require.NoError(t, err)
		return true
	}
	require.ErrorIs(t, err, context.Canceled, "%s %s was neither let run nor asked about", tool, path)
	return false
}

// As in Claude Code, acceptEdits lets the file edit tools change files in
// the project and in directories added with --add-dir without asking, and
// nothing else: a file elsewhere, or one that runs code or holds settings
// (a git hook, a shell's startup file, Gnosh's or Claude Code's config),
// is asked about as without the mode, as is anything a symlink leads out
// to. It used to let them write anywhere.
func TestAcceptEditsLetsEditsInTheProjectRun(t *testing.T) {
	t.Parallel()

	project, added, outside := t.TempDir(), t.TempDir(), t.TempDir()
	for _, f := range []string{filepath.Join(project, "a.go"), filepath.Join(outside, "target.go"), filepath.Join(project, ".git", "config")} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f), 0o755))
		require.NoError(t, os.WriteFile(f, []byte("x\n"), 0o644))
	}
	require.NoError(t, os.Symlink(filepath.Join(outside, "target.go"), filepath.Join(project, "out.go")))
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "outdir")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "nothing.go"), filepath.Join(project, "dangling.go")))
	require.NoError(t, os.Symlink(filepath.Join(project, ".git"), filepath.Join(project, "gitdir")))
	require.NoError(t, os.Mkdir(filepath.Join(project, "editor"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(project, "editor"), filepath.Join(project, ".vscode")))
	s := New(Options{WorkingDir: project, AddedDirs: []string{added}, AcceptEdits: true})

	for path, want := range map[string]bool{
		filepath.Join(project, "a.go"):                        true,
		filepath.Join(project, "new", "dir", "b.go"):          true,
		filepath.Join(added, "c.go"):                          true,
		filepath.Join(outside, "d.go"):                        false,
		filepath.Join(project, ".git", "hooks", "pre-commit"): false,
		filepath.Join(project, "sub", ".git", "config"):       false,
		filepath.Join(project, ".bashrc"):                     false,
		filepath.Join(project, ".zshrc"):                      false,
		filepath.Join(project, "gnosh.json"):                  false,
		filepath.Join(project, ".gnosh", "crush.db"):          false,
		filepath.Join(project, ".claude", "settings.json"):    false,
		filepath.Join(project, ".config", "git", "config"):    false,
		filepath.Join(project, "out.go"):                      false,
		filepath.Join(project, "outdir", "e.go"):              false,
		filepath.Join(project, "dangling.go"):                 false,
		filepath.Join(project, "gitdir", "config"):            false,
		filepath.Join(project, ".vscode", "settings.json"):    false,
		filepath.Join(project, ".GIT", "hooks", "pre-commit"): false,
		filepath.Join(project, "GNOSH.JSON"):                  false,
		filepath.Join(project, ".Claude", "settings.json"):    false,
		filepath.Join(project, ".config", "GIT", "config"):    false,
		filepath.Join(project, ".ssh", "authorized_keys"):     false,
		filepath.Join(project, ".gnupg", "gpg.conf"):          false,
		"a.go": false,
		"":     false,
	} {
		require.Equalf(t, want, runsUnasked(t, t.Context(), s, "edit", path), "edit %q", path)
	}
	for _, tool := range []string{"write", "multiedit"} {
		require.True(t, runsUnasked(t, t.Context(), s, tool, filepath.Join(project, "a.go")), tool)
	}
	for _, tool := range []string{"bash", "lsp_rename", "lsp_replace_symbol", "download"} {
		require.False(t, runsUnasked(t, t.Context(), s, tool, filepath.Join(project, "a.go")), "%s is no file edit acceptEdits lets run", tool)
	}
	require.False(t, runsUnasked(t, WithHookAsk(t.Context(), "call-1"), s, "edit", filepath.Join(project, "a.go")),
		"a hook's ask still goes to the user")
}

// Without acceptEdits, an edit in the project is asked about.
func TestWithoutAcceptEditsEditsAsk(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	s := New(Options{WorkingDir: project})
	require.False(t, runsUnasked(t, t.Context(), s, "edit", filepath.Join(project, "a.go")))
}

// A change of a protected file is asked about however its tool is allowed:
// by name in the allowed tools, or by an earlier "allow for this session"
// of a change in the project, which covers the project's other files. As
// in Claude Code, whose allow rules do not pre-approve protected-path
// writes, only yolo, a session that grants all its requests, and a hook
// let it run unasked.
func TestProtectedFilesAreAskedAboutHoweverTheToolIsAllowed(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	plain := filepath.Join(project, "a.go")
	hook := filepath.Join(project, ".git", "hooks", "pre-commit")

	dangling := filepath.Join(project, "dangling.go")
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "nothing.go"), dangling))

	allowed := New(Options{WorkingDir: project, AllowedTools: []string{"edit", "write:write", "download"}})
	require.True(t, runsUnasked(t, t.Context(), allowed, "edit", plain))
	require.False(t, runsUnasked(t, t.Context(), allowed, "edit", hook))
	require.False(t, runsUnasked(t, t.Context(), allowed, "edit", dangling), "a link that leads nowhere")
	require.False(t, runsUnasked(t, t.Context(), allowed, "write", filepath.Join(project, ".zshrc")))
	require.False(t, runsUnasked(t, t.Context(), allowed, "download", filepath.Join(project, ".claude", "settings.json")))

	// "Allow for this session" of an edit in the project.
	granted := New(Options{WorkingDir: project})
	events := granted.Subscribe(t.Context())
	done := make(chan bool, 1)
	go func() {
		ok, _ := granted.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolCallID: "call-0", ToolName: "edit", Action: "write", Path: project, FilePath: plain,
		})
		done <- ok
	}()
	granted.GrantPersistent((<-events).Payload)
	require.True(t, <-done)
	require.True(t, runsUnaskedAt(t, t.Context(), granted, "edit", filepath.Join(project, "b.go"), project), "the grant covers the project")
	require.False(t, runsUnaskedAt(t, t.Context(), granted, "edit", hook, project), "but not a git hook in it")

	yolo := New(Options{WorkingDir: project, Skip: true})
	require.True(t, runsUnasked(t, t.Context(), yolo, "edit", hook))
	auto := New(Options{WorkingDir: project})
	auto.AutoApproveSession("s1")
	require.True(t, runsUnasked(t, t.Context(), auto, "edit", hook), "a gnosh run's session")
	require.True(t, runsUnasked(t, WithHookApproval(t.Context(), "call-1"), New(Options{WorkingDir: project}), "edit", hook), "a hook's allow")
}

// A grant for the session covers a file only where it really is: a link in
// the project to a file outside it is asked about, as is a file in a
// directory given as protected (the session env files'), in any case.
func TestGrantsCoverFilesWhereTheyReallyAre(t *testing.T) {
	t.Parallel()

	project, outside, envDir := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "notes.txt"), []byte("x\n"), 0o644))
	link := filepath.Join(project, "notes.txt")
	require.NoError(t, os.Symlink(filepath.Join(outside, "notes.txt"), link))

	granted := New(Options{WorkingDir: project})
	events := granted.Subscribe(t.Context())
	done := make(chan bool, 1)
	go func() {
		ok, _ := granted.Request(t.Context(), CreatePermissionRequest{
			SessionID: "s1", ToolCallID: "call-0", ToolName: "write", Action: "write", Path: project, FilePath: filepath.Join(project, "a.go"),
		})
		done <- ok
	}()
	granted.GrantPersistent((<-events).Payload)
	require.True(t, <-done)
	require.True(t, runsUnaskedAt(t, t.Context(), granted, "write", filepath.Join(project, "b.go"), project))
	require.False(t, runsUnaskedAt(t, t.Context(), granted, "write", link, project), "a link out of the project")

	allowed := New(Options{WorkingDir: project, AllowedTools: []string{"write", "lsp_rename"}, ProtectedPaths: []string{envDir}})
	require.True(t, runsUnasked(t, t.Context(), allowed, "write", filepath.Join(project, "b.go")))
	require.False(t, runsUnasked(t, t.Context(), allowed, "write", filepath.Join(envDir, "s1.sh")), "a session env file")
	require.False(t, runsUnasked(t, t.Context(), allowed, "write", strings.ToUpper(envDir)+"/S1.SH"), "in any case")

	rename := func(files ...string) bool {
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		ok, err := allowed.Request(cancelled, CreatePermissionRequest{SessionID: "s1", ToolCallID: "call-2", ToolName: "lsp_rename", FilePaths: files})
		if !ok {
			require.ErrorIs(t, err, context.Canceled)
		}
		return ok
	}
	require.True(t, rename(filepath.Join(project, "a.go"), filepath.Join(project, "b.go")))
	require.False(t, rename(filepath.Join(project, "a.go"), filepath.Join(project, ".git", "config")), "a rename that reaches a protected file")
}

// The request put to the user says whether a grant for the session would
// cover it: not for a protected file, a link out of the project, or a
// hook's question, which are asked about every time.
func TestRequestsSayWhetherASessionGrantCoversThem(t *testing.T) {
	t.Parallel()

	project, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(project, "notes.txt")
	require.NoError(t, os.Symlink(filepath.Join(outside, "notes.txt"), link))
	s := New(Options{WorkingDir: project})
	events := s.Subscribe(t.Context())

	alwaysAsks := func(ctx context.Context, path string) bool {
		t.Helper()
		cancelled, cancel := context.WithCancel(ctx)
		go func() {
			_, _ = s.Request(cancelled, CreatePermissionRequest{
				SessionID: "s1", ToolCallID: "call-1", ToolName: "write", Action: "write", Path: project, FilePath: path,
			})
		}()
		req := (<-events).Payload
		cancel()
		return req.AlwaysAsks
	}
	require.False(t, alwaysAsks(t.Context(), filepath.Join(project, "a.go")))
	require.True(t, alwaysAsks(t.Context(), filepath.Join(project, ".git", "config")))
	require.True(t, alwaysAsks(t.Context(), link))
	require.True(t, alwaysAsks(WithHookAsk(t.Context(), "call-1"), filepath.Join(project, "a.go")))
}

// A gnoshrc is a Bash script that runs as Gnosh starts in the folder, so a
// change of one, under any of the names a project config file can have, is
// asked about in acceptEdits as a change of gnosh.json is.
func TestAcceptEditsAsksAboutShellConfigs(t *testing.T) {
	t.Parallel()

	project := t.TempDir()
	s := New(Options{WorkingDir: project, AcceptEdits: true})
	for _, name := range []string{".gnoshrc", "gnoshrc", ".klashrc", "klashrc", ".crushrc", "crushrc", "GNOSHRC"} {
		require.False(t, runsUnasked(t, t.Context(), s, "edit", filepath.Join(project, name)), "edit %s", name)
		require.False(t, runsUnasked(t, t.Context(), s, "write", filepath.Join(project, "sub", name)), "write sub/%s", name)
	}
	require.True(t, runsUnasked(t, t.Context(), s, "edit", filepath.Join(project, "gnoshrc.md")))
}

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// --fork-session copies the session resumed, so with no session named,
// none to pick (a bare -r) and no --continue there is nothing to copy:
// gnosh and gnosh run say so instead of starting a new session.
func TestForkAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		args  []string
		alone bool
	}{
		{[]string{"--fork-session"}, true},
		{[]string{"--fork-session", "-C"}, false},
		{[]string{"--fork-session", "-s", "abc"}, false},
		{[]string{"--fork-session", "-r", "abc"}, false},
		{[]string{"--fork-session", "--resume=" + pickSession}, false},
		{nil, false},
		{[]string{"-C"}, false},
	} {
		for _, add := range []func(*cobra.Command){addSessionFlags, addRunFlags} {
			cmd := &cobra.Command{Use: "gnosh"}
			add(cmd)
			require.NoError(t, cmd.ParseFlags(tc.args))
			if tc.alone {
				require.ErrorIs(t, forkAlone(cmd), errForkAlone, tc.args)
			} else {
				require.NoError(t, forkAlone(cmd), tc.args)
			}
		}
	}
}

// `gnosh run --fork-session`, as Claude Code's `claude -p --resume ID
// --fork-session`: the run goes on in a copy of the session it resumes,
// the most recent with -C or one by ID, and the session is left as it
// was. The copy's first prompt runs the SessionStart hooks with source
// "fork", and going back to the copy later is a resume.
func TestRunForkSession(t *testing.T) {
	sources := filepath.Join(t.TempDir(), "sources")
	proj := standInRunProject(t, `"hooks": {"SessionStart": [{"command": "echo \"$GNOSH_SESSION_START_SOURCE\" >> `+sources+`"}]}`)
	original := runSessionID(t, "list the files here")
	fork := runSessionID(t, "--fork-session", "-C", "list the files here")
	byID := runSessionID(t, "--fork-session", "-s", original, "list the files here")
	require.NotEqual(t, original, fork)
	require.NotContains(t, []string{original, fork}, byID)
	require.Equal(t, fork, runSessionID(t, "-s", fork, "list the files here"))
	data, err := os.ReadFile(sources)
	require.NoError(t, err)
	require.Equal(t, "startup\nfork\nfork\nresume\n", string(data))

	_, err = runForTest(t, "--fork-session", "list the files here")
	require.ErrorIs(t, err, errForkAlone)

	messages := message.NewService(projectQueries(t, proj))
	conversation := func(sessionID string) []message.Message {
		t.Helper()
		msgs, err := messages.List(t.Context(), sessionID)
		require.NoError(t, err)
		return msgs
	}
	// The stand-in answers a turn with a call to ls and then the answer,
	// and a turn whose conversation has ls's result at once.
	turn := conversation(original)
	require.Len(t, turn, 4, "the session forked is left as it was: its one turn")
	for id, later := range map[string]int{byID: 1, fork: 2} {
		copied := conversation(id)
		require.Len(t, copied, len(turn)+2*later, "the session's turn, then the copy's own")
		for i, msg := range turn {
			require.Equal(t, msg.Parts, copied[i].Parts)
		}
	}
}

// standInRunProject makes a project of its own the working directory, its
// gnosh.json using standInModel for both models, with more, when given, at
// the end of the config, and HOME and the global config and data dirs in a
// directory of their own. It returns the project's directory.
func standInRunProject(t *testing.T, more string) string {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("GNOSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "gnosh"))
	t.Setenv("GNOSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "gnosh"))
	for _, name := range []string{"GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "GNOSH_DISABLE_DEFAULT_PROVIDERS", "GNOSH_DISABLE_METRICS", "GNOSH_DISABLE_UPDATE_CHECK"} {
		t.Setenv(name, "1")
	}
	srv := standInModel(t)
	proj := t.TempDir()
	cfg := `{"providers": {"standin": {"type": "llamacpp", "base_url": "` + srv.URL + `"}},
		"models": {"large": {"model": "stand-in", "provider": "standin"}, "small": {"model": "stand-in", "provider": "standin"}}`
	if more != "" {
		cfg += ",\n\t\t" + more
	}
	require.NoError(t, os.WriteFile(filepath.Join(proj, "gnosh.json"), []byte(cfg+"}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "a.txt"), []byte("alpha\n"), 0o644))
	t.Chdir(proj)
	trustForTest(t, proj)
	return proj
}

// runSessionID runs `gnosh run --output-format json args...` against the
// stand-in model and returns the session it went on in.
func runSessionID(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runForTest(t, append([]string{"--output-format", "json"}, args...)...)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	return res["session_id"].(string)
}

// projectQueries opens the database of the project in proj, for a test to
// read what its runs left. Runs in this process share its connection, so
// it is released, not closed.
func projectQueries(t *testing.T, proj string) *db.Queries {
	t.Helper()
	dir := filepath.Join(proj, ".gnosh")
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dir) })
	return db.New(conn)
}

// `gnosh run -n`, as Claude Code's `claude -p --name`: a new session is
// named from its start and keeps the name, as no title is made for it, so
// -r finds it by it; with -C the session resumed is renamed, and with
// --fork-session the copy is named and the session left as it was.
func TestRunName(t *testing.T) {
	proj := standInRunProject(t, "")
	named := runSessionID(t, "-n", "  nightly\tcheck ", "list the files here")
	q := projectQueries(t, proj)
	title := func(id string) string {
		t.Helper()
		row, err := q.GetSessionByID(t.Context(), id)
		require.NoError(t, err)
		return row.Title
	}
	require.Equal(t, "nightly check", title(named), "cleaned as /rename cleans a name, and kept")
	require.Equal(t, named, runSessionID(t, "-r", "nightly check", "list the files here"), "found by it")

	require.Equal(t, named, runSessionID(t, "-C", "-n", "renamed", "list the files here"))
	require.Equal(t, "renamed", title(named))

	copied := runSessionID(t, "--fork-session", "-C", "-n", "the copy", "list the files here")
	require.NotEqual(t, named, copied)
	require.Equal(t, "the copy", title(copied))
	require.Equal(t, "renamed", title(named))
}

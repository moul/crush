package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/stretchr/testify/require"
)

// A command is a wait Claude Code blocks when its first command, of the
// lists and pipelines it is made of, is a plain sleep of 25 seconds or
// more, redirected or not; said in its words, with what follows it. A
// sleep later on, inside a loop or subshell, shorter, in the background,
// or not plainly in seconds (1m, 1e3, +30) is no such wait, nor is what
// does not parse.
func TestForegroundSleep(t *testing.T) {
	t.Parallel()

	for command, want := range map[string]string{
		"sleep 30":                       "standalone sleep 30",
		"sleep 25":                       "standalone sleep 25",
		"sleep 30.5":                     "standalone sleep 30.5",
		"sleep 30.":                      "standalone sleep 30",
		"sleep 30 > /dev/null":           "standalone sleep 30",
		"sleep 30 && tail -5 build.log":  "sleep 30 followed by: tail -5 build.log",
		"sleep 60; ps aux | grep ffmpeg": "sleep 60 followed by: ps aux grep ffmpeg",
		"sleep 24.9":                     "",
		"python3 x.py && sleep 30":       "",
		"until [ -f out.mp4 ]; do sleep 30; done": "",
		"(sleep 30)":   "",
		"sleep 30 &":   "",
		"X=1 sleep 30": "",
		"sleep 1m":     "",
		"sleep 1e3":    "",
		"sleep +30":    "",
		"sleep $T":     "",
		"sleep 30 40":  "",
		"sleep":        "",
		"sleep 30 &&":  "",
	} {
		require.Equal(t, want, foregroundSleep(command), command)
	}
}

// Where the agent is told when a background command ends, bash refuses a
// command that starts with a long sleep, in Claude Code's words, before
// asking to run it, and its description says so. In the background it
// runs, and so it does where nothing tells the agent: a sub-agent's bash,
// or gnosh run's.
func TestBashBlocksALongForegroundSleep(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	perms := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	watcher := func(context.Context, *shell.BackgroundShell) {}
	tool := NewBashTool(perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil, watcher, nil, nil)

	resp := runBashTool(t, tool, ctx, BashParams{Description: "wait", Command: "sleep 30 && tail -5 build.log"})
	require.True(t, resp.IsError)
	require.Equal(t, fmt.Sprintf(sleepBlocked, "sleep 30 followed by: tail -5 build.log"), resp.Content)
	require.Zero(t, perms.requestCount, "refused before asking")
	require.Contains(t, tool.Info().Description, "Foreground `sleep` of 25 seconds or more is blocked")

	resp = runBashTool(t, tool, ctx, BashParams{Description: "wait", Command: "sleep 30", RunInBackground: true})
	require.False(t, resp.IsError, resp.Content)
	id, ok := strings.CutPrefix(strings.SplitN(resp.Content, "\n", 2)[0], "Background shell started with ID: ")
	require.True(t, ok, resp.Content)
	_ = shell.GetBackgroundShellManager().Kill(id)

	unwatched := NewBashTool(perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil, nil, nil, nil)
	require.NotContains(t, unwatched.Info().Description, "is blocked")
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	input, err := json.Marshal(BashParams{Description: "wait", Command: "sleep 30"})
	require.NoError(t, err)
	resp, err = unwatched.Run(short, fantasy.ToolCall{ID: "call-2", Name: BashToolName, Input: string(input)})
	require.ErrorIs(t, err, context.DeadlineExceeded, "it ran until the deadline")
	require.NotContains(t, resp.Content, "Blocked:")
}

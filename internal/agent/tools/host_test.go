package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// capturingPermissionService keeps the last request, to see what a host
// run asks for.
type capturingPermissionService struct {
	*recordingPermissionService
	last permission.CreatePermissionRequest
}

func (c *capturingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	c.last = req
	return c.recordingPermissionService.Request(ctx, req)
}

func newBashToolWithHosts(workingDir string, hosts config.Hosts, allow bool) (fantasy.AgentTool, *capturingPermissionService) {
	perms := &capturingPermissionService{recordingPermissionService: &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  allow,
	}}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	return NewBashTool(perms, workingDir, workingDir, attribution, "test-model", config.DefaultMaxBackgroundJobs, nil, nil, nil, hosts, nil), perms
}

// onPath puts a script of the given name first on the PATH, so a host run
// (which runs ssh) can be checked with no machine to reach.
func onPath(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script on the PATH")
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeSSH prints the arguments ssh was given, one per line.
func fakeSSH(t *testing.T) {
	t.Helper()
	onPath(t, "ssh", `for a in "$@"; do printf 'arg: %s\n' "$a"; done`+"\n")
}

func sessionCtx() context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, "test-session")
}

func TestBashTool_HostRunsThroughSSH(t *testing.T) {
	fakeSSH(t)
	hosts := config.Hosts{"render": {SSH: "root@203.0.113.10", Description: "renders"}}
	tool, perms := newBashToolWithHosts(t.TempDir(), hosts, true)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls /root/renders", Host: "render", WorkingDir: "/root"})
	require.False(t, resp.IsError, resp.Content)
	for _, want := range []string{
		"arg: -o\narg: BatchMode=yes\n",
		"arg: -o\narg: ConnectTimeout=20\n",
		"arg: -o\narg: StrictHostKeyChecking=accept-new\n",
		"arg: --\narg: root@203.0.113.10\n",
		"arg: cd /root || exit\nls /root/renders\n",
		"<host>render (root@203.0.113.10)</host>\n<cwd>/root</cwd>",
	} {
		require.Contains(t, resp.Content, want)
	}
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "render", meta.Host)
	require.Equal(t, "root@203.0.113.10", meta.Machine)
	require.Equal(t, "/root", meta.WorkingDirectory)

	// ls is a read-only command here; on another machine it still asks,
	// and the grant is keyed on that machine, not on a local directory.
	require.Equal(t, 1, perms.requestCount)
	require.Equal(t, "ssh://root@203.0.113.10/root", perms.last.Path)
	require.Equal(t, "Execute on render (root@203.0.113.10): ls /root/renders", perms.last.Description)
	require.Equal(t, "render", perms.last.Params.(BashPermissionsParams).Host)
}

func TestBashTool_HostCommandReachesTheRemoteShellAsWritten(t *testing.T) {
	fakeSSH(t)
	tool, _ := newBashToolWithHosts(t.TempDir(), config.Hosts{"render": {SSH: "root@203.0.113.10"}}, true)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: `echo "it's $HOME"; ls 'a b'`, Host: "render"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "arg: echo \"it's $HOME\"; ls 'a b'\n", "quoted once for the local shell, expanded only on the host")
	require.Contains(t, resp.Content, "<host>render (root@203.0.113.10)</host>")
	require.NotContains(t, resp.Content, "<cwd>", "where the command ended up on the host is not known")
}

func TestBashTool_HostIsDeniedWhenTheUserSaysNo(t *testing.T) {
	fakeSSH(t)
	tool, perms := newBashToolWithHosts(t.TempDir(), config.Hosts{"render": {SSH: "root@203.0.113.10"}}, false)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls", Host: "render"})
	require.Equal(t, 1, perms.requestCount)
	require.Contains(t, resp.Content, "User denied permission")
	require.NotContains(t, resp.Content, "arg:", "ssh never ran")
}

func TestBashTool_UnknownHostNamesTheDeclaredOnes(t *testing.T) {
	tool, perms := newBashToolWithHosts(t.TempDir(), config.Hosts{"render": {SSH: "root@203.0.113.10"}, "vlm": {SSH: "root@203.0.113.11"}}, true)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls", Host: "rendr"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, `host "rendr" is not declared`)
	require.Contains(t, resp.Content, "render, vlm")
	require.Contains(t, resp.Content, config.LocalMachine())
	require.Equal(t, 0, perms.requestCount, "nothing to ask about")
}

func TestBashTool_HostWithNoneDeclaredSaysToAskTheUser(t *testing.T) {
	tool, _ := newBashToolWithHosts(t.TempDir(), nil, true)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls", Host: "render"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no other machine is declared")
	require.Contains(t, resp.Content, "ask the user")
}

func TestBashTool_EveryResultSaysWhereItRan(t *testing.T) {
	tool, _ := newBashToolWithHosts(t.TempDir(), nil, true)
	stamp := "<host>local (" + config.LocalMachine() + ")</host>"

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "echo hi"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "hi")
	require.Contains(t, resp.Content, stamp+"\n<cwd>")

	// "local" names this machine too.
	resp = runBashTool(t, tool, sessionCtx(), BashParams{Command: "echo hi", Host: "local"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, stamp)

	// So does a command with nothing to show.
	resp = runBashTool(t, tool, sessionCtx(), BashParams{Command: "true"})
	require.False(t, resp.IsError, resp.Content)
	require.True(t, strings.HasPrefix(resp.Content, BashNoOutput+"\n\n"+stamp), resp.Content)
}

func TestBashDescriptionListsTheHosts(t *testing.T) {
	t.Parallel()

	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	hosts := config.Hosts{"render": {SSH: "root@203.0.113.10", Description: "renders the videos"}, "vlm": {SSH: "vlm-box"}}
	d := bashDescription(attribution, "m", nil, false, hosts)
	require.Contains(t, d, "This machine is "+config.LocalMachine())
	require.Contains(t, d, "- render (root@203.0.113.10): renders the videos\n- vlm (vlm-box)\n")
	require.Contains(t, d, "A path belongs to the machine it was seen on")

	none := bashDescription(attribution, "m", nil, false, nil)
	require.Contains(t, none, "No other machine is declared")
	require.Contains(t, none, "ask the user to declare it")
}

// `host` is a config builtin (host add ...) and the DNS lookup; in the
// bash tool the one on the PATH runs.
func TestBashTool_ConfigBuiltinStepsAsideForTheCommandOfThatName(t *testing.T) {
	onPath(t, "host", `echo "looked up $1"`+"\n")
	tool, _ := newBashToolWithHosts(t.TempDir(), nil, true)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "host example.com"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "looked up example.com")
}

func TestBashTool_HostCommandObeysTheDenyRules(t *testing.T) {
	fakeSSH(t)
	perms := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	attribution := &config.Attribution{TrailerStyle: config.TrailerStyleNone}
	hosts := config.Hosts{"render": {SSH: "root@203.0.113.10"}}
	tool := NewBashTool(perms, t.TempDir(), t.TempDir(), attribution, "test-model", config.DefaultMaxBackgroundJobs, []string{"rm -rf *"}, nil, nil, hosts, nil)

	resp := runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls /root/renders; rm -rf /root/renders", Host: "render"})
	require.True(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "Bash(rm -rf *) deny rule")
	require.NotContains(t, resp.Content, "arg:", "ssh never ran")
	require.Equal(t, 0, perms.requestCount, "refused before anyone is asked")

	resp = runBashTool(t, tool, sessionCtx(), BashParams{Command: "ls /root/renders", Host: "render"})
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "arg: ls /root/renders\n")
}

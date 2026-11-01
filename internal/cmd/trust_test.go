package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// trustForTest records proj as trusted, in the test's own data directory,
// as the user who wrote its gnosh.json would have.
func trustForTest(t *testing.T, proj string) {
	t.Helper()
	_, err := config.TrustFolder(proj)
	require.NoError(t, err)
}

// runCapturing is runForTest, returning what the run wrote to stderr too.
func runCapturing(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	require.NoError(t, err)
	in, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	require.NoError(t, err)
	oldOut, oldIn := os.Stdout, os.Stdin
	os.Stdout, os.Stdin = out, in
	defer func() { os.Stdout, os.Stdin = oldOut, oldIn }()

	var errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "run", RunE: runCmd.RunE}
	addRunFlags(cmd)
	cmd.SetArgs(append([]string{"--quiet", "-m", "standin/stand-in", "--small-model", "standin/stand-in"}, args...))
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	runErr := cmd.Execute()
	b, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	return string(b), errBuf.String(), runErr
}

// standInGlobalProject is standInRunProject with the stand-in model in the
// user's own config, so that a run works whether or not the project is
// trusted, and the project's settings what is given: its gnosh.json and
// its gnoshrc.
func standInGlobalProject(t *testing.T, projectJSON, gnoshrc string) string {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	global := filepath.Join(isolated, ".config", "gnosh")
	t.Setenv("GNOSH_GLOBAL_CONFIG", global)
	t.Setenv("GNOSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "gnosh"))
	for _, name := range []string{"GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "GNOSH_DISABLE_DEFAULT_PROVIDERS", "GNOSH_DISABLE_METRICS", "GNOSH_DISABLE_UPDATE_CHECK"} {
		t.Setenv(name, "1")
	}
	srv := standInModel(t)
	cfg := `{"providers": {"standin": {"type": "llamacpp", "base_url": "` + srv.URL + `"}},
		"models": {"large": {"model": "stand-in", "provider": "standin"}, "small": {"model": "stand-in", "provider": "standin"}}}`
	require.NoError(t, os.MkdirAll(global, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(global, "gnosh.json"), []byte(cfg), 0o600))
	proj := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(proj, "gnosh.json"), []byte(projectJSON), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, ".gnoshrc"), []byte(gnoshrc), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "a.txt"), []byte("alpha\n"), 0o644))
	t.Chdir(proj)
	return proj
}

// gnosh run never asks: in a folder not trusted, it runs the turn without
// the folder's settings (its gnoshrc does not run, its hooks do not fire)
// and says so in one line on stderr, with how to trust it. --trust-folder
// trusts it for the run, and gnosh trust for good.
func TestRunInAnUntrustedFolder(t *testing.T) {
	rcRan := filepath.Join(t.TempDir(), "rc-ran")
	hookRan := filepath.Join(t.TempDir(), "hook-ran")
	proj := standInGlobalProject(t,
		`{"hooks": {"SessionStart": [{"command": "touch `+hookRan+`"}]}}`,
		"echo ran > "+rcRan+"\n")

	text, stderr, err := runCapturing(t, "list the files here")
	require.NoError(t, err)
	require.Equal(t, "Let me look.\n\nThere are two text files.\n", text, "the turn runs on the user's own settings")
	require.NoFileExists(t, rcRan, "an untrusted folder's gnoshrc must not run")
	require.NoFileExists(t, hookRan, "an untrusted folder's hooks must not run")
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	require.Len(t, lines, 1, stderr)
	require.Contains(t, lines[0], "is not trusted, so its project settings are off (runs .gnoshrc; 1 hook)")
	require.Contains(t, lines[0], "gnosh trust ")
	require.Contains(t, lines[0], "--trust-folder for one run")

	_, stderr, err = runCapturing(t, "--trust-folder", "list the files here")
	require.NoError(t, err)
	require.Empty(t, stderr)
	require.FileExists(t, rcRan)
	require.FileExists(t, hookRan)
	require.False(t, config.FolderTrusted(proj), "--trust-folder records nothing")

	require.NoError(t, os.Remove(rcRan))
	require.NoError(t, os.Remove(hookRan))
	out, err := runTrust(t, proj)
	require.NoError(t, err)
	require.Contains(t, out, "Trusted ")
	_, stderr, err = runCapturing(t, "list the files here")
	require.NoError(t, err)
	require.Empty(t, stderr)
	require.FileExists(t, rcRan)
	require.FileExists(t, hookRan)
}

// runTrust runs `gnosh trust args...` and returns what it printed.
func runTrust(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{Use: "trust", RunE: trustCmd.RunE, Args: trustCmd.Args}
	cmd.Flags().Bool("list", false, "")
	cmd.Flags().Bool("forget", false, "")
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

// gnosh trust records a folder, says what its settings now do, lists the
// trusted folders and forgets one.
func TestTrustCommand(t *testing.T) {
	proj := standInGlobalProject(t, `{"mcp": {"helper": {"command": "./mcp-server"}}}`, "option debug true\n")

	out, err := runTrust(t, proj)
	require.NoError(t, err)
	require.Contains(t, out, "Trusted ")
	require.Contains(t, out, "  - run .gnoshrc, a shell script, as gnosh starts\n")
	require.Contains(t, out, "  - start 1 MCP server: helper\n")
	require.Contains(t, out, "What could go wrong:\n  - Its gnoshrc and MCP server would run as you: ")
	real, err := filepath.EvalSymlinks(proj)
	require.NoError(t, err)
	require.Contains(t, out, "If you did not write them and have not read them: "+config.ForgetCommand(real)+"\n")
	require.True(t, config.FolderTrusted(proj))

	out, err = runTrust(t, "--list")
	require.NoError(t, err)
	require.Contains(t, out, "(trusted ")

	out, err = runTrust(t, "--forget", proj)
	require.NoError(t, err)
	require.Contains(t, out, "No longer trusted: ")
	require.False(t, config.FolderTrusted(proj))

	out, err = runTrust(t, "--list")
	require.NoError(t, err)
	require.Contains(t, out, "No folder is trusted yet")

	_, err = runTrust(t, filepath.Join(proj, "a.txt"))
	require.ErrorContains(t, err, "is not a folder")
}

// A relative --cwd names the same directory when the TUI resolves it the
// second time, to set up the workspace, after asking the trust question
// changed to it.
func TestResolveCwdTwiceWithARelativeCwd(t *testing.T) {
	parent := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(parent, "proj"), 0o755))
	t.Chdir(parent)
	cmd := &cobra.Command{Use: "gnosh"}
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"-c", "proj"}))
	first, err := ResolveCwd(cmd)
	require.NoError(t, err)
	second, err := ResolveCwd(cmd)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(parent, "proj"), first)
	require.Equal(t, first, second)
}

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestProjectsEmpty(t *testing.T) {
	// Use a temp directory for projects.json
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))
	err := projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)
	require.Equal(t, "No projects tracked yet.\n", b.String())
}

func TestProjectsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// Register a project
	err := projects.Register("/test/project", "/test/project/.crush")
	require.NoError(t, err)

	var b bytes.Buffer
	projectsCmd.SetOut(&b)
	projectsCmd.SetErr(&b)
	projectsCmd.SetIn(bytes.NewReader(nil))

	// Set the --json flag
	projectsCmd.Flags().Set("json", "true")
	defer projectsCmd.Flags().Set("json", "false")

	err = projectsCmd.RunE(projectsCmd, nil)
	require.NoError(t, err)

	// Parse the JSON output
	var result struct {
		Projects []projects.Project `json:"projects"`
	}
	err = json.Unmarshal(b.Bytes(), &result)
	require.NoError(t, err)

	require.Len(t, result.Projects, 1)
	require.Equal(t, "/test/project", result.Projects[0].Path)
	require.Equal(t, "/test/project/.crush", result.Projects[0].DataDir)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// what fn wrote there.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()
	fn()
	require.NoError(t, w.Close())
	return string(<-done)
}

func TestProjectsJSONGoesToStdout(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	require.NoError(t, projects.Register("/test/project", "/test/project/.gnosh"))

	// As in the real program, where nothing sets the command's output:
	// what it prints has to reach stdout, so `gnosh projects --json | jq`
	// sees it.
	projectsCmd.SetOut(nil)
	var stderr bytes.Buffer
	projectsCmd.SetErr(&stderr)
	require.NoError(t, projectsCmd.Flags().Set("json", "true"))
	defer projectsCmd.Flags().Set("json", "false")

	out := captureStdout(t, func() {
		require.NoError(t, projectsCmd.RunE(projectsCmd, nil))
	})

	var result struct {
		Projects []projects.Project `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result), "stdout: %q", out)
	require.Len(t, result.Projects, 1)
	require.Equal(t, "/test/project", result.Projects[0].Path)
}

func TestDirsGoesToStdout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	dirsCmd.SetOut(nil)
	var stderr bytes.Buffer
	dirsCmd.SetErr(&stderr)

	// The pipe is not a terminal, so dirs prints one directory a line.
	out := captureStdout(t, func() {
		dirsCmd.Run(dirsCmd, nil)
	})

	require.Equal(t, strings.Join(collectDirs(dirsCmd), "\n")+"\n", out)
}

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// standInModel is an OpenAI-compatible model server for a whole gnosh run
// turn: it answers model discovery (two models, stand-in and stand-in-2),
// gives a title request (one offering no tools) a title, answers the first
// request that offers tools with text and a call to ls, and the request
// carrying ls's result with an answer.
func standInModel(t *testing.T) *httptest.Server {
	t.Helper()
	sse := func(w http.ResponseWriter, chunks ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: {\"choices\":[%s]}\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data": [{"id": "stand-in", "object": "model"}, {"id": "stand-in-2", "object": "model"}]}`)
			return
		}
		type message struct {
			Role string `json:"role"`
		}
		var req struct {
			Tools    []json.RawMessage `json:"tools"`
			Messages []message         `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answered := slices.ContainsFunc(req.Messages, func(m message) bool { return m.Role == "tool" })
		switch {
		case len(req.Tools) == 0:
			sse(w, `{"index":0,"delta":{"role":"assistant","content":"Listing files"}}`, `{"index":0,"delta":{},"finish_reason":"stop"}`)
		case !answered:
			sse(w,
				`{"index":0,"delta":{"role":"assistant","content":"Let me look."}}`,
				`{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ls_1","type":"function","function":{"name":"ls","arguments":"{\"path\": \".\"}"}}]}}`,
				`{"index":0,"delta":{},"finish_reason":"tool_calls"}`)
		default:
			sse(w, `{"index":0,"delta":{"role":"assistant","content":"There are two text files."}}`, `{"index":0,"delta":{},"finish_reason":"stop"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runForTest runs `gnosh run args...` in-process against the stand-in
// model, in a project of its own, and returns what it wrote to stdout.
func runForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runForTestWith(t, append([]string{"--quiet", "-m", "standin/stand-in", "--small-model", "standin/stand-in"}, args...)...)
}

// runForTestWith is runForTest with no model flags but those in args.
func runForTestWith(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	require.NoError(t, err)
	in, err := os.Create(filepath.Join(t.TempDir(), "stdin")) // empty: nothing to prepend
	require.NoError(t, err)
	oldOut, oldIn := os.Stdout, os.Stdin
	os.Stdout, os.Stdin = out, in
	defer func() { os.Stdout, os.Stdin = oldOut, oldIn }()

	cmd := &cobra.Command{Use: "run", RunE: runCmd.RunE}
	addRunFlags(cmd)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	runErr := cmd.Execute()
	b, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	return string(b), runErr
}

// A whole gnosh run turn, a tool call and all, against a stand-in model
// server: text output keeps the steps apart, json's result is the answer,
// and stream-json writes each message in Claude Code's order. Tried by
// hand first, this flow found the step texts run together and json's
// result holding all of them.
func TestRunEndToEnd(t *testing.T) {
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
		"models": {"large": {"model": "stand-in", "provider": "standin"}, "small": {"model": "stand-in", "provider": "standin"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(proj, "gnosh.json"), []byte(cfg), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "a.txt"), []byte("alpha\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "b.txt"), []byte("beta\n"), 0o644))
	t.Chdir(proj)
	trustForTest(t, proj)

	text, err := runForTest(t, "list the files here")
	require.NoError(t, err)
	require.Equal(t, "Let me look.\n\nThere are two text files.\n", text)

	out, err := runForTest(t, "--output-format", "json", "list the files here")
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	require.Equal(t, "There are two text files.", res["result"])
	require.Equal(t, false, res["is_error"])
	require.Equal(t, float64(2), res["num_turns"], "json counts the turns too, as Claude Code's does")

	out, err = runForTest(t, "--output-format", "stream-json", "list the files here")
	require.NoError(t, err)
	var types []string
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var v map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &v), line)
		lines = append(lines, v)
		types = append(types, v["type"].(string))
	}
	require.Equal(t, []string{"system", "assistant", "user", "assistant", "result"}, types, out)
	init := lines[0]
	require.Equal(t, "stand-in", init["model"])
	require.Equal(t, "bypassPermissions", init["permissionMode"])
	require.Subset(t, init["tools"], []any{"bash", "edit", "ls", "view"}, "the tools the run offers, as Claude Code's init line lists them")
	require.NotContains(t, init["tools"], "question", "a run offers no tool that needs someone to answer")
	require.Equal(t, []any{}, init["mcp_servers"])
	toolUse := lines[1]["message"].(map[string]any)["content"].([]any)[1].(map[string]any)
	require.Equal(t, "ls", toolUse["name"])
	result := lines[2]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	require.Equal(t, "call_ls_1", result["tool_use_id"])
	require.Contains(t, result["content"], "a.txt")
	require.Equal(t, "There are two text files.", lines[4]["result"])
	require.Equal(t, float64(2), lines[4]["num_turns"])

	// In plan mode the init line says so, and lists the plan agent's tools.
	out, err = runForTest(t, "--permission-mode", "plan", "--output-format", "stream-json", "list the files here")
	require.NoError(t, err)
	init = streamLines(t, out)[0]
	require.Equal(t, "plan", init["permissionMode"])
	require.Contains(t, init["tools"], "view")
	require.NotContains(t, init["tools"], "bash")
	require.NotContains(t, init["tools"], "edit")

	// Resumed with no model asked for, a run goes on with the session's
	// model, and the init line names it, not the default.
	out, err = runForTest(t, "-m", "standin/stand-in-2", "--output-format", "json", "list the files here")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	resumed, err := runForTestWith(t, "--quiet", "-r", res["session_id"].(string), "--output-format", "stream-json", "and again")
	require.NoError(t, err)
	require.Equal(t, "stand-in-2", streamLines(t, resumed)[0]["model"], resumed)
}

package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

// fakeServerOut names the file the fake language server writes the
// client's answer to its workspace/configuration request into.
const fakeServerOut = "GNOSH_FAKE_LSP_OUT"

// TestWorkspaceConfigurationAnswersFromOptions starts a language server
// that, like gopls, pulls its settings with workspace/configuration once
// initialized, and checks that each item is answered from the section of
// the configured options it names.
func TestWorkspaceConfigurationAnswersFromOptions(t *testing.T) {
	t.Parallel()

	out := filepath.Join(t.TempDir(), "answer.json")
	cfg := config.LSPConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestFakeLanguageServer$"},
		Env:     map[string]string{fakeServerOut: out},
		Options: map[string]any{
			"gopls":  map[string]any{"staticcheck": true},
			"python": map[string]any{"analysis": map[string]any{"typeCheckingMode": "strict"}},
		},
	}
	resolver := config.NewShellVariableResolver(env.NewFromMap(map[string]string{}))
	client, err := New("fake", cfg, resolver, t.TempDir(), false)
	require.NoError(t, err)
	t.Cleanup(client.Kill)

	_, err = client.Initialize(t.Context(), t.TempDir())
	require.NoError(t, err)

	var answer []byte
	require.Eventually(t, func() bool {
		answer, err = os.ReadFile(out)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	require.JSONEq(t, `[{"staticcheck":true},{"typeCheckingMode":"strict"},{}]`, string(answer))
}

// TestFakeLanguageServer is the language server the test above starts. It
// does nothing unless started by it.
func TestFakeLanguageServer(t *testing.T) {
	out := os.Getenv(fakeServerOut)
	if out == "" {
		t.Skip("only run as a language server")
	}

	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := readMessage(r)
		if err != nil {
			os.Exit(0)
		}
		switch {
		case msg.Method == "initialize":
			writeMessage(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case msg.Method == "initialized":
			writeMessage(map[string]any{
				"jsonrpc": "2.0",
				"id":      "cfg",
				"method":  "workspace/configuration",
				"params": map[string]any{"items": []map[string]any{
					{"section": "gopls"},
					{"section": "python.analysis"},
					{"section": "missing"},
				}},
			})
		case string(msg.ID) == `"cfg"`:
			if err := os.WriteFile(out+".tmp", msg.Result, 0o644); err == nil {
				_ = os.Rename(out+".tmp", out)
			}
		case msg.ID != nil && msg.Method != "":
			writeMessage(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": nil})
		}
	}
}

type fakeMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
}

func readMessage(r *bufio.Reader) (fakeMessage, error) {
	header, err := textproto.NewReader(r).ReadMIMEHeader()
	if err != nil {
		return fakeMessage{}, err
	}
	n, err := strconv.Atoi(header.Get("Content-Length"))
	if err != nil {
		return fakeMessage{}, err
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fakeMessage{}, err
	}
	var msg fakeMessage
	err = json.Unmarshal(body, &msg)
	return msg, err
}

func writeMessage(msg map[string]any) {
	body, _ := json.Marshal(msg)
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

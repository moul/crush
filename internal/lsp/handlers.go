package lsp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/lsp/util"
	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
)

// HandleWorkspaceConfiguration answers workspace/configuration requests
// from the server's configured options. Each requested item gets the
// options under its section, a dotted path such as "python.analysis", or
// all of them when it names none, and an empty object, as VS Code sends,
// when there is nothing there. The answer has one entry per item, as the
// protocol requires.
func HandleWorkspaceConfiguration(options map[string]any) func(_ context.Context, _ string, params json.RawMessage) (any, error) {
	return func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var configParams protocol.ConfigurationParams
		if err := json.Unmarshal(params, &configParams); err != nil {
			return nil, err
		}
		result := make([]any, len(configParams.Items))
		for i, item := range configParams.Items {
			result[i] = configurationSection(options, item.Section)
		}
		return result, nil
	}
}

// configurationSection returns the value at a dotted section of options,
// or an empty object when there is none.
func configurationSection(options map[string]any, section string) any {
	var value any = options
	if section != "" {
		for key := range strings.SplitSeq(section, ".") {
			m, _ := value.(map[string]any)
			if value = m[key]; value == nil {
				break
			}
		}
	}
	if m, ok := value.(map[string]any); value == nil || ok && m == nil {
		return map[string]any{}
	}
	return value
}

// HandleWorkDoneProgressCreate handles server-initiated window/workDoneProgress/create
// requests. The client advertises window.workDoneProgress: true in its capabilities
// (see makeClientCapabilities in powernap), which per the LSP spec grants servers
// permission to send this request — so it must be answered, even as a no-op, or the
// server (e.g. typescript-language-server) treats the unhandled response as fatal and
// crashes. See github.com/charmbracelet/x issue tracking powernap capability gaps.
func HandleWorkDoneProgressCreate(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	return nil, nil
}

// HandleRegisterCapability answers a server's capability registrations.
// Gnosh does not watch the files a server registers for: it tells
// servers about changes itself (NotifyWorkspaceChange and the open
// files), so a workspace/didChangeWatchedFiles registration is accepted
// and dropped.
func HandleRegisterCapability(_ context.Context, _ string, params json.RawMessage) (any, error) {
	var registerParams protocol.RegistrationParams
	if err := json.Unmarshal(params, &registerParams); err != nil {
		slog.Error("Error unmarshaling registration params", "error", err)
		return nil, err
	}
	return nil, nil
}

// HandleApplyEdit handles workspace edit requests
func HandleApplyEdit(encoding powernap.OffsetEncoding) func(_ context.Context, _ string, params json.RawMessage) (any, error) {
	return func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var edit protocol.ApplyWorkspaceEditParams
		if err := json.Unmarshal(params, &edit); err != nil {
			return nil, err
		}

		err := util.ApplyWorkspaceEdit(edit.Edit, encoding)
		if err != nil {
			slog.Error("Error applying workspace edit", "error", err)
			return protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: err.Error()}, nil
		}

		return protocol.ApplyWorkspaceEditResult{Applied: true}, nil
	}
}

// HandleServerMessage handles server messages
func HandleServerMessage(_ context.Context, method string, params json.RawMessage) {
	var msg protocol.ShowMessageParams
	if err := json.Unmarshal(params, &msg); err != nil {
		slog.Debug("Error unmarshal server message", "error", err)
		return
	}

	switch msg.Type {
	case protocol.Error:
		slog.Error("LSP Server", "message", msg.Message)
	case protocol.Warning:
		slog.Warn("LSP Server", "message", msg.Message)
	case protocol.Info:
		slog.Info("LSP Server", "message", msg.Message)
	case protocol.Log:
		slog.Debug("LSP Server", "message", msg.Message)
	}
}

// HandleDiagnostics handles diagnostic notifications from the LSP server
func HandleDiagnostics(client *Client, params json.RawMessage) {
	var diagParams protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &diagParams); err != nil {
		slog.Error("Error unmarshaling diagnostics params", "error", err)
		return
	}

	client.diagnostics.Set(diagParams.URI, diagParams.Diagnostics)

	// Calculate total diagnostic count
	totalCount := 0
	for _, diagnostics := range client.diagnostics.Seq2() {
		totalCount += len(diagnostics)
	}

	// Trigger callback if set
	if client.onDiagnosticsChanged != nil {
		client.onDiagnosticsChanged(client.name, totalCount)
	}
}

package proto

import (
	"encoding/json"
	"errors"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
)

// Workspace represents a running app.App workspace with its associated
// resources and state.
type Workspace struct {
	ID       string         `json:"id"`
	Path     string         `json:"path"`
	YOLO     bool           `json:"yolo,omitempty"`
	Debug    bool           `json:"debug,omitempty"`
	DataDir  string         `json:"data_dir,omitempty"`
	Version  string         `json:"version,omitempty"`
	ClientID string         `json:"client_id,omitempty"`
	Config   *config.Config `json:"config,omitempty"`
	Env      []string       `json:"env,omitempty"`
	// Channels lists the MCP servers opted in as channels for this workspace
	// (from the --channels flag).
	Channels []string `json:"channels,omitempty"`
	// Skills carries the snapshot of skill discovery state at workspace
	// creation time. Subsequent updates flow through the SSE event
	// stream.
	Skills []SkillState `json:"skills,omitempty"`
	// TrustFolder is set when the client trusts the folder's project
	// settings for this session without recording it: the user answered
	// yes in the home directory, or passed --trust-folder. A workspace
	// another client created first keeps that client's answer.
	TrustFolder bool `json:"trust_folder,omitempty"`
}

// Error represents an error response.
type Error struct {
	Message string `json:"message"`
}

// ConfigChanged is published whenever the workspace's configuration is
// mutated by a backend operation. Clients react by re-fetching the
// workspace snapshot so cached config stays in sync across subscribers.
type ConfigChanged struct {
	WorkspaceID string `json:"workspace_id"`
}

// UpdateAvailable is published when a newer Crush release is detected
// on the server side. It mirrors app.UpdateAvailableMsg across the SSE
// boundary so client/server mode TUI clients see the same notification
// as local-mode clients.
type UpdateAvailable struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	IsDevelopment  bool   `json:"is_development"`
}

// CurrentSession is the request body for the per-client
// current-session endpoint. An empty SessionID clears the entry.
type CurrentSession struct {
	SessionID string `json:"session_id"`
}

// RunComplete is the authoritative end-of-run signal for a session,
// emitted exactly once per top-level agent turn after all message
// updates for the turn have flushed. Clients that need a reliable
// completion contract (notably `crush run` in client/server mode)
// should listen for this event filtered by RunID (preferred) — or
// by SessionID when no RunID was supplied — and use Text and
// MessageID to reconcile any output they have already streamed from
// earlier message events. Error is non-empty when the run terminated
// with an error; Cancelled is true when terminated due to context
// cancellation.
//
// RunID echoes the value the caller set on AgentMessage.RunID. It is
// the only safe correlator when the caller's prompt was queued
// behind a busy session: another turn's RunComplete for the same
// SessionID may arrive first, and filtering by SessionID alone
// would terminate the caller before its own turn ran.
type RunComplete struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	MessageID string `json:"message_id"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	// Halted mirrors notify.RunComplete.Halted: a tool result (a denied
	// permission, a cancelled question, a hook's halt) ended the turn.
	Halted bool `json:"halted,omitempty"`
}

// SkillInfo describes a visible skill exposed to a frontend.
type SkillInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Label         string `json:"label"`
	Source        string `json:"source"`
	UserInvocable bool   `json:"user_invocable"`
}

// ReadSkillRequest is the request body for reading a skill's content.
type ReadSkillRequest struct {
	SkillID string `json:"skill_id"`
}

// ReadSkillResponse is the response for reading a skill's content.
type ReadSkillResponse struct {
	Content []byte          `json:"content"`
	Result  SkillReadResult `json:"result"`
}

// SkillReadResult holds metadata about a skill returned alongside its
// content.
type SkillReadResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Builtin     bool   `json:"builtin"`
}

// AgentInfo represents information about the agent.
type AgentInfo struct {
	IsBusy   bool                 `json:"is_busy"`
	IsReady  bool                 `json:"is_ready"`
	Model    catwalk.Model        `json:"model"`
	ModelCfg config.SelectedModel `json:"model_cfg"`
}

// IsZero checks if the AgentInfo is zero-valued.
func (a AgentInfo) IsZero() bool {
	return !a.IsBusy && !a.IsReady && a.Model.ID == ""
}

// AgentMessage represents a message sent to the agent.
//
// RunID, when non-empty, is echoed back on the [RunComplete] event
// emitted for the resulting turn. Callers that need to correlate a
// specific SendMessage with its terminal event (notably
// `crush run`, which may attach to a busy session whose currently
// running turn finishes first) should set it to a fresh unique
// value before the request. Server-side propagation flows through
// agent.WithRunID on the request context into the
// SessionAgentCall; it is preserved across the busy-session queue.
// When empty the resulting RunComplete carries an empty RunID and
// callers must fall back to SessionID-only filtering, which
// remains correct only when no other turns are in flight for the
// same session.
type AgentMessage struct {
	HiddenUserMessage bool         `json:"hidden_user_message,omitempty"`
	SessionID         string       `json:"session_id"`
	RunID             string       `json:"run_id,omitempty"`
	Channel           string       `json:"channel,omitempty"`
	Prompt            string       `json:"prompt"`
	Attachments       []Attachment `json:"attachments,omitempty"`
}

// ShellCommandRequest represents a request to run a shell command directly.
type ShellCommandRequest struct {
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
	TermWidth int    `json:"term_width,omitempty"`
}

// ShellCommandResponse represents the result of a direct shell command.
type ShellCommandResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// ContextReport is what fills a session's context window, the Markdown
// Claude Code's /context prints.
type ContextReport struct {
	Markdown string `json:"markdown"`
}

// PermissionsReport is what the agent may do without asking, the
// Markdown Claude Code's /permissions shows.
type PermissionsReport struct {
	Markdown string `json:"markdown"`
}

// BackgroundJob is a job the agent started in the background, as /tasks
// lists it.
type BackgroundJob struct {
	ID          string    `json:"id"`
	Command     string    `json:"command"`
	Description string    `json:"description,omitempty"`
	WorkingDir  string    `json:"working_dir"`
	StartedAt   time.Time `json:"started_at"`
	Done        bool      `json:"done"`
	ExitCode    int       `json:"exit_code"`
	Err         string    `json:"error,omitempty"`
	Output      string    `json:"output,omitempty"`
}

// BranchRequest asks for a branch of a session, titled Title, or after
// the session when Title is "".
type BranchRequest struct {
	Title string `json:"title,omitempty"`
}

// Goal is a session's goal, as /goal sets one: a condition the agent keeps
// working toward, bound to the goal named Name in the project's mem, until
// a check finds it holds; Checks counts the checks, and LastCheck is what
// the last one found.
type Goal struct {
	Condition string `json:"condition"`
	Name      string `json:"name,omitempty"`
	Title     string `json:"title,omitempty"`
	Checks    int    `json:"checks"`
	LastCheck string `json:"last_check,omitempty"`
}

// GoalStatus is a session's goal: Goal is nil when it has none. Setting
// one answers the goal set and Directive, what to send the agent;
// clearing one answers the goal cleared and Notice, what that did.
type GoalStatus struct {
	Goal      *Goal  `json:"goal,omitempty"`
	Directive string `json:"directive,omitempty"`
	Notice    string `json:"notice,omitempty"`
}

// GoalRequest sets a session's goal, or clears it when Condition is "".
type GoalRequest struct {
	Condition string `json:"condition"`
}

// RewindRequest asks for a session's conversation, its code or both to be
// restored to the point before its prompt MessageID; with DryRun, only
// what restoring the code would change.
type RewindRequest struct {
	MessageID    string `json:"message_id"`
	Conversation bool   `json:"conversation,omitempty"`
	Code         bool   `json:"code,omitempty"`
	DryRun       bool   `json:"dry_run,omitempty"`
}

// RewindResult is what a rewind restored of the code, or would restore,
// and the session the whole conversation is kept in.
type RewindResult struct {
	Files     []string `json:"files,omitempty"`
	Additions int      `json:"additions"`
	Removals  int      `json:"removals"`
	Skipped   []string `json:"skipped,omitempty"`
	Kept      *Session `json:"kept,omitempty"`
}

// DoctorReport is the check of the workspace's setup that gnosh doctor
// prints, as the Markdown /doctor shows.
type DoctorReport struct {
	Markdown string `json:"markdown"`
}

// MemCommandRequest is /mem as the user typed it in a session: its
// arguments, which go in a body, never a URL, since /mem redact carries a
// secret.
type MemCommandRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Args      string `json:"args"`
}

// MemText is what /mem shows, or the line mem's notice shows as a session
// starts ("" when nothing is due or waits on the user).
type MemText struct {
	Text string `json:"text"`
}

// SummarizeRequest is the optional body of a summary request:
// Instructions is the text given after /compact.
type SummarizeRequest struct {
	Instructions string `json:"instructions,omitempty"`
}

// SessionNameResponse is the name a session was given from its
// conversation.
type SessionNameResponse struct {
	Name string `json:"name"`
}

// AgentSession represents a session with its busy status.
type AgentSession struct {
	Session
	IsBusy bool `json:"is_busy"`
}

// IsZero checks if the AgentSession is zero-valued.
func (a AgentSession) IsZero() bool {
	return a.ID == "" && !a.IsBusy
}

// PermissionAction represents an action taken on a permission request.
type PermissionAction string

const (
	PermissionAllow           PermissionAction = "allow"
	PermissionAllowForSession PermissionAction = "allow_session"
	PermissionDeny            PermissionAction = "deny"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (p PermissionAction) MarshalText() ([]byte, error) {
	return []byte(p), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (p *PermissionAction) UnmarshalText(text []byte) error {
	*p = PermissionAction(text)
	return nil
}

// PermissionGrant represents a permission grant request.
type PermissionGrant struct {
	Permission PermissionRequest `json:"permission"`
	Action     PermissionAction  `json:"action"`
}

// PermissionGrantResponse is the server's response to a permission
// grant call. Resolved is true when this call resolved the pending
// request, and false when the request had already been resolved by a
// previous caller (e.g., another client in a multi-subscriber UI). A
// false value is not an error.
type PermissionGrantResponse struct {
	Resolved bool `json:"resolved"`
}

// QuestionRequest is the wire format for a batch question
// sent from server to client over SSE.
type QuestionRequest struct {
	ID                 string         `json:"id"`
	SessionID          string         `json:"session_id"`
	ToolCallID         string         `json:"tool_call_id"`
	Questions          []QuestionItem `json:"questions"`
	ConfirmTitle       string         `json:"confirm_title,omitempty"`
	ConfirmDescription string         `json:"confirm_description,omitempty"`
}

// QuestionItem is a single question within a batch.
type QuestionItem struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Label       string           `json:"label,omitempty"`
	Question    string           `json:"question"`
	Description string           `json:"description,omitempty"`
	Choices     []QuestionChoice `json:"choices,omitempty"`
	// Secret is a free-text question answered blind (a password); the
	// client masks the input.
	Secret bool `json:"secret,omitempty"`
}

// QuestionChoice is a selectable option.
type QuestionChoice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionAnswer is the wire format for answering a batch
// question, sent from client to server via REST.
type QuestionAnswer struct {
	BatchRequestID string             `json:"batch_request_id"`
	Responses      []QuestionResponse `json:"responses"`
}

// QuestionResponse is a single answer within a batch response.
type QuestionResponse struct {
	QuestionID  string            `json:"request_id"`
	SelectedIDs []string          `json:"selected_ids,omitempty"`
	FillInText  string            `json:"fill_in_text,omitempty"`
	Yes         *bool             `json:"yes,omitempty"`
	Notes       map[string]string `json:"notes,omitempty"`
}

// QuestionAnswerResponse is the server's response to a
// question batch answer call.
type QuestionAnswerResponse struct {
	Resolved bool `json:"resolved"`
}

// QuestionNotification is published when a question batch is
// resolved so non-answering clients can dismiss their forms.
type QuestionNotification struct {
	BatchID   string `json:"batch_id"`
	SessionID string `json:"session_id"`
}

// PermissionSkipRequest represents a request to skip permission prompts.
type PermissionSkipRequest struct {
	Skip bool `json:"skip"`
}

// LSPEventType represents the type of LSP event.
type LSPEventType string

const (
	LSPEventStateChanged       LSPEventType = "state_changed"
	LSPEventDiagnosticsChanged LSPEventType = "diagnostics_changed"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (e LSPEventType) MarshalText() ([]byte, error) {
	return []byte(e), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (e *LSPEventType) UnmarshalText(data []byte) error {
	*e = LSPEventType(data)
	return nil
}

// LSPEvent represents an event in the LSP system.
type LSPEvent struct {
	Type            LSPEventType    `json:"type"`
	Name            string          `json:"name"`
	State           lsp.ServerState `json:"state"`
	Error           error           `json:"error,omitempty"`
	DiagnosticCount int             `json:"diagnostic_count,omitempty"`
}

// MarshalJSON implements the [json.Marshaler] interface.
func (e LSPEvent) MarshalJSON() ([]byte, error) {
	type Alias LSPEvent
	return json.Marshal(&struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Error: func() string {
			if e.Error != nil {
				return e.Error.Error()
			}
			return ""
		}(),
		Alias: Alias(e),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (e *LSPEvent) UnmarshalJSON(data []byte) error {
	type Alias LSPEvent
	aux := &struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Alias: Alias(*e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = LSPEvent(aux.Alias)
	if aux.Error != "" {
		e.Error = errors.New(aux.Error)
	}
	return nil
}

// LSPClientInfo holds information about an LSP client's state.
type LSPClientInfo struct {
	Name            string          `json:"name"`
	State           lsp.ServerState `json:"state"`
	Error           error           `json:"error,omitempty"`
	DiagnosticCount int             `json:"diagnostic_count,omitempty"`
	ConnectedAt     time.Time       `json:"connected_at"`
}

// MarshalJSON implements the [json.Marshaler] interface.
func (i LSPClientInfo) MarshalJSON() ([]byte, error) {
	type Alias LSPClientInfo
	return json.Marshal(&struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Error: func() string {
			if i.Error != nil {
				return i.Error.Error()
			}
			return ""
		}(),
		Alias: Alias(i),
	})
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (i *LSPClientInfo) UnmarshalJSON(data []byte) error {
	type Alias LSPClientInfo
	aux := &struct {
		Error string `json:"error,omitempty"`
		Alias
	}{
		Alias: Alias(*i),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*i = LSPClientInfo(aux.Alias)
	if aux.Error != "" {
		i.Error = errors.New(aux.Error)
	}
	return nil
}

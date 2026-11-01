package permission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

// WithoutHookAnswers returns ctx without a hook's approval of, or question
// about, any tool call: for the calls of a sub-agent that a tool call
// started, which can carry its ID, as a provider that numbers calls
// tool-call-0, tool-call-1 in each response gives them.
func WithoutHookAnswers(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, hookAnswerKey{}, hookAnswerer{})
	return context.WithValue(context.WithValue(ctx, hookApprovalKey{}, ""), hookAskKey{}, "")
}

// HookAnswer is what a tool call's PermissionRequest hooks answered when a
// dialog about it was due: allow or deny it in the user's place, or
// neither, and the user is asked. Note says why they gave no answer when
// one could not check, for the dialog to show.
type HookAnswer struct {
	Allow bool
	Deny  bool
	Note  string
}

// hookAnswerKey is the unexported context key of a tool call's
// PermissionRequest hooks: see WithHookAnswer.
type hookAnswerKey struct{}

type hookAnswerer struct {
	toolCallID string
	answer     func(context.Context) HookAnswer
}

// WithHookAnswer returns a context whose permission request about the
// given tool call runs answer right before the request would be put to
// the user, as Claude Code runs its PermissionRequest hooks: an allow
// grants the request and a deny denies it, with no dialog. A request that
// needs no dialog (an allowed tool, a grant for the session, yolo) does
// not run it.
func WithHookAnswer(ctx context.Context, toolCallID string, answer func(context.Context) HookAnswer) context.Context {
	return context.WithValue(ctx, hookAnswerKey{}, hookAnswerer{toolCallID: toolCallID, answer: answer})
}

// HookAskAction is the action of the request a PreToolUse hook that
// answered "ask" puts to the user about a tool call: see WithHookAsk.
const HookAskAction = "hook_ask"

// hookAskKey is the unexported context key used to mark a permission
// request as one a PreToolUse hook asked for. The value is the tool call
// ID, as for hookApprovalKey.
type hookAskKey struct{}

// WithHookAsk returns a context that marks the request for the given tool
// call ID as asked for by a hook, as Claude Code's "ask" is: it goes to
// the user even when yolo, --allowedTools, an added directory, an earlier
// "allow for this session" or a session that grants all its requests
// would grant it unasked. Whether anyone is there to answer is the
// caller's to know (gnosh run has no one).
func WithHookAsk(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookAskKey{}, toolCallID)
}

// hookAsked reports whether the context marks the request for the given
// tool call ID as asked for by a hook.
func hookAsked(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookAskKey{}).(string)
	return v == toolCallID
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	// FilePath is the file a tool would change, as an absolute path, for
	// acceptEdits (see acceptsEdit) and the protected files, which are
	// always asked about (see protectedFile). Path names only the working
	// directory for a file in it.
	FilePath string `json:"file_path,omitempty"`
	// FilePaths are more files the tool would change, as absolute paths:
	// lsp_rename changes every file a rename reaches. A protected one makes
	// the request always asked about, as FilePath does.
	FilePaths []string `json:"file_paths,omitempty"`
}

type PermissionNotification struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
}

type PermissionRequest struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	// AlwaysAsks is set for a request a grant for the session would not
	// cover (a protected file, or a link out of the granted directory),
	// which is asked about every time: the dialog offers no "Allow for
	// Session" for it.
	AlwaysAsks bool `json:"always_asks,omitempty"`
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	// GrantPersistent grants a permission request and remembers the grant
	// for the session. It returns true if this call actually resolved the
	// pending request; false if the request had already been resolved
	// (e.g., by another concurrent caller) or is unknown.
	GrantPersistent(permission PermissionRequest) bool
	// Grant grants a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Grant(permission PermissionRequest) bool
	// Deny denies a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Deny(permission PermissionRequest) bool
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	// ActiveRequest returns the request that is currently awaiting a
	// decision, or false when no request is pending. A request is only
	// reported while it can still be resolved; a request whose waiter
	// was cancelled is not returned.
	ActiveRequest() (PermissionRequest, bool)
	AutoApproveSession(sessionID string)
	// IsSessionAutoApproved reports whether AutoApproveSession was called
	// for sessionID.
	IsSessionAutoApproved(sessionID string) bool
	SetSkipRequests(skip bool)
	SkipRequests() bool
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	// AddDir adds dir, an absolute path, to the directories read without
	// asking, as Claude Code's /add-dir does.
	AddDir(dir string)
}

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    *csync.Map[PermissionKey, bool]
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  atomic.Bool
	allowedTools          []string
	// addedDirs are the directories added with --add-dir (Claude Code's):
	// reading or listing under them needs no permission, as under the
	// working directory. Edits there are asked about as usual, but under
	// acceptEdits. /add-dir adds to them while the session runs, so
	// they are read under addedDirsMu.
	addedDirs   []string
	addedDirsMu sync.RWMutex
	// acceptEdits is Claude Code's acceptEdits mode: an edit of a file in
	// the working directory or an added one runs without asking.
	acceptEdits bool
	// dontAsk is Claude Code's dontAsk mode: what would be asked about is
	// denied.
	dontAsk bool
	// protectedPaths are directories, besides the protected names, that a
	// file tool's change in is always asked about (see protectedFile).
	protectedPaths []string

	// used to make sure we only process one request at a time
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex
}

// resolve atomically removes the pending request entry for the given
// permission and, if it was still pending, publishes exactly one
// PermissionNotification and forwards the outcome to the waiter on
// respCh. It returns true if this call resolved the request, false if
// it had already been resolved (e.g., by another concurrent caller) or
// the request ID is unknown.
//
// If onResolve is non-nil it runs after the pending entry has been
// taken but before the notification is published or the waiter is
// unblocked. This lets GrantPersistent record the session permission
// only when it actually wins the race, so a losing GrantPersistent
// that lost to a Deny does not leak an auto-approve entry.
//
// All three public resolution methods (Grant, GrantPersistent, Deny)
// route through this helper so multi-subscriber UIs can race safely:
// the first caller wins, the rest become no-ops.
func (s *permissionService) resolve(permission PermissionRequest, granted, denied bool, onResolve func()) bool {
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		return false
	}

	if onResolve != nil {
		onResolve()
	}

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  permission.SessionID,
		ToolCallID: permission.ToolCallID,
		Granted:    granted,
		Denied:     denied,
	})

	// respCh is buffered (cap 1) and only ever has at most one sender
	// per request because Take removes the entry under the map lock,
	// so this send never blocks.
	respCh <- granted

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
	return true
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) bool {
	// Record the persistent grant only if this call wins the
	// pending-request race. Otherwise a losing GrantPersistent that
	// lost to a Deny would still leave an auto-approve entry behind,
	// silently flipping later denied calls to allowed.
	return s.resolve(permission, true, false, func() {
		s.sessionPermissions.Set(PermissionKey{
			SessionID: permission.SessionID,
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
		}, true)
	})
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	return s.resolve(permission, true, false, nil)
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	return s.resolve(permission, false, true, nil)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if hookAsked(ctx, opts.ToolCallID) {
		return s.ask(ctx, opts)
	}
	if s.skip.Load() {
		return true, nil
	}

	// A file tool's change of a protected file is asked about however the
	// tool is allowed or was granted for the session, as in Claude Code,
	// where allow rules do not pre-approve protected-path writes: only
	// yolo, a session that grants all its requests, and a hook skip the
	// question. A bash command is not read for the files it writes, so
	// allowing bash still allows whatever it does, as in Claude Code.
	protected := s.protectedFile(opts.FilePath) || slices.ContainsFunc(opts.FilePaths, s.protectedFile)

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if !protected && (slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName)) {
		return true, nil
	}

	if action, ok := addedDirTools[opts.ToolName]; ok && action == opts.Action && s.inAddedDir(opts.Path) {
		return true, nil
	}

	if s.acceptEdits && acceptEditsTools[opts.ToolName] && s.acceptsEdit(opts.FilePath) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  opts.SessionID,
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}
	permission := s.newRequest(opts, dir)

	// A grant for the session covers a file only where it really is: a
	// link in the project to a file outside is not in the project.
	coverable := !protected && fileIn(opts.FilePath, permission.Path) &&
		!slices.ContainsFunc(opts.FilePaths, func(f string) bool { return !fileIn(f, permission.Path) })
	permission.AlwaysAsks = !coverable
	if _, ok := s.sessionPermissions.Get(PermissionKey{
		SessionID: permission.SessionID,
		ToolName:  permission.ToolName,
		Action:    permission.Action,
		Path:      permission.Path,
	}); ok && coverable {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			SessionID:  opts.SessionID,
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	if granted, ok := s.hookAnswered(ctx, &permission); ok {
		return granted, nil
	}
	if s.dontAsk {
		return s.notAsked(ctx, permission), nil
	}
	return s.wait(ctx, permission)
}

// NotAsked records whether a tool call's permission request was denied
// without being put to the user, as Claude Code's dontAsk mode denies
// every request it would ask about: see WithNotAsked.
type NotAsked struct {
	toolCallID string
	denied     atomic.Bool
}

// Denied reports whether a request of the call was denied unasked.
func (n *NotAsked) Denied() bool {
	return n != nil && n.denied.Load()
}

// notAskedKey is the unexported context key of a call's NotAsked.
type notAskedKey struct{}

// WithNotAsked returns a context whose permission requests about the given
// tool call record on the returned NotAsked when they are denied without
// asking, so the call can tell the model why it did not run.
func WithNotAsked(ctx context.Context, toolCallID string) (context.Context, *NotAsked) {
	n := &NotAsked{toolCallID: toolCallID}
	return context.WithValue(ctx, notAskedKey{}, n), n
}

// notAsked denies req without putting it to the user, under dontAsk.
func (s *permissionService) notAsked(ctx context.Context, req PermissionRequest) bool {
	if n, _ := ctx.Value(notAskedKey{}).(*NotAsked); n != nil && n.toolCallID == req.ToolCallID {
		n.denied.Store(true)
	}
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Denied:     true,
	})
	return false
}

// hookAnswered runs the PermissionRequest hooks of the request's call
// (WithHookAnswer), now that the request is about to be put to the user,
// and reports whether they answered it and how. When they did not, a
// note on why one could not check goes in the request's description.
func (s *permissionService) hookAnswered(ctx context.Context, req *PermissionRequest) (granted, ok bool) {
	h, _ := ctx.Value(hookAnswerKey{}).(hookAnswerer)
	if h.answer == nil || req.ToolCallID == "" || h.toolCallID != req.ToolCallID {
		return false, false
	}
	answer := h.answer(ctx)
	if !answer.Allow && !answer.Deny {
		if answer.Note != "" {
			req.Description = strings.TrimSpace(req.Description + "\n\n" + answer.Note)
		}
		return false, false
	}
	granted = answer.Allow && !answer.Deny
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  req.SessionID,
		ToolCallID: req.ToolCallID,
		Granted:    granted,
		Denied:     !granted,
	})
	return granted, true
}

// ask puts a request a hook asked for (WithHookAsk) to the user, whatever
// would grant it unasked.
func (s *permissionService) ask(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		SessionID:  opts.SessionID,
		ToolCallID: opts.ToolCallID,
	})
	// A hook's question comes every time, whatever was granted before.
	req := s.newRequest(opts, opts.Path)
	req.AlwaysAsks = true
	if granted, ok := s.hookAnswered(ctx, &req); ok {
		return granted, nil
	}
	if s.dontAsk {
		return s.notAsked(ctx, req), nil
	}
	return s.wait(ctx, req)
}

// newRequest is the request opts make, about dir.
func (s *permissionService) newRequest(opts CreatePermissionRequest, dir string) PermissionRequest {
	return PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
	}
}

// wait publishes permission for the user to answer and waits for the
// answer. The caller holds requestMu, so one request is asked at a time.
func (s *permissionService) wait(ctx context.Context, permission PermissionRequest) (bool, error) {
	s.activeRequestMu.Lock()
	s.activeRequest = &permission
	s.activeRequestMu.Unlock()

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)
	defer s.pendingRequests.Del(permission.ID)

	// Publish the request
	s.Publish(pubsub.CreatedEvent, permission)

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

// ActiveRequest returns the request that is currently awaiting a
// decision. The active pointer can outlive the waiter that published
// it (Request leaves it behind when its context is cancelled), so a
// request is only reported while its ID is still registered in
// pendingRequests, i.e. while Grant/Deny can actually resolve it.
func (s *permissionService) ActiveRequest() (PermissionRequest, bool) {
	s.activeRequestMu.Lock()
	active := s.activeRequest
	s.activeRequestMu.Unlock()
	if active == nil {
		return PermissionRequest{}, false
	}
	if _, ok := s.pendingRequests.Get(active.ID); !ok {
		return PermissionRequest{}, false
	}
	return *active, true
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) IsSessionAutoApproved(sessionID string) bool {
	s.autoApproveSessionsMu.RLock()
	defer s.autoApproveSessionsMu.RUnlock()
	return s.autoApproveSessions[sessionID]
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

// addedDirTools are the tools whose reads and listings are approved under
// a directory added with --add-dir, each with the action it asks for:
// view reading a file, ls listing a directory, grep and glob searching
// one. Only theirs: the MCP resource tools and gnokey ask with the same
// action names for a path that names no file on disk (an MCP URI joined
// to the working directory, a directory the model chose), and still ask.
// The names are internal/agent/tools', which imports this package; a
// test there runs the real tools against an added directory.
var addedDirTools = map[string]string{"view": "read", "ls": "list", "grep": "search", "glob": "search"}

// AddDir implements Service.
func (s *permissionService) AddDir(dir string) {
	s.addedDirsMu.Lock()
	defer s.addedDirsMu.Unlock()
	s.addedDirs = append(s.addedDirs, dir)
}

// dirs are the directories added with --add-dir or /add-dir.
func (s *permissionService) dirs() []string {
	s.addedDirsMu.RLock()
	defer s.addedDirsMu.RUnlock()
	return s.addedDirs
}

// inAddedDir reports whether path, where a read goes with its symlinks
// resolved, lies in one of the directories added with --add-dir, as
// written or with their own symlinks resolved.
func (s *permissionService) inAddedDir(path string) bool {
	for _, dir := range s.dirs() {
		if within(dir, path) {
			return true
		}
		if real, ok := fsext.ResolvedPath(dir); ok && within(real, path) {
			return true
		}
	}
	return false
}

// NewPermissionService makes the permission service for a workspace in
// workingDir. addedDirs are the directories added with --add-dir, as
// absolute paths.
func NewPermissionService(workingDir string, skip bool, allowedTools []string, addedDirs ...string) Service {
	return New(Options{WorkingDir: workingDir, Skip: skip, AllowedTools: allowedTools, AddedDirs: addedDirs})
}

// Options are what a workspace's permission service is made with.
type Options struct {
	// WorkingDir is the workspace's directory.
	WorkingDir string
	// Skip grants every request (yolo).
	Skip bool
	// AllowedTools are granted without asking: a tool's name, or
	// "tool:action".
	AllowedTools []string
	// AddedDirs are the directories added with --add-dir, as absolute
	// paths.
	AddedDirs []string
	// AcceptEdits is Claude Code's acceptEdits mode (see acceptsEdit).
	AcceptEdits bool
	// DontAsk is Claude Code's dontAsk mode: a request that would be put
	// to the user is denied instead (see NotAsked).
	DontAsk bool
	// ProtectedPaths are directories, as absolute paths, a file tool's
	// change in is always asked about, as in the protected names: the
	// session env files, which run ahead of every bash command.
	ProtectedPaths []string
}

// New makes the permission service opts describe.
func New(opts Options) Service {
	svc := &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          opts.WorkingDir,
		sessionPermissions:  csync.NewMap[PermissionKey, bool](),
		autoApproveSessions: make(map[string]bool),
		allowedTools:        opts.AllowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
		addedDirs:           opts.AddedDirs,
		acceptEdits:         opts.AcceptEdits,
		dontAsk:             opts.DontAsk,
		protectedPaths:      opts.ProtectedPaths,
	}
	svc.skip.Store(opts.Skip)
	return svc
}

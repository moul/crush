package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/tooldefs"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// RunOptions configures a single stateless shell execution via [Run].
//
// The zero value is not useful; at minimum Command must be set. Stdin,
// Stdout, and Stderr may be nil (nil readers/writers are treated as
// empty/discard). BlockFuncs may be nil to disable block-list enforcement —
// hooks use this to run user-authored commands with the same trust level as
// a shell alias.
type RunOptions struct {
	// Command is the shell source to parse and execute.
	Command string
	// Cwd is the working directory for the execution. Required: callers
	// must supply a non-empty value. Run does not silently fall back to
	// the Crush process cwd — hooks and the bash tool have different
	// notions of "default" and each owns that decision.
	Cwd string
	// Env is the full environment visible to the command. The caller is
	// responsible for inheriting from os.Environ() if that's desired.
	Env []string
	// Stdin is the command's standard input. nil is equivalent to an empty
	// input stream.
	Stdin io.Reader
	// Stdout receives the command's standard output. nil discards output.
	Stdout io.Writer
	// Stderr receives the command's standard error. nil discards output.
	Stderr io.Writer
	// BlockFuncs is an optional list of deny-list matchers applied before
	// each command reaches the exec layer. nil disables blocking entirely.
	BlockFuncs []BlockFunc
	// TermWidth is the terminal width in columns for PTY execution.
	// Zero uses a default of 200.
	TermWidth int
}

// Run parses and executes a shell command using the same mvdan.cc/sh
// interpreter stack that the stateful [Shell] type uses (builtins,
// optional block list, optional Go coreutils). It is safe to call
// concurrently from multiple goroutines: each call builds its own
// [interp.Runner] and shares no state with other callers or with any
// [Shell] instance.
//
// Errors returned from the command itself (non-zero exit, context
// cancellation, parse failures) follow the same conventions as
// [Shell.Exec]: inspect with [IsInterrupt] and [ExitCode].
func Run(ctx context.Context, opts RunOptions) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("command execution panic: %v", r)
		}
	}()

	if opts.Cwd == "" {
		return fmt.Errorf("shell.Run: Cwd is required")
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	line, err := syntax.NewParser().Parse(strings.NewReader(opts.Command), "")
	if err != nil {
		return fmt.Errorf("could not parse command: %w", err)
	}

	runner, err := newRunner(opts.Cwd, opts.Env, opts.Stdin, stdout, stderr, opts.BlockFuncs)
	if err != nil {
		return fmt.Errorf("could not run command: %w", err)
	}

	err = runner.Run(ctx, line)
	noteLookups(stderr, line, runner)
	return err
}

// CaptureResult holds the combined output and exit code from a
// captured shell execution.
type CaptureResult struct {
	Output   string
	ExitCode int
}

// PersistFunc is a callback that persists a shell command result.
// Used by RunAndPersist to decouple execution from storage.
type PersistFunc func(command, output string, exitCode int) error

// RunAndPersist executes a shell command via PTY and optionally
// persists the result through the provided callback. This unifies
// the run-and-save pattern used by both AppWorkspace and Backend.
func RunAndPersist(ctx context.Context, opts RunOptions, persist PersistFunc) (CaptureResult, error) {
	result, err := RunAndCapturePTY(ctx, opts)
	if err != nil {
		return CaptureResult{}, err
	}

	if persist != nil {
		if persistErr := persist(opts.Command, result.Output, result.ExitCode); persistErr != nil {
			slog.Error("Failed to persist shell command output", "error", persistErr, "command", opts.Command)
		}
	}

	return result, nil
}

// RunAndCapture executes a shell command and returns its combined
// stdout/stderr output along with the exit code. It inherits the
// current process environment when opts.Env is nil.
func RunAndCapture(ctx context.Context, opts RunOptions) (CaptureResult, error) {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}

	var stdout, stderr bytes.Buffer
	opts.Stdout = &stdout
	opts.Stderr = &stderr

	runErr := Run(ctx, opts)

	exitCode := 0
	if runErr != nil {
		exitCode = ExitCode(runErr)
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	return CaptureResult{
		Output:   withRunError(output, runErr),
		ExitCode: exitCode,
	}, nil
}

// withRunError appends to output why the shell could not run the command,
// when it could not. Only then is the reason in the error alone: a command
// that ran and exited non-zero has already said why on its stderr.
func withRunError(output string, err error) string {
	if err == nil || IsExitStatus(err) || IsInterrupt(err) {
		return output
	}
	if output != "" && !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return output + err.Error()
}

// ptyColorEnvVars force color output for programs running inside a
// PTY. These are only applied in RunAndCapturePTY, not in the plain
// Run/RunAndCapture paths where ANSI codes would be noise.
var ptyColorEnvVars = []string{
	"COLORTERM=truecolor",
	"CLICOLOR_FORCE=1",
	"FORCE_COLOR=1",
}

// RunAndCapturePTY executes a shell command through the mvdan.cc/sh
// interpreter with color-forcing environment variables set. Programs
// that respect FORCE_COLOR or CLICOLOR_FORCE (git, cargo, npm, eza,
// bat, ripgrep, etc.) will emit ANSI color sequences even without a
// real PTY. This approach is fully cross-platform — no /bin/sh or
// unix PTY required.
//
// The name is preserved for API compatibility; the PTY path has been
// replaced by the portable interpreter + env-var approach.
func RunAndCapturePTY(ctx context.Context, opts RunOptions) (CaptureResult, error) {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	opts.Env = append(opts.Env, ptyColorEnvVars...)
	return RunAndCapture(ctx, opts)
}

// newRunner constructs an [interp.Runner] configured with the standard
// Crush handler stack. Shared by the stateless [Run] entrypoint and the
// stateful [Shell] so the two surfaces cannot drift.
func newRunner(cwd string, env []string, stdin io.Reader, stdout, stderr io.Writer, blockFuncs []BlockFunc) (*interp.Runner, error) {
	env = withNonInteractiveEnv(env)
	return interp.New(
		interp.StdIO(stdin, stdout, stderr),
		interp.Interactive(false),
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir(cwd),
		execHandlerOption(blockFuncs),
	)
}

// execHandlerOption returns an interp.RunnerOption that installs the
// standard Crush middleware chain (builtins, script dispatch, block list)
// on top of a process-group-isolated base exec handler.
//
// We use interp.ExecHandler (singular) with a manually-built chain rather
// than interp.ExecHandlers because the latter always appends
// interp.DefaultExecHandler as the final handler, which lacks process group
// isolation. Without isolation, shells like zsh that set up job control
// when sourcing framework files can send SIGINT/SIGTERM to Crush's process
// group and gnosh the parent.
func execHandlerOption(blockFuncs []BlockFunc) interp.RunnerOption {
	base := processGroupExecHandler(defaultKillTimeout)
	handler := base
	for _, mw := range slices.Backward(standardHandlers(blockFuncs)) {
		handler = mw(handler)
	}
	// ExecHandlers always appends DefaultExecHandler which lacks process
	// group isolation, so we use the deprecated ExecHandler instead.
	return interp.ExecHandler(handler)
}

// nonInteractiveEnvVars are forced on every shell execution to prevent
// commands from hanging on a nonexistent TTY. These are always applied
// regardless of the caller's environment because Crush shells are never
// interactive — preserving user preferences like EDITOR=nvim only causes
// hangs, not useful behavior.
var nonInteractiveEnvVars = []string{
	"TERM=xterm-256color",
	"GIT_EDITOR=false",
	"EDITOR=false",
	"VISUAL=false",
	"JJ_EDITOR=false",
	"JJ_PAGER=cat",
	"GIT_PAGER=cat",
	"PAGER=cat",
}

// withNonInteractiveEnv returns env with nonInteractiveEnvVars forced in,
// replacing any existing values for those keys. The returned slice is a
// new allocation safe to use concurrently with the input.
func withNonInteractiveEnv(env []string) []string {
	// Build a set of override keys for fast lookup.
	overrideKeys := make(map[string]bool, len(nonInteractiveEnvVars))
	for _, kv := range nonInteractiveEnvVars {
		if key, _, ok := strings.Cut(kv, "="); ok {
			overrideKeys[key] = true
		}
	}

	// Copy env, filtering out any keys we will override.
	result := make([]string, 0, len(env)+len(nonInteractiveEnvVars))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && overrideKeys[key] {
			continue
		}
		result = append(result, e)
	}

	return append(result, nonInteractiveEnvVars...)
}

// herdrEnvVars are the environment variables herdr injects into panes
// so agents can report state over its Unix socket API. Subprocesses
// must not inherit these: a child process that calls herdr.Init()
// would attach to the parent's pane and, on exit, release its agent
// authority — making the status vanish. Stripping them here closes
// that gap for every command the bash tool runs.
var herdrEnvVars = []string{
	"HERDR_ENV",
	"HERDR_SOCKET_PATH",
	"HERDR_PANE_ID",
}

// withoutHerdrEnv returns env with all HERDR_* variables removed.
// The returned slice is a new allocation safe to use concurrently
// with the input.
func withoutHerdrEnv(env []string) []string {
	strip := make(map[string]bool, len(herdrEnvVars))
	for _, k := range herdrEnvVars {
		strip[k] = true
	}
	result := make([]string, 0, len(env))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok && strip[key] {
			continue
		}
		result = append(result, e)
	}
	return result
}

// execMiddleware wraps a base [interp.ExecHandlerFunc], composing like HTTP
// middleware: each layer either handles a command itself or delegates to the
// next handler in the chain.
type execMiddleware = func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc

// standardHandlers returns the exec-handler middleware chain used by both
// [Run] and [Shell]. Order matters:
//  1. builtins first (so Crush's in-process jq wins over any PATH binary);
//  2. script dispatch (shebang / binary / shell-source for path-prefixed
//     argv[0], no-op for bare commands) — runs before the block list so
//     that deny rules see the already-resolved argv of anything the
//     script exec's rather than the outer path-prefixed wrapper;
//  3. block list;
//  4. tool names (a Gnosh tool run as a command is told to be called);
//  5. missing commands (told what to do instead of searching for them);
//  6. optional Go coreutils (only when useGoCoreUtils is on).
func standardHandlers(blockFuncs []BlockFunc) []execMiddleware {
	handlers := []execMiddleware{
		builtinHandler(),
		scriptDispatchHandler(blockFuncs),
		blockHandler(blockFuncs),
		toolNameHandler(),
		missingCommandHandler(),
	}
	if useGoCoreUtils && coreUtilsExecHandler != nil {
		handlers = append(handlers, coreUtilsExecHandler)
	}
	return handlers
}

// builtinHandler returns middleware that dispatches recognized Crush
// builtins to their in-process Go implementations. All builtins (jq plus the
// config builtins registered by shellconfig) live in the builtins map; config
// builtins are no-ops without a ConfigBuilder on the context.
func builtinHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			if h, ok := builtins[args[0]]; ok {
				hc := interp.HandlerCtx(ctx)
				err := h(ctx, args, hc.Stdin, hc.Stdout, hc.Stderr)
				if !errors.Is(err, ErrNotABuiltinHere) {
					return err
				}
			}
			return next(ctx, args)
		}
	}
}

// blockHandler returns middleware that rejects commands matched by any of
// the provided [BlockFunc]s before they reach the underlying exec path.
// A nil or empty blockFuncs slice is a no-op.
func blockHandler(blockFuncs []BlockFunc) execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return next(ctx, args)
			}
			for _, blockFunc := range blockFuncs {
				if blockFunc(args) {
					return fmt.Errorf("command is not allowed for security reasons: %q", strings.Join(args, " "))
				}
			}
			return next(ctx, args)
		}
	}
}

// toolNameHandler returns middleware that says so when a command the
// shell cannot find is named like one of Gnosh's tools: a model that runs
// "job_kill 077" in bash meant the job_kill tool, and the shell's own
// "executable file not found" left an agent on the AI box with a server
// it never stopped. A command of that name on the PATH runs as usual.
func toolNameHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			err := next(ctx, args)
			if len(args) == 0 || ExitCode(err) != 127 || !tooldefs.IsKnown(args[0]) {
				return err
			}
			hc := interp.HandlerCtx(ctx)
			if _, lookErr := interp.LookPathDir(hc.Dir, hc.Env, args[0]); lookErr == nil {
				return err
			}
			fmt.Fprintf(hc.Stderr, "%s is a Gnosh tool, not a shell command: call the %s tool instead.\n", args[0], tooldefs.Canonical(args[0]))
			return err
		}
	}
}

// missingCommandHandler returns middleware that says what to do when the
// shell cannot find a command by name. Told only "executable file not
// found", agents searched the whole disk for it with `find /`, a prompt
// line against it notwithstanding: a note under the failure is read
// before the next call (QA, 2026-10-30).
func missingCommandHandler() execMiddleware {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			err := next(ctx, args)
			if len(args) == 0 || ExitCode(err) != 127 {
				return err
			}
			if hc := interp.HandlerCtx(ctx); notInstalled(hc.Dir, hc.Env, args[0]) {
				fmt.Fprintln(hc.Stderr, MissingCommandNote(args[0]))
			}
			return err
		}
	}
}

// noteLookups writes the same note for each program that file, once r
// has run it, looked up by a name its text writes out and r did not
// find: `command -v`, `type` and `which` say nothing of it, or "not
// found", and an agent whose empty `command -v` sat in a longer command
// searched the disk next (QA, 2026-10-30). Coming after the run, the
// notes are not thrown away with a lookup's own output (`>/dev/null
// 2>&1`), and a function the run defined, nvm's say, counts as found. A
// file the command sources, a .bashrc say, probes for tools by names
// the command does not write, and those get no note.
func noteLookups(w io.Writer, file *syntax.File, r *interp.Runner) {
	if w == nil {
		return
	}
	env := expand.FuncEnviron(func(name string) string { return r.Vars[name].String() })
	noted := map[string]bool{}
	for _, name := range lookups(file) {
		if noted[name] || r.Funcs[name] != nil || !notInstalled(r.Dir, env, name) {
			continue
		}
		noted[name] = true
		fmt.Fprintln(w, MissingCommandNote(name))
	}
}

// lookups returns the names file's lookups write out: a word as written,
// or for a loop's variable alone (`for t in go gofumpt; do command -v
// "$t"`), the loop's words. A lookup with assignments of its own
// (`PATH=… command -v x`) looks where the run does not, and is left out.
func lookups(file *syntax.File) []string {
	loops := map[string][]*syntax.Word{}
	var names []string
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ForClause:
			if it, ok := n.Loop.(*syntax.WordIter); ok {
				loops[it.Name.Value] = it.Items
			}
		case *syntax.CallExpr:
			var args []string
			for _, w := range n.Args {
				if items, ok := loops[loopVar(w)]; ok {
					for _, item := range items {
						args = append(args, wordText(item))
					}
				} else {
					args = append(args, wordText(w))
				}
			}
			if len(args) > 0 && len(n.Assigns) == 0 {
				names = append(names, lookedUp(args)...)
			}
		}
		return true
	})
	return names
}

// wordText returns w as written when nothing in it expands, else "".
func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[{") {
				return "" // a glob or braces
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return ""
				}
				b.WriteString(lit.Value)
			}
		default:
			return ""
		}
	}
	return b.String()
}

// loopVar returns t when w is $t or "$t", else "".
func loopVar(w *syntax.Word) string {
	parts := w.Parts
	if len(parts) == 1 {
		if dq, ok := parts[0].(*syntax.DblQuoted); ok {
			parts = dq.Parts
		}
	}
	if len(parts) == 1 {
		if pe, ok := parts[0].(*syntax.ParamExp); ok && pe.Short {
			return pe.Param.Value
		}
	}
	return ""
}

// lookedUp returns the names args look up, when they are a lookup:
// `command -v`, `type` or `which`, after any flags.
func lookedUp(args []string) []string {
	if args[0] != "command" && args[0] != "type" && args[0] != "which" {
		return nil
	}
	names, lookup := args[1:], args[0] != "command"
	for len(names) > 0 && strings.HasPrefix(names[0], "-") {
		lookup = lookup || names[0] == "-v"
		names = names[1:]
	}
	if !lookup {
		return nil // `command name` runs it
	}
	return names
}

// notInstalled reports whether nothing answers to name in a shell in dir
// with env: no keyword, no builtin of the shell's or Gnosh's own, and no
// program on the PATH. A path, and a Gnosh tool's name, have notes of
// their own or none.
func notInstalled(dir string, env expand.Environ, name string) bool {
	if name == "" || strings.Contains(name, "/") || tooldefs.IsKnown(name) ||
		syntax.IsKeyword(name) || interp.IsBuiltin(name) || IsBuiltin(name) {
		return false
	}
	_, err := interp.LookPathDir(dir, env, name)
	return err != nil
}

// MissingCommandNote is what an agent is told of a command that is not
// installed.
func MissingCommandNote(name string) string {
	return name + " is not installed here: use another way if there is one, or ask the user where it is or how to get it; do not search the disk for it."
}

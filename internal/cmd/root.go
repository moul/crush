package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	fang "charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/gnovm"
	"github.com/charmbracelet/crush/internal/lock"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/server"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/exitbanner"
	"github.com/charmbracelet/crush/internal/ui/logo"
	ui "github.com/charmbracelet/crush/internal/ui/model"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/charmbracelet/crush/internal/workspace"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	xstrings "github.com/charmbracelet/x/exp/strings"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var clientHost string

func init() {
	rootCmd.PersistentFlags().StringP("cwd", "c", "", "Current working directory")
	rootCmd.PersistentFlags().StringP("data-dir", "D", "", "Custom data directory")
	rootCmd.PersistentFlags().BoolP("debug", "d", false, "Debug")
	rootCmd.PersistentFlags().StringVarP(&clientHost, "host", "H", server.DefaultHost(), "Connect to a specific Gnosh server host (for advanced users)")
	rootCmd.Flags().BoolP("help", "h", false, "Help")
	addYoloFlags(rootCmd)
	addToolFlags(rootCmd)
	addSystemPromptFlags(rootCmd)
	addPermissionModeFlag(rootCmd)
	addAddDirFlag(rootCmd)
	addMCPConfigFlags(rootCmd)
	rootCmd.PersistentFlags().StringSlice("channels", nil, "MCP servers to enable as channels (repeatable), e.g. --channels server:webhook")
	_ = rootCmd.PersistentFlags().MarkHidden("channels")
	addSessionFlags(rootCmd)
	addSafeModeFlag(rootCmd)
	addTrustFolderFlag(rootCmd)
	rootCmd.Flags().StringP("model", "m", "", "Model for this session, in memory only, as Claude Code's --model: 'model' or 'provider/model', as gnosh run's -m takes; /model still switches and saves as before")

	rootCmd.AddCommand(
		promptCmd,
		runCmd,
		dirsCmd,
		projectsCmd,
		updateProvidersCmd,
		logsCmd,
		logoutCmd,
		schemaCmd,
		loginCmd,
		statsCmd,
		sessionCmd,
		memCmd,
		doctorCmd,
		trustCmd,
	)
}

var rootCmd = &cobra.Command{
	Use:   version.Command,
	Short: "A security-first harness for coding agents, in your terminal",
	Long:  "A security-first harness for coding agents, in your terminal.",
	Example: `
# Run in interactive mode
gnosh

# Run non-interactively (Claude Code's -p works too: gnosh -p "...")
gnosh run "Guess my 5 favorite Pokémon"

# Run a non-interactively with pipes and redirection
cat README.md | gnosh run "make this more glamorous" > GLAMOROUS_README.md

# Run with debug logging in a specific directory
gnosh --debug --cwd /path/to/project

# Run in yolo mode (auto-accept all permissions; use with care)
gnosh --yolo

# Run with custom data directory
gnosh --data-dir /path/to/custom/.gnosh

# Continue a previous session
gnosh --session {session-id}

# Continue the most recent session
gnosh --continue

# Start with a prompt, as Claude Code's claude "query" does, or resume a
# session with one
gnosh "explain this project"
gnosh -r auth-refactor "finish this PR"
  `,
	// A prompt may follow the flags, as in Claude Code; a first word that
	// names a subcommand is that subcommand.
	Args: cobra.ArbitraryArgs,
	RunE: stopsOnSignal(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		if err := forkAlone(cmd); err != nil {
			return err
		}
		// Ask once per machine, before anything sets up metrics or the TUI
		// takes the terminal.
		maybeAskMetricsConsent()

		sessionID := sessionFlag(cmd)
		continueLast, _ := cmd.Flags().GetBool("continue")
		fork, _ := cmd.Flags().GetBool("fork-session")
		pick := sessionID == pickSession
		if pick {
			sessionID = ""
		}

		// Ask before the config loads: whether the folder is trusted
		// decides whether its gnoshrc runs and its settings apply.
		if err := askFolderTrust(cmd); err != nil {
			return err
		}

		ws, cleanup, err := setupWorkspaceWithProgressBar(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		var search string
		if sessionID != "" {
			sess, err := resolveWorkspaceSessionID(ctx, ws, sessionID)
			if resume, _ := cmd.Flags().GetString("resume"); err != nil && resume != "" {
				sess, search, err = resumeByName(ctx, ws, resume)
			}
			if err != nil {
				return err
			}
			sessionID = sess.ID
		}

		chosen, _ := cmd.Flags().GetString("model")
		if chosen != "" {
			if err := overrideTUIModel(ctx, ws, chosen); err != nil {
				return err
			}
		}

		event.AppInitialized()

		com := common.DefaultCommon(ws)
		model := ui.New(com, sessionID, continueLast)
		if chosen != "" {
			model.ModelChosenAtStart()
		}
		if pick || search != "" {
			model.PickSessionAtStart(search)
		}
		if fork {
			model.ForkSessionAtStart()
		}
		if name := nameFlag(cmd); name != "" {
			model.NameSessionAtStart(name)
		}
		if prompt := strings.Join(args, " "); strings.TrimSpace(prompt) != "" {
			model.SendAtStart(prompt)
		}
		if mode, _ := permissionModeFlag(cmd); mode == config.PermissionModePlan {
			model.StartInPlanMode()
		}

		inputFilter := ui.NewFilter()
		var env uv.Environ = os.Environ()
		program := tea.NewProgram(
			model,
			tea.WithEnvironment(env),
			// Not ctx: Bubble Tea quits on SIGTERM by itself, its shutdown
			// done and the resume hint printed; ctx only makes that a 143
			// (stopsOnSignal). Killed through ctx, the program skipped both.
			tea.WithContext(cmd.Context()),
			tea.WithFilter(inputFilter.Filter),
		)
		go ws.Subscribe(program)

		_, err = program.Run()
		// Stopped by SIGINT or SIGTERM: the open session ends with reason
		// "other", as Claude Code's does, not "prompt_input_exit".
		if _, ok := errors.AsType[stoppedBySignal](context.Cause(ctx)); ok {
			if appWs, ok := ws.(*workspace.AppWorkspace); ok {
				appWs.App().StoppedBySignal()
			}
		}
		if err != nil {
			if stopped := tuiStopped(ctx, err); stopped != nil {
				return stopped
			}
			event.Error(err)
			slog.Error("TUI run error", "error", err)
			return errors.New(version.Name + " hit an internal error. If metrics are enabled, we were notified about it. If you'd like to report it, please copy the stacktrace above and open an issue at https://github.com/charmbracelet/crush/issues/new?template=bug.yml") //nolint:staticcheck
		}
		var banner config.ExitBanner
		if cfg := com.Config(); cfg != nil {
			banner = cfg.Options.TUI.ExitBanner
		}
		printSessionResume(model, banner)
		return nil
	}),
}

// versionArt is the compact GNOSH wordmark printed above `gnosh --version`,
// in the default theme's colors.
func versionArt() string {
	t := styles.GnoshCosmos()
	return logo.Render(t.Logo.GradCanvas, "", true, logo.Opts{
		FieldColor:   t.Logo.FieldColor,
		FieldColorB:  t.Logo.FieldColorB,
		TitleColorA:  t.Logo.TitleColorA,
		TitleColorB:  t.Logo.TitleColorB,
		CharmColor:   t.Logo.CharmColor,
		VersionColor: t.Logo.VersionColor,
	})
}

// colorScheme is fang's own, Charm's colors, with one of Gnosh's: the
// program's name in gold.
func colorScheme(c lipgloss.LightDarkFunc) fang.ColorScheme {
	s := fang.DefaultColorScheme(c)
	s.Program = c(lipgloss.Color("#8A6400"), styles.GnoshGold)
	return s
}

// printSessionResume prints the exit banner after the TUI exits, including the
// session title and the hint to resume it with `crush -s <id>`. The banner
// style decides how much of that is shown; see config.ExitBanner.
func printSessionResume(model *ui.UI, banner config.ExitBanner) {
	tw, _, _ := term.GetSize(os.Stdout.Fd())
	body := exitbanner.Render(banner, model.CurrentSession(), tw)
	if body == "" {
		return
	}
	fmt.Fprintln(colorprofile.NewWriter(os.Stderr, os.Environ()), body)
}

// copied from cobra:
const defaultVersionTemplate = `{{with .DisplayName}}{{printf "%s " .}}{{end}}{{printf "version %s" .Version}}
` + gnovm.AttributionNotice + "\n"

// printAsRun maps Claude Code's way of asking one question without the
// TUI, -p/--print, onto gnosh run: `gnosh -p "question"`, with any flags
// after it, runs as `gnosh run "question"`. Only a leading -p/--print is
// rewritten, the form Claude Code users type.
func printAsRun(args []string) ([]string, bool) {
	if len(args) == 0 || (args[0] != "-p" && args[0] != "--print") {
		return args, false
	}
	return append([]string{"run"}, args[1:]...), true
}

func Execute() {
	// config.Load logs during provider resolution, but the file-based
	// logger isn't set up until after config is loaded (the log path
	// depends on the data directory from config). Until then, records are
	// kept rather than written to stderr, where they would land on the
	// TUI, and crushlog.Setup writes them to the log file. They used to
	// be discarded, so config warnings were never seen.
	slog.SetDefault(slog.New(crushlog.Early()))

	// NOTE: very hacky: we create a colorprofile writer with STDOUT, then make
	// it forward to a bytes.Buffer, write the colored wordmark to it, and then
	// finally prepend it in the version template.
	// Unfortunately cobra doesn't give us a way to set a function to handle
	// printing the version, and PreRunE runs after the version is already
	// handled, so that doesn't work either.
	// This is the only way I could find that works relatively well.
	if term.IsTerminal(os.Stdout.Fd()) {
		var b bytes.Buffer
		w := colorprofile.NewWriter(os.Stdout, os.Environ())
		w.Forward = &b
		_, _ = w.WriteString(versionArt())
		rootCmd.SetVersionTemplate(b.String() + "\n" + defaultVersionTemplate)
	}
	args := os.Args[1:]
	if run, ok := printAsRun(args); ok {
		args = run
	}
	rootCmd.SetArgs(bareResume(args))
	if err := fang.Execute(
		context.Background(),
		rootCmd,
		fang.WithVersion(version.Version),
		fang.WithColorSchemeFunc(colorScheme),
		fang.WithNotifySignal(os.Interrupt),
		fang.WithErrorHandler(errorHandler),
	); err != nil {
		os.Exit(exitCode(err))
	}
}

// errorHandler prints a command's error as fang does, but capitalizes only
// its first letter, and prints nothing for an exitStatus, which only sets
// the status gnosh exits with. fang title-cases the whole first word,
// capitalizing after every - or ( and lowercasing the rest, so an error
// that starts with a flag, a quoted name, an environment variable or a
// path showed it changed: --Disallowed-Tools, "Bash(Git:*)",
// Gnosh_client_server.
func errorHandler(w io.Writer, styles fang.Styles, err error) {
	if _, ok := errors.AsType[exitStatus](err); ok {
		return
	}
	styles.ErrorText = styles.ErrorText.Transform(capitalizeFirstLetter)
	fang.DefaultErrorHandler(w, styles, err)
}

// capitalizeFirstLetter upper-cases s's first character if it is a
// lower-case letter, and leaves the rest of s as it is.
func capitalizeFirstLetter(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// supportsProgressBar tries to determine whether the current terminal supports
// progress bars by looking into environment variables.
func supportsProgressBar() bool {
	if !term.IsTerminal(os.Stderr.Fd()) {
		return false
	}
	termProg := os.Getenv("TERM_PROGRAM")
	_, isWindowsTerminal := os.LookupEnv("WT_SESSION")

	return isWindowsTerminal || xstrings.ContainsAnyOf(strings.ToLower(termProg), "ghostty", "iterm2", "rio")
}

// useClientServer returns true when the client/server architecture is
// enabled via the GNOSH_CLIENT_SERVER environment variable.
func useClientServer() bool {
	v, _ := strconv.ParseBool(env.Getenv("GNOSH_CLIENT_SERVER"))
	return v
}

// setupWorkspaceWithProgressBar wraps setupWorkspace with an optional
// terminal progress bar shown during initialization.
func setupWorkspaceWithProgressBar(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	showProgress := supportsProgressBar()
	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
	}

	ws, cleanup, err := setupWorkspace(cmd)

	if showProgress {
		_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
	}

	return ws, cleanup, err
}

// setupWorkspace returns a Workspace and cleanup function. When
// GNOSH_CLIENT_SERVER=1, it connects to a server process and returns a
// ClientWorkspace. Otherwise it creates an in-process app.App and
// returns an AppWorkspace.
func setupWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	if useClientServer() {
		return setupClientServerWorkspace(cmd)
	}
	return setupLocalWorkspace(cmd)
}

// addYoloFlags adds --yolo and Claude Code's name for it,
// --dangerously-skip-permissions; read them with yoloFlag.
func addYoloFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP("yolo", "y", false, "Automatically accept all permissions (dangerous mode)")
	cmd.Flags().Bool("dangerously-skip-permissions", false, "Same as --yolo, by Claude Code's name for it")
}

// yoloFlag reports whether every permission is to be accepted.
func yoloFlag(cmd *cobra.Command) bool {
	yolo, _ := cmd.Flags().GetBool("yolo")
	skip, _ := cmd.Flags().GetBool("dangerously-skip-permissions")
	return yolo || skip || flagValue(cmd, "permission-mode") == config.PermissionModeBypass
}

// workspaceYolo reports whether the workspace cmd sets up accepts every
// permission request. gnosh run takes the yolo flags but keeps them to
// itself: it already accepts its own session's requests, and a yolo
// workspace it created on a shared server would hand yolo to a TUI that
// joined it later, since the first client's flags win.
func workspaceYolo(cmd *cobra.Command) bool {
	return cmd.Name() != "run" && yoloFlag(cmd)
}

// addSessionFlags adds --session, Claude Code's name for it (-r/--resume),
// --continue and --fork-session; read the session with sessionFlag.
func addSessionFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("session", "s", "", "Continue a previous session by ID")
	cmd.Flags().StringP("resume", "r", "", "Same as --session, by Claude Code's name for it, and also by a session's name; with no ID, or a name no session or several have, pick a session from a list")
	cmd.Flags().BoolP("continue", "C", false, "Continue the most recent session")
	cmd.MarkFlagsMutuallyExclusive("session", "resume", "continue")
	cmd.Flags().Bool("fork-session", false, forkSessionUsage)
	cmd.Flags().StringP("name", "n", "", nameUsage)
}

// nameUsage is the help of Claude Code's -n/--name, in gnosh and gnosh
// run.
const nameUsage = "A name for the session, as /rename gives one: the new session, the one resumed, or with --fork-session its copy"

// addSafeModeFlag adds Claude Code's --safe-mode to cmd.
func addSafeModeFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("safe-mode", false, "Start with the config's customizations off, to troubleshoot a broken one: hooks, MCP servers (but --mcp-config's), context files (GNOSH.md, AGENTS.md, CLAUDE.md), skills (but the built-in ones) and custom commands; providers, models and permissions work as before. CLAUDE_CODE_SAFE_MODE=1 does the same")
}

// safeModeOn reports whether Claude Code's --safe-mode is asked for, by
// the flag or by CLAUDE_CODE_SAFE_MODE (or GNOSH_SAFE_MODE) set true.
func safeModeOn(cmd *cobra.Command) bool {
	if on, _ := cmd.Flags().GetBool("safe-mode"); on {
		return true
	}
	for _, name := range []string{"CLAUDE_CODE_SAFE_MODE", "GNOSH_SAFE_MODE"} {
		if on, _ := strconv.ParseBool(os.Getenv(name)); on {
			return true
		}
	}
	return false
}

// errSafeModeServer is --safe-mode through a server, whose config every
// client of a workspace shares.
var errSafeModeServer = errors.New("--safe-mode is not supported with GNOSH_CLIENT_SERVER=1 yet")

// nameFlag is the name -n/--name gives the session, as /rename keeps one;
// "" for none.
func nameFlag(cmd *cobra.Command) string {
	name, _ := cmd.Flags().GetString("name")
	return session.CleanName(name)
}

// overrideTUIModel has the TUI's session use model, as Claude Code's
// --model does, in memory only: the config files are not changed. A
// server's models are shared by every client of its workspace and it can
// only save a choice, so through one --model is refused.
func overrideTUIModel(ctx context.Context, ws workspace.Workspace, model string) error {
	appWs, ok := ws.(*workspace.AppWorkspace)
	if !ok {
		return fmt.Errorf("--model is not supported with %s=1 yet; pick the model with /model", "GNOSH_CLIENT_SERVER")
	}
	return appWs.App().OverrideModels(ctx, model, "")
}

// forkSessionUsage is the help of Claude Code's --fork-session, in gnosh
// and gnosh run.
const forkSessionUsage = "When resuming, go on in a copy of the session, as /branch makes one, and leave the session as it was (with --continue, --resume or --session)"

// errForkAlone is --fork-session with no session to go back to.
var errForkAlone = errors.New("--fork-session goes on in a copy of the session you resume, so it needs --continue, --resume or --session")

// forkAlone is errForkAlone when cmd has --fork-session but no session to
// resume: none named or to be picked, and no --continue.
func forkAlone(cmd *cobra.Command) error {
	fork, _ := cmd.Flags().GetBool("fork-session")
	last, _ := cmd.Flags().GetBool("continue")
	if fork && sessionFlag(cmd) == "" && !last {
		return errForkAlone
	}
	return nil
}

// pickSession is the --resume value that asks for the list of sessions to
// pick from: what a bare -r or --resume becomes (see bareResume), as
// Claude Code's `claude -r` opens its picker. Session IDs are UUIDs, so it
// cannot name one.
const pickSession = ":pick:"

// bareResume gives a -r or --resume with no ID after it, at the end or
// before another flag, the value pickSession. The flag cannot take an
// optional value instead: pflag would then no longer read `-r <id>`, the
// form Claude Code users type.
func bareResume(args []string) []string {
	out := slices.Clone(args)
	if len(out) > 0 && (out[0] == cobra.ShellCompRequestCmd || out[0] == cobra.ShellCompNoDescRequestCmd) {
		return out
	}
	for i, arg := range out {
		if arg == "--" {
			break
		}
		if (arg == "-r" || arg == "--resume") && (i+1 == len(out) || strings.HasPrefix(out[i+1], "-")) {
			out[i] = "--resume=" + pickSession
		}
	}
	return out
}

// sessionFlag is the ID of the session to continue, from --session or
// --resume.
func sessionFlag(cmd *cobra.Command) string {
	id, _ := cmd.Flags().GetString("session")
	if id == "" {
		id, _ = cmd.Flags().GetString("resume")
	}
	return id
}

// setupLocalWorkspace creates an in-process app.App and wraps it in an
// AppWorkspace.
func setupLocalWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	debug, _ := cmd.Flags().GetBool("debug")
	yolo := workspaceYolo(cmd)
	safeMode := safeModeOn(cmd)
	channels, _ := cmd.Flags().GetStringSlice("channels")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	ctx := cmd.Context()

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, err
	}

	allowedTools, allowRules, disallowedTools, denyRules, err := toolFlags(cmd)
	if err != nil {
		return nil, nil, err
	}
	systemPrompt, appendSystemPrompt, err := systemPromptFlags(cmd)
	if err != nil {
		return nil, nil, err
	}
	permissionMode, err := permissionModeFlag(cmd)
	if err != nil {
		return nil, nil, err
	}
	addedDirs, err := addDirFlag(cmd)
	if err != nil {
		return nil, nil, err
	}
	mcpServers, strictMCP, err := mcpConfigFlags(cmd)
	if err != nil {
		return nil, nil, err
	}

	store, err := config.Init(cwd, dataDir, debug, trustFolderOptions(cmd)...)
	if err != nil {
		return nil, nil, err
	}

	cfg := store.Config()
	store.Overrides().SkipPermissionRequests = yolo
	store.Overrides().EnabledChannels = channels
	store.Overrides().SystemPrompt = systemPrompt
	store.Overrides().AppendSystemPrompt = appendSystemPrompt
	applyPermissionMode(store.Overrides(), permissionMode)
	store.Overrides().AddedDirs = addedDirs
	if len(allowedTools) > 0 || len(allowRules) > 0 || len(disallowedTools) > 0 || len(denyRules) > 0 || len(mcpServers) > 0 || strictMCP || safeMode {
		store.Overrides().AllowedTools = allowedTools
		store.Overrides().AllowRules = allowRules
		store.Overrides().DisabledTools = disallowedTools
		store.Overrides().DenyRules = denyRules
		store.Overrides().MCP = mcpServers
		store.Overrides().StrictMCP = strictMCP
		store.Overrides().SafeMode = safeMode
		store.SetupAgents()
	}

	if err := os.MkdirAll(cfg.Options.DataDirectory, 0o700); err != nil {
		return nil, nil, fmt.Errorf("failed to create data directory: %q %w", cfg.Options.DataDirectory, err)
	}

	gitIgnorePath := filepath.Join(cfg.Options.DataDirectory, ".gitignore")
	if _, err := os.Stat(gitIgnorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitIgnorePath, []byte("*\n"), 0o644); err != nil {
			return nil, nil, fmt.Errorf("failed to create .gitignore file: %q %w", gitIgnorePath, err)
		}
	}

	if err := projects.Register(cwd, cfg.Options.DataDirectory); err != nil {
		slog.Warn("Failed to register project", "error", err)
	}

	conn, err := db.Connect(ctx, cfg.Options.DataDirectory)
	if err != nil {
		return nil, nil, err
	}

	logFile := config.LogFile(filepath.Join(cfg.Options.DataDirectory, "logs"))
	crushlog.Setup(logFile, debug)

	// Discover skills once before app.New. Local mode hosts a single
	// workspace per process, so WithGlobalMirror keeps the package
	// globals (which the TUI reads via skills.GetLatestStates) in sync
	// with the manager.
	discoveryCfg := localSkillsDiscoveryConfig(store)
	allSkills, activeSkills, skillStates := skills.DiscoverFromConfig(discoveryCfg)
	skillsMgr := skills.NewManager(
		allSkills, activeSkills, skillStates,
		skills.WithGlobalMirror(),
		skills.WithResolvedPaths(discoveryCfg.ResolvePaths()),
		skills.WithWorkingDir(discoveryCfg.WorkingDir),
	)

	appInstance, err := app.New(ctx, conn, store, skillsMgr)
	if err != nil {
		_ = conn.Close()
		slog.Error("Failed to create app instance", "error", err)
		return nil, nil, err
	}

	if shouldEnableMetrics(cfg) {
		event.Init(cfg.Options.MetricsSink())
	}

	ws := workspace.NewAppWorkspace(appInstance, store)
	cleanup := func() { appInstance.Shutdown() }
	return ws, cleanup, nil
}

// localSkillsDiscoveryConfig adapts a *config.ConfigStore to the inputs
// skills.DiscoverFromConfig expects.
func localSkillsDiscoveryConfig(store *config.ConfigStore) skills.DiscoveryConfig {
	opts := store.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := store.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	return skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		WorkingDir:     store.WorkingDir(),
		Resolver:       resolver,
	}
}

// setupClientServerWorkspace connects to a server process and wraps the
// result in a ClientWorkspace.
func setupClientServerWorkspace(cmd *cobra.Command) (workspace.Workspace, func(), error) {
	if safeModeOn(cmd) {
		return nil, nil, errSafeModeServer
	}
	c, protoWs, _, err := connectToServer(cmd)
	if err != nil {
		return nil, nil, err
	}

	clientWs := workspace.NewClientWorkspace(c, *protoWs)

	if protoWs.Config.IsConfigured() {
		if err := clientWs.InitCoderAgent(cmd.Context()); err != nil {
			slog.Error("Failed to initialize coder agent", "error", err)
		}
	}

	// Clean up via Shutdown rather than connectToServer's closure: it stops
	// the subscription's reconnect/recovery loop first, so our own exit
	// cannot be mistaken for a lost workspace and re-created mid-quit.
	return clientWs, clientWs.Shutdown, nil
}

// connectToServer ensures the server is running, creates a client and
// workspace, and returns a cleanup function that deletes the workspace.
func connectToServer(cmd *cobra.Command) (*client.Client, *proto.Workspace, func(), error) {
	// The workspace's config lives on the server, so tools named here
	// would not reach it; say so rather than run with them ignored.
	if allowed, allowRules, disallowed, denyRules, err := toolFlags(cmd); err != nil {
		return nil, nil, nil, err
	} else if len(allowed) > 0 || len(allowRules) > 0 || len(disallowed) > 0 || len(denyRules) > 0 {
		return nil, nil, nil, fmt.Errorf("tools named with --allowed-tools or --disallowed-tools cannot reach the server's config with %s=1 yet; set options.disabled_tools, permissions.allow or permissions.deny in the project's gnosh.json instead", "GNOSH_CLIENT_SERVER")
	}
	if dirs, err := addDirFlag(cmd); err != nil {
		return nil, nil, nil, err
	} else if len(dirs) > 0 {
		return nil, nil, nil, fmt.Errorf("--add-dir cannot reach the server's permissions with %s=1 yet", "GNOSH_CLIENT_SERVER")
	}
	if servers, strict, err := mcpConfigFlags(cmd); err != nil {
		return nil, nil, nil, err
	} else if len(servers) > 0 || strict {
		return nil, nil, nil, fmt.Errorf("--mcp-config cannot reach the server's MCP clients with %s=1 yet", "GNOSH_CLIENT_SERVER")
	}
	if mode, err := permissionModeFlag(cmd); err != nil {
		return nil, nil, nil, err
	} else if mode == config.PermissionModeAcceptEdits || mode == config.PermissionModePlan {
		return nil, nil, nil, fmt.Errorf("--permission-mode %s cannot reach the server's agent with %s=1 yet", mode, "GNOSH_CLIENT_SERVER")
	}
	if replace, appended, err := systemPromptFlags(cmd); err != nil {
		return nil, nil, nil, err
	} else if replace != "" || appended != "" {
		return nil, nil, nil, fmt.Errorf("--system-prompt and --append-system-prompt cannot reach the server's agent with %s=1 yet", "GNOSH_CLIENT_SERVER")
	}

	hostURL, err := server.ParseHostURL(clientHost)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid host URL: %v", err)
	}

	if err := ensureServer(cmd, hostURL); err != nil {
		return nil, nil, nil, err
	}

	debug, _ := cmd.Flags().GetBool("debug")
	yolo := workspaceYolo(cmd)
	channels, _ := cmd.Flags().GetStringSlice("channels")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	c, err := client.NewClient(cwd, hostURL.Scheme, hostURL.Host)
	if err != nil {
		return nil, nil, nil, err
	}

	wsReq := proto.Workspace{
		Path:        cwd,
		DataDir:     dataDir,
		Debug:       debug,
		YOLO:        yolo,
		Channels:    channels,
		Version:     version.Version,
		Env:         os.Environ(),
		TrustFolder: trustFolderFlag(cmd),
	}

	ws, err := createWorkspaceOnLiveServer(cmd.Context(), c, wsReq, func() error {
		return replaceExitingServer(cmd, hostURL)
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if shouldEnableMetrics(ws.Config) {
		event.Init(ws.Config.Options.MetricsSink())
	}

	if ws.Config != nil {
		logFile := config.LogFile(filepath.Join(ws.Config.Options.DataDirectory, "logs"))
		crushlog.Setup(logFile, debug)
	}

	// Retiring the client releases every claim it holds, so it covers
	// workspaces this process created but never learned the ID of.
	cleanup := func() {
		if err := c.RetireClient(context.Background()); err != nil {
			_ = c.DeleteWorkspace(context.Background(), ws.ID)
		}
	}
	return c, ws, cleanup, nil
}

// maxStaleServerRetries bounds how many times workspace creation may be
// retried against a replacement server. Only one client can lose the race
// against a given server's shutdown, so a single retry is normally enough;
// the bound just keeps a pathological loop finite.
const maxStaleServerRetries = 3

// createWorkspaceOnLiveServer creates the workspace, retrying against a
// replacement when the server it reached has already committed to shutting
// itself down for being idle.
//
// That race is unavoidable: the server decides to exit while no client is
// talking to it, and a client can arrive between that decision and the
// socket going away. The decision is final on the server's side, so the
// only correct response is to bring up a fresh server and ask again
// instead of failing the command.
func createWorkspaceOnLiveServer(
	ctx context.Context, c *client.Client, req proto.Workspace, replace func() error,
) (*proto.Workspace, error) {
	for attempt := range maxStaleServerRetries {
		ws, err := c.CreateWorkspace(ctx, req)
		if err == nil {
			return ws, nil
		}
		if !errors.Is(err, client.ErrServerShuttingDown) || attempt == maxStaleServerRetries-1 {
			return nil, fmt.Errorf("failed to create workspace: %v", err)
		}
		slog.Warn("Server is shutting down; retrying against a replacement",
			"attempt", attempt+1, "error", err)
		if err := replace(); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("failed to create workspace: server kept shutting down")
}

// replaceExitingServer waits out the socket of a server that has committed
// to exiting, then brings up a fresh one.
func replaceExitingServer(cmd *cobra.Command, hostURL *url.URL) error {
	if hostURL.Scheme == "unix" {
		if err := awaitSocketGone(cmd.Context(), hostURL); err != nil {
			return err
		}
	}
	if err := spawnAndWaitReady(cmd, hostURL); err != nil {
		return fmt.Errorf("failed to initialize the Gnosh server: %v", err)
	}
	return nil
}

// ensureServer auto-starts a detached server if the socket file does not
// exist. When the socket exists, it verifies that the running server
// version matches the client; on mismatch it shuts down the old server
// and starts a fresh one.
func ensureServer(cmd *cobra.Command, hostURL *url.URL) error {
	// Initialize the persistent log here so stale-socket diagnostics
	// emitted before connectToServer runs are captured in the per-host
	// server log file. crushlog.Setup uses sync.Once internally, so the
	// later call from connectToServer becomes a no-op.
	debug, _ := cmd.Flags().GetBool("debug")
	logFile := config.LogFile(filepath.Join(config.GlobalCacheDir(), "server-"+safeHostName(hostURL)))
	crushlog.Setup(logFile, debug)

	switch hostURL.Scheme {
	case "unix", "npipe":
		needsStart := false
		_, statErr := os.Stat(hostURL.Host)
		switch {
		case statErr == nil:
			// Probe the socket explicitly before the version-check
			// path. A stale unix socket file (the previous server
			// exited without cleaning up) would otherwise make
			// restartIfStale spin on a non-responsive endpoint; here
			// we detect it with a short DialTimeout and remove the
			// orphaned file so the normal spawn path can run.
			if hostURL.Scheme == "unix" {
				conn, dialErr := net.DialTimeout( //nolint:noctx
					hostURL.Scheme, hostURL.Host, 200*time.Millisecond,
				)
				if dialErr == nil {
					conn.Close()
				} else if server.IsStaleSocketErr(dialErr) {
					slog.Warn("Stale socket detected, removing",
						"path", hostURL.Host, "error", dialErr)
					if err := os.Remove(hostURL.Host); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return fmt.Errorf("failed to remove stale server socket %q: %v", hostURL.Host, err)
					}
					needsStart = true
					break
				}
			}
			restarted, err := restartIfStale(cmd, hostURL)
			if err != nil {
				slog.Warn("Failed to check server version", "error", err)
			}
			needsStart = restarted || err != nil
		case errors.Is(statErr, fs.ErrNotExist):
			needsStart = true
		default:
			slog.Warn("Unexpected error stat'ing server socket, attempting cleanup",
				"path", hostURL.Host, "error", statErr)
			if err := os.Remove(hostURL.Host); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("failed to remove stale server socket %q: %v", hostURL.Host, err)
			}
			needsStart = true
		}

		if needsStart {
			if err := spawnAndWaitReady(cmd, hostURL); err != nil {
				return fmt.Errorf("failed to initialize the Gnosh server: %v", err)
			}
			return nil
		}

		if err := waitForServerReady(cmd.Context(), hostURL); err != nil {
			return fmt.Errorf("failed to initialize the Gnosh server: %v", err)
		}
	}

	return nil
}

// spawnAndWaitReady serializes the spawn-and-wait-for-readiness sequence
// across concurrent clients via an exclusive flock on
// $XDG_CACHE_HOME/crush/server-<safeHost>/start.lock.
//
// After acquiring the lock it re-probes readiness so that a client that
// blocked while another client was spawning can skip its own spawn and
// just use the now-running server. The lock is held only for the
// duration of "spawn + readiness probe" and released before the caller
// resumes its normal lifetime.
func spawnAndWaitReady(cmd *cobra.Command, hostURL *url.URL) error {
	chDir, err := perHostServerDir(hostURL)
	if err != nil {
		return err
	}
	release, err := lock.File(cmd.Context(), filepath.Join(chDir, "start.lock"))
	if err != nil {
		// If the lock itself is unavailable, fall back to the
		// unsynchronized path rather than blocking the user.
		slog.Warn("Failed to acquire spawn lock, proceeding without single-flight", "error", err)
		if err := startDetachedServer(cmd, hostURL); err != nil {
			return err
		}
		return waitForServerReady(cmd.Context(), hostURL)
	}
	defer release()

	// Another client may have just finished spawning while we were
	// waiting on the lock; if the server is already responsive, skip
	// the spawn entirely.
	probeCtx, cancel := context.WithTimeout(cmd.Context(), 200*time.Millisecond)
	probeErr := quickHealthProbe(probeCtx, hostURL)
	cancel()
	if probeErr == nil {
		return nil
	}

	if err := startDetachedServer(cmd, hostURL); err != nil {
		return err
	}
	return waitForServerReady(cmd.Context(), hostURL)
}

// quickHealthProbe issues a single readiness request with the caller's
// context and returns nil iff the server is responsive right now.
func quickHealthProbe(ctx context.Context, hostURL *url.URL) error {
	httpClient, reqURL, err := readinessHTTPClient(hostURL)
	if err != nil {
		return err
	}
	return probeHealth(ctx, httpClient, reqURL, hostURL)
}

// perHostServerDir returns (and creates) the cache directory used for
// per-host server state (logs, start.lock, etc.). The path is derived
// from the parsed host URL rather than the global flag so the same key
// is computed regardless of where the host came from.
func perHostServerDir(hostURL *url.URL) (string, error) {
	chDir := filepath.Join(config.GlobalCacheDir(), "server-"+safeHostName(hostURL))
	if err := os.MkdirAll(chDir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create server working directory: %v", err)
	}
	return chDir, nil
}

// safeHostName returns a filesystem-safe identifier for hostURL,
// suitable for use as a directory name. It mirrors the input shape of
// the --host flag so client and server compute the same key.
func safeHostName(hostURL *url.URL) string {
	return safeNameRegexp.ReplaceAllString(
		hostURL.Scheme+"://"+hostURL.Host+hostURL.Path, "_",
	)
}

// serverReadyTimeout returns the total budget for the readiness probe.
// Overridable via GNOSH_SERVER_READY_TIMEOUT (parsed as a Go duration).
func serverReadyTimeout() time.Duration {
	const def = 10 * time.Second
	v := env.Getenv("GNOSH_SERVER_READY_TIMEOUT")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// waitForServerReady polls GET /v1/health until the server responds with
// any 2xx status or the total timeout elapses. Each attempt uses a short
// per-attempt timeout so a hung listener doesn't burn the whole budget.
//
// The HTTP transport is built to mirror how *client.Client dials so the
// same unix socket / npipe / tcp setups all work uniformly here.
func waitForServerReady(ctx context.Context, hostURL *url.URL) error {
	httpClient, reqURL, err := readinessHTTPClient(hostURL)
	if err != nil {
		return err
	}

	const perAttempt = 100 * time.Millisecond
	deadline := time.Now().Add(serverReadyTimeout())

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("timed out waiting for server readiness")
		}

		attemptCtx, cancel := context.WithTimeout(ctx, perAttempt)
		err := probeHealth(attemptCtx, httpClient, reqURL, hostURL)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(perAttempt):
		}
	}
}

// readinessHTTPClient builds an *http.Client whose transport dials the
// server using the same scheme-aware logic as *client.Client (unix
// socket, named pipe, or tcp).
func readinessHTTPClient(hostURL *url.URL) (*http.Client, string, error) {
	c, err := client.NewClient("", hostURL.Scheme, hostURL.Host)
	if err != nil {
		return nil, "", err
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return c.Dial(ctx, network, addr)
	}
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		tr.DisableCompression = true
	}

	httpClient := &http.Client{Transport: tr}

	// For unix sockets / named pipes we still need a syntactically valid
	// HTTP URL; the actual address is resolved by the dialer.
	host := hostURL.Host
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		host = client.DummyHost
	}
	reqURL := (&url.URL{Scheme: "http", Host: host, Path: "/v1/health"}).String()
	return httpClient, reqURL, nil
}

// probeHealth issues a single GET to the readiness endpoint and treats
// any 2xx response as success.
func probeHealth(ctx context.Context, h *http.Client, reqURL string, hostURL *url.URL) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if hostURL.Scheme == "unix" || hostURL.Scheme == "npipe" {
		req.Host = client.DummyHost
	}
	rsp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	_, _ = io.Copy(io.Discard, rsp.Body)
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		return fmt.Errorf("server health check failed: %s", rsp.Status)
	}
	return nil
}

// restartIfStale checks whether the running server matches the current
// client version. When they differ it asks the server to stand down and,
// if it agrees, removes the stale socket so the caller can start a fresh
// server.
//
// The request is conditional and the server has the last word: it refuses
// while it is hosting anything, because BuildID derives from the
// executable's mtime, so any rebuild (including every `go run`) makes a
// second session look like an upgrade and would otherwise kill the first
// session's workspaces. Servers too old to understand the conditional
// command are left running for the same reason — the request they do
// understand is unconditional. They shut themselves down when they go
// idle, and the next client then finds no socket and spawns a current one.
//
// It returns restarted=true only when the server accepted the shutdown and
// the caller must spawn a replacement.
func restartIfStale(cmd *cobra.Command, hostURL *url.URL) (restarted bool, err error) {
	c, err := client.NewClient("", hostURL.Scheme, hostURL.Host)
	if err != nil {
		return false, err
	}
	vi, err := c.VersionInfo(cmd.Context())
	if err != nil {
		return false, err
	}
	if vi.Version == version.Version && vi.BuildID == version.BuildID {
		return false, nil
	}
	versionFields := []any{
		"server_version", vi.Version,
		"client_version", version.Version,
		"server_build_id", vi.BuildID,
		"client_build_id", version.BuildID,
	}
	// Every refusal — in use, too old to be asked, or unreachable — leads to
	// the same safe outcome: keep using the running server. The wrapped
	// error says which it was.
	if err := c.ShutdownServerIfIdle(cmd.Context()); err != nil {
		if !errors.Is(err, client.ErrUnsupported) {
			slog.Warn("Server version differs but it will not stand down; reusing it",
				append(versionFields, "error", err)...)
			return false, nil
		}
		// The server predates shutdown_if_idle. Fall back to the
		// unconditional command, but only after verifying it is idle.
		if !shutdownLegacyStaleServer(cmd.Context(), c, versionFields) {
			return false, nil
		}
	}
	slog.Info("Stale server accepted shutdown, restarting", versionFields...)
	if err := awaitSocketGone(cmd.Context(), hostURL); err != nil {
		return true, err
	}
	return true, nil
}

// shutdownLegacyStaleServer handles a stale server too old to understand the
// idle-checked shutdown. That server's only "shutdown" command is
// unconditional and would take live sessions down, so it is used only after
// listing workspaces confirms the server is idle. It reports whether the
// server accepted the shutdown; every other outcome (unreachable, busy, or a
// refused shutdown) is logged and reported as false so the caller reuses the
// running server.
func shutdownLegacyStaleServer(ctx context.Context, c *client.Client, versionFields []any) bool {
	workspaces, err := c.ListWorkspaces(ctx)
	if err != nil {
		slog.Warn("Server version differs but it will not stand down; reusing it",
			append(versionFields, "list_error", err)...)
		return false
	}
	if len(workspaces) > 0 {
		slog.Warn("Server version differs and has active workspaces; reusing it",
			append(versionFields, "workspaces", len(workspaces))...)
		return false
	}
	if err := c.ShutdownServer(ctx); err != nil {
		slog.Warn("Server version differs but it will not stand down; reusing it",
			append(versionFields, "error", err)...)
		return false
	}
	return true
}

// awaitSocketGone gives a server that has committed to exiting a moment to
// release its socket, then force-removes whatever is left: the old process
// has latched its decision and will not serve requests again.
func awaitSocketGone(ctx context.Context, hostURL *url.URL) error {
	for range 20 {
		if _, err := os.Stat(hostURL.Host); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = os.Remove(hostURL.Host)
	return nil
}

var safeNameRegexp = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func startDetachedServer(cmd *cobra.Command, hostURL *url.URL) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	chDir, err := perHostServerDir(hostURL)
	if err != nil {
		return err
	}

	cmdArgs := []string{"server"}
	if clientHost != server.DefaultHost() {
		cmdArgs = append(cmdArgs, "--host", clientHost)
	}

	// Use context.Background() so the parent's context cancellation does not
	// kill the spawned server. detachProcess (Setsid on !windows,
	// DETACHED_PROCESS on windows) is what truly detaches the child from
	// this process's lifetime.
	c := exec.CommandContext(context.Background(), exe, cmdArgs...)
	stdoutPath := filepath.Join(chDir, "stdout.log")
	stderrPath := filepath.Join(chDir, "stderr.log")
	detachProcess(c)

	stdout, err := os.Create(stdoutPath)
	if err != nil {
		return fmt.Errorf("failed to create stdout log file: %v", err)
	}
	defer stdout.Close()
	c.Stdout = stdout

	stderr, err := os.Create(stderrPath)
	if err != nil {
		return fmt.Errorf("failed to create stderr log file: %v", err)
	}
	defer stderr.Close()
	c.Stderr = stderr

	if err := c.Start(); err != nil {
		return fmt.Errorf("failed to start the Gnosh server: %v", err)
	}

	if err := c.Process.Release(); err != nil {
		return fmt.Errorf("failed to detach the Gnosh server process: %v", err)
	}

	return nil
}

// shouldEnableMetrics reports whether to send usage metrics at all. Upstream Crush sends them unless told not to; this fork sends
// none unless asked, and only to the configured metrics_url, never to Charm.
// DO_NOT_TRACK=1 always wins, then
// GNOSH_DISABLE_METRICS when it holds a value, then option metrics.
func shouldEnableMetrics(cfg *config.Config) bool {
	if v, _ := strconv.ParseBool(os.Getenv("DO_NOT_TRACK")); v {
		return false
	}
	if disable, ok := env.LookupBool("GNOSH_DISABLE_METRICS"); ok {
		return !disable
	}
	// An explicit option metrics in the config wins over the machine's
	// first-run answer; with no option set, the stored consent decides, and
	// it is off until the first-run question is answered.
	if cfg != nil && cfg.Options != nil && cfg.Options.DisableMetrics != nil {
		return !*cfg.Options.DisableMetrics
	}
	_, allow := config.MetricsConsent()
	return allow
}

// metricsConsentPrompt is the first-run analytics question, shown once per
// machine on an interactive run. It is cookie-style: off unless the person
// turns it on, and pressing Enter declines.
const metricsConsentPrompt = `
Gnosh can send anonymous usage analytics to help improve it: which models and
commands you use, token counts and timings, your OS and terminal, under a
hashed machine id. Never your prompts, your code, or the model's replies. It
is off unless you turn it on, goes only to gnosh's own collector, and you can
change it any time with ` + "`option metrics true|false`" + `.

Allow anonymous analytics? [y/N] (press Enter to decline) `

// maybeAskMetricsConsent asks the first-run analytics question once per
// machine, before the TUI starts, and records the answer so it is never asked
// again. It does nothing when the machine has already answered, when metrics
// are already settled by DO_NOT_TRACK/GNOSH_DISABLE_METRICS/option, or when
// input or output is not a terminal (a piped or non-interactive run never
// prompts and sends nothing).
func maybeAskMetricsConsent() {
	if config.MetricsConsentDecided() {
		return
	}
	if v, _ := strconv.ParseBool(os.Getenv("DO_NOT_TRACK")); v {
		return
	}
	if _, ok := env.LookupBool("GNOSH_DISABLE_METRICS"); ok {
		return
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return
	}
	fmt.Fprint(os.Stdout, metricsConsentPrompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	allow := answer == "y" || answer == "yes"
	if err := config.RecordMetricsConsent(allow); err != nil {
		slog.Warn("Could not save the analytics choice", "error", err)
	}
	if allow {
		fmt.Fprintln(os.Stdout, "Analytics on. Thank you. Turn it off any time with `option metrics false`.")
	} else {
		fmt.Fprintln(os.Stdout, "No analytics will be sent. You can allow it later with `option metrics true`.")
	}
}

// stdinWait is how long a piped stdin may stay silent before gnosh run
// goes on without it, as Claude Code's -p does. A pipe that is open but
// never written to, as an ssh session without -n gives, otherwise kept
// the run waiting forever with nothing on the screen to say why. It is a
// var so tests can shorten it.
var stdinWait = 3 * time.Second

// MaybePrependStdin puts what is piped or redirected into stdin before
// the prompt: `cat log | gnosh run "why does this fail?"`.
func MaybePrependStdin(prompt string) (string, error) {
	return prependInput(os.Stdin, os.Stderr, prompt)
}

func prependInput(in *os.File, warn io.Writer, prompt string) (string, error) {
	if term.IsTerminal(in.Fd()) {
		return prompt, nil
	}
	fi, err := in.Stat()
	if err != nil {
		return prompt, err
	}
	var bts []byte
	switch {
	case fi.Mode().IsRegular():
		// A redirected file ( < ) is all there already.
		bts, err = io.ReadAll(in)
	case fi.Mode()&os.ModeNamedPipe != 0:
		// A pipe ( | ) may never be written to.
		var arrived bool
		bts, arrived, err = readPipe(in, stdinWait)
		if !arrived {
			fmt.Fprintf(warn, "Warning: no stdin data received in %s, proceeding without it. "+
				"To give a slow command's output, write it to a file and redirect from that; "+
				"to skip stdin, redirect it from /dev/null.\n", stdinWait)
			return prompt, nil
		}
	default:
		return prompt, nil
	}
	if err != nil {
		return prompt, err
	}
	if len(bts) == 0 {
		return prompt, nil
	}
	return string(bts) + "\n\n" + prompt, nil
}

// readPipe reads in to the end, provided something arrives (data, or the
// end of it) within wait. Otherwise it reports arrived false and leaves
// the read to an abandoned goroutine, which stdin's closing or the
// process's exit ends.
func readPipe(in io.Reader, wait time.Duration) (data []byte, arrived bool, err error) {
	first := make(chan struct{})
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		chunk := make([]byte, 32*1024)
		signaled := false
		for {
			n, err := in.Read(chunk)
			buf.Write(chunk[:n])
			if !signaled && (n > 0 || err != nil) {
				close(first)
				signaled = true
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				done <- result{buf.Bytes(), err}
				return
			}
		}
	}()
	select {
	case <-first:
		r := <-done
		return r.data, true, r.err
	case <-time.After(wait):
		return nil, false, nil
	}
}

// resolveWorkspaceSessionID resolves a session ID that may be a full
// UUID, full hash, or hash prefix. Works against the Workspace
// interface so both local and client/server paths get hash prefix
// support.
// resumeByName is the session --resume names when it is no session's ID,
// as Claude Code's --resume takes a name: the one session whose title it
// is, ignoring case. When no session or several have it, search is the
// name instead, for the list of sessions to open on.
func resumeByName(ctx context.Context, ws workspace.Workspace, name string) (sess session.Session, search string, err error) {
	sessions, err := ws.ListSessions(ctx)
	if err != nil {
		return session.Session{}, "", err
	}
	if named := session.Named(sessions, name); len(named) == 1 {
		return named[0], "", nil
	}
	return session.Session{}, strings.TrimSpace(name), nil
}

func resolveWorkspaceSessionID(ctx context.Context, ws workspace.Workspace, id string) (session.Session, error) {
	if sess, err := ws.GetSession(ctx, id); err == nil {
		return sess, nil
	}

	sessions, err := ws.ListSessions(ctx)
	if err != nil {
		return session.Session{}, err
	}

	var matches []session.Session
	for _, s := range sessions {
		hash := session.HashID(s.ID)
		if hash == id || strings.HasPrefix(hash, id) {
			matches = append(matches, s)
		}
	}

	switch len(matches) {
	case 0:
		return session.Session{}, fmt.Errorf("session not found: %s", id)
	case 1:
		return matches[0], nil
	default:
		return session.Session{}, fmt.Errorf("session ID %q is ambiguous (%d matches)", id, len(matches))
	}
}

// ResolveCwd changes to the directory --cwd names, if any, and returns the
// working directory. A relative --cwd is made absolute first and kept so:
// once the directory has changed it would name another one, and the TUI
// resolves it twice, to ask the trust question and to set up the workspace.
func ResolveCwd(cmd *cobra.Command) (string, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil && abs != cwd {
			cwd = abs
			_ = cmd.Flags().Set("cwd", abs)
		}
		err := os.Chdir(cwd)
		if err != nil {
			return "", fmt.Errorf("failed to change directory: %v", err)
		}
		return cwd, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %v", err)
	}
	return cwd, nil
}

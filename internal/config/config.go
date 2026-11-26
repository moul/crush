package config

import (
	"encoding/json"
	"time"

	"github.com/gnosh-ai/gnosh/internal/hooks"
	"github.com/gnosh-ai/gnosh/internal/shellquote"
)

// Config is everything gnosh's configuration says, after every file, the
// state file, the environment and the command line have been read, merged
// and checked. It is plain data: other packages are handed the parts they
// need. Printing a Config, or any part of one, never shows a secret.
type Config struct {
	// Paths are the directories and files in use.
	Paths Paths
	// Files are the config files that were found, in the order they were
	// read, each with whether it is in force or held.
	Files []File

	// Trusted reports whether the project's own settings are in force.
	Trusted bool
	// Held is what the project's own files would do if the folder were
	// trusted, and do not do because it is not. It is empty in a trusted
	// folder, and in one that has no settings waiting for trust. The trust
	// question is asked only when Held is not empty, and lists it.
	Held []Held

	// Providers are the model providers, in the order they were first
	// declared. Disabled ones and ones that cannot be used are included,
	// each saying why; Usable picks the rest.
	Providers []Provider
	// Models are the models registered by hand, under "models".
	Models []Model
	// Large is the session's model, the one that does the work ("model"),
	// and Small the one that does the short side jobs ("smallModel"). A
	// slot whose Model is not set has no choice made for it: Small then
	// follows Large, and Large is asked for.
	Large, Small Slot
	// Fallback are the models a turn goes on with, in order, when the
	// session's model cannot answer ("fallbackModel", --fallback-model; at
	// most MaxFallbacks). A model named without its provider by the flag
	// has an empty Provider, which the app settles as it does for -m.
	Fallback Value[[]ModelRef]

	// Options are the settings of the "options" object.
	Options Options
	// Limits are the tool and request limits that Claude Code's
	// environment variables set.
	Limits Limits
	// Switches are the on/off settings of gnosh's own variables and flags.
	Switches Switches

	// Hosts are the other machines the model may run commands on.
	Hosts []Host
	// Hooks are the hooks in force, in the order they were configured.
	// The list is empty in safe mode and when disableAllHooks is set.
	Hooks []Hook
	// Agent names the kind of agent the session runs as (the setting
	// "agent"; --agent outranks it), or is empty.
	Agent Value[string]
	// AllHooks are the hooks the configuration holds, in force or not.
	AllHooks []Hook
	// HooksOff says why Hooks is empty although AllHooks is not: "safe
	// mode", or the setting that switched them off. Empty otherwise.
	HooksOff string
	// HTTPHookURLs is Claude Code's allowedHttpHookUrls, the lists of all
	// the files that give it, merged: an http hook runs only when its URL
	// fits one of the patterns. Nil when no file gives it (any URL), empty
	// but not nil when the files give only empty lists (no http hook
	// runs). A project's waits for the folder's trust, as its
	// disableAllHooks does: it would switch off the person's own hooks.
	HTTPHookURLs []ListItem
	// Sandbox is the sandbox settings: whether the shell's commands run in
	// the operating system's sandbox, and what they may do there.
	Sandbox Sandbox
	// HTTPHookEnvVars is Claude Code's httpHookAllowedEnvVars, merged the
	// same way: an http hook's header takes a variable only when both the
	// hook's own allowedEnvVars and this list name it. Nil when no file
	// gives it.
	HTTPHookEnvVars []ListItem
	// StatusLine is the command whose output is the status line under the
	// prompt (Claude Code's statusLine), or nil. It is nil in safe mode
	// and when disableAllHooks is set, as hooks are; StatusLineOff then
	// says why.
	StatusLine    *StatusLine
	StatusLineOff string
	// FileSuggestion is the command that offers the paths an @ mention
	// completes to, in place of gnosh's own search (Claude Code's
	// fileSuggestion), or nil. It is nil in safe mode and when
	// disableAllHooks is set, as the status line is; FileSuggestionOff
	// then says why.
	FileSuggestion    *FileSuggestion
	FileSuggestionOff string
	// LSPServers are the language servers of the lspServers key, by name
	// (lsp.go): none in safe mode.
	LSPServers []LSPServer
	// Attribution is what the model adds to the git commits and pull
	// requests it makes (Claude Code's attribution). gnosh adds none of
	// its own: both are empty unless a file sets them.
	Attribution Attribution
	// Permissions are the permission rules, the starting mode and the
	// extra directories.
	Permissions Permissions
	// Env are the variables the env block sets for the commands and hooks
	// gnosh starts, sorted by name.
	Env []EnvVar
	// SkillOverrides is Claude Code's skillOverrides: how far each skill is
	// seen, by its name: "on" (as its file says), "name-only" (the model is
	// told its name alone), "user-invocable-only" (the person's alone) or
	// "off". A later file wins for each name.
	SkillOverrides map[string]Value[string]
	// Context says which instruction files gnosh reads, and in what order.
	Context ContextFiles

	// BadEnv are the environment variables whose values could not be
	// used, for doctor.
	BadEnv []Problem

	secrets *secrets
}

// File is a config file Load found.
type File struct {
	// Path is where the file is. "--settings" stands for settings given on
	// the command line as JSON.
	Path string
	// Role is "settings" for a file a person or gnosh writes settings in
	// (gnosh.json, settings.local.json, what --settings names), and
	// "state" for the state file.
	Role string
	// Scope is "user", "project", "local" or "command line".
	Scope string
	// Held reports that the file is the project's and the folder is not
	// trusted, so only what restricts the agent was taken from it.
	Held bool
}

// Held is one thing a project's file says that waits for the folder to be
// trusted.
type Held struct {
	// Kind sorts what it would do; the Kind constants list the values.
	Kind string
	// Text is the setting in a few words taken from the file. It comes
	// from a stranger's file: show it escaped.
	Text string
	// Origin is the line that says it.
	Origin Origin
}

// The kinds of thing a project's settings can hold. The trust package has
// the same words: its Restricts says which kinds still apply in a folder
// that is not trusted, and its question says what each could do.
const (
	KindProvider     = "provider"
	KindModel        = "model"
	KindHost         = "host"
	KindHook         = "hook"
	KindHookOff      = "hook-off" // switching all hooks off
	KindAllowRule    = "allow-rule"
	KindAskRule      = "ask-rule"
	KindDenyRule     = "deny-rule"
	KindMode         = "mode"
	KindAddDir       = "add-dir"
	KindEnv          = "env"
	KindOption       = "option"
	KindExtraPath    = "extra-path"
	KindAddress      = "address"
	KindHiddenSkill  = "hidden-skill"
	KindRestriction  = "restriction" // a setting that only takes a power away, such as disableBypassPermissionsMode
	KindRemoval      = "removal"     // taking away a provider, a model or a host a weaker file declared
	KindStatusLine   = "status-line"
	KindFileSuggest  = "file-suggestion"
	KindLSPServer    = "lsp"           // a language server, a program gnosh runs (the trust package's word)
	KindSandboxWider = "sandbox-wider" // a sandbox setting that lets its commands do more
)

// StatusLine is a command whose output takes the place of the status line
// under the prompt: Claude Code's statusLine setting, of type "command".
type StatusLine struct {
	// Command is the command line, run by sh with a JSON object about the
	// session on its standard input.
	Command string
	// Padding is how many columns its lines are set in from the left.
	Padding int
	// Refresh is how often it is run again besides the moments it runs at
	// (Claude Code's refreshInterval); zero means at those moments alone.
	Refresh time.Duration
	// Origin is the line that sets it.
	Origin Origin
}

// Attribution is Claude Code's attribution setting: the lines the model
// ends a commit's message with, and a pull request's description.
type Attribution struct {
	Commit, PR string
	// Origin is the line that sets it.
	Origin Origin
}

// FileSuggestion is a command that offers the paths an @ mention completes
// to: Claude Code's fileSuggestion setting, of type "command".
type FileSuggestion struct {
	// Command is the command line, run by sh with {"query": what was typed
	// after the @} on its standard input; it prints a path a line.
	Command string
	// Origin is the line that sets it.
	Origin Origin
}

// Provider is one model provider.
type Provider struct {
	// ID is the name it is declared under.
	ID string
	// Name is the name shown to the user; ID when none was given.
	Name string
	// Type is the type as configured: llamacpp, openai-compatible, and so
	// on.
	Type string
	// Wire is the request format the type speaks: "llamacpp", "openai" or
	// "anthropic".
	Wire string
	// Endpoints are the addresses, in the order given. The first that
	// accepts a connection is the one to use (see SelectEndpoint).
	Endpoints []Endpoint
	// APIKey is the key. It may be empty for a local server.
	APIKey Secret
	// KeyFrom says where the key came from when it was not written out:
	// the variable's name with a dollar sign, such as "$OPENAI_API_KEY".
	KeyFrom string
	// Headers are the extra request headers, in the order given. One whose
	// value is empty is not sent.
	Headers []Header
	// ExtraBody is merged into every request body. Nil when none was given.
	ExtraBody map[string]any
	// ListModels says whether to ask the server which models it has.
	// When nothing set it, it is true exactly when no model is registered
	// for the provider.
	ListModels Value[bool]
	// Disabled keeps the entry without using it.
	Disabled bool
	// LeftOut says why the provider cannot be used, in a plain sentence.
	// Empty means nothing in the configuration stands in its way. A
	// provider that is left out stops nothing: gnosh starts, and every
	// place that lists providers shows the reason.
	LeftOut string
	// Origin is the last place that set the provider.
	Origin Origin
}

// Usable reports whether the provider can be used as far as the
// configuration can tell.
func (p Provider) Usable() bool { return !p.Disabled && p.LeftOut == "" }

// Endpoint is one address of a provider.
type Endpoint struct {
	// Given is the address as it was configured, with its variables filled
	// in.
	Given string
	// URL is the root of the API: what "/chat/completions", "/messages" or
	// "/models" is added to. For a server on the user's own machine it
	// ends in /v1 whether or not the configured address did.
	//
	// Given and URL are for the client that connects. An address can
	// carry a password or a token in its query string, so neither is for
	// showing or logging: Display is.
	URL string
	// Display is URL without any user name, password or query string.
	Display string
	// Local reports that the address is on the user's own machine or
	// network: localhost, a loopback, unspecified, private or link-local
	// address, the 100.64.0.0/10 range overlay networks use, a host name
	// of one label, or a name under .localhost, .local, .lan, .internal
	// or .home.arpa.
	Local bool
	// Timeouts are the time limits of a request to this address.
	Timeouts Timeouts
}

// Timeouts are the time limits of one model request. A zero limit means
// there is none.
type Timeouts struct {
	// Connect is how long reaching the server may take.
	Connect time.Duration
	// FirstToken is how long the server may take, from the moment the
	// request is sent, to send the first piece of its answer.
	FirstToken time.Duration
	// Idle is how long the answer may go quiet between pieces.
	Idle time.Duration
	// Total is how long the whole request may take. gnosh sets none.
	Total time.Duration
	// FirstTokenFrom and IdleFrom say what set those two limits.
	FirstTokenFrom, IdleFrom Origin
}

// Header is one extra request header.
type Header struct {
	Name  string
	Value Secret
}

// Model is a model registered by hand.
type Model struct {
	Provider string
	ID       string
	// Name is the name shown to the user; ID when none was given.
	Name string
	// ContextWindow is the model's window in tokens. Not set means: ask
	// the server, and take 32,768 if it cannot say.
	ContextWindow Value[int]
	// MaxOutputTokens is the reply limit sent with a request. Not set
	// means none is sent to a local server.
	MaxOutputTokens Value[int]
	// ThinkingLevels are the model's thinking levels, the least thinking
	// first. Not set means the model's ID decides; set and empty means the
	// model has no thinking switch.
	ThinkingLevels Value[[]string]
	// ThinkingBudget is the most tokens the model may think before a
	// reply, when it thinks; 0 means no cap. Not set means MAX_THINKING_TOKENS,
	// or gnosh's own cap for a llama.cpp server, or none.
	ThinkingBudget Value[int]
	// Origin is the last place that set the model.
	Origin Origin
}

// Ref is the model's name in the form the user types: provider/id.
func (m Model) Ref() string { return m.Provider + "/" + m.ID }

// ModelRef names a model: a provider's ID and the provider's name for the
// model.
type ModelRef struct {
	// Provider is the provider's ID. It is empty when the command line
	// named a model without a provider and more than one provider could
	// have it: whoever lists the providers' models settles it.
	Provider string
	ID       string
}

// String is "provider/id", or the bare ID when no provider is known.
func (r ModelRef) String() string {
	if r.Provider == "" {
		return r.ID
	}
	return r.Provider + "/" + r.ID
}

// Slot is one of the two model choices, the session's model or the small
// one, with how the model is asked.
type Slot struct {
	// Model is the model chosen. Not set means no choice has been made.
	Model Value[ModelRef]
	// Thinking is the thinking level: "on", "off", or one of the model's
	// own levels such as "low". Not set means on, for a model that can
	// think.
	Thinking Value[string]
	// Temperature is left to the provider when not set.
	Temperature Value[float64]
}

// Host is another machine the model may run commands on.
type Host struct {
	Name string
	// SSH is the destination handed to ssh: user@address, or a Host alias
	// from the user's ssh configuration.
	SSH         string
	Description string
	Origin      Origin
}

// Hook is one hook: a command gnosh runs at a named moment.
type Hook struct {
	// Event is the moment, one of HookEventNames.
	Event string
	// Matcher says which tools or sources the hook runs for. Empty means
	// all of them.
	Matcher string
	// Command is the command line, handed to the shell; or, when Args is
	// not nil, the program to run (Claude Code's exec form).
	Command string
	// Args are the program's arguments in the exec form: no shell reads
	// them. Nil is the shell form.
	Args []string
	// Async and AsyncRewake run the hook in the background: nothing waits
	// for it and it decides nothing; with AsyncRewake an exit with 2 wakes
	// the model.
	Async, AsyncRewake bool
	// StatusMessage is what the person is shown while the hook runs.
	StatusMessage string
	// URL makes the hook an http hook: the event is POSTed to it. Headers
	// go with the request, with $NAME filled in for the variables
	// AllowedEnvVars names.
	URL            string
	Headers        map[string]string
	AllowedEnvVars []string
	// Server, Tool and Input make the hook a call of a tool of a
	// connected MCP server (mcp_tool), with Input as its arguments.
	Server, Tool string
	Input        json.RawMessage
	// Prompt makes the hook a prompt hook: a rule or a condition the model
	// judges ($ARGUMENTS is the event); Model is the model it names, and
	// ContinueOnBlock its continueOnBlock (hooks/prompthook.go).
	Prompt          string
	Model           string
	ContinueOnBlock bool
	// Agent makes the hook an agent hook: Prompt is a task for a sub-agent
	// that looks with its tools before it answers (hooks/prompthook.go).
	Agent bool
	// Timeout is how long the hook may run. Zero means the event's default.
	Timeout time.Duration
	// If is the hook's "if" filter: one permission rule. On a tool event
	// the hook runs only for a call a deny rule of that text would refuse
	// or could apply to; on any other event it never runs.
	If string
	// Scope is "user", "project", "local" or "command line".
	Scope string
	// BaseDir is the directory of the file that configured the hook.
	BaseDir string
	Origin  Origin
}

// CommandLine is the hook's command as a person reads it: the shell
// command line, or in the exec form the program and its arguments, each
// quoted where a shell would need it.
func (h Hook) CommandLine() string {
	if h.URL != "" {
		return "POST " + hooks.ShownURL(h.URL)
	}
	if h.Server != "" {
		return "mcp " + h.Server + "/" + h.Tool
	}
	if h.Prompt != "" && h.Agent {
		return "agent: " + h.Prompt
	}
	if h.Prompt != "" {
		return "prompt: " + h.Prompt
	}
	if h.Args == nil {
		return h.Command
	}
	return shellquote.Words(append([]string{h.Command}, h.Args...))
}

// Permissions are the permission settings.
type Permissions struct {
	// Mode is the mode the session starts in: default, plan, acceptEdits,
	// dontAsk or bypassPermissions.
	Mode Value[string]
	// BypassAvailable reports that the session may switch to
	// bypassPermissions later: it was started with -y, in that mode, or
	// with that mode as the user's own default.
	BypassAvailable bool
	// BypassDisabled reports that a settings file forbids
	// bypassPermissions.
	BypassDisabled Value[bool]
	// DenyInsteadOfPrompting refuses what would ask, and goes on.
	DenyInsteadOfPrompting Value[bool]
	// BlockReads and BlockWrites are Claude Code's
	// blockReadsOutsideWorkingDirectories and
	// blockWritesOutsideWorkingDirectories: the file tools refuse to read,
	// or to change, a path outside the working directories, in every
	// mode. A true in any file holds; false is the same as not set.
	BlockReads, BlockWrites Value[bool]
	// ProjectDirs are those of Dirs that a file the project supplied
	// added (its gnosh.json, or a settings.local.json gnosh did not
	// write). The blocks do not count them as working directories, as
	// Claude Code's do not: a repository can turn a block on for itself,
	// and cannot widen the person's.
	ProjectDirs []string
	// Layers are the rules, one layer per file or flag that gave any,
	// weakest first. Deny rules of every layer count, then ask rules, then
	// allow rules, so the order matters only for display.
	Layers []RuleLayer
	// Dirs are the extra directories the file tools may read.
	Dirs []ListItem
	// Tools is the --tools flag: the only built-in tools the model is
	// given. Not set means all of them; set and empty means none.
	Tools Value[[]string]
}

// RuleLayer is the permission rules of one source.
type RuleLayer struct {
	// Scope is "user", "project", "local" or "command line".
	Scope string
	// File is the file the rules are in, or "" for a flag.
	File string
	// BaseDir is what a rule path that starts with a single slash is
	// under: the config directory for the user's file, the working
	// directory for the project's files and for flags, and the file's own
	// directory for a file given with --settings.
	BaseDir          string
	Allow, Ask, Deny []Rule
}

// Rule is one permission rule, as written: a tool name, or a tool name
// with a pattern in parentheses. Load checks that the tool is one gnosh
// has. What the pattern means is the permission package's to say.
type Rule struct {
	Text   string
	Origin Origin
}

// EnvVar is one variable of the env block.
type EnvVar struct {
	Name string
	// Value is the value. An empty one takes the variable away from the
	// commands gnosh starts.
	Value  Secret
	Origin Origin
}

// Switches are the on/off settings that gnosh's own environment variables
// and flags control.
type Switches struct {
	// SafeMode starts gnosh with the config's customizations off: hooks,
	// context files, skills found on disk and custom commands.
	SafeMode Value[bool]
	// DebugPage serves a debugging page on loopback.
	DebugPage Value[bool]
}

// ContextFiles says which instruction files the model is given, as data
// for the package that reads them. Load reads none of them.
type ContextFiles struct {
	// Off reports that no context file is read at all (safe mode).
	Off bool
	// ProjectNames are the file names looked for in the working directory
	// and in every directory above it, in the order they are read inside
	// one directory. Directories are read outermost first, the closest
	// last.
	ProjectNames []string
	// LocalNames are the names of the person's private file about a
	// project, read after ProjectNames in each directory.
	LocalNames []string
	// DotDirs are the folders of a directory that hold its files too
	// (.claude/CLAUDE.md beside CLAUDE.md) and its rules, in a "rules"
	// folder inside them (.claude/rules/*.md): gnosh's own and Claude
	// Code's.
	DotDirs []string
	// Cwd and ProjectRoot say where the walk ends and where the project
	// begins. A directory above ProjectRoot gives only files owned by the
	// user or by root.
	Cwd, ProjectRoot string
	// Global are the user's global files and folders, in the order they
	// are read: GNOSH.md in the config directory and the rules folder
	// beside it, the AGENTS.md shared with other programs, then what the
	// userInstructionFiles option names. A folder gives every Markdown
	// file in it. A file that does not exist is passed over.
	Global []ListItem
	// Extra are more instruction files of the project, from the
	// projectInstructionFiles option.
	Extra []ListItem
	// Exclude are the files never read, by any route, as patterns matched
	// against their absolute paths (the claudeMdExcludes option).
	Exclude []ListItem
	// ImportDepth is how many levels deep an @path line is followed.
	ImportDepth int
	// MaxFileBytes is the size past which a file is passed over, with a
	// word to the user.
	MaxFileBytes int64
	// NestedMaxChars is how much of a subdirectory's file, given when the
	// agent first touches that subdirectory, is shown before it is cut.
	NestedMaxChars int
}

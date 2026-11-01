package config

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/tooldefs"
	"github.com/invopop/jsonschema"
)

// The program has been renamed twice, from Crush to Klash and then to Gnosh.
// Its directories and config files take the newest name, but where only one
// from before a rename exists, that one is used (see fsext.FirstExisting), so
// existing setups and their sessions keep working without being moved.
const (
	appName              = "gnosh"
	defaultDataDirectory = "." + appName
	defaultInitializeAs  = "AGENTS.md"
)

// appNames are the program's name and its names from before the renames,
// newest first: the order every lookup prefers them in.
var appNames = []string{appName, "klash", "crush"}

var defaultContextPaths = []string{
	".github/copilot-instructions.md",
	".cursorrules",
	".cursor/rules/",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	"gemini.md",
	"gnosh.md",
	"gnosh.local.md",
	"Gnosh.md",
	"Gnosh.local.md",
	"GNOSH.md",
	"GNOSH.local.md",
	"klash.md",
	"klash.local.md",
	"Klash.md",
	"Klash.local.md",
	"KLASH.md",
	"KLASH.local.md",
	"crush.md",
	"crush.local.md",
	"Crush.md",
	"Crush.local.md",
	"CRUSH.md",
	"CRUSH.local.md",
	"AGENTS.md",
	"agents.md",
	"Agents.md",
}

type SelectedModelType string

// String returns the string representation of the [SelectedModelType].
func (s SelectedModelType) String() string {
	return string(s)
}

const (
	SelectedModelTypeLarge SelectedModelType = "large"
	SelectedModelTypeSmall SelectedModelType = "small"
)

// knownModelTypes lists every model type Crush selects, in the order
// they are reported to the user.
var knownModelTypes = []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall}

// KnownModelTypes returns the model types a `models` entry may be keyed by.
func KnownModelTypes() []SelectedModelType {
	return slices.Clone(knownModelTypes)
}

// IsKnownModelType reports whether t is a model type Crush actually uses.
func IsKnownModelType(t SelectedModelType) bool {
	return slices.Contains(knownModelTypes, t)
}

const (
	AgentCoder string = "coder"
	AgentPlan  string = "plan"
	AgentTask  string = "task"
)

// Permission modes, by Claude Code's names: the values of
// --permission-mode, and the permission_mode hooks are told.
const (
	PermissionModeDefault     = "default"
	PermissionModeAcceptEdits = "acceptEdits"
	PermissionModeBypass      = "bypassPermissions"
	PermissionModePlan        = "plan"
	PermissionModeDontAsk     = "dontAsk"
)

type SelectedModel struct {
	// The model id as used by the provider API.
	// Required.
	Model string `json:"model" jsonschema:"required,description=The model ID as used by the provider API,example=gpt-4o"`
	// The model provider, same as the key/id used in the providers config.
	// Required.
	Provider string `json:"provider" jsonschema:"required,description=The model provider ID that matches a key in the providers config,example=openai"`

	// Only used by models that use the openai provider and need this set.
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"description=Reasoning effort level for OpenAI models that support it,enum=low,enum=medium,enum=high"`

	// Used by anthropic models that can reason to indicate if the model should think.
	Think bool `json:"think,omitempty" jsonschema:"description=Enable thinking mode for Anthropic models that support reasoning"`

	// Overrides the default model configuration.
	MaxTokens        int64    `json:"max_tokens,omitempty" jsonschema:"description=Maximum number of tokens for model responses,maximum=200000,example=4096"`
	Temperature      *float64 `json:"temperature,omitempty" jsonschema:"description=Sampling temperature,minimum=0,maximum=1,example=0.7"`
	TopP             *float64 `json:"top_p,omitempty" jsonschema:"description=Top-p (nucleus) sampling parameter,minimum=0,maximum=1,example=0.9"`
	TopK             *int64   `json:"top_k,omitempty" jsonschema:"description=Top-k sampling parameter"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty" jsonschema:"description=Frequency penalty to reduce repetition"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty" jsonschema:"description=Presence penalty to increase topic diversity"`

	// Override provider specific options.
	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for the model"`
}

type ProviderConfig struct {
	// The provider's id.
	ID string `json:"id,omitempty" jsonschema:"description=Unique identifier for the provider,example=openai"`
	// The provider's name, used for display purposes.
	Name string `json:"name,omitempty" jsonschema:"description=Human-readable name for the provider,example=OpenAI"`
	// The provider's API endpoint.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Base URL for the provider's API,format=uri,example=https://api.openai.com/v1"`
	// The provider type, e.g. "openai", "anthropic", etc. if empty it defaults to openai.
	Type catwalk.Type `json:"type,omitempty" jsonschema:"description=Provider type that determines the API format,default=openai"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
	// The original API key template before resolution (for re-resolution on auth errors).
	APIKeyTemplate string `json:"-"`
	// OAuthToken for providers that use OAuth2 authentication.
	OAuthToken *oauth.Token `json:"oauth,omitempty" jsonschema:"description=OAuth2 token for authentication with the provider"`
	// Marks the provider as disabled.
	Disable bool `json:"disable,omitempty" jsonschema:"description=Whether this provider is disabled,default=false"`

	// Custom system prompt prefix.
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty" jsonschema:"description=Custom prefix to add to system prompts for this provider"`

	// Extra headers to send with each request to the provider. Values
	// run through shell expansion at config-load time, so $VAR and
	// $(cmd) work the same way they do in MCP headers. A header whose
	// value resolves to the empty string (unset bare $VAR under
	// lenient nounset, $(echo), or literal "") is omitted from the
	// outgoing request rather than sent as "Header:".
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" jsonschema:"description=Additional HTTP headers to send with requests"`
	// ExtraBody is merged verbatim into OpenAI-compatible request
	// bodies. String values are NOT shell-expanded: this is a plain
	// JSON passthrough so that arbitrary provider-extension fields
	// (numbers, nested objects, booleans) round-trip without a
	// recursive walker guessing at intent. If you need an env-var-
	// driven value at request time, put it in extra_headers, or in
	// the provider's top-level api_key / base_url, all of which do
	// expand.
	ExtraBody map[string]any `json:"extra_body,omitempty" jsonschema:"description=Additional fields to include in request bodies\\, only works with openai-compatible providers"`

	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for this provider"`

	// Used to pass extra parameters to the provider.
	ExtraParams map[string]string `json:"-"`

	// AWSAuthRefresh is a shell command run when Bedrock returns a
	// credential error. Output is discarded to avoid corrupting the TUI.
	AWSAuthRefresh string `json:"aws_auth_refresh,omitempty" jsonschema:"description=Shell command to run when AWS credentials expire (Bedrock only)."`

	// Skip cost accumulation for this provider when using subscription or flat rate billing.
	FlatRate bool `json:"flat_rate,omitempty" jsonschema:"description=Flat-rate mode for this provider"`

	// AutoDiscoverModels controls model discovery via /v1/models endpoint.
	// When Models is empty and this is nil or true, Crush auto-discovers
	// models. When true and Models is non-empty, discovered models are
	// merged in (user-specified models take precedence). When false,
	// only explicitly listed models are used.
	AutoDiscoverModels *bool `json:"discover_models,omitempty" jsonschema:"description=Auto-discover models from /v1/models endpoint. When true with existing models they are merged (yours win),default=true"`

	// The provider models
	Models []catwalk.Model `json:"models,omitempty" jsonschema:"description=List of models available from this provider"`

	// ChatGPTModels lists the models the ChatGPT (Codex) backend grants
	// when the provider is authenticated with a ChatGPT account. It is
	// the provider's whole catalog in that case: the API-key models in
	// Models are not served by the subscription.
	ChatGPTModels []catwalk.Model `json:"chatgpt_models,omitempty" jsonschema:"-"`

	// GrokModels lists the models the Grok plan grants when the provider
	// is authenticated with a Grok account. Like ChatGPTModels, it is
	// the provider's whole catalog in that case.
	GrokModels []catwalk.Model `json:"grok_models,omitempty" jsonschema:"-"`
}

// ToProvider converts the [ProviderConfig] to a [catwalk.Provider].
func (c *ProviderConfig) ToProvider() catwalk.Provider {
	// Convert config provider to provider.Provider format
	provider := catwalk.Provider{
		Name:   c.Name,
		ID:     catwalk.InferenceProvider(c.ID),
		Models: make([]catwalk.Model, len(c.Models)),
	}

	// Convert models
	for i, model := range c.Models {
		provider.Models[i] = catwalk.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
		}
	}

	return provider
}

func (c *ProviderConfig) SetupGitHubCopilot() {
	maps.Copy(c.ExtraHeaders, copilot.Headers())
}

// HasAPIKey reports whether the provider's api_key resolves to a usable
// credential. The stored value is often an unresolved template like
// $OPENAI_API_KEY, which is not a credential until the variable exists.
func (c *ProviderConfig) HasAPIKey(resolver VariableResolver) bool {
	if c.APIKey == "" {
		return false
	}
	v, err := resolver.ResolveValue(c.APIKey)
	return err == nil && v != ""
}

type MCPType string

const (
	MCPStdio MCPType = "stdio"
	MCPSSE   MCPType = "sse"
	MCPHttp  MCPType = "http"
)

// knownMCPTypes lists every MCP connection type Crush can open, in the
// order they are reported to the user.
var knownMCPTypes = []MCPType{MCPStdio, MCPSSE, MCPHttp}

// KnownMCPTypes returns the connection types an MCP server may declare.
func KnownMCPTypes() []MCPType {
	return slices.Clone(knownMCPTypes)
}

// IsKnownMCPType reports whether t is a connection type Crush can open.
func IsKnownMCPType(t MCPType) bool {
	return slices.Contains(knownMCPTypes, t)
}

type MCPConfig struct {
	Command       string            `json:"command,omitempty" jsonschema:"description=Command to execute for stdio MCP servers,example=npx"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set for the MCP server"`
	Args          []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the MCP server command"`
	Type          MCPType           `json:"type,omitempty" jsonschema:"description=Type of MCP connection,enum=stdio,enum=sse,enum=http,default=stdio"`
	URL           string            `json:"url,omitempty" jsonschema:"description=URL for HTTP or SSE MCP servers,format=uri,example=http://localhost:3000/mcp"`
	Disabled      bool              `json:"disabled,omitempty" jsonschema:"description=Whether this MCP server is disabled,default=false"`
	DisabledTools []string          `json:"disabled_tools,omitempty" jsonschema:"description=List of tools from this MCP server to disable,example=get-library-doc"`
	EnabledTools  []string          `json:"enabled_tools,omitempty" jsonschema:"description=Allow list of tools from this MCP server,example=get-library-doc"`
	Timeout       int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for MCP server connections,default=10,example=30,example=60,example=120"`

	// Sessionless marks a server that does not maintain an MCP session (it
	// never issues a Mcp-Session-Id). When true, Crush omits the
	// tools/prompts/resources list-changed handlers: the go-sdk opens a
	// SEP-2575 "subscriptions/listen" stream whenever any of those handlers
	// is set, and sessionless streamable-HTTP servers (e.g. GitHub MCP)
	// answer that POST with 404 ("session not found"), which the SDK treats
	// as fatal. The cost is no live list-changed notifications from this
	// server.
	//
	// When nil, Crush auto-detects a set of known sessionless servers (see
	// IsSessionless); set it explicitly to override that detection.
	Sessionless *bool `json:"sessionless,omitempty" jsonschema:"description=Mark a sessionless MCP server (no Mcp-Session-Id) so Gnosh skips the subscriptions/listen stream it would otherwise reject. Leave unset to auto-detect known sessionless servers (e.g. GitHub MCP),default=false"`

	// Headers are HTTP headers for HTTP/SSE MCP servers. Values run
	// through shell expansion at MCP startup, so $VAR and $(cmd)
	// work. A header whose value resolves to the empty string (unset
	// bare $VAR under lenient nounset, $(echo), or literal "") is
	// omitted from the outgoing request rather than sent as
	// "Header:".
	Headers map[string]string `json:"headers,omitempty" jsonschema:"description=HTTP headers for HTTP/SSE MCP servers"`

	// OAuth enables the MCP OAuth 2.1 authorization flow for HTTP
	// transport servers. When true, the client uses dynamic client
	// registration and opens a browser for the user to authorize.
	// Tokens are persisted automatically. Only supported for type=http.
	OAuth bool `json:"oauth,omitempty" jsonschema:"description=Enable OAuth 2.1 authorization flow for this MCP server (HTTP transport only),default=false"`

	// OAuthClientID is an optional pre-registered OAuth client ID. Set
	// it for servers that do not support dynamic client registration
	// (e.g. GitHub, Slack) and instead issue client credentials when you
	// register an OAuth app. Values run through shell expansion, so
	// $VAR and $(cmd) work.
	OAuthClientID string `json:"oauth_client_id,omitempty" jsonschema:"description=Pre-registered OAuth client ID for servers without dynamic client registration"`

	// OAuthClientSecret is the optional secret paired with
	// OAuthClientID for confidential clients. Values run through shell
	// expansion, so $VAR and $(cmd) work.
	OAuthClientSecret string `json:"oauth_client_secret,omitempty" jsonschema:"description=Pre-registered OAuth client secret paired with oauth_client_id"`

	// OAuthCallbackPort pins the localhost port used for the OAuth
	// redirect listener. Set this when the OAuth provider requires an
	// exact-match callback URL (e.g. GitHub OAuth Apps). When omitted,
	// Crush picks the first free port from its default range.
	OAuthCallbackPort int `json:"oauth_callback_port,omitempty" jsonschema:"description=Fixed localhost port for the OAuth callback, required by providers that enforce exact-match redirect URIs"`

	// OAuthToken is the persisted OAuth token for this server. It is
	// managed internally and stored in the global data config.
	OAuthToken *oauth.Token `json:"oauth_token,omitempty" jsonschema:"-"`

	// ChannelEnabled enables an MCP server as a channel directly from config,
	// equivalent to passing its name via --channels on the CLI. This lets
	// channels be declared persistently in crush.json without needing a CLI
	// flag on every launch.
	ChannelEnabled bool `json:"channel_enabled,omitempty" jsonschema:"description=Enable this MCP server as a channel (equivalent to --channels),default=false"`

	// ChannelReply, when set on a server enabled as a channel, makes Crush
	// route the final assistant response of every turn that originated from
	// this channel back through one of the server's own tools, so a message
	// received on the channel gets a reply on the channel even when the
	// model only produced terminal output.
	ChannelReply *MCPChannelReply `json:"channel_reply,omitempty" jsonschema:"description=Automatically route replies for turns originating from this channel back through one of the server's tools"`
}

// MCPChannelReply configures deterministic reply routing for an MCP server
// acting as a channel: which of the server's tools deliver a reply for
// direct and group pushes, and how the reply text and target are mapped
// onto tool arguments.
type MCPChannelReply struct {
	// User routes replies to direct (person-to-person) channel pushes.
	User *MCPChannelReplyRoute `json:"user,omitempty" jsonschema:"description=Reply route for direct messages"`
	// Group routes replies to group channel pushes. It is preferred over
	// User when the push carries the group route's meta attribute.
	Group *MCPChannelReplyRoute `json:"group,omitempty" jsonschema:"description=Reply route for group messages"`
	// MessageParam is the tool argument that receives the reply text.
	MessageParam string `json:"message_param,omitempty" jsonschema:"description=Tool argument name that receives the reply text,default=message"`
	// SuppressTools lists additional tool names (beyond the two route
	// tools) that count as the model having already replied on the channel
	// during the turn, e.g. an operator-shortcut send tool.
	SuppressTools []string `json:"suppress_tools,omitempty" jsonschema:"description=Additional tool names that suppress the automatic reply when the model already called one of them during the turn,example=send"`
}

// MCPChannelReplyRoute maps one kind of inbound channel push onto the MCP
// tool call that delivers a reply to it.
type MCPChannelReplyRoute struct {
	// Tool is the MCP tool (bare name, without the mcp_<server>_ prefix)
	// invoked to deliver the reply.
	Tool string `json:"tool" jsonschema:"required,description=MCP tool name that sends the reply,example=send_message_to_user"`
	// TargetParam is the tool argument that receives the reply target
	// (recipient or group ID).
	TargetParam string `json:"target_param" jsonschema:"required,description=Tool argument name that receives the reply target,example=user_id"`
	// TargetMeta is the <channel> meta attribute whose value identifies
	// the reply target. Defaults to "sender" for the user route and
	// "group" for the group route.
	TargetMeta string `json:"target_meta,omitempty" jsonschema:"description=Channel meta attribute carrying the reply target; defaults to sender (user route) or group (group route)"`
}

// isOrphanedToken reports whether this entry is a leftover OAuth token
// with no real server config.
func (m MCPConfig) isOrphanedToken() bool {
	return m.Type == "" && m.Command == "" && m.URL == "" && m.OAuthToken != nil
}

type LSPConfig struct {
	Disabled    bool              `json:"disabled,omitempty" jsonschema:"description=Whether this LSP server is disabled,default=false"`
	Command     string            `json:"command,omitempty" jsonschema:"description=Command to execute for the LSP server,example=gopls"`
	Args        []string          `json:"args,omitempty" jsonschema:"description=Arguments to pass to the LSP server command"`
	Env         map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set to the LSP server command"`
	FileTypes   []string          `json:"filetypes,omitempty" jsonschema:"description=File types this LSP server handles,example=go,example=mod,example=rs,example=c,example=js,example=ts"`
	RootMarkers []string          `json:"root_markers,omitempty" jsonschema:"description=Files or directories that indicate the project root,example=go.mod,example=package.json,example=Cargo.toml"`
	InitOptions map[string]any    `json:"init_options,omitempty" jsonschema:"description=Initialization options passed to the LSP server during initialize request"`
	Options     map[string]any    `json:"options,omitempty" jsonschema:"description=LSP server-specific settings passed during initialization"`
	Timeout     int               `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for LSP server initialization,default=30,example=60,example=120"`
}

type TUIOptions struct {
	CompactMode bool        `json:"compact_mode,omitempty" jsonschema:"description=Enable compact mode for the TUI interface,default=false"`
	DiffMode    string      `json:"diff_mode,omitempty" jsonschema:"description=Diff mode for the TUI interface,enum=unified,enum=split"`
	ActiveTheme string      `json:"active_theme,omitempty" jsonschema:"description=Name of the currently active theme,default=gnosh-cosmos,example=gnosh-cosmos,example=charmtone-panther,example=gruvbox-dark"`
	Completions Completions `json:"completions,omitzero" jsonschema:"description=Completions UI options"`
	Transparent *bool       `json:"transparent,omitempty" jsonschema:"description=Enable transparent background for the TUI interface,default=false"`
	Scrollbar   string      `json:"scrollbar,omitempty" jsonschema:"description=Chat scrollbar visibility,enum=default,enum=always,enum=never,default=default"`
	Mouse       *bool       `json:"mouse,omitempty" jsonschema:"description=Enable terminal mouse capture for selection\\, clicks\\, and scrolling in the TUI. Disable to let the terminal emulator or tmux handle text selection and copy/paste,default=true"`
	ExitBanner  ExitBanner  `json:"exit_banner,omitempty" jsonschema:"description=Exit banner style after quitting Gnosh,enum=default,enum=compact,enum=none,default=default"`
}

// IsTransparent reports whether the TUI draws a transparent background. The
// nil receiver and the unset pointer both mean opaque, so callers can ask
// without unwrapping either.
func (t *TUIOptions) IsTransparent() bool {
	return t != nil && t.Transparent != nil && *t.Transparent
}

// UnmarshalJSON tolerates the legacy string form of the "theme" field.
// Older Crush builds stored the selected theme as `"theme":
// "gruvbox-dark"`. When that string is encountered it is promoted to the
// active theme name so existing configs keep loading.
func (t *TUIOptions) UnmarshalJSON(data []byte) error {
	type tuiOptionsAlias TUIOptions
	var loose struct {
		tuiOptionsAlias
		Theme json.RawMessage `json:"theme"`
	}
	if err := json.Unmarshal(data, &loose); err != nil {
		return err
	}
	*t = TUIOptions(loose.tuiOptionsAlias)
	if len(loose.Theme) == 0 || string(loose.Theme) == "null" {
		return nil
	}

	var legacyName string
	if err := json.Unmarshal(loose.Theme, &legacyName); err != nil {
		// Legacy inline theme maps are ignored now that files are the only
		// palette source, but they must remain loadable during migration.
		var legacyThemes map[string]json.RawMessage
		return json.Unmarshal(loose.Theme, &legacyThemes)
	}
	if legacyName != "" && t.ActiveTheme == "" {
		t.ActiveTheme = legacyName
	}
	return nil
}

// Completions defines options for the completions UI.
type Completions struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

// Limits returns the configured completion limits. Zero means the user has not
// pinned that limit, and callers fall back to their own built-in cap.
func (c Completions) Limits() (depth, items int) {
	return ptrValOr(c.MaxDepth, 0), ptrValOr(c.MaxItems, 0)
}

// Diff mode options.
const (
	DiffModeUnified = "unified" // Inline unified diffs
	DiffModeSplit   = "split"   // Side-by-side diffs
)

// Scrollbar visibility options.
const (
	ScrollbarDefault = "default" // Auto-hide after 2 seconds
	ScrollbarAlways  = "always"  // Always show when content exceeds viewport
	ScrollbarNever   = "never"   // Never show scrollbar
)

// ExitBanner selects what Crush prints after the TUI exits.
type ExitBanner string

const (
	// ExitBannerDefault renders the full ASCII art logo with padding. It is
	// also what the zero value and any unrecognized value fall back to.
	ExitBannerDefault ExitBanner = "default"
	// ExitBannerCompact renders only the session and resume lines, with no
	// logo and no padding. With no active session it renders nothing at all,
	// so Crush exits silently.
	ExitBannerCompact ExitBanner = "compact"
	// ExitBannerNone renders nothing.
	ExitBannerNone ExitBanner = "none"
)

type Permissions struct {
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"description=List of tools that don't require permission prompts,example=bash,example=view"`
	// Allow are Claude Code's permissions.allow rules of the two kinds
	// gnosh applies: a tool's name, which lets the tool run without
	// asking as allowed_tools does, and Bash(pattern), which lets the
	// commands the pattern matches run without asking (see
	// shell.CommandRule). Another kind of rule is refused at load.
	Allow []string `json:"allow,omitempty" jsonschema:"description=Claude Code's allow rules: a tool's name to let it run without asking or Bash(pattern) to let the commands the pattern matches run without asking,example=Bash(git diff *),example=Read"`
	// Deny are Claude Code's permissions.deny rules of the two kinds gnosh
	// applies: a tool's name, which takes the tool from the agent as
	// options.disabled_tools does, and Bash(pattern), which stops every
	// command the pattern matches (shell.CommandRuleBlocker). Another kind
	// of rule is refused at load, so a rule meant to protect something is
	// never ignored.
	Deny []string `json:"deny,omitempty" jsonschema:"description=Claude Code's deny rules: a tool's name to take the tool away or Bash(pattern) to stop the commands the pattern matches,example=Bash(git push *),example=WebFetch"`
}

// BashDenyPatterns returns the patterns of the Bash(pattern) rules in
// permissions.deny.
func (p *Permissions) BashDenyPatterns() []string {
	if p == nil {
		return nil
	}
	var patterns []string
	for _, rule := range p.Deny {
		if pattern, ok := BashRulePattern(rule); ok {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// BashAllowPatterns returns the patterns of the Bash(pattern) rules in
// permissions.allow, but Bash(*): that is Bash, which ValidateTools puts
// in allowed_tools.
func (p *Permissions) BashAllowPatterns() []string {
	if p == nil {
		return nil
	}
	var patterns []string
	for _, rule := range p.Allow {
		if pattern, ok := BashRulePattern(rule); ok && strings.TrimSpace(pattern) != "*" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// BashRulePattern is the pattern of rule when it is a Bash(pattern) rule,
// in Claude Code's spelling or gnosh's (bash).
func BashRulePattern(rule string) (string, bool) {
	tool, rest, ok := strings.Cut(rule, "(")
	if !ok || !strings.EqualFold(tool, "bash") {
		return "", false
	}
	return strings.CutSuffix(rest, ")")
}

type TrailerStyle string

const (
	TrailerStyleNone         TrailerStyle = "none"
	TrailerStyleCoAuthoredBy TrailerStyle = "co-authored-by"
	TrailerStyleAssistedBy   TrailerStyle = "assisted-by"
)

type Attribution struct {
	TrailerStyle  TrailerStyle `json:"trailer_style,omitempty" jsonschema:"description=Style of attribution trailer to add to commits,enum=none,enum=co-authored-by,enum=assisted-by,default=assisted-by"`
	CoAuthoredBy  *bool        `json:"co_authored_by,omitempty" jsonschema:"description=Deprecated: use trailer_style instead"`
	GeneratedWith bool         `json:"generated_with,omitempty" jsonschema:"description=Add Generated with Gnosh line to commit messages and issues and PRs,default=true"`
}

// JSONSchemaExtend marks the co_authored_by field as deprecated in the schema.
func (Attribution) JSONSchemaExtend(schema *jsonschema.Schema) {
	if schema.Properties != nil {
		if prop, ok := schema.Properties.Get("co_authored_by"); ok {
			prop.Deprecated = true
		}
	}
}

type Options struct {
	ContextPaths       []string `json:"context_paths,omitempty" jsonschema:"description=Paths to files containing context information for the AI,example=.cursorrules,example=CRUSH.md"`
	GlobalContextPaths []string `json:"global_context_paths,omitempty" jsonschema:"description=Paths to files containing global context information for the AI,default=~/.config/crush/CRUSH.md,default=~/.config/AGENTS.md"`
	SkillsPaths        []string `json:"skills_paths,omitempty" jsonschema:"description=Paths to directories containing Agent Skills (folders with SKILL.md files),example=~/.config/crush/skills,example=./skills"`
	// Reasoning holds how hard each model thinks, keyed by
	// "provider/model". A Qwen takes nothink or think, a gpt-oss takes
	// low, medium or high; see [ReasoningAxis]. /model and the left and
	// right arrows set it.
	Reasoning            map[string]string `json:"reasoning,omitempty" jsonschema:"description=How hard each model thinks\\, keyed by provider/model: nothink or think for a Qwen\\, low or medium or high for a gpt-oss. Set it with /model and the left and right arrows."`
	TUI                  *TUIOptions       `json:"tui,omitempty" jsonschema:"description=Terminal user interface options"`
	Debug                bool              `json:"debug,omitempty" jsonschema:"description=Enable debug logging,default=false"`
	DebugLSP             bool              `json:"debug_lsp,omitempty" jsonschema:"description=Enable debug logging for LSP servers,default=false"`
	DisableAutoSummarize bool              `json:"disable_auto_summarize,omitempty" jsonschema:"description=Disable automatic conversation summarization,default=false"`
	// DisableIdleCompaction turns off Claude Code's idle compaction: a
	// long conversation left idle for 54 minutes is summarized, so the
	// next prompt does not send the whole of it.
	DisableIdleCompaction bool `json:"disable_idle_compaction,omitempty" jsonschema:"description=Do not summarize a long conversation left idle for 54 minutes,default=false"`
	// AutoCompactWindow is how full a conversation's context gets before
	// it is summarized, in tokens (100k to 1M, and never past the model's
	// window), as Claude Code's autoCompactWindow setting: 0 for auto, the
	// point tuned for the model. /autocompact sets it.
	AutoCompactWindow int64 `json:"auto_compact_window,omitempty" jsonschema:"description=How full the context gets before it is summarized: tokens from 100000 to 1000000 and never past the model's window; unset for auto. Set it in chat with /autocompact,minimum=100000,maximum=1000000"`
	// DataDirectory is where Crush keeps per-project state such as
	// the SQLite database and workspace overrides. Relative paths are
	// resolved against the working directory; absolute paths are used
	// verbatim. After defaulting the stored value is always absolute.
	DataDirectory             string       `json:"data_directory,omitempty" jsonschema:"description=Directory for storing application data. Relative paths are resolved against the working directory; absolute paths are used as-is.,default=.crush,example=.crush"`
	DisabledTools             []string     `json:"disabled_tools,omitempty" jsonschema:"description=List of built-in tools to disable and hide from the agent,example=bash,example=sourcegraph"`
	DisableProviderAutoUpdate *bool        `json:"disable_provider_auto_update,omitempty" jsonschema:"description=Disable fetching the provider catalog from Charm's servers (catwalk.charm.land and hyper.charm.land). Unset it is not fetched and the catalog built into Gnosh is used; set false to fetch it,default=true"`
	DisableDefaultProviders   bool         `json:"disable_default_providers,omitempty" jsonschema:"description=Ignore all default/embedded providers. When enabled\\, providers must be fully specified in the config file with base_url\\, models\\, and api_key - no merging with defaults occurs,default=false"`
	Attribution               *Attribution `json:"attribution,omitempty" jsonschema:"description=Attribution settings for generated content"`
	DisableMetrics            *bool        `json:"disable_metrics,omitempty" jsonschema:"description=Disable sending usage metrics. Unset none are sent; set false to send them to metrics_url (never to Charm),default=true"`
	MetricsURL                string       `json:"metrics_url,omitempty" jsonschema:"description=Where opted-in usage metrics go: a PostHog-compatible endpoint such as https://metrics.klash.pro. Empty sends nowhere; Charm is never a default."`
	MetricsKey                string       `json:"metrics_key,omitempty" jsonschema:"description=Project key sent with metrics to metrics_url; a self-hosted collector may ignore it. Defaults to gnosh when metrics_url is set."`
	UpdateRepo                string       `json:"update_repo,omitempty" jsonschema:"description=GitHub owner/repo whose latest release the update check compares against (e.g. gnosh-ai/gnosh). Empty disables the check; Charm's repo is never a default."`
	InitializeAs              string       `json:"initialize_as,omitempty" jsonschema:"description=Name of the context file to create/update during project initialization,default=AGENTS.md,example=AGENTS.md,example=CRUSH.md,example=CLAUDE.md,example=docs/LLMs.md"`
	AutoLSP                   *bool        `json:"auto_lsp,omitempty" jsonschema:"description=Automatically setup LSPs based on root markers,default=true"`
	Progress                  *bool        `json:"progress,omitempty" jsonschema:"description=Show indeterminate progress updates during long operations,default=true"`
	Notifications             string       `json:"notifications,omitempty" jsonschema:"description=Notification style to use. Options: auto (default)\\, native\\, osc\\, bell\\, disabled. Auto selects based on environment: native for local sessions\\, osc for SSH (with automatic OSC 99/777 detection).,enum=auto,enum=native,enum=osc,enum=bell,enum=disabled,default=auto"`
	DisabledSkills            []string     `json:"disabled_skills,omitempty" jsonschema:"description=List of skill names to disable and hide from the agent,example=gnosh-config"`
	RequestTimeout            *int         `json:"request_timeout,omitempty" jsonschema:"description=Timeout in seconds for each LLM API request. Streaming responses are aborted only after this much inactivity\\, so slow but active streams are never killed. 0 disables it\\, negative values are invalid.,default=60,example=120,example=300,example=0"`
	MaxBackgroundJobs         *int         `json:"max_background_jobs,omitempty" jsonschema:"description=How many background shell jobs may run at once. Jobs that have finished never count against it. 0 or negative restores the default.,default=50,example=10,example=200"`
	// ArtifactServer is where Gnosh serves the deliverables the agent
	// records with the artifact tool, so the user can open one from the
	// machine they are sitting at rather than being handed a path on the
	// machine Gnosh runs on. Unset listens on every interface at port
	// 7777 (a port already taken falls back to a free one); an address of
	// its own narrows it, as "127.0.0.1:7777" does; "off" serves none,
	// and an artifact is then recorded as its path. Only a file the agent
	// recorded is served, each behind an unguessable token.
	ArtifactServer string `json:"artifact_server,omitempty" jsonschema:"description=Where recorded artifacts (see the artifact tool) are served so the user can open them from another machine. Unset listens on 127.0.0.1:7777\\, reachable from another machine through an ssh -L forward; \":7777\" serves every interface; off serves none. Only files the agent recorded are served\\, each behind an unguessable token.,example=127.0.0.1:7777,example=off"`
	// MemEmbeddingsURL is the embedding server mem finds by meaning with,
	// and recalls with each typed prompt what bears on it: an
	// OpenAI-compatible base URL, as llama-server --embedding serves. mem's
	// text and the prompts typed are sent there. Unset, mem finds by words
	// alone and sends nothing anywhere: there is no default.
	MemEmbeddingsURL string `json:"mem_embeddings_url,omitempty" jsonschema:"description=An OpenAI-compatible embeddings server (such as llama-server --embedding) that mem uses to find by meaning\\, and to recall what bears on each prompt you type. mem's text and your prompts are sent to it to be embedded. Unset\\, mem finds by words alone and sends nothing.,example=http://127.0.0.1:8091/v1"`
	LoopMaxTurns     *int   `json:"loop_max_turns,omitempty" jsonschema:"description=An optional hard cap on how many turns /loop may drive. Unset (or 0 or negative) means no cap: every 25 turns the agent is asked whether it has a genuine reason to stop and otherwise keeps going.,example=200"`
}

// DefaultMaxBackgroundJobs is how many background shell jobs may run at once
// when the user has not set max_background_jobs.
const DefaultMaxBackgroundJobs = 50

// GetLoopMaxTurns returns the user's hard cap on the turns /loop may drive,
// or 0 for none, the default. Without a cap the loop stops when the agent
// reports the task finished, or judges at one of its periodic checkpoints
// that there is a genuine reason to stop, or when the user presses Esc.
func (o *Options) GetLoopMaxTurns() int {
	if o == nil || o.LoopMaxTurns == nil || *o.LoopMaxTurns <= 0 {
		return 0
	}
	return *o.LoopMaxTurns
}

// ProviderAutoUpdate reports whether the provider catalog is fetched from
// Charm's servers. Upstream Crush fetches it unless told not to; this fork
// uses the catalog built into the binary unless the user asks for the
// fetch (option provider-auto-update true, or
// GNOSH_DISABLE_PROVIDER_AUTO_UPDATE=0).
func (o *Options) ProviderAutoUpdate() bool {
	return o != nil && o.DisableProviderAutoUpdate != nil && !*o.DisableProviderAutoUpdate
}

// MetricsOptedIn reports whether the config asks for usage metrics to be
// sent to Charm (option metrics true). Upstream Crush sends them unless told
// not to; this fork sends none unless asked.
func (o *Options) MetricsOptedIn() bool {
	return o != nil && o.DisableMetrics != nil && !*o.DisableMetrics
}

// MetricsSink is where opted-in metrics are sent. It is read only once the
// metrics gate is on (DO_NOT_TRACK off, and either an explicit opt-in or this
// machine's stored consent). The configured metrics_url wins; with none, the
// default is gnosh's own analytics (DefaultMetricsURL), never Charm's.
func (o *Options) MetricsSink() (url, key string) {
	if o != nil {
		url, key = o.MetricsURL, o.MetricsKey
	}
	if url == "" {
		url = DefaultMetricsURL
	}
	if key == "" {
		key = "gnosh"
	}
	return url, key
}

// UpdateCheckRepo is the GitHub owner/repo the update check compares against,
// or "" when none is configured (no check). Charm's repo is never a default.
func (o *Options) UpdateCheckRepo() string {
	if o == nil {
		return ""
	}
	return o.UpdateRepo
}

// GetMaxBackgroundJobs returns the running-job ceiling, always positive.
func (o *Options) GetMaxBackgroundJobs() int {
	if o == nil || o.MaxBackgroundJobs == nil || *o.MaxBackgroundJobs <= 0 {
		return DefaultMaxBackgroundJobs
	}
	return *o.MaxBackgroundJobs
}

// DefaultRequestTimeout bounds each LLM API request when the user has not
// configured a timeout. Slow or unreachable providers fail after it instead
// of blocking a session forever; streamed responses are only aborted after
// this much inactivity, and users running slow local models can raise or
// disable it via options.request_timeout.
const DefaultRequestTimeout = 2 * time.Minute

// GetRequestTimeout returns the per-request timeout for LLM API calls (a
// hard deadline for non-streaming requests and an idle timeout for
// streams), or zero when disabled. The nil receiver and the unset field
// both mean DefaultRequestTimeout, so callers can ask without unwrapping
// either.
func (o *Options) GetRequestTimeout() time.Duration {
	if o == nil || o.RequestTimeout == nil {
		return DefaultRequestTimeout
	}
	if *o.RequestTimeout <= 0 {
		return 0
	}
	return time.Duration(*o.RequestTimeout) * time.Second
}

type MCPs map[string]MCPConfig

type MCP struct {
	Name string    `json:"name"`
	MCP  MCPConfig `json:"mcp"`
}

func (m MCPs) Sorted() []MCP {
	sorted := make([]MCP, 0, len(m))
	for k, v := range m {
		sorted = append(sorted, MCP{
			Name: k,
			MCP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b MCP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

type LSPs map[string]LSPConfig

type LSP struct {
	Name string    `json:"name"`
	LSP  LSPConfig `json:"lsp"`
}

func (l LSPs) Sorted() []LSP {
	sorted := make([]LSP, 0, len(l))
	for k, v := range l {
		sorted = append(sorted, LSP{
			Name: k,
			LSP:  v,
		})
	}
	slices.SortFunc(sorted, func(a, b LSP) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}

// ResolvedEnv returns m.Env with every value expanded through the
// given resolver. The returned slice is of the form "KEY=value" sorted
// by key so callers get deterministic output; the receiver's Env map is
// not mutated. On the first resolution failure it returns nil and an
// error that identifies the offending key; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Callers are expected to surface it
// (for MCP, via StateError on the status card) rather than silently
// spawn the server with an empty credential.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim and expansion happens on the server.
func (m MCPConfig) ResolvedEnv(r VariableResolver) ([]string, error) {
	return resolveEnvs(m.Env, r)
}

// ResolvedArgs returns m.Args with every element expanded through the
// given resolver. A fresh slice is allocated; m.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(m.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(m.Args))
	for i, a := range m.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedURL returns m.URL expanded through the given resolver. The
// receiver is not mutated. Errors from the resolver are already
// sanitized by ResolveValue and are wrapped with %w for errors.Is/As.
//
// URLs run through the same shell-expansion pipeline as the other
// fields, so a literal '$' (e.g. OData query strings containing
// $filter/$select) must be escaped as '\$' or '${DOLLAR:-$}' to avoid
// being interpreted as a variable reference. Same constraint already
// applies to command, args, env, and headers.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedURL(r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := r.ResolveValue(m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// knownSessionlessMCPs is the set of MCP endpoint URLs (normalized, no
// trailing slash) that are known not to maintain an MCP session — they
// never issue a Mcp-Session-Id and reject the SEP-2575
// "subscriptions/listen" stream. Add an entry when a server is confirmed to
// behave this way.
var knownSessionlessMCPs = map[string]struct{}{
	"https://api.github.com/mcp":        {},
	"https://api.githubcopilot.com/mcp": {},
}

// IsSessionless reports whether the server should be treated as sessionless.
// An explicit Sessionless value wins; when unset, the resolved URL is matched
// against knownSessionlessMCPs (trailing slash ignored). The URL is resolved
// through r so $VAR-expanded endpoints are detected too; on a resolution
// error the explicit value (or false) is used.
func (m MCPConfig) IsSessionless(r VariableResolver) bool {
	if m.Sessionless != nil {
		return *m.Sessionless
	}
	url, err := m.ResolvedURL(r)
	if err != nil {
		return false
	}
	_, ok := knownSessionlessMCPs[strings.TrimSuffix(url, "/")]
	return ok
}

// ResolvedHeaders returns m.Headers with every value expanded through
// the given resolver. A fresh map is allocated; m.Headers is never
// mutated. On the first resolution failure it returns nil and an error
// identifying the offending header name; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// A header whose value resolves to the empty string (unset bare $VAR
// under lenient nounset, $(echo), or literal "") is omitted from the
// returned map — sending "X-Auth:" with an empty value is rejected by
// some providers and the user's intent in "optional, env-gated
// header" is clearly "absent when the var isn't set."
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedHeaders(r VariableResolver) (map[string]string, error) {
	if len(m.Headers) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m.Headers))
	// Sort keys so failures are reported deterministically when more
	// than one header would fail.
	keys := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(m.Headers[k])
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		if v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// ResolvedArgs returns l.Args with every element expanded through the
// given resolver. A fresh slice is allocated; l.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// Empty resolved values are kept (a deliberate "empty positional arg"
// like --flag "" is sometimes valid), matching MCPConfig.ResolvedArgs.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim.
func (l LSPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	if len(l.Args) == 0 {
		return nil, nil
	}
	out := make([]string, len(l.Args))
	for i, a := range l.Args {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// ResolvedEnv returns l.Env with every value expanded through the
// given resolver. A fresh map is allocated; l.Env is never mutated.
// On the first resolution failure it returns nil and an error that
// identifies the offending key; the inner resolver error is already
// sanitized by ResolveValue and is wrapped with %w so errors.Is/As
// continues to work.
//
// Empty resolved values are kept ("FOO=" is a legitimate request;
// opt out via ${VAR:+...}), matching MCPConfig.ResolvedEnv.
//
// Shape note: this returns map[string]string rather than the []string
// shape MCPConfig.ResolvedEnv uses because the consumer
// (powernap.ClientConfig.Environment in internal/lsp/client.go) takes
// a map directly — returning a []string here would only force a
// round-trip back to a map at the call site.
//
// See ResolvedArgs for guidance on picking a resolver.
func (l LSPConfig) ResolvedEnv(r VariableResolver) (map[string]string, error) {
	if len(l.Env) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(l.Env))
	// Sort keys so failures are reported deterministically when more
	// than one value would fail.
	keys := make([]string, 0, len(l.Env))
	for k := range l.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(l.Env[k])
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

type Agent struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// This is the id of the system prompt used by the agent
	Disabled bool `json:"disabled,omitempty"`

	Model SelectedModelType `json:"model" jsonschema:"required,description=The model type to use for this agent,enum=large,enum=small,default=large"`

	// The available tools for the agent
	//  if this is nil, all tools are available
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// this tells us which MCPs are available for this agent
	//  if this is empty all mcps are available
	//  the string array is the list of tools from the AllowedMCP the agent has available
	//  if the string array is nil, all tools from the AllowedMCP are available
	AllowedMCP map[string][]string `json:"allowed_mcp,omitempty"`

	// Overrides the context paths for this agent
	ContextPaths []string `json:"context_paths,omitempty"`
}

type Tools struct {
	Ls   ToolLs   `json:"ls,omitzero"`
	Grep ToolGrep `json:"grep,omitzero"`
	Glob ToolGlob `json:"glob,omitzero"`
}

type ToolLs struct {
	MaxDepth *int `json:"max_depth,omitempty" jsonschema:"description=Maximum depth for the ls tool,default=0,example=10"`
	MaxItems *int `json:"max_items,omitempty" jsonschema:"description=Maximum number of items to return for the ls tool,default=1000,example=100"`
}

// Limits returns the user-defined max-depth and max-items, or their defaults.
func (t ToolLs) Limits() (depth, items int) {
	return ptrValOr(t.MaxDepth, 0), ptrValOr(t.MaxItems, 0)
}

type ToolGrep struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the grep tool call,default=5s,example=10s"`
}

// GetTimeout returns the user-defined timeout or the default.
func (t ToolGrep) GetTimeout() time.Duration {
	return ptrValOr(t.Timeout, 5*time.Second)
}

type ToolGlob struct {
	Timeout *time.Duration `json:"timeout,omitempty" jsonschema:"description=Timeout for the glob tool call,default=30s,example=10s"`
}

// GetTimeout returns how long a glob may search:
// CLAUDE_CODE_GLOB_TIMEOUT_SECONDS, as in Claude Code, when it is a
// positive number, else the configured timeout, else 30 seconds.
func (t ToolGlob) GetTimeout() time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CLAUDE_CODE_GLOB_TIMEOUT_SECONDS"))); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return ptrValOr(t.Timeout, 30*time.Second)
}

// HookConfig defines a user-configured shell command that fires on a hook
// event (e.g. PreToolUse). This is a pure-data struct: matcher compilation
// is owned by hooks.Runner so a JSON round-trip, merge, or reload can't
// silently drop compiled state.
type HookConfig struct {
	// Friendly display name shown in the TUI. Falls back to Command when empty.
	Name string `json:"name,omitempty" jsonschema:"description=Friendly display name shown in the TUI for this hook"`
	// Matcher picks the tools (or the SessionStart source, the PreCompact
	// trigger) the hook runs for, as in Claude Code: empty or "*" for all,
	// exact names separated by | or , ("Bash", "edit|write"), or else an
	// unanchored regular expression ("^mcp_github_"). See HookMatcher.
	Matcher string `json:"matcher,omitempty" jsonschema:"description=Which tools the hook runs for as in Claude Code: empty or * for all; exact names separated by | such as Bash|edit (gnosh's or Claude Code's names); anything else is an unanchored regular expression."`
	// Shell command to execute.
	Command string `json:"command" jsonschema:"required,description=Shell command to execute when the hook fires"`
	// Timeout in seconds. Default 60, as in Claude Code.
	Timeout int `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds for the hook command. As in Claude Code it defaults to 600\\, or 30 for UserPromptSubmit.,default=600"`
}

// DisplayName returns the hook name for display purposes. It returns Name
// when set, otherwise falls back to Command.
func (h *HookConfig) DisplayName() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Command
}

// TimeoutDuration returns the hook's timeout for event: the one it sets,
// capped at 60 on SessionEnd, which runs while the process exits and
// should not hold the exit up for long; or else Claude Code's default for
// a command hook, 600 seconds, 1.5 on SessionEnd, and 30 on
// UserPromptSubmit, which holds up the prompt the user just sent. The
// default was 60 here, called Claude Code's, and a Claude Code Stop hook
// that runs the tests for longer was killed as a failed hook, which lets
// the turn end unchecked.
func (h *HookConfig) TimeoutDuration(event string) time.Duration {
	switch {
	case h.Timeout > 0:
		if event == EventSessionEnd {
			if d := time.Duration(h.Timeout) * time.Second; d <= 60*time.Second {
				return d
			}
			return 60 * time.Second
		}
		return time.Duration(h.Timeout) * time.Second
	case event == EventSessionEnd:
		return 1500 * time.Millisecond
	case event == EventUserPromptSubmit:
		return 30 * time.Second
	}
	return 600 * time.Second
}

// Config holds the configuration for crush.
type Config struct {
	Schema string `json:"$schema,omitempty"`

	// We currently only support large/small as values here.
	Models map[SelectedModelType]SelectedModel `json:"models,omitempty" jsonschema:"description=Model configurations for different model types,example={\"large\":{\"model\":\"gpt-4o\",\"provider\":\"openai\"}}"`

	// Recently used models stored in the data directory config.
	RecentModels map[SelectedModelType][]SelectedModel `json:"recent_models,omitempty" jsonschema:"-"`

	// The providers that are configured
	Providers *csync.Map[string, ProviderConfig] `json:"providers,omitempty" jsonschema:"description=AI provider configurations"`

	MCP MCPs `json:"mcp,omitempty" jsonschema:"description=Model Context Protocol server configurations"`

	LSP LSPs `json:"lsp,omitempty" jsonschema:"description=Language Server Protocol configurations"`

	Hosts Hosts `json:"hosts,omitempty" jsonschema:"description=Other machines the bash tool can run a command on by name; each is reached through ssh"`

	Options *Options `json:"options,omitempty" jsonschema:"description=General application options"`

	Permissions *Permissions `json:"permissions,omitempty" jsonschema:"description=Permission settings for tool usage"`

	Tools Tools `json:"tools,omitzero" jsonschema:"description=Tool configurations"`

	Hooks map[string][]HookConfig `json:"hooks,omitempty" jsonschema:"description=User-defined shell commands that fire on hook events (e.g. PreToolUse)"`

	// Env is a map of environment variables set on startup.
	Env map[string]string `json:"env,omitempty" jsonschema:"description=Environment variables to set on startup"`

	Agents map[string]Agent `json:"-"`

	// SafeMode is Claude Code's --safe-mode for this process: the config's
	// customizations are off (see applySafeMode).
	SafeMode bool `json:"-"`

	// HeldProject is what of the project's own settings this config holds
	// back because the folder is not trusted, or nil when it holds nothing
	// (see TrustFolderOf).
	HeldProject *ProjectSettings `json:"-"`

	// skippedProviders records, for each provider the user configured
	// that loading had to leave out, why: a model server that did not
	// answer, a missing key or endpoint. With no provider left, that is
	// what the user needs to hear, not "no providers configured".
	skippedProviders map[string]string
}

// SkippedProviders returns why each provider the user configured was left
// out when the config loaded, by provider ID.
func (c *Config) SkippedProviders() map[string]string {
	return maps.Clone(c.skippedProviders)
}

// SkippedProvidersSummary says, in one line, which configured providers
// were left out and why, or "" when none were.
func (c *Config) SkippedProvidersSummary() string {
	var parts []string
	for _, id := range slices.Sorted(maps.Keys(c.skippedProviders)) {
		parts = append(parts, id+": "+c.skippedProviders[id])
	}
	return strings.Join(parts, "; ")
}

// skipProvider leaves provider id out of the config, recording why.
func (c *Config) skipProvider(id, reason string) {
	if c.skippedProviders == nil {
		c.skippedProviders = map[string]string{}
	}
	c.skippedProviders[id] = reason
	c.Providers.Del(id)
}

// SkipProviderForTest records that loading left provider id out, and why,
// as loading does, for tests of what reports it.
func (c *Config) SkipProviderForTest(id, reason string) {
	c.skipProvider(id, reason)
}

// cloneForWrite returns a copy of c that the store's typed field mutators
// may modify without racing readers of the currently published Config.
//
// Reads of a published Config take no lock beyond the pointer load, so a
// mutator must never write through the live pointer. Instead it clones,
// mutates the clone, and atomically swaps it in. The clone gives fresh
// copies of every field a typed mutator touches in place — Models,
// RecentModels, MCP, and Options (with its nested TUI pointer). Providers
// is a *csync.Map (internally synchronized) and is shared by reference;
// the remaining fields are immutable after load from the mutators'
// standpoint and are likewise shared.
func (c *Config) cloneForWrite() *Config {
	nc := *c
	nc.Models = maps.Clone(c.Models)
	nc.RecentModels = maps.Clone(c.RecentModels)
	nc.MCP = maps.Clone(c.MCP)
	if c.Options != nil {
		opts := *c.Options
		if c.Options.TUI != nil {
			tui := *c.Options.TUI
			opts.TUI = &tui
		}
		nc.Options = &opts
	}
	return &nc
}

// ensureTUI returns c.Options.TUI, allocating Options and TUI as needed so
// callers can assign TUI fields without nil checks.
func (c *Config) ensureTUI() *TUIOptions {
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	return c.Options.TUI
}

func (c *Config) EnabledProviders() []ProviderConfig {
	var enabled []ProviderConfig
	for p := range c.Providers.Seq() {
		if !p.Disable {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

// IsConfigured  return true if at least one provider is configured
func (c *Config) IsConfigured() bool {
	return len(c.EnabledProviders()) > 0
}

func (c *Config) GetModel(provider, model string) *catwalk.Model {
	if providerConfig, ok := c.Providers.Get(provider); ok {
		for _, m := range providerConfig.Models {
			if m.ID == model {
				return &m
			}
		}
		for _, m := range providerConfig.ChatGPTModels {
			if m.ID == model {
				return &m
			}
		}
		for _, m := range providerConfig.GrokModels {
			if m.ID == model {
				return &m
			}
		}
	}
	return nil
}

// ValidateReasoningEffort checks that effort is a reasoning level the
// given provider/model supports. It returns an error listing the accepted
// levels when the model cannot use it.
func (c *Config) ValidateReasoningEffort(provider, modelID, effort string) error {
	model := c.GetModel(provider, modelID)
	if model == nil {
		return fmt.Errorf("model %q not found for provider %q", modelID, provider)
	}
	if len(model.ReasoningLevels) == 0 {
		return fmt.Errorf("model %q does not support reasoning effort", modelID)
	}
	if slices.Contains(model.ReasoningLevels, effort) {
		return nil
	}
	return fmt.Errorf(
		"model %q does not support reasoning effort %q, accepted values: %s",
		modelID, effort, strings.Join(model.ReasoningLevels, ", "),
	)
}

// IsModelAvailable returns true if the provider is enabled and the model
// exists in its catalog. Unlike GetModel, it rejects disabled providers.
func (c *Config) IsModelAvailable(provider, model string) bool {
	providerConfig, ok := c.Providers.Get(provider)
	if !ok || providerConfig.Disable {
		return false
	}
	// A model is available in any of the provider's catalogs: the
	// API-key list, or the ChatGPT/Grok subscription list fetched on
	// login. Subscription models live outside Models, so checking only
	// there would reject a model that is selectable and usable.
	for _, m := range providerConfig.Models {
		if m.ID == model {
			return true
		}
	}
	for _, m := range providerConfig.ChatGPTModels {
		if m.ID == model {
			return true
		}
	}
	for _, m := range providerConfig.GrokModels {
		if m.ID == model {
			return true
		}
	}
	return false
}

func (c *Config) GetProviderForModel(modelType SelectedModelType) *ProviderConfig {
	model, ok := c.Models[modelType]
	if !ok {
		return nil
	}
	if providerConfig, ok := c.Providers.Get(model.Provider); ok {
		return &providerConfig
	}
	return nil
}

func (c *Config) GetModelByType(modelType SelectedModelType) *catwalk.Model {
	model, ok := c.Models[modelType]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) LargeModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeLarge]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

func (c *Config) SmallModel() *catwalk.Model {
	model, ok := c.Models[SelectedModelTypeSmall]
	if !ok {
		return nil
	}
	return c.GetModel(model.Provider, model.Model)
}

const maxRecentModelsPerType = 5

// MCPToolPrefix is the prefix every MCP-provided tool name carries
// ("mcp_<server>_<tool>").
const MCPToolPrefix = tooldefs.MCPPrefix

// KnownToolNames returns the built-in tools the agent can be given, the
// set that options.disabled_tools filters.
func KnownToolNames() []string {
	return tooldefs.Names()
}

// IsKnownToolName reports whether name is a built-in tool.
func IsKnownToolName(name string) bool {
	return tooldefs.IsKnown(name)
}

func allToolNames() []string {
	return tooldefs.Names()
}

func resolveAllowedTools(allTools []string, disabledTools []string) []string {
	if disabledTools == nil {
		return allTools
	}
	return toolsExcept(allTools, disabledTools)
}

// readOnlyToolNames are the tools that only look at the project. The
// names are duplicated from internal/agent/tools, which imports this
// package and so cannot be imported back; TestToolSubsetsAreRealTools
// is what keeps the copies honest, because a name that drifts out of
// allToolNames() would silently drop the tool from these agents rather
// than fail.
func readOnlyToolNames() []string {
	return []string{
		"glob",
		"grep",
		"ls",
		"lsp_call_hierarchy",
		"lsp_definition",
		"lsp_symbols",
		"sourcegraph",
		"view",
	}
}

func resolveReadOnlyTools(tools []string) []string {
	return toolsOnly(tools, readOnlyToolNames())
}

// planToolNames are the tools a planning turn may use. The read-only
// LSP lookups mirror the task agent's tool set: planning needs symbol
// navigation just as much as research does. See readOnlyToolNames for
// why these names are spelled out rather than referenced.
func planToolNames() []string {
	return []string{
		"agent",
		"glob",
		"gnovm_list",
		"grep",
		"ls",
		"lsp_call_hierarchy",
		"lsp_definition",
		"lsp_symbols",
		"question",
		"sourcegraph",
		"view",
	}
}

func resolvePlanTools(tools []string) []string {
	return toolsOnly(tools, planToolNames())
}

// toolsOnly keeps the entries of data that appear in mask.
func toolsOnly(data []string, mask []string) []string {
	return filterTools(data, mask, true)
}

// toolsExcept keeps the entries of data that do not appear in mask.
func toolsExcept(data []string, mask []string) []string {
	return filterTools(data, mask, false)
}

// filterTools is the shared body of toolsOnly and toolsExcept. Callers
// use those instead: filterTools(all, disabled, false) reads as nothing
// at a call site, while toolsExcept(all, disabled) reads as itself.
func filterTools(data []string, mask []string, keepMatching bool) []string {
	var filtered []string
	for _, s := range data {
		if slices.Contains(mask, s) == keepMatching {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func (c *Config) SetupAgents() {
	allowedTools := resolveAllowedTools(allToolNames(), c.Options.DisabledTools)

	agents := map[string]Agent{
		AgentCoder: {
			ID:           AgentCoder,
			Name:         "Coder",
			Description:  "An agent that helps with executing coding tasks.",
			Model:        SelectedModelTypeLarge,
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: allowedTools,
		},

		AgentTask: {
			ID:           AgentTask,
			Name:         "Task",
			Description:  "An agent that helps with searching for context and finding implementation details.",
			Model:        SelectedModelTypeLarge,
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: resolveReadOnlyTools(allowedTools),
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},

		AgentPlan: {
			ID:           AgentPlan,
			Name:         "Plan",
			Description:  "An agent that performs deep analysis and prepares implementation plans without modifying files.",
			Model:        SelectedModelTypeLarge,
			ContextPaths: c.Options.ContextPaths,
			AllowedTools: resolvePlanTools(allowedTools),
			// NO MCPs or LSPs by default
			AllowedMCP: map[string][]string{},
		},
	}
	c.Agents = agents
}

func (c *ProviderConfig) TestConnection(resolver VariableResolver) error {
	var (
		providerID = catwalk.InferenceProvider(c.ID)
		testURL    = ""
		headers    = make(map[string]string)
		apiKey, _  = resolver.ResolveValue(c.APIKey)
	)

	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		// NOTE: MiniMax has no good endpoint we can use to validate the API key.
		return nil
	case catwalk.InferenceProviderAlibabaSingapore:
		// NOTE: Alibaba has no good endpoint we can use to validate the API key.
		// Let's at least check the pattern.
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	switch c.Type {
	case catwalk.TypeOpenAI, catwalk.TypeOpenAICompat, catwalk.TypeOpenRouter:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.openai.com/v1")

		switch providerID {
		case catwalk.InferenceProviderOpenRouter:
			testURL = baseURL + "/credits"
		case catwalk.InferenceProviderOpenCodeGo:
			testURL = strings.Replace(baseURL, "/go", "", 1) + "/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["Authorization"] = "Bearer " + apiKey
	case catwalk.TypeAnthropic:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")

		switch providerID {
		case catwalk.InferenceKimiCoding:
			testURL = baseURL + "/v1/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case catwalk.TypeGoogle:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		testURL = baseURL + "/v1beta/models?key=" + url.QueryEscape(apiKey)
	case catwalk.TypeBedrock:
		// NOTE: Bedrock has a `/foundation-models` endpoint that we could in
		// theory use, but apparently the authorization is region-specific,
		// so it's not so trivial.
		if strings.HasPrefix(apiKey, "ABSK") { // Bedrock API keys
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case catwalk.TypeVercel:
		// NOTE: Vercel does not validate API keys on the `/models` endpoint.
		if strings.HasPrefix(apiKey, "vck_") { // Vercel API keys
			return nil
		}
		return errors.New("not a valid vercel api key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	switch providerID {
	case catwalk.InferenceProviderZAI:
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	default:
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	}
	return nil
}

// resolveEnvs expands every value in envs through the given resolver
// and returns a fresh "KEY=value" slice sorted by key. The input map is
// not mutated. On the first resolution failure it returns nil and an
// error identifying the offending variable; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w.
func resolveEnvs(envs map[string]string, r VariableResolver) ([]string, error) {
	if len(envs) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res := make([]string, 0, len(envs))
	for _, k := range keys {
		v, err := r.ResolveValue(envs[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res, nil
}

func ptrValOr[T any](t *T, el T) T {
	if t == nil {
		return el
	}
	return *t
}

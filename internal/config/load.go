package config

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/shellconfig"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/tooldefs"
	powernapConfig "github.com/charmbracelet/x/powernap/pkg/config"
	"github.com/qjebbs/go-jsons"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const defaultCatwalkURL = "https://catwalk.charm.land"

// LoadOption changes how a load treats the project's own settings.
type LoadOption func(*loadOptions)

type loadOptions struct {
	trustFolder bool
}

// TrustFolderForSession has a load use the project's own settings, as a
// trusted folder's, without recording the trust: for the home directory,
// which is trusted for a session only, and for --trust-folder. Every reload
// of the store keeps it.
func TrustFolderForSession() LoadOption {
	return func(o *loadOptions) { o.trustFolder = true }
}

// LoadFiles loads the configuration files from the default paths, merged
// and with their defaults, and stops short of the providers: for commands
// that call no model, as gnosh mem and gnosh session, which must work on a
// machine with none configured. Load goes on from here.
//
// The project's own settings are used in full only where its folder is
// trusted (see TrustFolderOf); elsewhere its gnoshrc does not run and only
// the settings that take something away apply, the rest reported in
// Config.HeldProject.
func LoadFiles(workingDir, dataDir string, debug bool, opts ...LoadOption) (*ConfigStore, error) {
	store, _, err := loadFiles(workingDir, dataDir, debug, opts...)
	return store, err
}

// loadFiles is LoadFiles, with the config paths Load watches for changes:
// every one it looked at, loaded or not, so a config file created after
// startup (a crushrc added mid-session, say) counts as a change.
func loadFiles(workingDir, dataDir string, debug bool, opts ...LoadOption) (*ConfigStore, []string, error) {
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}

	// Migrate deprecated disable_notifications before loading config.
	migrateDisableNotifications()

	if err := checkProjectConfigOwners(workingDir); err != nil {
		return nil, nil, err
	}
	// Whether the project's own settings are used is decided once, here,
	// and every reload keeps the decision: a session's answer to the trust
	// question holds for the whole session.
	trust := decideTrust(workingDir, o.trustFolder)
	gate := newProjectGate(workingDir, trust)
	configPaths := lookupConfigs(workingDir)

	cfg, loadedPaths, err := loadFromConfigPathsGated(context.Background(), configPaths, gate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	cfg.setDefaults(workingDir, dataDir)

	store := &ConfigStore{
		config:         cfg,
		workingDir:     workingDir,
		globalDataPath: GlobalConfigData(),
		workspacePath:  configFileIn(cfg.Options.DataDirectory),
		loadedPaths:    loadedPaths,
		trust:          trust,
	}

	if debug {
		cfg.Options.Debug = true
	}

	// Load workspace config last so it has highest priority.
	merged, err := mergeWorkspaceConfigGated(cfg, store.workspacePath, workingDir, gate)
	if err != nil {
		return nil, nil, err
	}
	if merged {
		store.config = cfg
		store.loadedPaths = append(store.loadedPaths, store.workspacePath)
	}
	// After the workspace merge, whose setDefaults adds the project's
	// skills folders again.
	gate.holdSkillDirs(cfg, workingDir)
	cfg.HeldProject = gate.heldSettings()

	// Validate after all config merging is complete, so the checks see
	// the same config the session will actually run with.
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return store, append(slices.Clone(configPaths), loadedPaths...), nil
}

// Load loads the configuration from the default paths and returns a
// ConfigStore that owns both the pure-data Config and all runtime state.
// As LoadFiles, it uses the project's own settings in full only where its
// folder is trusted.
func Load(workingDir, dataDir string, debug bool, opts ...LoadOption) (*ConfigStore, error) {
	store, watched, err := loadFiles(workingDir, dataDir, debug, opts...)
	if err != nil {
		return nil, err
	}
	cfg := store.config

	if !isInsideWorktree() {
		const depth = 2
		const items = 100
		slog.Warn("No git repository detected in working directory, will limit file walk operations", "depth", depth, "items", items)
		assignIfNil(&cfg.Tools.Ls.MaxDepth, depth)
		assignIfNil(&cfg.Tools.Ls.MaxItems, items)
		assignIfNil(&cfg.Options.TUI.Completions.MaxDepth, depth)
		assignIfNil(&cfg.Options.TUI.Completions.MaxItems, items)
	}

	if isAppleTerminal() {
		slog.Warn("Detected Apple Terminal, enabling transparent mode")
		assignIfNil(&cfg.Options.TUI.Transparent, true)
	}

	// Load known providers, this loads the config from catwalk. A failed
	// refresh still yields the cached or embedded catalog, so only an empty
	// list is fatal: starting up without providers is worse than starting
	// up with slightly stale ones. Pass a Hyper token refresher so the
	// catalog fetch can retry on 401.
	providers, err := Providers(cfg, func(ctx context.Context) error {
		return store.RefreshOAuthToken(ctx, ScopeGlobal, "hyper")
	})
	if err != nil {
		if len(providers) == 0 {
			return nil, err
		}
		slog.Warn("Continuing with the previously known providers", "error", err)
	}
	store.knownProviders = providers

	// When Catwalk refreshed its catalog this run, give the model
	// catalogs that shadow Catwalk's the same treatment: the ChatGPT and
	// Grok catalogs are otherwise only fetched at login and would freeze
	// there while Catwalk keeps moving. Best effort; a failed fetch keeps
	// the catalog loaded from config.
	//
	// The refetches publish a copy-on-write config, so re-read it
	// afterwards: the mutations below must land on the live config rather
	// than a snapshot the store has already replaced.
	if CatwalkUpdated() {
		fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		store.refetchOpenAIModels(fetchCtx, ScopeGlobal)
		store.refetchGrokModels(fetchCtx, ScopeGlobal)
		cancel()
		cfg = store.Config()
	}

	env := env.New()
	// Configure providers
	valueResolver := NewShellVariableResolver(env)
	store.resolver = valueResolver

	// Hold writeMu during initial load to prevent configureProviders
	// from triggering auto-reload via RemoveConfigField.
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	// Apply top-level env vars before configuring providers so variables
	// like AWS_PROFILE are visible to the AWS SDK credential chain.
	cfg.applyEnv(valueResolver)

	if err := cfg.configureProviders(context.Background(), store, env, valueResolver, store.knownProviders); err != nil {
		return nil, fmt.Errorf("failed to configure providers: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured")
		return store, nil
	}

	resolved, err := resolveSelectedModels(cfg, store.knownProviders)
	if err != nil {
		return nil, fmt.Errorf("failed to configure selected models: %w", err)
	}
	cfg.Models[SelectedModelTypeLarge] = resolved.Large
	cfg.Models[SelectedModelTypeSmall] = resolved.Small

	// Persist any fallback corrections while we still hold writeMu.
	if resolved.LargeFallback {
		if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
			return store.updatePreferredModelFields(c, SelectedModelTypeLarge, resolved.Large)
		}); err != nil {
			return nil, fmt.Errorf("failed to update preferred large model: %w", err)
		}
	}
	if resolved.SmallFallback {
		if err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
			return store.updatePreferredModelFields(c, SelectedModelTypeSmall, resolved.Small)
		}); err != nil {
			return nil, fmt.Errorf("failed to update preferred small model: %w", err)
		}
	}
	store.SetupAgents()

	// Capture initial staleness snapshot.
	store.captureStalenessSnapshot(watched)

	return store, nil
}

// mustMarshalConfig marshals the config to JSON bytes, returning empty JSON on
// error.
func mustMarshalConfig(cfg *Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// PushPopCrushEnv lets a GNOSH_, KLASH_ or CRUSH_ prefixed variable stand in
// for the unprefixed one while providers are configured, so that, e.g.,
// GNOSH_OPENAI_API_KEY overrides OPENAI_API_KEY for this program alone. The
// newer name wins: GNOSH_ over KLASH_, KLASH_ over CRUSH_. The returned func
// restores the previous values.
func PushPopCrushEnv() func() {
	overrides := make(map[string]string)
	for _, prefix := range []string{"CRUSH_", "KLASH_", "GNOSH_"} { // the later prefix wins
		for _, ev := range os.Environ() {
			name, value, ok := strings.Cut(ev, "=")
			if !ok {
				continue
			}
			if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" {
				overrides[rest] = value
			}
		}
	}
	backups := make(map[string]string)
	for name, value := range overrides {
		backups[name] = os.Getenv(name)
		os.Setenv(name, value)
	}
	return func() {
		for k, v := range backups {
			os.Setenv(k, v)
		}
	}
}

func (c *Config) configureProviders(ctx context.Context, store *ConfigStore, env env.Env, resolver VariableResolver, knownProviders []catwalk.Provider) error {
	knownProviderNames := make(map[string]bool)
	restore := PushPopCrushEnv()
	defer restore()

	// When disable_default_providers is enabled, skip all default/embedded
	// providers entirely. Users must fully specify any providers they want.
	// We skip to the custom provider validation loop which handles all
	// user-configured providers uniformly.
	if c.Options.DisableDefaultProviders {
		knownProviders = nil
	}

	for _, p := range knownProviders {
		knownProviderNames[string(p.ID)] = true
		config, configExists := c.Providers.Get(string(p.ID))
		// if the user configured a known provider we need to allow it to override a couple of parameters
		if configExists {
			if config.BaseURL != "" {
				p.APIEndpoint = config.BaseURL
			}
			if config.APIKey != "" {
				p.APIKey = config.APIKey
			}
			if len(config.Models) > 0 {
				models := []catwalk.Model{}
				seen := make(map[string]bool)

				for _, model := range config.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}
				for _, model := range p.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}

				p.Models = models
			}
		}

		headers := map[string]string{}
		if len(p.DefaultHeaders) > 0 {
			maps.Copy(headers, p.DefaultHeaders)
		}
		if len(config.ExtraHeaders) > 0 {
			maps.Copy(headers, config.ExtraHeaders)
		}
		// Provider headers use the same error contract as MCP headers:
		// a failing $(...) aborts the provider load with a clear
		// message, and a header that resolves to the empty string
		// (unset bare $VAR under lenient nounset, $(echo), or literal
		// "") is dropped from the outgoing request.
		for k, v := range headers {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", p.ID, k, err)
			}
			if resolved == "" {
				delete(headers, k)
				continue
			}
			headers[k] = resolved
		}
		// Start from user config so all user fields survive without
		// explicit copying. Overlay catwalk identity/endpoint fields
		// (already merged with user overrides above).
		prepared := config
		prepared.ID = string(p.ID)
		prepared.Name = p.Name
		prepared.BaseURL = p.APIEndpoint
		prepared.APIKey = p.APIKey
		prepared.APIKeyTemplate = p.APIKey // Store original template for re-resolution
		prepared.Type = p.Type
		prepared.Models = p.Models
		prepared.ExtraHeaders = headers
		if prepared.ExtraParams == nil {
			prepared.ExtraParams = make(map[string]string)
		}

		switch {
		case p.ID == catwalk.InferenceProviderAnthropic && config.OAuthToken != nil:
			// Claude Code subscription is not supported anymore. Remove to show onboarding.
			// RemoveConfigField persists the deletion to disk. The in-memory
			// state is kept consistent by the Providers.Del call below; any
			// concurrent reload that races with this write will also see the
			// removal because it re-reads from disk.
			store.RemoveConfigField(ScopeGlobal, "providers.anthropic")
			c.skipProvider(string(p.ID), "a Claude subscription login is no longer supported; set an API key")
			continue
		case p.ID == catwalk.InferenceProviderCopilot && config.OAuthToken != nil:
			prepared.SetupGitHubCopilot()
		}

		switch p.ID {
		// Handle specific providers that require additional configuration
		case catwalk.InferenceProviderVertexAI:
			var (
				project  = env.Get("VERTEXAI_PROJECT")
				location = env.Get("VERTEXAI_LOCATION")
			)
			if project == "" || location == "" {
				if configExists {
					slog.Warn("Skipping Vertex AI provider due to missing credentials")
					c.skipProvider(string(p.ID), "VERTEXAI_PROJECT and VERTEXAI_LOCATION are not set")
				}
				continue
			}
			prepared.ExtraParams["project"] = project
			prepared.ExtraParams["location"] = location
		case catwalk.InferenceProviderAzure:
			endpoint, err := resolver.ResolveValue(p.APIEndpoint)
			if err != nil || endpoint == "" {
				if configExists {
					slog.Warn("Skipping Azure provider due to missing API endpoint", "provider", p.ID, "error", err)
					c.skipProvider(string(p.ID), "it has no API endpoint")
				}
				continue
			}
			prepared.BaseURL = endpoint
			prepared.ExtraParams["apiVersion"] = env.Get("AZURE_OPENAI_API_VERSION")
		case catwalk.InferenceProviderBedrock, catwalk.InferenceProviderBedrockEurope:
			if p.APIKey == "" && !hasAWSCredentials(env) {
				if configExists {
					slog.Warn("Skipping Bedrock provider due to missing AWS credentials")
					c.skipProvider(string(p.ID), "there are no AWS credentials")
				}
				continue
			}
		case catwalk.InferenceProvider("hyper"):
			if apiKey := env.Get("HYPER_API_KEY"); apiKey != "" {
				prepared.APIKey = apiKey
				prepared.APIKeyTemplate = apiKey
			} else {
				v, err := resolver.ResolveValue(p.APIKey)
				if v == "" || err != nil {
					if configExists {
						slog.Warn("Skipping Hyper provider due to missing API key", "provider", p.ID)
						c.skipProvider(string(p.ID), "it has no API key")
					}
					continue
				}
			}
		default:
			// An OAuth login is a credential too: providers signed in
			// through OAuth (e.g. OpenAI with a ChatGPT account) are
			// configured even when no API key is present.
			if config.OAuthToken != nil {
				break
			}
			// if the provider api or endpoint are missing we skip them
			v, err := resolver.ResolveValue(p.APIKey)
			if v == "" || err != nil {
				if configExists {
					slog.Warn("Skipping provider due to missing API key", "provider", p.ID)
					c.skipProvider(string(p.ID), "it has no API key")
				}
				continue
			}
		}
		c.Providers.Set(string(p.ID), prepared)
	}

	// Discover models concurrently for custom providers that need it.
	// A provider needs discovery when discover_models is explicitly true,
	// or when the models list is empty (auto-trigger, unless opted out).
	// A provider that lists its models, one of them with no context
	// window, has the server fill that in, as a discovered model has, when
	// its type can (an enricher) and discovery is not turned off: unknown,
	// the window is taken for 32,768 tokens, and the AI box's Qwen, named
	// in a crushrc with no window, was compacted at 26,215 of its 262,144.
	type discoveryResult struct {
		models []catwalk.Model
		err    error
		// filledIn is set when the listed models only had their context
		// windows filled in.
		filledIn bool
	}

	discoveryResults := make(map[string]discoveryResult)
	var mu sync.Mutex
	var wg sync.WaitGroup

	discoverCtx, discoverCancel := context.WithTimeout(ctx, 3*time.Second)
	for id, pc := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
		autoTrigger := len(pc.Models) == 0 && (pc.AutoDiscoverModels == nil || *pc.AutoDiscoverModels)
		enricher := discover.GetEnricher(string(cmp.Or(pc.Type, catwalk.TypeOpenAICompat)))
		fillIn := !wantsDiscovery && !autoTrigger && pc.AutoDiscoverModels == nil && enricher != nil &&
			slices.ContainsFunc(pc.Models, func(m catwalk.Model) bool { return m.ContextWindow == 0 })
		if !wantsDiscovery && !autoTrigger && !fillIn {
			continue
		}
		providerID := cmp.Or(pc.ID, id)
		cfg := discover.Config{
			ID:             providerID,
			BaseURL:        pc.BaseURL,
			APIKey:         pc.APIKey,
			ExtraHeaders:   pc.ExtraHeaders,
			ExistingModels: pc.Models,
		}
		wg.Go(func() {
			if fillIn {
				models, _ := enricher.EnrichModels(discoverCtx, cfg, resolver, slices.Clone(pc.Models))
				mu.Lock()
				discoveryResults[id] = discoveryResult{models: models, filledIn: true}
				mu.Unlock()
				return
			}
			models, err := discover.DiscoverModels(discoverCtx, cfg, resolver)
			if err == nil && len(models) > 0 && enricher != nil {
				models, _ = enricher.EnrichModels(discoverCtx, cfg, resolver, models)
			}
			mu.Lock()
			discoveryResults[id] = discoveryResult{models: models, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()
	discoverCancel()

	// Validate the custom providers.
	for id, providerConfig := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}

		// Make sure the provider ID is set.
		providerConfig.ID = id
		providerConfig.Name = cmp.Or(providerConfig.Name, id) // Use ID as name if not set
		// Default to OpenAI if not set.
		providerConfig.Type = cmp.Or(providerConfig.Type, catwalk.TypeOpenAICompat)
		if !slices.Contains(catwalk.KnownProviderTypes(), providerConfig.Type) &&
			providerConfig.Type != hyper.Name &&
			!discover.IsKnownCustomProvider(string(providerConfig.Type)) {
			slog.Warn("Skipping custom provider due to unsupported provider type", "provider", id)
			c.skipProvider(id, fmt.Sprintf("its type %q is not supported", providerConfig.Type))
			continue
		}

		if providerConfig.Disable {
			slog.Debug("Skipping custom provider due to disable flag", "provider", id)
			c.Providers.Del(id)
			continue
		}
		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id, "error", err)
			c.skipProvider(id, "it has no base_url")
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					slog.Warn("Skipping provider with no models after failed discovery", "provider", id)
					c.skipProvider(id, fmt.Sprintf("its model server did not answer (%v)", result.err))
					continue
				}
			} else if result.filledIn {
				providerConfig.Models = result.models
			} else if len(result.models) > 0 {
				providerConfig.Models = result.models
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
			}
		}

		if len(providerConfig.Models) == 0 {
			slog.Warn("Skipping custom provider because the provider has no models", "provider", id)
			c.skipProvider(id, "it lists no models")
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// the known-provider loop above.
		for k, v := range providerConfig.ExtraHeaders {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", id, k, err)
			}
			if resolved == "" {
				delete(providerConfig.ExtraHeaders, k)
				continue
			}
			providerConfig.ExtraHeaders[k] = resolved
		}

		c.Providers.Set(id, providerConfig)
	}

	// Providers that were configured but left out (a model server that did
	// not answer, a missing base_url) do not fail the load: the TUI, gnosh
	// run and gnosh doctor then say which and why (SkippedProvidersSummary).
	// Failing here said none were configured, and gnosh doctor could not
	// show which one failed.
	if c.Providers.Len() == 0 && c.Options.DisableDefaultProviders && len(c.skippedProviders) == 0 {
		return fmt.Errorf("default providers are disabled and no custom providers are configured")
	}

	return nil
}

// applyEnv sets top-level env vars from the config. Keys are sorted for
// deterministic ordering so that vars referencing other vars via the
// value resolver produce consistent results.
func (c *Config) applyEnv(resolver VariableResolver) {
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		resolved, err := resolver.ResolveValue(c.Env[k])
		if err != nil {
			slog.Warn("Skipping env var due to resolution failure.", "key", k, "value", c.Env[k], "error", err)
			continue
		}
		os.Setenv(k, resolved)
	}
}

// NormalizeOptions allocates Options and Options.TUI and fills in the option
// defaults the UI relies on, so readers can dereference them without guarding.
// Configs loaded from disk get this via setDefaults; configs arriving over the
// wire from a Crush server need the same treatment before the UI reads them.
//
// DiffMode is deliberately left alone: the permissions dialog reads its zero
// value as "choose split or unified from the terminal width".
func (c *Config) NormalizeOptions() {
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	if c.Options.TUI.Scrollbar == "" {
		c.Options.TUI.Scrollbar = ScrollbarDefault
	}
	if c.Options.TUI.ExitBanner == "" {
		c.Options.TUI.ExitBanner = ExitBannerDefault
	}
}

func (c *Config) setDefaults(workingDir, dataDir string) {
	c.NormalizeOptions()
	if len(c.Options.GlobalContextPaths) == 0 {
		configDir := filepath.Dir(GlobalConfig())
		c.Options.GlobalContextPaths = []string{
			filepath.Join(configDir, "GNOSH.md"),
			filepath.Join(configDir, "KLASH.md"),
			filepath.Join(configDir, "CRUSH.md"),
			filepath.Join(filepath.Dir(configDir), "AGENTS.md"),
		}
	}
	slices.Sort(c.Options.GlobalContextPaths)
	c.Options.GlobalContextPaths = slices.Compact(c.Options.GlobalContextPaths)

	if dataDir != "" {
		c.Options.DataDirectory = dataDir
	} else if c.Options.DataDirectory == "" {
		c.Options.DataDirectory = projectDataDir(workingDir)
	}
	c.Options.DataDirectory = filepath.Clean(filepathext.SmartJoin(workingDir, c.Options.DataDirectory))
	if c.Providers == nil {
		c.Providers = csync.NewMap[string, ProviderConfig]()
	}
	if c.Models == nil {
		c.Models = make(map[SelectedModelType]SelectedModel)
	}
	if c.RecentModels == nil {
		c.RecentModels = make(map[SelectedModelType][]SelectedModel)
	}
	if c.MCP == nil {
		c.MCP = make(map[string]MCPConfig)
	}
	// Drop orphaned OAuth token entries left behind when a user removes
	// an MCP from crush.json. See MCPConfig.isOrphanedToken.
	for name, m := range c.MCP {
		if m.isOrphanedToken() {
			delete(c.MCP, name)
		}
	}
	if c.LSP == nil {
		c.LSP = make(map[string]LSPConfig)
	}

	// Apply defaults to LSP configurations
	c.applyLSPDefaults()

	// Add the default context paths if they are not already present
	c.Options.ContextPaths = append(slices.Clone(defaultContextPaths), c.Options.ContextPaths...)

	slices.Sort(c.Options.ContextPaths)
	c.Options.ContextPaths = slices.Compact(c.Options.ContextPaths)

	// Add the default skills directories if not already present.
	for _, dir := range GlobalSkillsDirs() {
		if !slices.Contains(c.Options.SkillsPaths, dir) {
			c.Options.SkillsPaths = append(c.Options.SkillsPaths, dir)
		}
	}

	// Project specific skills dirs.
	c.Options.SkillsPaths = append(c.Options.SkillsPaths, ProjectSkillsDir(workingDir)...)

	// Only an explicit value counts: unset or empty leaves the catalog fetch
	// as the config has it, which is off unless asked for.
	if disable, ok := env.LookupBool("GNOSH_DISABLE_PROVIDER_AUTO_UPDATE"); ok {
		c.Options.DisableProviderAutoUpdate = new(disable)
	}

	if str, ok := env.LookupEnv("GNOSH_DISABLE_DEFAULT_PROVIDERS"); ok {
		c.Options.DisableDefaultProviders, _ = strconv.ParseBool(str)
	}

	// The fork's own endpoints, replacing Charm's: where metrics go, and
	// whose releases the update check reads. Each is empty by default, so
	// nothing is sent or checked until it is set.
	if v, ok := env.LookupEnv("GNOSH_METRICS_URL"); ok {
		c.Options.MetricsURL = v
	}
	if v, ok := env.LookupEnv("GNOSH_METRICS_KEY"); ok {
		c.Options.MetricsKey = v
	}
	if v, ok := env.LookupEnv("GNOSH_UPDATE_REPO"); ok {
		c.Options.UpdateRepo = v
	}

	if c.Options.Attribution == nil {
		c.Options.Attribution = &Attribution{
			TrailerStyle:  TrailerStyleAssistedBy,
			GeneratedWith: true,
		}
	} else if c.Options.Attribution.TrailerStyle == "" {
		// Migrate deprecated co_authored_by or apply default
		if c.Options.Attribution.CoAuthoredBy != nil {
			if *c.Options.Attribution.CoAuthoredBy {
				c.Options.Attribution.TrailerStyle = TrailerStyleCoAuthoredBy
			} else {
				c.Options.Attribution.TrailerStyle = TrailerStyleNone
			}
		} else {
			c.Options.Attribution.TrailerStyle = TrailerStyleAssistedBy
		}
	}

	c.Options.InitializeAs = cmp.Or(c.Options.InitializeAs, defaultInitializeAs)

	// Last, so the orphaned-OAuth-entry prune above still sees an entry
	// that carries nothing but a token as empty: stamping a default type
	// on it first would make it look like a configured stdio server.
	c.setMCPDefaults()
}

// powernapDefaults caches the powernap default LSP server catalog. The
// catalog is static and immutable for the life of the process, but
// building it (NewManager + LoadDefaults) is expensive and was previously
// repeated on every config reload. We load it once and only ever read from
// it via GetServer, so a shared instance is safe.
var (
	powernapDefaultsOnce sync.Once
	powernapDefaults     *powernapConfig.Manager
)

func lspDefaultsManager() *powernapConfig.Manager {
	powernapDefaultsOnce.Do(func() {
		m := powernapConfig.NewManager()
		// LoadDefaults only fails on malformed embedded defaults, which
		// would be a build-time bug; treat the manager as usable either
		// way so a transient error never wedges config loading.
		_ = m.LoadDefaults()
		AddExtraLSPServers(m)
		powernapDefaults = m
	})
	return powernapDefaults
}

// applyLSPDefaults applies default values from powernap to LSP configurations
func (c *Config) applyLSPDefaults() {
	// Reuse the process-wide default catalog; building it per reload was a
	// significant chunk of reload latency.
	configManager := lspDefaultsManager()

	// Apply defaults to each LSP configuration
	for name, cfg := range c.LSP {
		// Try to get defaults from powernap based on name or command name.
		base, ok := configManager.GetServer(name)
		if !ok {
			base, ok = configManager.GetServer(cfg.Command)
			if !ok {
				continue
			}
		}
		if cfg.Options == nil {
			cfg.Options = base.Settings
		}
		if cfg.InitOptions == nil {
			cfg.InitOptions = base.InitOptions
		}
		if len(cfg.FileTypes) == 0 {
			cfg.FileTypes = base.FileTypes
		}
		if len(cfg.RootMarkers) == 0 {
			cfg.RootMarkers = base.RootMarkers
		}
		cfg.Command = cmp.Or(cfg.Command, base.Command)
		if len(cfg.Args) == 0 {
			cfg.Args = base.Args
		}
		if len(cfg.Env) == 0 {
			cfg.Env = base.Environment
		}
		// Update the config in the map
		c.LSP[name] = cfg
	}
}

func (c *Config) defaultModelSelection(knownProviders []catwalk.Provider) (largeModel SelectedModel, smallModel SelectedModel, err error) {
	if len(knownProviders) == 0 && c.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := c.Providers.Get(string(p.ID))
		if !ok || providerConfig.Disable {
			continue
		}
		defaultLargeModel := c.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultLargeModel == nil {
			slog.Warn("Default large model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default large model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultLargeModel = &providerConfig.Models[0]
		}
		largeModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultLargeModel.ID,
			MaxTokens:       defaultLargeModel.DefaultMaxTokens,
			ReasoningEffort: defaultLargeModel.DefaultReasoningEffort,
		}

		defaultSmallModel := c.GetModel(string(p.ID), p.DefaultSmallModelID)
		if defaultSmallModel == nil {
			slog.Warn("Default small model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default small model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			}
			defaultSmallModel = &providerConfig.Models[0]
		}
		smallModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultSmallModel.ID,
			MaxTokens:       defaultSmallModel.DefaultMaxTokens,
			ReasoningEffort: defaultSmallModel.DefaultReasoningEffort,
		}
		return largeModel, smallModel, err
	}

	enabledProviders := c.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	providerConfig := enabledProviders[0]
	if len(providerConfig.Models) == 0 {
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return largeModel, smallModel, err
	}
	defaultLargeModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	largeModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultLargeModel.ID,
		MaxTokens: defaultLargeModel.DefaultMaxTokens,
	}
	defaultSmallModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	smallModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultSmallModel.ID,
		MaxTokens: defaultSmallModel.DefaultMaxTokens,
	}
	return largeModel, smallModel, err
}

// resolvedModels holds the result of resolving user-configured model
// selections against the provider catalog.
type resolvedModels struct {
	Large         SelectedModel
	Small         SelectedModel
	LargeFallback bool // true if Large was corrected to a default
	SmallFallback bool // true if Small was corrected to a default
}

// resolveSelectedModels validates the user's configured model selections
// against the provider catalog, falling back to defaults when a model ID is
// invalid. It is pure resolution logic: it does not mutate the store or
// touch disk. The caller assigns the results to c.Models and persists any
// fallback corrections as appropriate.
func resolveSelectedModels(cfg *Config, knownProviders []catwalk.Provider) (resolvedModels, error) {
	var result resolvedModels
	defaultLarge, defaultSmall, err := cfg.defaultModelSelection(knownProviders)
	if err != nil {
		return result, fmt.Errorf("failed to select default models: %w", err)
	}
	large, small := defaultLarge, defaultSmall

	largeModelSelected, largeModelConfigured := cfg.Models[SelectedModelTypeLarge]
	if largeModelConfigured {
		if largeModelSelected.Model != "" {
			large.Model = largeModelSelected.Model
		}
		if largeModelSelected.Provider != "" {
			large.Provider = largeModelSelected.Provider
		}
		model := cfg.GetModel(large.Provider, large.Model)
		if model == nil {
			large = defaultLarge
			result.LargeFallback = true
		} else {
			if largeModelSelected.MaxTokens > 0 {
				large.MaxTokens = largeModelSelected.MaxTokens
			} else {
				large.MaxTokens = model.DefaultMaxTokens
			}
			if largeModelSelected.ReasoningEffort != "" {
				large.ReasoningEffort = largeModelSelected.ReasoningEffort
			} else {
				large.ReasoningEffort = model.DefaultReasoningEffort
			}
			large.Think = largeModelSelected.Think
			if largeModelSelected.Temperature != nil {
				large.Temperature = largeModelSelected.Temperature
			}
			if largeModelSelected.TopP != nil {
				large.TopP = largeModelSelected.TopP
			}
			if largeModelSelected.TopK != nil {
				large.TopK = largeModelSelected.TopK
			}
			if largeModelSelected.FrequencyPenalty != nil {
				large.FrequencyPenalty = largeModelSelected.FrequencyPenalty
			}
			if largeModelSelected.PresencePenalty != nil {
				large.PresencePenalty = largeModelSelected.PresencePenalty
			}
			if largeModelSelected.ProviderOptions != nil {
				large.ProviderOptions = maps.Clone(largeModelSelected.ProviderOptions)
			}
		}
	}
	smallModelSelected, smallModelConfigured := cfg.Models[SelectedModelTypeSmall]
	if smallModelConfigured {
		if smallModelSelected.Model != "" {
			small.Model = smallModelSelected.Model
		}
		if smallModelSelected.Provider != "" {
			small.Provider = smallModelSelected.Provider
		}

		model := cfg.GetModel(small.Provider, small.Model)
		if model == nil {
			small = defaultSmall
			result.SmallFallback = true
		} else {
			if smallModelSelected.MaxTokens > 0 {
				small.MaxTokens = smallModelSelected.MaxTokens
			} else {
				small.MaxTokens = model.DefaultMaxTokens
			}
			if smallModelSelected.ReasoningEffort != "" {
				small.ReasoningEffort = smallModelSelected.ReasoningEffort
			} else {
				small.ReasoningEffort = model.DefaultReasoningEffort
			}
			if smallModelSelected.Temperature != nil {
				small.Temperature = smallModelSelected.Temperature
			}
			if smallModelSelected.TopP != nil {
				small.TopP = smallModelSelected.TopP
			}
			if smallModelSelected.TopK != nil {
				small.TopK = smallModelSelected.TopK
			}
			if smallModelSelected.FrequencyPenalty != nil {
				small.FrequencyPenalty = smallModelSelected.FrequencyPenalty
			}
			if smallModelSelected.PresencePenalty != nil {
				small.PresencePenalty = smallModelSelected.PresencePenalty
			}
			if smallModelSelected.ProviderOptions != nil {
				small.ProviderOptions = maps.Clone(smallModelSelected.ProviderOptions)
			}
			small.Think = smallModelSelected.Think
		}
	}

	// When small isn't explicitly configured and the provider isn't a
	// known built-in, use the large model as the small model. This
	// prevents two different models from being requested concurrently
	// for local/openai-compat providers.
	if !smallModelConfigured {
		isKnownProvider := false
		for _, kp := range knownProviders {
			if string(kp.ID) == small.Provider {
				isKnownProvider = true
				break
			}
		}
		if !isKnownProvider {
			slog.Warn("Using large model as small model for unknown provider", "provider", large.Provider, "model", large.Model)
			small = large
		}
	}

	result.Large = large
	result.Small = small
	return result, nil
}

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated crush.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	configPaths := globalConfigPaths()

	foundConfigs, err := fsext.LookupBounded(cwd, projectBoundary(cwd), projectConfigNames()...)
	if err != nil {
		// returns at least default configs
		return configPaths
	}

	// reverse order so last config has more priority
	slices.Reverse(foundConfigs)

	return append(configPaths, foundConfigs...)
}

// globalConfigPaths are the configs every load reads first, whatever the
// project: the system-wide ones, the user's config and its shell config, and
// the machine-owned data JSON. Only the user config directory contributes a
// crushrc; the data directory is writable machine state and must never be
// executed as Bash. Missing files are skipped when loaded.
func globalConfigPaths() []string {
	configPaths := append(slices.Clone(systemConfigPaths), GlobalConfig())
	configPaths = append(configPaths, shellConfigSiblings(GlobalConfig())...)
	return append(configPaths, GlobalConfigData())
}

// projectConfigNames are the names a project config file can have, in
// priority order within a directory. LookupBounded returns matches in
// this order, and the later reverse + merge make the earliest listed
// name win on conflict. So: .gnoshrc beats gnoshrc, both beat the JSON
// configs, and .gnosh.json beats gnosh.json; each beats its names from
// before the renames.
func projectConfigNames() []string {
	var names []string
	for _, name := range appNames {
		names = append(names, "."+name+"rc", name+"rc", "."+name+".json", name+".json")
	}
	return names
}

// checkProjectConfigOwners refuses a project config file that the lookup
// passes over because someone other than the owner of the project
// directory owns it. The lookup skips such a file so that another user
// cannot plant settings in a directory they can write to, but it skipped
// it without a word: a gnosh.json written with sudo into your own
// project, say, was ignored, and the settings in it, a disabled tool
// among them, silently did not apply.
func checkProjectConfigOwners(cwd string) error {
	foreign, err := fsext.ForeignOwnedBounded(cwd, projectBoundary(cwd), projectConfigNames()...)
	if err != nil || len(foreign) == 0 {
		// A lookup that cannot run is lookupConfigs' to handle.
		return nil
	}
	path := foreign[0]
	fileOwner, err := fsext.Owner(path)
	if err != nil {
		return nil
	}
	dirOwner, err := fsext.Owner(cwd)
	if err != nil {
		return nil
	}
	return fmt.Errorf(
		"project config %s belongs to %s, but %s belongs to %s. Gnosh reads a project config only from the owner of the project directory, so its settings would not apply. If it is meant for this project, give it to %s (chown %s %s); otherwise remove it",
		path, userName(fileOwner), cwd, userName(dirOwner), userName(dirOwner), userName(dirOwner), path,
	)
}

// userName is the login name for uid, or "uid N" when there is none.
func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return "uid " + strconv.Itoa(uid)
}

func loadFromConfigPaths(ctx context.Context, configPaths []string) (*Config, []string, error) {
	return loadFromConfigPathsGated(ctx, configPaths, nil)
}

// loadFromConfigPathsGated is loadFromConfigPaths with the project's own
// config files passed through gate: where the folder is not trusted, its
// shell configs do not run and its JSON configs keep only what
// untrustedKept lists. A nil gate uses every file in full.
func loadFromConfigPathsGated(ctx context.Context, configPaths []string, gate *projectGate) (*Config, []string, error) {
	var configs [][]byte
	var loaded []string

	// Track directories that have both crush.json and crushrc to warn
	// about potential confusion, along with the top-level keys each
	// defines so we can report conflicts.
	jsonDirKeys := make(map[string]map[string]bool)
	shDirKeys := make(map[string]map[string]bool)

	for _, path := range configPaths {
		if path == "" {
			continue
		}
		data, err := readFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("failed to open config file %s: %w", path, err)
		}
		if len(data) == 0 {
			continue
		}

		dir := filepath.Dir(path)
		held := gate.holds(path)
		if isShellConfig(path) {
			if held {
				// Running it is what trust is for.
				gate.held.addScript(path)
				continue
			}
			jsonBytes, err := shellconfig.LoadShellConfig(ctx, path, data)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load shell config %s: %w", path, err)
			}
			if len(jsonBytes) > 0 {
				if !json.Valid(jsonBytes) {
					return nil, nil, fmt.Errorf("shell config %s produced invalid JSON", path)
				}
				if jsonBytes, err = flattenHookGroups(jsonBytes); err != nil {
					return nil, nil, fmt.Errorf("shell config %s: %w", path, err)
				}
				addTopLevelKeys(shDirKeys, dir, jsonBytes)
				configs = append(configs, jsonBytes)
				loaded = append(loaded, path)
			}
		} else {
			if !json.Valid(data) {
				return nil, nil, fmt.Errorf("invalid JSON in config file %s", path)
			}
			if held {
				if data, err = gate.held.split(path, data); err != nil {
					return nil, nil, err
				}
			}
			if data, err = flattenHookGroups(data); err != nil {
				return nil, nil, fmt.Errorf("config file %s: %w", path, err)
			}
			addTopLevelKeys(jsonDirKeys, dir, data)
			configs = append(configs, data)
			loaded = append(loaded, path)
		}
	}

	// Warn if both a JSON config and a crushrc exist in the same directory
	// and define overlapping top-level keys. Disjoint coexistence is
	// intentional and not worth warning about.
	for dir, jKeys := range jsonDirKeys {
		sKeys, ok := shDirKeys[dir]
		if !ok {
			continue
		}
		var conflicts []string
		for k := range jKeys {
			if sKeys[k] {
				conflicts = append(conflicts, k)
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			slog.Warn("Found both a JSON config and a crushrc in the same directory; merging with crushrc taking precedence",
				"dir", dir, "conflicting_keys", strings.Join(conflicts, ", "))
		}
	}

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, err
	}
	return cfg, loaded, nil
}

// addTopLevelKeys records the top-level JSON keys present in data into the
// set for dir.
func addTopLevelKeys(m map[string]map[string]bool, dir string, data []byte) {
	keys := m[dir]
	if keys == nil {
		keys = make(map[string]bool)
		m[dir] = keys
	}
	gjson.ParseBytes(data).ForEach(func(key, _ gjson.Result) bool {
		keys[key.String()] = true
		return true
	})
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	data, err := jsons.Merge(configs)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func hasAWSCredentials(env env.Env) bool {
	if env.Get("AWS_BEARER_TOKEN_BEDROCK") != "" {
		return true
	}

	if env.Get("AWS_ACCESS_KEY_ID") != "" && env.Get("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}

	if env.Get("AWS_PROFILE") != "" || env.Get("AWS_DEFAULT_PROFILE") != "" {
		return true
	}

	if env.Get("AWS_REGION") != "" || env.Get("AWS_DEFAULT_REGION") != "" {
		return true
	}

	if env.Get("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" ||
		env.Get("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return true
	}

	// File-based credential discovery requires filesystem stats, so do it
	// last and skip it under test. Checking testing.Testing() before the
	// os.Stat call (rather than after, in the && tail) ensures the syscall
	// is never issued during tests, where it otherwise ran unconditionally
	// and only had its result discarded.
	if testing.Testing() {
		return false
	}
	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/credentials")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(home.Dir(), ".aws/login")); err == nil {
		return true
	}

	return false
}

// migrateDisableNotifications migrates the deprecated disable_notifications
// and notification_style fields to the unified notifications field. It checks
// both the user config (~/.config) and data config (~/.local) files. If
// disable_notifications is true, it sets notifications to "disabled" in the
// data file. If notification_style is set, it moves the value to notifications.
// Regardless of value, it removes the deprecated fields from any file that
// contains them.
func migrateDisableNotifications() {
	globalConfig := GlobalConfig()
	dataConfig := GlobalConfigData()

	var wasDisabled bool
	var styleValue string
	filesToClean := []string{}

	for _, path := range []string{globalConfig, dataConfig} {
		data, err := readFile(path)
		if err != nil {
			continue
		}
		needsClean := false
		if gjson.Get(string(data), "options.disable_notifications").Exists() {
			needsClean = true
			if gjson.Get(string(data), "options.disable_notifications").Bool() {
				wasDisabled = true
			}
		}
		if v := gjson.Get(string(data), "options.notification_style"); v.Exists() {
			needsClean = true
			if styleValue == "" {
				styleValue = v.String()
			}
		}
		if needsClean {
			filesToClean = append(filesToClean, path)
		}
	}

	if len(filesToClean) == 0 {
		return
	}

	// Determine the value to persist: notification_style takes precedence,
	// then disable_notifications: true maps to "disabled".
	migratedValue := styleValue
	if migratedValue == "" && wasDisabled {
		migratedValue = "disabled"
	}

	if migratedValue != "" {
		data, err := readFile(dataConfig)
		if err == nil {
			if !gjson.Get(string(data), "options.notifications").Exists() {
				updated, err := sjson.Set(string(data), "options.notifications", migratedValue)
				if err == nil {
					if err := atomicWriteFile(dataConfig, []byte(updated), 0o600); err != nil {
						slog.Warn("Failed to migrate to notifications field", "error", err)
					} else {
						slog.Info("Migrated notification settings to notifications field", "value", migratedValue)
					}
				}
			}
		}
	}

	// Remove deprecated fields from all files that contain them.
	for _, path := range filesToClean {
		data, err := readFile(path)
		if err != nil {
			continue
		}
		updated := string(data)
		updated, _ = sjson.Delete(updated, "options.disable_notifications")
		updated, _ = sjson.Delete(updated, "options.notification_style")
		if updated == string(data) {
			continue
		}
		if err := atomicWriteFile(path, []byte(updated), 0o600); err != nil {
			slog.Warn("Failed to write migrated config", "path", path, "error", err)
		}
	}
}

// projectDataDir is the data directory for a project in workingDir: the
// closest .gnosh, .klash or .crush within the project, so that a .gnosh
// made farther up (by a run at the repository root) cannot hide the
// sessions in a closer .klash or .crush. Where there is none, it is a new
// .gnosh in workingDir, or the older one already there: the lookup passes
// over the home directory, where they would otherwise be missed.
func projectDataDir(workingDir string) string {
	if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), appPaths("", ".", "")...); ok {
		return path
	}
	return fsext.FirstExisting(appPaths(workingDir, ".", "")...)
}

// LogFile returns the log file for logDir, a data directory's logs or a
// server's cache directory: gnosh.log, or the klash.log or crush.log from
// before the renames where only that one exists.
func LogFile(logDir string) string {
	return fsext.FirstExisting(appPaths(logDir, "", ".log")...)
}

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	if dir := env.Getenv("GNOSH_GLOBAL_CONFIG"); dir != "" {
		return configFileIn(dir)
	}
	return configFileIn(appConfigDir(home.Config()))
}

// appPaths are base joined with prefix+name+suffix for each of the
// program's names, newest first; with base empty, they are just the names.
func appPaths(base, prefix, suffix string) []string {
	paths := make([]string, len(appNames))
	for i, name := range appNames {
		paths[i] = filepath.Join(base, prefix+name+suffix)
	}
	return paths
}

// appDir is the program's directory under base: gnosh, or klash or crush
// where only that one exists.
func appDir(base string) string {
	return fsext.FirstExisting(appPaths(base, "", "")...)
}

// appConfigDir is appDir for a directory that holds a config file, the
// global config and global data directories: the one holding a config
// file wins, gnosh before klash before crush, and only where none does is
// it appDir's choice. A gnosh directory made for anything else (mkdir -p
// ~/.config/gnosh/commands) must not take over from an older one in use:
// it would hide its config, and with it the hooks, MCP servers and
// permissions, its shell config, templates, tools and skills, and in the
// data directory the OAuth tokens.
func appConfigDir(base string) string {
	for _, dir := range appPaths(base, "", "") {
		if hasConfigFile(dir) {
			return dir
		}
	}
	return appDir(base)
}

// hasConfigFile reports whether dir holds a JSON or shell config file under
// any of the program's names.
func hasConfigFile(dir string) bool {
	for _, name := range appNames {
		for _, file := range []string{name + ".json", name + "rc"} {
			if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
				return true
			}
		}
	}
	return false
}

// configFileIn is the JSON config file in dir: gnosh.json, or klash.json or
// crush.json where only that one exists.
func configFileIn(dir string) string {
	return fsext.FirstExisting(appPaths(dir, "", ".json")...)
}

// shellConfigSiblings returns the shell config paths that sit alongside a
// given JSON config path (same directory): crushrc, klashrc, then gnoshrc,
// so that the newest name wins where several exist. All are listed even
// when missing, so that creating any one is noticed. Used so global config
// locations pick up a shell config, not just JSON.
func shellConfigSiblings(jsonPath string) []string {
	paths := appPaths(filepath.Dir(jsonPath), "", "rc")
	slices.Reverse(paths) // oldest first: the later one wins
	return paths
}

// isShellConfig reports whether a config path is a shell config (gnoshrc,
// klashrc or crushrc, or a hidden one), as opposed to a JSON config.
func isShellConfig(path string) bool {
	base := filepath.Base(path)
	for _, name := range appNames {
		if base == name+"rc" || base == "."+name+"rc" {
			return true
		}
	}
	return false
}

// GlobalCacheDir returns the path to the global cache directory for the
// application.
func GlobalCacheDir() string {
	if crushCache := env.Getenv("GNOSH_CACHE_DIR"); crushCache != "" {
		return crushCache
	}
	if xdgCacheHome := os.Getenv("XDG_CACHE_HOME"); xdgCacheHome != "" {
		return appDir(xdgCacheHome)
	}
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(appDir(localAppData), "cache")
	}
	return appDir(filepath.Join(home.Dir(), ".cache"))
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	return lookupConfigs(cwd)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	if dir := env.Getenv("GNOSH_GLOBAL_DATA"); dir != "" {
		return configFileIn(dir)
	}
	if xdgDataHome := os.Getenv("XDG_DATA_HOME"); xdgDataHome != "" {
		return configFileIn(appConfigDir(xdgDataHome))
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/gnosh/`
	// for linux and macOS, it should be in `$HOME/.local/share/gnosh/`
	// (or crush, where only that one exists)
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		return configFileIn(appConfigDir(localAppData))
	}

	return configFileIn(appConfigDir(filepath.Join(home.Dir(), ".local", "share")))
}

// GlobalWorkspaceDir returns the path to the global server workspace
// directory. This directory acts as a meta-workspace for the server
// process, giving it a real workingDir so that config loading, scoped
// writes, and provider resolution behave identically to project
// workspaces.
func GlobalWorkspaceDir() string {
	return filepath.Dir(GlobalConfigData())
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isInsideWorktree() bool {
	bts, err := exec.CommandContext(
		context.Background(),
		"git", "rev-parse",
		"--is-inside-work-tree",
	).CombinedOutput()
	return err == nil && strings.TrimSpace(string(bts)) == "true"
}

// worktreeRoot returns the absolute path of the git working tree root for
// dir, or the empty string if dir is not inside a working tree (bare
// repositories, missing git binary, plain directories, or any other
// failure mode). Linked worktrees and submodules each report their own
// top-level, which is what callers want when bounding lookups.
// worktreeRootCache memoizes the git worktree root per directory. The root
// is stable for the life of the process, so we avoid re-shelling out to
// "git rev-parse" on every config reload. Keyed by the requested dir; the
// value is the resolved root ("" when dir is not in a git worktree).
var worktreeRootCache sync.Map // map[string]string

func worktreeRoot(dir string) string {
	if cached, ok := worktreeRootCache.Load(dir); ok {
		return cached.(string)
	}
	root := computeWorktreeRoot(dir)
	worktreeRootCache.Store(dir, root)
	return root
}

func computeWorktreeRoot(dir string) string {
	cmd := exec.CommandContext(
		context.Background(),
		"git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return abs
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Crush from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	if root := projectRoot(dir); root != "" {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// projectRoot is the git working tree root of the repository dir is in,
// or "" outside one. It must hold dir: a repository's config can name
// any folder its work tree (core.worktree), and a folder's own .git can
// say so, so a root elsewhere is not dir's project, and the searches it
// bounds would otherwise run past dir to the filesystem's root, or treat
// a stranger's data directory as outside the project.
func projectRoot(dir string) string {
	root := worktreeRoot(dir)
	if root == "" || !isWithin(canonicalDir(root), canonicalDir(dir)) {
		return ""
	}
	return root
}

// GlobalSkillsDirs returns the default directories for Agent Skills.
// Skills in these directories are auto-discovered and their files can be read
// without permission prompts.
func GlobalSkillsDirs() []string {
	if crushSkills := env.Getenv("GNOSH_SKILLS_DIR"); crushSkills != "" {
		return []string{crushSkills}
	}

	configDir := appConfigDir(home.Config())
	paths := []string{filepath.Join(configDir, "skills")}
	// The skills directory under the other names too, gnosh's, klash's or
	// crush's, after the one the config is in, so skills kept under any are
	// found.
	for _, name := range appNames {
		if dir := filepath.Join(home.Config(), name); dir != configDir {
			paths = append(paths, filepath.Join(dir, "skills"))
		}
	}
	paths = append(paths,
		filepath.Join(home.Config(), "agents", "skills"),
		// Per the Agent Skills spec, scan ~/.agents/skills
		filepath.Join(home.Dir(), ".agents", "skills"),
		filepath.Join(home.Dir(), ".claude", "skills"),
	)

	// On Windows, also load from app data on top of `$HOME/.config/crush`.
	// This is here mostly for backwards compatibility.
	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(
			paths,
			filepath.Join(appConfigDir(appData), "skills"),
			filepath.Join(appData, "agents", "skills"),
		)
	}

	return paths
}

// projectSkillSubdirs lists the conventional subdirectories where
// project-level skills are discovered. Shared across working-dir and
// git-root lookups to prevent drift when a new convention is added.
var projectSkillSubdirs = []string{
	".agents/skills",
	".gnosh/skills",
	".klash/skills",
	".crush/skills",
	".claude/skills",
	".cursor/skills",
}

// ProjectSkillsDir returns the default project directories for which Crush
// will look for skills. In addition to the working directory, it also
// checks the git working tree root so that monorepo-level skills are
// discovered when the user is inside a subdirectory.
// Working-directory paths come first so local skills take precedence
// over monorepo-level ones.
func ProjectSkillsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectSkillSubdirs)*2)
	for _, sub := range projectSkillSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}

	// When the working directory is inside a git repository, also look at
	// the repository root so monorepo-level .agents/skills are found.
	if root := projectRoot(workingDir); root != "" && root != workingDir {
		for _, sub := range projectSkillSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}

	return dirs
}

func isAppleTerminal() bool { return os.Getenv("TERM_PROGRAM") == "Apple_Terminal" }

// mergeWorkspaceConfig merges the workspace config file at path into
// cfg and reports whether it was merged. A file that is simply absent is
// not an error; anything else is.
//
// Both the initial load and the on-disk reload go through this, so the
// workspace file is read, checked and merged exactly one way. It used to
// be open-coded in both, and both quietly dropped the whole file when
// the merge failed: a workspace config with a valid-JSON-but-wrong-shape
// entry was discarded in silence, and the session ran on as though the
// file were not there.
func mergeWorkspaceConfig(cfg *Config, path, workingDir string) (bool, error) {
	return mergeWorkspaceConfigGated(cfg, path, workingDir, nil)
}

// mergeWorkspaceConfigGated is mergeWorkspaceConfig with the workspace
// config passed through gate: one in the project's own data directory, as
// .gnosh/gnosh.json is, is the project's, and a repository can carry one,
// so where the folder is not trusted it keeps only what untrustedKept
// lists.
func mergeWorkspaceConfigGated(cfg *Config, path, workingDir string, gate *projectGate) (bool, error) {
	wsData, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	if len(wsData) == 0 {
		return false, nil
	}
	if !json.Valid(wsData) {
		return false, fmt.Errorf("invalid JSON in config file %s", path)
	}
	if gate.holdsWorkspace(path) {
		if wsData, err = gate.held.split(path, wsData); err != nil {
			return false, err
		}
	}
	if wsData, err = flattenHookGroups(wsData); err != nil {
		return false, fmt.Errorf("config file %s: %w", path, err)
	}
	merged, err := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
	if err != nil {
		return false, fmt.Errorf("failed to merge config file %s: %w", path, err)
	}
	// Preserve the defaults setDefaults already applied.
	dataDir := cfg.Options.DataDirectory
	*cfg = *merged
	cfg.setDefaults(workingDir, dataDir)
	return true, nil
}

// setMCPDefaults applies the stdio default to any MCP server that does
// not name a connection type.
//
// The schema has always documented `default=stdio` and the crushrc
// `mcp add` builtin already writes it, so a hand-written config omitting
// the field — the shape most MCP server snippets are copied in — should
// mean the same thing rather than being refused.
func (c *Config) setMCPDefaults() {
	for name, m := range c.MCP {
		if m.Type == "" {
			m.Type = MCPStdio
			c.MCP[name] = m
		}
	}
}

// ValidateMCPs checks that every enabled MCP server names a connection
// type Crush can actually open. Without this the mistake surfaces late
// and obscurely: the server is accepted at load, and the transport layer
// fails at connect time with "unsupported mcp type: X", long after the
// line that caused it.
//
// An omitted type is not an error — setMCPDefaults has already made it
// stdio, which is what the schema documents and what `mcp add` writes.
//
// Disabled servers are skipped. A server that is switched off is not
// asked to connect, so holding up startup over its transport would be
// like validating commented-out config; it is checked when re-enabled.
func (c *Config) ValidateMCPs() error {
	for _, name := range slices.Sorted(maps.Keys(c.MCP)) {
		m := c.MCP[name]
		if m.Disabled || IsKnownMCPType(m.Type) {
			continue
		}
		known := make([]string, 0, len(knownMCPTypes))
		for _, t := range KnownMCPTypes() {
			known = append(known, string(t))
		}
		return fmt.Errorf(
			"mcp %q: unknown type %q; known types are %s",
			name, m.Type, strings.Join(known, ", "),
		)
	}
	return nil
}

// ValidateTools checks that every tool named in options.disabled_tools
// and permissions.allowed_tools is one Crush can act on.
//
// Both lists are matched by exact name against the built-in tool set, so
// a misspelling is not a near miss, it is a no-op: a disabled_tools entry
// that matches nothing leaves the tool enabled while the config says it
// is off, and an allowed_tools entry that matches nothing keeps prompting
// for a tool the user believes they pre-approved.
//
// allowed_tools additionally accepts any MCP tool name (the "mcp_"
// prefix). Those are discovered when a server connects, so at config
// time only the shape can be checked.
func (c *Config) ValidateTools() error {
	// A tool or skill named as it was before the rename (crush_logs,
	// crush-config) means the one it is now (gnosh_logs, gnosh-config).
	// Both the load and the reload come through here, so the lists are put
	// in current names first.
	if c.Options != nil {
		for i, name := range c.Options.DisabledTools {
			c.Options.DisabledTools[i] = tooldefs.Canonical(name)
		}
		for i, name := range c.Options.DisabledSkills {
			c.Options.DisabledSkills[i] = skills.CanonicalSkillName(name)
		}
	}
	if c.Permissions != nil {
		for i, name := range c.Permissions.AllowedTools {
			c.Permissions.AllowedTools[i] = tooldefs.Canonical(name)
		}
		for _, rule := range c.Permissions.Deny {
			if pattern, ok := BashRulePattern(rule); ok {
				if strings.TrimSpace(strings.TrimSuffix(pattern, ":*")) == "" {
					return fmt.Errorf("permissions.deny: %q names no command", rule)
				}
				continue
			}
			tool := tooldefs.FromClaudeCode(rule)
			if tool == "" {
				tool = tooldefs.Canonical(strings.ToLower(rule))
			}
			if !IsKnownToolName(tool) {
				return fmt.Errorf("permissions.deny: %q is neither a built-in tool nor a Bash(...) rule, the deny rules gnosh applies", rule)
			}
			// A tool's name takes the tool away, as in Claude Code.
			if c.Options == nil {
				c.Options = &Options{}
			}
			if !slices.Contains(c.Options.DisabledTools, tool) {
				c.Options.DisabledTools = append(c.Options.DisabledTools, tool)
			}
		}
		for _, rule := range c.Permissions.Allow {
			name := rule
			if pattern, ok := BashRulePattern(rule); ok {
				if strings.TrimSpace(pattern) != "*" {
					if strings.TrimSpace(strings.TrimSuffix(pattern, ":*")) == "" {
						return fmt.Errorf("permissions.allow: %q names no command", rule)
					}
					continue
				}
				// Bash(*) is Bash, as in Claude Code.
				name = "Bash"
			}
			// A tool's name lets the tool run without asking, as
			// allowed_tools does.
			tool := name
			if !tooldefs.IsMCPToolName(name) {
				tool = tooldefs.FromClaudeCode(name)
				if tool == "" {
					tool = tooldefs.Canonical(strings.ToLower(name))
				}
				if !IsKnownToolName(tool) {
					return fmt.Errorf("permissions.allow: %q is neither a tool's name nor a Bash(...) rule, the allow rules gnosh applies", rule)
				}
			}
			if !slices.Contains(c.Permissions.AllowedTools, tool) {
				c.Permissions.AllowedTools = append(c.Permissions.AllowedTools, tool)
			}
		}
	}
	if c.Options != nil {
		for _, name := range c.Options.DisabledTools {
			if IsKnownToolName(name) {
				continue
			}
			return fmt.Errorf(
				"options.disabled_tools: %q is not a built-in tool; known tools are %s",
				name, strings.Join(KnownToolNames(), ", "),
			)
		}
	}
	if c.Permissions != nil {
		for _, name := range c.Permissions.AllowedTools {
			if IsKnownToolName(name) || strings.HasPrefix(name, MCPToolPrefix) {
				continue
			}
			return fmt.Errorf(
				"permissions.allowed_tools: %q is not a built-in tool and is not an MCP tool (%s...); known tools are %s",
				name, MCPToolPrefix, strings.Join(KnownToolNames(), ", "),
			)
		}
	}
	return nil
}

// Validate runs every whole-config check and returns the first failure.
//
// Both the initial load and the on-disk reload call this one function, so
// a check added here applies at both seams. Adding a validator to only
// one of them is how a config that can't be loaded becomes a config that
// can still be reloaded into a running session.
func (c *Config) Validate() error {
	if err := c.ValidateHooks(); err != nil {
		return fmt.Errorf("invalid hook configuration: %w", err)
	}
	if err := c.ValidateModels(); err != nil {
		return fmt.Errorf("invalid model configuration: %w", err)
	}
	if err := c.ValidateMCPs(); err != nil {
		return fmt.Errorf("invalid mcp configuration: %w", err)
	}
	if err := c.ValidateTools(); err != nil {
		return fmt.Errorf("invalid tool configuration: %w", err)
	}
	if err := c.ValidateHosts(); err != nil {
		return fmt.Errorf("invalid host configuration: %w", err)
	}
	return nil
}

// ValidateModels checks that every key under `models` names a model type
// Crush actually selects. An unrecognised key would otherwise sit in the
// config looking like a configured model while Crush quietly used its
// default, so it is a load error instead.
func (c *Config) ValidateModels() error {
	for _, modelType := range slices.Sorted(maps.Keys(c.Models)) {
		if IsKnownModelType(modelType) {
			continue
		}
		known := make([]string, 0, len(knownModelTypes))
		for _, t := range KnownModelTypes() {
			known = append(known, t.String())
		}
		return fmt.Errorf(
			"models %q: unknown model type; known types are %s",
			modelType, strings.Join(known, ", "),
		)
	}
	return nil
}

// ValidateHooks checks that every configured hook is filed under an
// event Crush actually fires and has a command, and that any matcher
// compiles (see HookMatcher); it logs a warning for a tool event's
// matcher that names no tool there can be. Event names are matched exactly: an
// unrecognised name is an error rather than a key nothing ever reads,
// so a typo surfaces at load time instead of leaving a hook that looks
// configured and silently never runs. Matcher compilation used for
// matching is owned by hooks.Runner; this function only validates up
// front so the user sees config errors at load time rather than on the
// first tool call.
func (c *Config) ValidateHooks() error {
	for _, event := range slices.Sorted(maps.Keys(c.Hooks)) {
		if !IsKnownHookEvent(event) {
			return fmt.Errorf(
				"hook %q: unknown event name; known events are %s",
				event, strings.Join(KnownHookEvents(), ", "),
			)
		}
		for i, h := range c.Hooks[event] {
			if h.Command == "" {
				return fmt.Errorf("hook %s[%d]: command is required", event, i)
			}
			// "*" matches everything, as no matcher does, as in Claude Code.
			if m := strings.TrimSpace(h.Matcher); m == "" || m == "*" {
				continue
			}
			if !HookEventSupportsMatcher(event) {
				return fmt.Errorf(
					"hook %s[%d]: matcher is not supported for the %s event; there is no tool to match",
					event, i, event,
				)
			}
			m, err := CompileHookMatcher(h.Matcher)
			if err != nil {
				return fmt.Errorf("hook %s[%d]: invalid matcher regex %q: %w", event, i, h.Matcher, err)
			}
			if event == EventPreToolUse || event == EventPostToolUse || event == EventPostToolUseFailure || event == EventPermissionRequest {
				for _, name := range m.Names() {
					if !namesSomeTool(name) {
						slog.Warn("A hook's matcher names no tool, so it matches nothing; a name is matched exactly, and a regular expression such as mcp_github_.* matches a server's tools",
							"event", event, "matcher", h.Matcher, "name", name)
					}
				}
			}
		}
	}
	return nil
}

// namesSomeTool reports whether an exact matcher name can be a tool's: a
// built-in tool's own name, its name before the rename or Claude Code's
// name for it; or an MCP tool's whole name, mcp_<server>_<tool> or Claude
// Code's mcp__<server>__<tool>, which is not checked further, since MCP
// servers' tools come and go.
func namesSomeTool(name string) bool {
	if tooldefs.NamesATool(name) {
		return true
	}
	rest, ok := strings.CutPrefix(name, tooldefs.MCPPrefix)
	if !ok {
		return false
	}
	server, tool, ok := strings.Cut(strings.TrimPrefix(rest, "_"), "_")
	return ok && server != "" && strings.TrimPrefix(tool, "_") != ""
}

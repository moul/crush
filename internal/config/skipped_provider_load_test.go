package config_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// modelServer answers a llama.cpp provider's model discovery, as the AI
// box's llama-server does.
func modelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "qwen3.8-27b", "object": "model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeProviderConfig(t *testing.T, dir, baseURL string) {
	t.Helper()
	cfg := `{"options": {"disable_default_providers": true},
  "providers": {"qwen": {"type": "llamacpp", "base_url": "` + baseURL + `"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gnosh.json"), []byte(cfg), 0o644))
}

// With the default providers off, a configured provider whose model
// server does not answer no longer fails the whole load as "no custom
// providers are configured": the load goes on, and says which provider
// was left out and why, for the TUI, gnosh run and gnosh doctor to show.
func TestLoadWithOnlyAnUnreachableProviderSaysWhy(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	t.Setenv("GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	srv := modelServer(t)
	url := srv.URL
	srv.Close()
	writeProviderConfig(t, workDir, url)

	store, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
	require.NoError(t, err)
	require.False(t, store.Config().IsConfigured())
	require.Contains(t, store.Config().SkippedProvidersSummary(), "qwen: its model server did not answer")
}

// A reload while the model server is down keeps the config in use, with
// its provider, rather than drop every model from under the session.
func TestReloadWhileTheModelServerIsDownKeepsTheConfig(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	t.Setenv("GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	srv := modelServer(t)
	writeProviderConfig(t, workDir, srv.URL)

	store, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
	require.NoError(t, err)
	require.True(t, store.Config().IsConfigured())

	srv.Close()
	err = store.ReloadFromDisk(context.Background())
	require.ErrorContains(t, err, "qwen: its model server did not answer")
	_, ok := store.Config().Providers.Get("qwen")
	require.True(t, ok, "the provider in use is kept")
}

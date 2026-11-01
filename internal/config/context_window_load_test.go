package config_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// contextServer answers /v1/models as the AI box's llama-server does,
// with each model's runtime context (n_ctx), and counts the requests.
func contextServer(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [
  {"id": "qwen3.8-27b", "object": "model", "meta": {"n_ctx": 262144, "n_ctx_train": 262144}},
  {"id": "gemma-4", "object": "model", "meta": {"n_ctx": 65536}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// loadQwen loads a config whose llama.cpp provider qwen, at baseURL, lists
// models (JSON) and has discover_models set to discover ("" for unset),
// and gives the provider's models after the load.
func loadQwen(t *testing.T, baseURL, models, discover string) []int64 {
	t.Helper()
	workDir, dataDir := isolateReloadEnv(t)
	t.Setenv("GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	if discover != "" {
		discover = `, "discover_models": ` + discover
	}
	// The default providers stay on: the first load in the test binary
	// fixes the provider catalog (providerOnce), and with them off it is
	// empty, so a later test's "provider add openai" is no known provider.
	cfg := `{"providers": {"qwen": {"type": "llamacpp", "base_url": "` + baseURL + `", "models": ` + models + discover + `}}}`
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "gnosh.json"), []byte(cfg), 0o644))
	store, err := config.Load(workDir, dataDir, false, config.TrustFolderForSession())
	require.NoError(t, err)
	p, ok := store.Config().Providers.Get("qwen")
	require.True(t, ok)
	var windows []int64
	for _, m := range p.Models {
		windows = append(windows, m.ContextWindow)
	}
	return windows
}

// A model the config lists with no context window gets it from its
// llama.cpp server, as a discovered model does. Gnosh asked the server
// only when a provider listed no models, so the AI box's Qwen, named in a
// crushrc with no window, was taken for 32,768 tokens and compacted at
// 26,215 of its 262,144. The list stays as written: the server's other
// models are not added, and a window the config gives is kept.
func TestAListedModelWithNoContextWindowGetsTheServers(t *testing.T) {
	var requests atomic.Int32
	srv := contextServer(t, &requests)

	windows := loadQwen(t, srv.URL, `[{"id": "qwen3.8-27b", "name": "Qwen"}, {"id": "small", "name": "Small", "context_window": 8192}]`, "")
	require.Equal(t, []int64{262144, 8192}, windows)
	require.Equal(t, int32(1), requests.Load())

	// Every listed model has its window: the server is not asked.
	requests.Store(0)
	windows = loadQwen(t, srv.URL, `[{"id": "qwen3.8-27b", "name": "Qwen", "context_window": 100000}]`, "")
	require.Equal(t, []int64{100000}, windows)
	require.Zero(t, requests.Load())

	// discover_models false: the server is not asked.
	windows = loadQwen(t, srv.URL, `[{"id": "qwen3.8-27b", "name": "Qwen"}]`, "false")
	require.Equal(t, []int64{0}, windows)
	require.Zero(t, requests.Load())

	// discover_models true merges the server's models in, as before.
	windows = loadQwen(t, srv.URL, `[{"id": "qwen3.8-27b", "name": "Qwen"}]`, "true")
	require.Equal(t, []int64{262144, 65536}, windows)
}

// A server that does not answer leaves the listed models as they are, and
// the load goes on.
func TestAListedModelKeepsNoContextWindowWhenTheServerIsDown(t *testing.T) {
	var requests atomic.Int32
	srv := contextServer(t, &requests)
	url := srv.URL
	srv.Close()

	windows := loadQwen(t, url, `[{"id": "qwen3.8-27b", "name": "Qwen"}]`, "")
	require.Equal(t, []int64{0}, windows)
}

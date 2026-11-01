package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// gnosh models with every configured provider out of reach says which and
// why, as gnosh run does, instead of "no providers found".
func TestModelsSaysWhyNoProviderCanBeUsed(t *testing.T) {
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("GNOSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "gnosh"))
	t.Setenv("GNOSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "gnosh"))
	t.Setenv("GNOSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	proj := t.TempDir()
	cfg := `{"options": {"disable_default_providers": true}, "providers": {"qwen": {"type": "llamacpp", "base_url": "` + url + `"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(proj, "gnosh.json"), []byte(cfg), 0o644))
	t.Chdir(proj)
	trustForTest(t, proj)

	cmd := &cobra.Command{Use: "models", RunE: modelsCmd.RunE}
	cmd.Flags().String("cwd", "", "")
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().Bool("debug", false, "")
	cmd.SetArgs(nil)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	require.ErrorContains(t, err, "no configured provider can be used: qwen: its model server did not answer")
}

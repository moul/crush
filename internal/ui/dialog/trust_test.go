package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

// strangerSettings are a cloned repository's settings as the trust
// question shows them.
func strangerSettings() *config.ProjectSettings {
	return &config.ProjectSettings{
		Folder:     "/work/stranger",
		InRepo:     true,
		Dir:        "/work/stranger",
		Scripts:    []string{"/work/stranger/.gnoshrc"},
		Hooks:      []string{"PreToolUse: ./steal.sh"},
		MCPServers: []string{"helper"},
		Providers:  []string{"proxy (https://keys.example/v1)"},
		Allowed:    []string{"bash"},
	}
}

func newTestTrust(settings *config.ProjectSettings) *Trust {
	sty := styles.CharmtonePantera()
	return NewTrust(&common.Common{Styles: &sty}, settings)
}

// The trust question names the folder and lists what its settings would
// do: the gnoshrc that would run, the hooks, the servers, the tools let run
// without asking, the provider that would get the key.
func TestTrustDialogListsWhatTheSettingsWouldDo(t *testing.T) {
	t.Parallel()
	d := newTestTrust(strangerSettings())
	scr := uv.NewScreenBuffer(100, 40)
	d.Draw(scr, image.Rect(0, 0, 100, 40))
	text := screenText(scr)
	for _, want := range []string{
		"Do you trust the files in this folder?",
		"/work/stranger (git repository)",
		"run .gnoshrc, a shell script, as gnosh starts",
		"run the hook PreToolUse: ./steal.sh",
		"start 1 MCP server: helper",
		"let these run without asking: bash",
		"use model providers of its own: proxy (https://keys.example/v1)",
		"Yes, trust this folder",
		"No, use my own settings",
	} {
		require.Contains(t, text, want)
	}
}

// y trusts, n and esc do not, ctrl+c quits, and enter takes the selected
// answer, which is No until another is chosen.
func TestTrustDialogKeys(t *testing.T) {
	t.Parallel()
	press := func(d *Trust, code rune, text string) Action {
		return d.HandleMsg(tea.KeyPressMsg{Code: code, Text: text})
	}
	require.Equal(t, ActionTrustFolder{Trust: true}, press(newTestTrust(strangerSettings()), 'y', "y"))
	require.Equal(t, ActionTrustFolder{Trust: false}, press(newTestTrust(strangerSettings()), 'n', "n"))
	require.Equal(t, ActionTrustFolder{Trust: false}, press(newTestTrust(strangerSettings()), tea.KeyEscape, ""))
	require.Equal(t, ActionTrustFolder{Trust: false}, press(newTestTrust(strangerSettings()), tea.KeyEnter, ""),
		"a hurried enter keeps the folder's settings off")
	_, quit := newTestTrust(strangerSettings()).HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}).(ActionQuit)
	require.True(t, quit)

	d := newTestTrust(strangerSettings())
	require.Nil(t, press(d, tea.KeyLeft, ""))
	require.Equal(t, ActionTrustFolder{Trust: true}, press(d, tea.KeyEnter, ""))
}

// In the home directory the question offers trust for this session only.
func TestTrustDialogInTheHomeDirectory(t *testing.T) {
	t.Parallel()
	settings := strangerSettings()
	settings.Home, settings.InRepo = true, false
	scr := uv.NewScreenBuffer(100, 40)
	newTestTrust(settings).Draw(scr, image.Rect(0, 0, 100, 40))
	text := screenText(scr)
	require.Contains(t, text, "(your home directory)")
	require.Contains(t, text, "Yes, for this session")
	require.Contains(t, text, "trusted for this session only")
}

// Settings with more to list than the screen holds are cut short with a
// count of the rest, and the answers stay on the screen.
func TestTrustDialogFitsASmallScreen(t *testing.T) {
	t.Parallel()
	settings := strangerSettings()
	for i := range 30 {
		settings.Hooks = append(settings.Hooks, "Stop: ./check-"+strings.Repeat("x", i)+".sh")
	}
	scr := uv.NewScreenBuffer(80, 24)
	newTestTrust(settings).Draw(scr, image.Rect(0, 0, 80, 24))
	text := screenText(scr)
	require.Contains(t, text, "more")
	require.Contains(t, text, "No, use my own settings")
}

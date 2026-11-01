package dialog

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

// Below what the settings would do, the question says in plain words what
// could go wrong: only the harms these settings could do, the gnoshrc,
// hook and server that would run as the user first.
func TestTrustDialogSaysWhatCouldGoWrong(t *testing.T) {
	t.Parallel()
	scr := uv.NewScreenBuffer(100, 40)
	newTestTrust(strangerSettings()).Draw(scr, image.Rect(0, 0, 100, 40))
	text := screenText(scr)
	require.Less(t, strings.Index(text, "Its project settings would:"), strings.Index(text, "What could go wrong:"))
	require.Less(t, strings.Index(text, "What could go wrong:"), strings.Index(text, "No, use my own settings"))
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "•", " ").Replace(text)), " ")
	for _, danger := range strangerSettings().Dangers() {
		require.Contains(t, flat, danger)
	}
	require.Contains(t, flat, "steal your SSH and wallet keys")
	require.Contains(t, flat, "take your API key")
	require.NotContains(t, flat, "log in to the machine", "a harm the settings could not do is not listed")
}

// Every line of the question stays inside the dialog: a bullet is wrapped
// to the room the dialog has, bullet and padding counted, so no line is
// wrapped a second time with a word left on a line of its own.
func TestTrustDialogWrapsInsideItsBorder(t *testing.T) {
	t.Parallel()
	d := newTestTrust(strangerSettings())
	for _, width := range []int{30, 56, 76} {
		for _, b := range d.bullets(append(strangerSettings().Lines(), strangerSettings().Dangers()...), width) {
			for _, line := range strings.Split(b, "\n") {
				require.LessOrEqual(t, lipgloss.Width(line), width, "%q", line)
			}
		}
	}
	for _, width := range []int{60, 80, 100} {
		scr := uv.NewScreenBuffer(width, 50)
		newTestTrust(strangerSettings()).Draw(scr, image.Rect(0, 0, width, 50))
		flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "•", " ").Replace(screenText(scr))), " ")
		for _, danger := range strangerSettings().Dangers() {
			require.Contains(t, flat, danger, "width %d", width)
		}
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
	require.Contains(t, text, "What could go wrong:", "what could go wrong keeps its place before the long list")
	require.Contains(t, text, "would run as you")
}

// How the lines are shared out when they do not all fit: what could go
// wrong first, the list keeping a bullet and its count, and on a screen
// too small for the heading and a line, the list alone.
func TestTrustDialogShareHeight(t *testing.T) {
	t.Parallel()
	does, dangers := shareHeight(20, 5, 9)
	require.Equal(t, [2]int{5, 9}, [2]int{does, dangers}, "both fit")
	does, dangers = shareHeight(10, 30, 9)
	require.Equal(t, [2]int{2, 8}, [2]int{does, dangers})
	does, dangers = shareHeight(10, 1, 12)
	require.Equal(t, [2]int{1, 9}, [2]int{does, dangers})
	does, dangers = shareHeight(4, 30, 9)
	require.Equal(t, [2]int{4, 0}, [2]int{does, dangers}, "no room for the heading and a line")
	does, dangers = shareHeight(10, 30, 0)
	require.Equal(t, [2]int{10, 0}, [2]int{does, dangers}, "nothing could go wrong")
}

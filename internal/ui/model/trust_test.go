package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func heldForTest() *config.ProjectSettings {
	return &config.ProjectSettings{
		Folder:  "/work/stranger",
		Dir:     "/work/stranger",
		Scripts: []string{"/work/stranger/.gnoshrc"},
		Hooks:   []string{"Stop: ./x.sh"},
	}
}

// The question asked before the TUI starts shows the trust dialog on its
// own and ends with the answer: yes, no, or quit.
func TestTrustPromptAnswers(t *testing.T) {
	t.Parallel()
	sty := styles.CharmtonePantera()
	for _, tc := range []struct {
		key  tea.KeyPressMsg
		want TrustAnswer
	}{
		{tea.KeyPressMsg{Code: 'y', Text: "y"}, TrustYes},
		{tea.KeyPressMsg{Code: 'n', Text: "n"}, TrustNo},
		{tea.KeyPressMsg{Code: tea.KeyEnter}, TrustNo},
		{tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, TrustQuit},
	} {
		m := newTrustPrompt(&common.Common{Styles: &sty}, heldForTest())
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		require.Contains(t, ansi.Strip(m.View().Content), "Do you trust the files in this folder?")
		_, cmd := m.Update(tc.key)
		require.NotNil(t, cmd, tc.key.String())
		_, quits := cmd().(tea.QuitMsg)
		require.True(t, quits, tc.key.String())
		require.Equal(t, tc.want, m.answer, tc.key.String())
	}
}

// A session that started without its folder's settings says so, what they
// would do, and how to trust the folder.
func TestUntrustedNotice(t *testing.T) {
	t.Parallel()
	u := newTestUI()
	u.com.Workspace = safeModeWorkspace{cfg: &config.Config{HeldProject: heldForTest()}}
	info, ok := u.untrustedNotice()().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeWarn, info.Type)
	require.Equal(t, "Folder not trusted: its settings are off until gnosh trust /work/stranger and a restart (runs .gnoshrc; 1 hook).", info.Msg)

	u.com.Workspace = safeModeWorkspace{cfg: &config.Config{}}
	require.Nil(t, u.untrustedNotice())
}

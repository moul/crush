package model

import (
	"context"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/version"
	uv "github.com/charmbracelet/ultraviolet"
)

// TrustAnswer is the answer to the folder trust question.
type TrustAnswer int

const (
	// TrustNo starts the session with the user's own settings only.
	TrustNo TrustAnswer = iota
	// TrustYes trusts the folder's project settings.
	TrustYes
	// TrustQuit quits gnosh without starting a session.
	TrustQuit
)

// AskFolderTrust asks whether to trust a folder's project settings, on a
// screen of its own before the session's TUI starts, since the answer
// decides what config the session loads: settings say what they would do,
// and theme is the user's theme ("" for the default).
func AskFolderTrust(ctx context.Context, settings *config.ProjectSettings, theme string) (TrustAnswer, error) {
	styles := common.LoadThemeStyles(theme)
	com := &common.Common{
		Styles:    &styles,
		TrueColor: colorprofile.Detect(os.Stdout, os.Environ()) == colorprofile.TrueColor,
	}
	m := newTrustPrompt(com, settings)
	if _, err := tea.NewProgram(m, tea.WithContext(ctx)).Run(); err != nil {
		return TrustQuit, err
	}
	return m.answer, nil
}

// trustPrompt is the program that asks the folder trust question: the
// trust dialog, alone on the screen.
type trustPrompt struct {
	com           *common.Common
	dialog        *dialog.Trust
	dir           string
	width, height int
	answer        TrustAnswer
}

func newTrustPrompt(com *common.Common, settings *config.ProjectSettings) *trustPrompt {
	return &trustPrompt{
		com:    com,
		dialog: dialog.NewTrust(com, settings),
		dir:    settings.Dir,
		answer: TrustQuit,
	}
}

// Init implements [tea.Model].
func (m *trustPrompt) Init() tea.Cmd {
	return nil
}

// Update implements [tea.Model].
func (m *trustPrompt) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		return m, nil
	}
	switch a := m.dialog.HandleMsg(msg).(type) {
	case dialog.ActionTrustFolder:
		m.answer = TrustNo
		if a.Trust {
			m.answer = TrustYes
		}
		return m, tea.Quit
	case dialog.ActionQuit:
		m.answer = TrustQuit
		return m, tea.Quit
	}
	return m, nil
}

// View implements [tea.Model].
func (m *trustPrompt) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.BackgroundColor = m.com.Styles.Background
	v.WindowTitle = version.Command + " " + config.ShowPath(m.dir)
	if m.width <= 0 || m.height <= 0 {
		return v
	}
	canvas := uv.NewScreenBuffer(m.width, m.height)
	m.dialog.Draw(canvas, canvas.Bounds())
	v.Content = canvas.Render()
	return v
}

// untrustedNotice says, as a session starts in a folder whose project
// settings are held back for want of trust, that they are off and how to
// turn them on.
func (m *UI) untrustedNotice() tea.Cmd {
	cfg := m.com.Config()
	if cfg == nil || !cfg.HeldProject.Holds() {
		return nil
	}
	return util.CmdHandler(util.InfoMsg{
		Type: util.InfoTypeWarn,
		Msg:  UntrustedNotice(cfg.HeldProject),
		TTL:  20 * time.Second,
	})
}

// UntrustedNotice is the line that says a folder's project settings are
// off because it is not trusted, how to trust it, and what they would do,
// in that order: the status bar cuts a long line at its end.
func UntrustedNotice(held *config.ProjectSettings) string {
	return "Folder not trusted: its settings are off until " + config.TrustCommand(held.Folder) +
		" and a restart (" + held.Brief() + ")."
}

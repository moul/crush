package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// TrustID is the identifier for the folder trust dialog.
const TrustID = "trust"

// TrustTitle is the folder trust question, as Claude Code asks it.
const TrustTitle = "Do you trust the files in this folder?"

// trustMaxWidth is the widest the trust dialog gets: wider than most
// dialogs, for the paths and commands it lists.
const trustMaxWidth = 80

// ActionTrustFolder is the answer to the folder trust question: Trust is
// set when the user trusts the folder's project settings.
type ActionTrustFolder struct {
	Trust bool
}

// Trust asks, before a session starts in a folder whose project settings
// are not trusted yet, whether to use them. It lists what they would do
// (the shell config that would run, the hooks, the MCP and LSP servers, the
// tools let run without asking and the rest) and then, in plain words, what
// could go wrong if whoever wrote them meant harm. No is selected at first,
// so a hurried enter keeps the folder's settings off.
type Trust struct {
	com        *common.Common
	settings   *config.ProjectSettings
	selectedNo bool
	help       help.Model
	keyMap     struct {
		Choose,
		Confirm,
		Yes,
		No,
		Quit key.Binding
	}
}

var _ Dialog = (*Trust)(nil)

// NewTrust creates the folder trust dialog for settings, what the folder's
// project settings would do.
func NewTrust(com *common.Common, settings *config.ProjectSettings) *Trust {
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	d := &Trust{
		com:        com,
		settings:   settings,
		selectedNo: true,
		help:       h,
	}
	d.keyMap.Choose = key.NewBinding(
		key.WithKeys("left", "right", "tab", "h", "l"),
		key.WithHelp("←/→", "choose"),
	)
	d.keyMap.Confirm = key.NewBinding(
		key.WithKeys("enter", " ", "space"),
		key.WithHelp("enter", "confirm"),
	)
	d.keyMap.Yes = key.NewBinding(
		key.WithKeys("y", "Y"),
		key.WithHelp("y", "trust"),
	)
	d.keyMap.No = key.NewBinding(
		key.WithKeys("n", "N", "esc", "alt+esc"),
		key.WithHelp("n/esc", "don't trust"),
	)
	d.keyMap.Quit = key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "quit"),
	)
	return d
}

// ID implements [Dialog].
func (*Trust) ID() string {
	return TrustID
}

// HandleMsg implements [Dialog].
func (d *Trust) HandleMsg(msg tea.Msg) Action {
	press, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(press, d.keyMap.Quit):
		return ActionQuit{}
	case key.Matches(press, d.keyMap.Choose):
		d.selectedNo = !d.selectedNo
	case key.Matches(press, d.keyMap.Confirm):
		return ActionTrustFolder{Trust: !d.selectedNo}
	case key.Matches(press, d.keyMap.Yes):
		return ActionTrustFolder{Trust: true}
	case key.Matches(press, d.keyMap.No):
		return ActionTrustFolder{Trust: false}
	}
	return nil
}

// Draw implements [Dialog].
func (d *Trust) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	DrawCenter(scr, area, d.View(area.Dx(), area.Dy()))
	return nil
}

// View renders the dialog to fit in width by height cells.
func (d *Trust) View(width, height int) string {
	t := d.com.Styles
	const padding = 1
	width = max(0, min(trustMaxWidth, width))
	dialogStyle := t.Dialog.View.Width(width).Padding(0, padding)
	contentWidth := max(0, width-dialogStyle.GetHorizontalFrameSize())

	title := t.Dialog.Title.Render(common.DialogTitle(t, TrustTitle,
		contentWidth-t.Dialog.Title.GetHorizontalFrameSize(),
		t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor))

	s := d.settings
	head := []string{title, "", d.keyValue("Folder", d.folderLine(), contentWidth)}
	if s.Dir != "" && s.Dir != s.Folder {
		head = append(head, d.keyValue("Started in", config.ShowPath(s.Dir), contentWidth))
	}
	head = append(head, "", t.Dialog.PrimaryText.Render("Its project settings would:"))

	note := "Trust it only if you wrote these settings or have read them. " +
		"No starts gnosh with your own settings only, and it asks again next time."
	if s.Home {
		note = "Your home directory is trusted for this session only: " +
			"gnosh asks each time it starts here. No starts it with your own settings only."
	}
	tail := []string{
		"",
		t.Dialog.SecondaryText.Width(contentWidth).Render(note),
		"",
		d.renderButtons(contentWidth),
		"",
		shortHelpLine(&d.help, d.ShortHelp(), contentWidth),
	}

	fixed := lipgloss.Height(strings.Join(head, "\n")) + lipgloss.Height(strings.Join(tail, "\n")) +
		dialogStyle.GetVerticalFrameSize()
	does := d.bullets(s.Lines(), contentWidth)
	dangers := d.bullets(s.Dangers(), contentWidth)
	dangersHeight := 0
	if len(dangers) > 0 {
		dangersHeight = len(dangersHead) + bulletsHeight(dangers)
	}
	listMax, dangersMax := shareHeight(height-fixed, bulletsHeight(does), dangersHeight)

	parts := append(head, d.fit(does, listMax))
	if dangersMax > 0 {
		heading := t.Dialog.PrimaryText.Foreground(t.Dialog.TitleError.GetForeground()).Bold(true)
		parts = append(parts, dangersHead[0], heading.Render(dangersHead[1]), d.fit(dangers, dangersMax-len(dangersHead)))
	}
	parts = append(parts, tail...)
	return dialogStyle.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// dangersHead are the lines above the dangers: a blank one and the
// heading.
var dangersHead = [2]string{"", "What could go wrong:"}

// shareHeight splits avail lines between what the settings would do, which
// wants does lines, and what could go wrong, which wants dangers: each all
// it wants when both fit, and otherwise what could go wrong first, while
// the list keeps two lines (a bullet and the count of the rest), since the
// first danger names what would run. On a screen with no room for the
// heading and a line under it, what could go wrong gets none (0).
func shareHeight(avail, does, dangers int) (doesMax, dangersMax int) {
	if does+dangers <= avail {
		return does, dangers
	}
	dangersMax = min(dangers, avail-min(does, 2))
	if dangersMax < len(dangersHead)+1 {
		return max(1, avail), 0
	}
	return max(1, avail-dangersMax), dangersMax
}

// folderLine names the folder and says what kind it is.
func (d *Trust) folderLine() string {
	s := d.settings
	line := config.ShowPath(s.Folder)
	switch {
	case s.Home:
		line += " (your home directory)"
	case s.InRepo:
		line += " (git repository)"
	}
	return line
}

// bullets renders lines as bullets, each wrapped to fit in width.
func (d *Trust) bullets(lines []string, width int) []string {
	style := d.com.Styles.Dialog.PrimaryText
	// The bullet takes two cells, and the style pads a cell each side.
	wrapAt := max(1, width-2-style.GetHorizontalFrameSize())
	var bullets []string
	for _, line := range lines {
		wrapped := strings.Split(ansi.Wrap(line, wrapAt, ""), "\n")
		for i := range wrapped {
			prefix := "  "
			if i == 0 {
				prefix = "• "
			}
			wrapped[i] = style.Render(prefix + wrapped[i])
		}
		bullets = append(bullets, strings.Join(wrapped, "\n"))
	}
	return bullets
}

// bulletsHeight is how many lines bullets take.
func bulletsHeight(bullets []string) int {
	h := 0
	for _, b := range bullets {
		h += lipgloss.Height(b)
	}
	return h
}

// fit is bullets in at most maxHeight lines; what does not fit is counted
// on a last line.
func (d *Trust) fit(bullets []string, maxHeight int) string {
	style := d.com.Styles.Dialog.PrimaryText
	maxHeight = max(1, maxHeight)
	var shown []string
	used := 0
	for i, b := range bullets {
		h := lipgloss.Height(b)
		rest := len(bullets) - i - 1
		room := maxHeight - used
		if rest > 0 {
			room-- // for the line that counts the rest
		}
		if h > room {
			shown = append(shown, style.Render(fmt.Sprintf("• … and %d more", len(bullets)-i)))
			break
		}
		shown = append(shown, b)
		used += h
	}
	return strings.Join(shown, "\n")
}

// keyValue renders a row of the folder block, as the permissions dialog
// renders its rows.
func (d *Trust) keyValue(key, value string, width int) string {
	t := d.com.Styles
	keyStr := t.Dialog.Permissions.KeyText.Render(key)
	valueStr := t.Dialog.Permissions.ValueText.Width(max(0, width-lipgloss.Width(keyStr)-1)).Render(" " + value)
	return lipgloss.JoinHorizontal(lipgloss.Left, keyStr, valueStr)
}

// renderButtons renders the two answers, side by side where they fit.
func (d *Trust) renderButtons(width int) string {
	yes := "Yes, trust this folder"
	if d.settings.Home {
		yes = "Yes, for this session"
	}
	buttons := []common.ButtonOpts{
		{Text: yes, UnderlineIndex: 0, Selected: !d.selectedNo},
		{Text: "No, use my own settings", UnderlineIndex: 0, Selected: d.selectedNo},
	}
	content := common.ButtonGroup(d.com.Styles, buttons, "  ")
	if lipgloss.Width(content) > width {
		content = common.ButtonGroup(d.com.Styles, buttons, "\n")
	}
	return lipgloss.NewStyle().Width(width).Align(lipgloss.Center).Render(content)
}

// ShortHelp implements [help.KeyMap].
func (d *Trust) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Choose, d.keyMap.Confirm, d.keyMap.Yes, d.keyMap.No, d.keyMap.Quit}
}

// FullHelp implements [help.KeyMap].
func (d *Trust) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

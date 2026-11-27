package shelltool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gnosh-ai/gnosh/internal/core"
)

// This file is the Monitor tool (spec/cc/tools.md 16). It runs a command in
// the background, as Bash does with run_in_background, and passes on what
// the command prints as it comes: its lines go to Options.OnMonitor, a few
// at a time, until the command ends, TaskStop stops it, or the time the
// call gave it runs out. The news of its end follows the last of them,
// through Options.OnEnd as for any background command. The permission
// rules judge the command as they judge the same command in a Bash call.
//
// What the session takes, it takes at its own pace: OnMonitor may answer
// that it cannot take lines yet (the last ones have not been read), and
// the watcher then keeps them, with what comes after, and offers them
// again a moment later. A model that is slow to read gets one note with
// many lines rather than a pile of notes. What does not fit in one note is
// counted and left in the output file.
//
// Claude Code's Monitor can also watch a WebSocket (ws). gnosh has no
// WebSocket client, so its Monitor watches commands only.

const (
	// monitorDefault is how long a watched command runs when the call
	// gives no time, and monitorMost the longest a call may give it.
	monitorDefault = 5 * time.Minute
	monitorMost    = 30 * time.Minute
	// monitorEvery is how often the lines that wait are offered.
	monitorEvery = time.Second
	// monitorLines and monitorBytes bound one note; monitorLineBytes
	// bounds one line, which is cut there.
	monitorLines     = 100
	monitorBytes     = 8 << 10
	monitorLineBytes = 1000
)

// Watched is what a command that Monitor watches has printed since it was
// last passed on, as Options.OnMonitor gets it.
type Watched struct {
	// ID is the command's background id, and Description what the call
	// said it watches.
	ID, Description string
	// Lines are the lines it printed, without their newlines. Blank lines
	// are left out.
	Lines []string
	// Left is how many more lines it printed that did not fit; they are in
	// OutputFile.
	Left int
	// OutputFile is the file that collects everything it prints.
	OutputFile string
	// Last says the command has ended: these are its last lines, and the
	// news of its end comes next. A last offer must be taken.
	Last bool
}

// monitorArgNames are the arguments Monitor takes.
var monitorArgNames = []string{argDescription, argCommand, argTimeoutMs}

// monitorTool is the Monitor tool.
type monitorTool struct{ m *Manager }

// Spec says what the model is told about the tool.
func (t monitorTool) Spec() core.ToolSpec { return t.m.monitorSpec() }

// Describe says what a call would run, for the permission rules, which
// judge it as the same command in a Bash call.
func (t monitorTool) Describe(call core.ToolCall, env *core.ToolEnv) (core.Action, error) {
	a, err := t.m.monitorArgs(call.Input)
	if err == nil {
		err = t.m.rules(a, env, false)
	}
	if a.command == "" {
		return core.Action{}, err
	}
	return core.Action{Kind: core.ActionExecute, Command: a.command, Summary: a.summary()}, err
}

// Run starts the command in the background and returns at once.
func (t monitorTool) Run(ctx context.Context, call core.ToolCall, env *core.ToolEnv) (core.ToolResult, error) {
	m := t.m
	a, err := m.monitorArgs(call.Input)
	if err == nil {
		err = m.rules(a, env, false)
	}
	if err != nil {
		return core.ErrorResult(err.Error()), nil
	}
	if env == nil || !filepath.IsAbs(env.Cwd) {
		return core.ToolResult{}, errors.New("shelltool: the tool was given no working directory to run the command in")
	}
	if ctx.Err() != nil {
		return core.ErrorResult("The command was not started: the call was cancelled first."), nil
	}
	j, err := m.start(a, env, call.ID)
	if err != nil {
		var no refusal
		if errors.As(err, &no) {
			return core.ErrorResult(string(no)), nil
		}
		return core.ToolResult{}, err
	}
	id := j.taskID()
	text := fmt.Sprintf("%s %s watches: %s. What the command prints comes to you as notices from gnosh, a few lines at a time, until it ends, %s stops it (task_id %s), or %s pass. All of it is also in %s. Go on with other work, or end your reply to wait for the lines.",
		MonitorName, id, a.summary(), StopName, id, seconds(a.limit), j.out.path)
	if a.asked > 0 {
		text += fmt.Sprintf(" (%s asked for %s; the most a %s call may have is %s.)", argTimeoutMs, seconds(a.asked), MonitorName, seconds(a.limit))
	}
	res := core.TextResult(text)
	res.Meta = map[string]any{"taskId": id, "timeoutMs": a.limit.Milliseconds(), metaOutputFile: j.out.path}
	return res, nil
}

// monitorArgs reads and checks the arguments of a Monitor call, as a Bash
// call in the background that is watched. An error is worded for the
// model.
func (m *Manager) monitorArgs(raw json.RawMessage) (bashArgs, error) {
	a := bashArgs{background: true, watch: true}
	fields, err := objectFields(raw)
	if err != nil {
		return a, err
	}
	if _, ok := present(fields, argWS); ok {
		return a, fmt.Errorf("gnosh's %s watches a command and cannot watch a WebSocket (%s). Give a %s that prints what you want to watch, such as one that reads the socket with a program that is installed", MonitorName, argWS, argCommand)
	}
	command, ok, err := stringField(fields, argCommand)
	switch {
	case err != nil:
		return a, err
	case !ok:
		return a, fmt.Errorf("%s is missing: give the shell command whose output to watch", argCommand)
	case strings.TrimSpace(command) == "":
		return a, fmt.Errorf("%s is empty: give the shell command whose output to watch", argCommand)
	case strings.IndexByte(command, 0) >= 0:
		return a, fmt.Errorf("%s holds a NUL byte, which a shell command cannot", argCommand)
	}
	a.command = command
	if a.description, _, err = stringField(fields, argDescription); err != nil {
		return a, err
	}
	asked, err := millisField(fields, argTimeoutMs)
	if err != nil {
		return a, err
	}
	for name := range fields {
		if !slices.Contains(monitorArgNames, name) {
			return a, fmt.Errorf("%q is not an argument of %s, so nothing was started. %s takes: %s", name, MonitorName, MonitorName, strings.Join(monitorArgNames, ", "))
		}
	}
	a.limit = asked
	if a.limit == 0 {
		a.limit = monitorDefault
	}
	if a.limit > monitorMost {
		a.limit, a.asked = monitorMost, a.limit
	}
	// (A Monitor's command runs in the sandbox as a Bash command does, and
	// has no way to ask out of it.)
	a.sandboxed = m.sandboxes(a.command, nil, false)
	return a, nil
}

// monitorSpec describes the Monitor tool to the model.
func (m *Manager) monitorSpec() core.ToolSpec {
	text := fmt.Sprintf(`Runs a command in the background and passes on what it prints as it comes: each line reaches you as a notice from gnosh, a few lines at a time, while you work or wait. Use it to watch for something: a line in a log, the end of each test, a server that is up.

- Every line it prints is something you must read, so make it print only what you need: filter with grep --line-buffered (tail -f app.log | grep --line-buffered -E 'ERROR|ready'), or write a loop that checks for a change and prints one line when it sees it.
- It runs until the command ends, %[1]s stops it, or %[2]s milliseconds pass: %[3]d by default, at most %[4]d.
- When the command ends you are told how, after its last lines.
- To be told only that a command ended, use %[5]s with %[6]s instead.
- The command is allowed or refused exactly as the same command in %[5]s would be.`,
		StopName, argTimeoutMs, monitorDefault.Milliseconds(), monitorMost.Milliseconds(), BashName, argBackground)
	return core.ToolSpec{
		Name:        MonitorName,
		Description: text,
		InputSchema: schema([]string{argDescription, argCommand},
			param{argDescription, "string", "What it watches for, in a few plain words, for the person watching and for each notice."},
			param{argCommand, "string", "The shell command whose output to watch, exactly as you would type it."},
			param{argTimeoutMs, "number", fmt.Sprintf("How long it may run, in milliseconds. Default %d, at most %d.", monitorDefault.Milliseconds(), monitorMost.Milliseconds())},
		),
	}
}

// watcher passes on what a watched command prints. The job's sink hands it
// whole lines as they come (take); its goroutine offers them every
// monitorEvery, and once more after the command has ended (close).
type watcher struct {
	j  *job
	fn func(Watched) bool

	mu    sync.Mutex
	lines []string
	size  int
	left  int
	// part is the start of a line whose end has not come: the sink hands
	// over a line that waits a moment as far as it has come.
	part string

	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// newWatcher starts the watcher of j, which offers its lines to fn.
func newWatcher(j *job, fn func(Watched) bool) *watcher {
	w := &watcher{j: j, fn: fn, stop: make(chan struct{}), done: make(chan struct{})}
	j.m.wg.Go(w.run)
	return w
}

// take is the job's sink's listener: it keeps the whole lines of a piece
// of output, and the start of one whose end has not come yet, as far as
// they fit; once one does not fit, it counts those that follow, so that
// what is passed on has no gap in it.
func (w *watcher) take(p core.Progress) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Stream == "status" {
		w.keep("[" + strings.TrimSpace(p.Text) + "]")
		return
	}
	text := w.part + p.Text
	end := strings.LastIndexByte(text, '\n')
	w.part = text[end+1:]
	if end >= 0 {
		// The whole lines first: the piece they came in may end in the
		// middle of the line that follows them.
		for _, line := range strings.Split(text[:end], "\n") {
			w.keep(line)
		}
	}
	if len(w.part) > monitorLineBytes {
		// A line that never ends is passed on as far as it has come.
		w.keep(w.part)
		w.part = ""
	}
}

// keep takes one line, as far as it fits. It is called with mu held.
func (w *watcher) keep(line string) {
	line = strings.TrimRight(line, " \t\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	if len(line) > monitorLineBytes {
		cut := monitorLineBytes
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + " [... the rest of this line is in the output file]"
	}
	if w.left > 0 || len(w.lines) >= monitorLines || w.size+len(line) > monitorBytes {
		w.left++
		return
	}
	w.lines = append(w.lines, line)
	w.size += len(line) + 1
}

// run offers the lines that wait every monitorEvery, until close.
func (w *watcher) run() {
	defer close(w.done)
	tick := time.NewTicker(monitorEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			w.offer(false)
		case <-w.stop:
			w.offer(true)
			return
		}
	}
}

// offer hands the lines that wait to fn. What fn does not take stays,
// and what comes after joins it. The last offer has the start of a line
// whose end never came as well.
func (w *watcher) offer(last bool) {
	w.mu.Lock()
	if last && w.part != "" {
		w.keep(w.part)
		w.part = ""
	}
	lines, left := w.lines, w.left
	w.mu.Unlock()
	if len(lines) == 0 && left == 0 {
		return
	}
	news := Watched{ID: w.j.taskID(), Description: w.j.description, Lines: slices.Clone(lines), Left: left, OutputFile: w.j.out.path, Last: last}
	if !w.fn(news) && !last {
		return
	}
	w.mu.Lock()
	// What came in while fn ran stays for the next offer.
	w.lines = append(w.lines[:0], w.lines[len(lines):]...)
	w.left -= left
	w.size = 0
	for _, l := range w.lines {
		w.size += len(l) + 1
	}
	w.mu.Unlock()
}

// close offers the last lines and stops the goroutine. It returns once
// they have been passed on.
func (w *watcher) close() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

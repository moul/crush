package shelltool

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnosh-ai/gnosh/internal/core"
)

// watches collects what Options.OnMonitor is offered and the news of ends,
// in the one order they come in. While refuse is set it turns down every
// offer it may.
type watches struct {
	mu     sync.Mutex
	events []string
	offers []Watched
	refuse atomic.Bool
	news   chan struct{}
}

func newWatches() *watches { return &watches{news: make(chan struct{}, 10_000)} }

func (w *watches) offer(x Watched) bool {
	if w.refuse.Load() && !x.Last {
		return false
	}
	w.mu.Lock()
	w.events = append(w.events, fmt.Sprintf("lines %s: %s (left %d, last %v)", x.ID, strings.Join(x.Lines, " / "), x.Left, x.Last))
	w.offers = append(w.offers, x)
	w.mu.Unlock()
	w.news <- struct{}{}
	return true
}

func (w *watches) end(e Ended) {
	w.mu.Lock()
	w.events = append(w.events, "end "+e.ID+" "+e.Reason)
	w.mu.Unlock()
	w.news <- struct{}{}
}

func (w *watches) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

// until waits for what has come to satisfy cond.
func (w *watches) until(t *testing.T, cond func([]string) bool) []string {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if got := w.all(); cond(got) {
			return got
		}
		select {
		case <-w.news:
		case <-deadline:
			t.Fatalf("gave up waiting; what came: %q", w.all())
		}
	}
}

// ended reports whether the news of an end has come.
func ended(ev []string) bool {
	return len(ev) > 0 && strings.HasPrefix(ev[len(ev)-1], "end ")
}

// watch starts a Monitor call and returns its task id.
func watch(t *testing.T, m *Manager, env *core.ToolEnv, a args) (string, core.ToolResult) {
	t.Helper()
	res := run(t, t.Context(), m, MonitorName, env, a)
	if res.IsError {
		t.Fatalf("Monitor %v: %s", a, core.TextOf(res.Content))
	}
	id, _ := res.Meta["taskId"].(string)
	return id, res
}

// What a watched command prints is passed on in order, a few lines at a
// time, without its blank lines, and the news of its end comes after the
// last of it. The call itself returns at once and says how to stop it.
func TestMonitorPassesLinesOn(t *testing.T) {
	t.Parallel()
	w := newWatches()
	m := newManager(t, Options{OnEnd: w.end, OnMonitor: w.offer})
	env := newEnv(t)
	id, res := watch(t, m, env, args{argDescription: "count to three", argCommand: "for i in 1 2 3; do echo line $i; echo; sleep 0.3; done; printf 'no newline'"})
	text := core.TextOf(res.Content)
	for _, want := range []string{"Monitor " + id + " watches: count to three", StopName + " stops it (task_id " + id + ")", "300 s pass", res.Meta[metaOutputFile].(string)} {
		if !strings.Contains(text, want) {
			t.Errorf("the result does not say %q:\n%s", want, text)
		}
	}
	if res.Meta["timeoutMs"] != int64(300_000) {
		t.Errorf("timeoutMs = %v", res.Meta["timeoutMs"])
	}
	got := w.until(t, ended)
	all := strings.Join(got, "\n")
	last := -1
	for _, want := range []string{"line 1", "line 2", "line 3", "no newline", "end " + id + " " + ReasonExited} {
		at := strings.Index(all, want)
		if at <= last {
			t.Errorf("%q came out of order, or not at all:\n%s", want, all)
		}
		last = at
	}
	if strings.Contains(all, " /  / ") || strings.Contains(all, ":  (") {
		t.Errorf("a blank line was passed on:\n%s", all)
	}
	for _, task := range m.Tasks() {
		if task.ID == id && !task.Monitor {
			t.Errorf("the task does not say it is a Monitor's: %+v", task)
		}
	}
}

// Lines the session does not take are kept and offered again with what
// came after them, in one offer.
func TestMonitorKeepsWhatIsNotTaken(t *testing.T) {
	t.Parallel()
	w := newWatches()
	w.refuse.Store(true)
	m := newManager(t, Options{OnEnd: w.end, OnMonitor: w.offer})
	env := newEnv(t)
	id, _ := watch(t, m, env, args{argDescription: "two lines", argCommand: "echo one; sleep 1.2; echo two; sleep 30"})
	time.Sleep(2500 * time.Millisecond)
	if got := w.all(); len(got) != 0 {
		t.Errorf("lines were passed on while they were turned down: %q", got)
	}
	w.refuse.Store(false)
	got := w.until(t, func(ev []string) bool { return len(ev) > 0 })
	if !strings.Contains(got[0], "one / two") {
		t.Errorf("the first offer taken = %q, want both lines in it", got[0])
	}
	// Stopping it does not speak of output not read: its lines came as
	// notes.
	stop := core.TextOf(run(t, t.Context(), m, StopName, env, args{argTaskID: id}).Content)
	if !strings.Contains(stop, "Stopped "+id) || strings.Contains(stop, "not been read") || !strings.Contains(stop, "Everything it printed is in ") {
		t.Errorf("TaskStop of a Monitor: %s", stop)
	}
}

// One offer holds at most monitorLines lines and monitorBytes bytes, and a
// line is cut at monitorLineBytes; what does not fit is counted, and is in
// the output file.
func TestMonitorBounds(t *testing.T) {
	t.Parallel()
	w := newWatches()
	m := newManager(t, Options{OnEnd: w.end, OnMonitor: w.offer})
	env := newEnv(t)
	watch(t, m, env, args{argDescription: "a lot", argCommand: "head -c 3000 /dev/zero | tr '\\0' x; echo; seq 1 300"})
	w.until(t, ended)
	w.mu.Lock()
	defer w.mu.Unlock()
	lines, left := 0, 0
	for _, o := range w.offers {
		if len(o.Lines) > monitorLines {
			t.Errorf("an offer holds %d lines", len(o.Lines))
		}
		size := 0
		for _, l := range o.Lines {
			size += len(l) + 1
		}
		if size > monitorBytes {
			t.Errorf("an offer holds %d bytes", size)
		}
		lines += len(o.Lines)
		left += o.Left
	}
	if lines+left != 301 {
		t.Errorf("%d lines were passed on and %d counted, want 301 in all", lines, left)
	}
	first := w.offers[0].Lines[0]
	if !strings.HasPrefix(first, strings.Repeat("x", monitorLineBytes)) || !strings.HasSuffix(first, "[... the rest of this line is in the output file]") || len(first) > monitorLineBytes+60 {
		t.Errorf("the long line was passed on as %d bytes: %q...", len(first), first[:40])
	}
}

// What a Monitor call may and may not be given.
func TestMonitorArguments(t *testing.T) {
	t.Parallel()
	w := newWatches()
	m := newManager(t, Options{OnEnd: w.end, OnMonitor: w.offer})
	env := newEnv(t)
	for _, c := range []struct {
		a    args
		want string
	}{
		{args{argDescription: "a socket", argWS: map[string]any{"url": "wss://example.com"}}, "cannot watch a WebSocket"},
		{args{argDescription: "x"}, "command is missing"},
		{args{argDescription: "x", argCommand: "  "}, "command is empty"},
		{args{argDescription: "x", argCommand: "true", "cwd": "/tmp"}, `"cwd" is not an argument of Monitor`},
		{args{argDescription: "x", argCommand: "true", argTimeoutMs: -5}, "must not be negative"},
	} {
		res := run(t, t.Context(), m, MonitorName, env, c.a)
		if !res.IsError || !strings.Contains(core.TextOf(res.Content), c.want) {
			t.Errorf("Monitor %v = %q, want a refusal with %q", c.a, core.TextOf(res.Content), c.want)
		}
	}
	// Too long a time is lowered to the most, and said to be.
	id, res := watch(t, m, env, args{argDescription: "long", argCommand: "sleep 60", argTimeoutMs: 99_000_000})
	if text := core.TextOf(res.Content); !strings.Contains(text, "the most a Monitor call may have is "+strconv.Itoa(int(monitorMost.Seconds()))+" s") || res.Meta["timeoutMs"] != monitorMost.Milliseconds() {
		t.Errorf("a time over the most: %s %v", text, res.Meta)
	}
	run(t, t.Context(), m, StopName, env, args{argTaskID: id})
	// Its time runs out like a background command's.
	id, _ = watch(t, m, env, args{argDescription: "short", argCommand: "echo up; sleep 30", argTimeoutMs: 600})
	got := w.until(t, func(ev []string) bool { return ended(ev) && strings.Contains(ev[len(ev)-1], id) })
	if !strings.Contains(strings.Join(got, "\n"), "up") || !strings.HasSuffix(got[len(got)-1], ReasonTimeout) {
		t.Errorf("a Monitor whose time ran out: %q", got)
	}
}

// Monitor is a tool only where its lines have somewhere to go and the
// background is on.
func TestMonitorIsOfferedWhereItCanWork(t *testing.T) {
	t.Parallel()
	has := func(m *Manager) bool {
		for _, tool := range m.Tools() {
			if tool.Spec().Name == MonitorName {
				return true
			}
		}
		return false
	}
	ok := func(Watched) bool { return true }
	if has(newManager(t, Options{})) {
		t.Errorf("Monitor is offered with nowhere to pass its lines")
	}
	if has(newManager(t, Options{OnMonitor: ok, NoBackground: true})) {
		t.Errorf("Monitor is offered with the background off")
	}
	if !has(newManager(t, Options{OnMonitor: ok})) {
		t.Errorf("Monitor is not offered")
	}
}

// A line printed in two pieces, with a pause between them, is passed on
// whole; and once a line does not fit, the lines after it are counted too,
// so that no offer shows lines with a gap between them.
func TestMonitorWholeLinesNoGaps(t *testing.T) {
	t.Parallel()
	w := newWatches()
	m := newManager(t, Options{OnEnd: w.end, OnMonitor: w.offer})
	env := newEnv(t)
	watch(t, m, env, args{argDescription: "a slow test", argCommand: "printf 'test_x '; sleep 0.4; echo PASSED"})
	got := w.until(t, ended)
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "test_x PASSED") {
		t.Errorf("the line came apart:\n%s", all)
	}

	w2 := newWatches()
	m2 := newManager(t, Options{OnEnd: w2.end, OnMonitor: w2.offer})
	watch(t, m2, env, args{argDescription: "big lines", argCommand: "for i in 1 2 3 4 5 6 7 8 9 10; do head -c 900 /dev/zero | tr '\\0' a; echo; done; echo tail"})
	w2.until(t, ended)
	w2.mu.Lock()
	defer w2.mu.Unlock()
	for _, o := range w2.offers {
		if o.Left > 0 && slices.Contains(o.Lines, "tail") {
			t.Errorf("an offer shows a line after one that did not fit: %d lines, %d left out", len(o.Lines), o.Left)
		}
	}
}

// The last piece of output a command's sink hands over ends wherever the
// command stopped writing, so it can hold whole lines and then the start of
// a line that never ended. The lines keep the order the command printed
// them in.
func TestMonitorKeepsTheOrderOfLines(t *testing.T) {
	t.Parallel()
	w := &watcher{}
	tail := strings.Repeat("z", monitorLineBytes+500)
	w.take(core.Progress{Stream: "stdout", Text: "first\nsecond\n" + tail})
	w.mu.Lock()
	defer w.mu.Unlock()
	got := make([]string, len(w.lines))
	for i, l := range w.lines {
		got[i] = firstLine(l, 12)
	}
	if !slices.Equal(got, []string{"first", "second", strings.Repeat("z", 11) + "…"}) {
		t.Errorf("the lines came out as %q, want first, then second, then the start of the long line", got)
	}
}

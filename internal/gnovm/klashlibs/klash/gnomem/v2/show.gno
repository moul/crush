// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnomem

import (
	"strconv"
	"strings"
	"time"
)

const (
	maxShow       = 3000 // about the most Show or Find writes
	maxShowClosed = 5    // closed goals Show lists
	maxShowFacts  = 10   // facts Show lists
	maxShowGroup  = 20   // facts and rules Show("group") lists
	maxLine       = 160  // bytes of a result, wait or fact a line shows
	maxList       = 5    // names a receipt lists before "and N more"
	maxRun        = 5    // closed steps in a row an outline lists before folding them
)

// Show returns mem as an outline: the rules, the questions for the user,
// what is due, the open goals newest first with their steps and progress,
// the newest closed goals, then the newest facts. Given names, it shows
// those items, facts or rules in full; a name that only has names under it
// ("cmd") lists them. About 3000 bytes at most.
func (m *Mem) Show(names ...string) string {
	var sb strings.Builder
	if len(names) > 0 {
		for _, n := range names {
			m.showName(&sb, n)
		}
		return cut(sb.String())
	}
	m.showHeader(&sb)
	if m.rules.Size() > 0 {
		sb.WriteString("rules:\n")
		m.rules.Iterate("", "", func(n string, v any) bool {
			sb.WriteString("  `" + n + "` = " + line(v.(*Rule).Text, maxText) + "\n")
			return false
		})
	}
	if m.asks.Size() > 0 {
		sb.WriteString("questions for the user:\n")
		m.asks.Iterate("", "", func(n string, _ any) bool {
			sb.WriteString("  [?] " + ref(n, m.get(n).Title) + "\n")
			return false
		})
	}
	var due []string
	m.days.Iterate("", today()+"|\xff", func(_ string, v any) bool {
		due = append(due, v.(string))
		return false
	})
	if len(due) > 0 {
		sb.WriteString("due:\n")
		for _, n := range due {
			sb.WriteString("  [ ] " + ref(n, m.get(n).Title) + " (since " + m.get(n).Wait + ")\n")
		}
	}
	if m.open.Size() > 0 {
		sb.WriteString("todos:\n")
		// Newest first.
		m.open.ReverseIterate("", "", func(_ string, v any) bool {
			m.outline(&sb, v.(string))
			return sb.Len() > maxShow
		})
	}
	if m.closed.Size() > 0 {
		sb.WriteString("closed lately:\n")
		i := 0
		m.closed.ReverseIterate("", "", func(_ string, v any) bool {
			n := v.(string)
			it := m.get(n)
			sb.WriteString("  " + mark(it) + " " + ref(n, it.Title) + arrow(it.Result) + " (" + day(it.Changed) + ")\n")
			i++
			return i == maxShowClosed
		})
	}
	if m.facts.Size() > 0 {
		sb.WriteString("facts, newest first:\n")
		shown := map[string]bool{}
		m.recent.ReverseIterate("", "", func(_ string, v any) bool {
			n := v.(string)
			sb.WriteString("  `" + n + "` = " + line(m.getFact(n).Text, maxLine) + "\n")
			shown[n] = true
			return len(shown) == maxShowFacts
		})
		if more := m.facts.Size() - len(shown); more > 0 {
			sb.WriteString("  … " + plural(more, "more fact") + ", " + m.factGroups(shown) + "\n")
		}
	}
	return cut(sb.String())
}

// maxShowGroups is how many groups of facts Show names for the facts it
// leaves out.
const maxShowGroups = 12

// factGroups names the facts not shown by their groups, a name's first
// segment, each with how many it holds ("`infra/` (12)"), and a fact with
// no group by its name; then how to see a group. Names are paths so that a
// model looking for a deploy command finds `cmd/` here in one look.
func (m *Mem) factGroups(shown map[string]bool) string {
	var parts []string
	group, count, first := "", 0, ""
	flush := func() {
		switch {
		case count == 0:
		case strings.HasSuffix(group, "/"):
			parts = append(parts, "`"+group+"` ("+strconv.Itoa(count)+")")
			if first == "" {
				first = strings.TrimSuffix(group, "/")
			}
		default:
			parts = append(parts, "`"+group+"`")
		}
	}
	// The groups index counts each group's facts; those shown already
	// are taken off, so the walk is over groups, not every fact.
	less := map[string]int{}
	for n := range shown {
		less[factGroup(n)]++
	}
	m.groups.Iterate("", "", func(g string, v any) bool {
		group, count = g, v.(int)-less[g]
		flush()
		return false
	})
	if len(parts) > maxShowGroups {
		parts = append(parts[:maxShowGroups], plural(len(parts)-maxShowGroups, "more group"))
	}
	text := "in " + strings.Join(parts, ", ")
	if first != "" {
		text += ": mem.Show(\"" + first + "\") lists a group"
	}
	return text
}

// String is Show(), so that println(mem) shows mem.
func (m *Mem) String() string { return m.Show() }

func (m *Mem) showHeader(sb *strings.Builder) {
	c := m.counts
	if c.Open+c.Done+c.Dropped == 0 && m.facts.Size() == 0 && m.rules.Size() == 0 {
		sb.WriteString("mem is empty: mem.Todo(\"what to get done\") adds a todo, and mem.Fact(\"cmd/test\", \"…\") keeps a fact\n")
		return
	}
	parts := []string{plural(m.open.Size(), "todo") + " open"}
	if c.ForUser > 0 {
		parts[0] += ", " + plural(c.ForUser, "question") + " for the user"
	}
	if closed := m.closed.Size(); closed > 0 {
		parts = append(parts, strconv.Itoa(closed)+" closed")
	}
	if n := m.rules.Size(); n > 0 {
		parts = append(parts, plural(n, "rule"))
	}
	if n := m.facts.Size(); n > 0 {
		parts = append(parts, plural(n, "fact"))
	}
	sb.WriteString("mem: " + strings.Join(parts, " · ") + "\n")
}

// outline writes goal name, and its steps if it is open, with its next
// step marked.
func (m *Mem) outline(sb *strings.Builder, name string) {
	it := m.get(name)
	sb.WriteString("  " + mark(it) + " " + ref(name, it.Title) + arrow(it.Result) + m.aside(name, it, true) + "\n")
	m.outlineSteps(sb, name, 2, m.nextFor(name, it, caller()))
}

// outlineSteps writes the steps of item name at depth, until Show has
// written enough. More than maxRun closed steps in a row fold into one line,
// all but the last two, so that a long task's outline shows where it is.
func (m *Mem) outlineSteps(sb *strings.Builder, name string, depth int, next string) {
	var run []string
	var runIts []*Todo
	fold := func() {
		if len(run) > maxRun {
			n := len(run) - 2
			done := 0
			for _, it := range runIts[:n] {
				if it.Status == "done" {
					done++
				}
			}
			box, counts := "[x]", []string{strconv.Itoa(done) + " done"}
			if dropped := n - done; dropped > 0 {
				counts = append(counts, strconv.Itoa(dropped)+" dropped")
			}
			if done == 0 {
				box, counts = "[-]", counts[1:]
			}
			sb.WriteString(strings.Repeat("  ", depth) + box + " `" + run[0] + "` … `" + run[n-1] + "`: " + strings.Join(counts, ", ") + "\n")
			run, runIts = run[n:], runIts[n:]
		}
		for i, step := range run {
			m.outlineStep(sb, step, runIts[i], depth, next)
		}
		run, runIts = nil, nil
	}
	m.eachStep(name, func(step string, s *Todo) bool {
		switch s.Status {
		case "forgotten":
		case "":
			fold()
			m.outlineStep(sb, step, s, depth, next)
		default:
			run = append(run, step)
			runIts = append(runIts, s)
		}
		return sb.Len() > maxShow
	})
	fold()
}

func (m *Mem) outlineStep(sb *strings.Builder, name string, it *Todo, depth int, next string) {
	sb.WriteString(strings.Repeat("  ", depth) + mark(it) + " " + ref(name, it.Title) + arrow(it.Result) + m.aside(name, it, false))
	if name == next {
		sb.WriteString("  ← next")
	}
	sb.WriteString("\n")
	if it.Status == "" && len(it.Steps) > 0 {
		m.outlineSteps(sb, name, depth+1, next)
	}
}

// aside is what a line says in parentheses: progress, who and when for a
// goal; a wait, a claim or whose move it is for any item.
func (m *Mem) aside(name string, it *Todo, top bool) string {
	var parts []string
	if top {
		if len(it.Steps) > 0 {
			parts = append(parts, progress(it))
		}
		parts = append(parts, who(it.By), ago(it.Changed))
	}
	if it.Status == "" && it.Parallel {
		parts = append(parts, "its todos at once")
	}
	if it.Status == "" {
		switch {
		case it.Wait != "" && !m.waiting(it):
			parts = append(parts, "due since "+it.Wait)
		case it.Wait != "":
			parts = append(parts, strings.TrimSpace(waitText(it.Wait)))
		case m.asksUnder(name):
			parts = append(parts, "waits on the user")
		}
		if it.Doing != "" {
			parts = append(parts, "held by "+who(it.Doing)+", "+ago(it.Changed))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " · ") + ")"
}

// asksUnder reports whether an open step of item name is for the user.
func (m *Mem) asksUnder(name string) bool {
	if m.counts.ForUser == 0 {
		return false
	}
	found := false
	m.asks.Iterate(name+"/", name+"/\xff", func(n string, _ any) bool {
		found = strings.IndexByte(n[len(name)+1:], '/') < 0
		return found
	})
	return found
}

// progress is "1/3 done": the steps done of those not dropped.
func progress(it *Todo) string {
	return strconv.Itoa(it.DoneSteps) + "/" + strconv.Itoa(it.DoneSteps+it.OpenSteps) + " done"
}

func (m *Mem) showName(sb *strings.Builder, name string) {
	shown := false
	if it := m.get(name); it != nil && it.Status != "forgotten" {
		m.showItem(sb, name, it)
		shown = true
	}
	if f := m.getFact(name); f != nil {
		sb.WriteString("fact `" + name + "` = " + f.Text + " (" + day(f.Changed) + ")\n")
		shown = true
	}
	if r := m.getRule(name); r != nil {
		sb.WriteString("rule `" + name + "` = " + r.Text + " (" + who(r.By) + ", " + day(r.Changed) + ")\n")
		shown = true
	}
	if shown {
		return
	}
	// A group: its facts and rules with their text, as Show lists facts,
	// so reading one needs no other call.
	var under []string
	more := 0
	m.facts.Iterate(name+"/", name+"/\xff", func(n string, v any) bool {
		if len(under) == maxShowGroup {
			more++
			return false
		}
		under = append(under, "  `"+n+"` = "+line(v.(*fact).Text, maxLine))
		return false
	})
	m.rules.Iterate(name+"/", name+"/\xff", func(n string, v any) bool {
		if len(under) == maxShowGroup {
			more++
			return false
		}
		under = append(under, "  rule `"+n+"` = "+line(v.(*Rule).Text, maxLine))
		return false
	})
	if len(under) > 0 {
		sb.WriteString("under `" + name + "/`:\n" + strings.Join(under, "\n") + "\n")
		if more > 0 {
			sb.WriteString("  … " + strconv.Itoa(more) + " more: mem.Show(\"" + name + "/…\") shows one\n")
		}
		return
	}
	sb.WriteString(m.miss(name) + "\n")
}

// Has reports whether Show shows something under name: an item, a fact, a
// rule, or a group of facts and rules. gnosh mem's reads use it to say
// that a name is not there, which Show itself only writes.
func (m *Mem) Has(name string) bool {
	if it := m.get(name); it != nil && it.Status != "forgotten" {
		return true
	}
	if m.getFact(name) != nil || m.getRule(name) != nil {
		return true
	}
	has := false
	found := func(string, any) bool {
		has = true
		return true
	}
	m.facts.Iterate(name+"/", name+"/\xff", found)
	m.rules.Iterate(name+"/", name+"/\xff", found)
	return has
}

// showItem writes item name in full: its status, result, wait, claim, notes
// and every item under it.
func (m *Mem) showItem(sb *strings.Builder, name string, it *Todo) {
	sb.WriteString(mark(it) + " " + ref(name, it.Title))
	if p := parentOf(name); p != "" {
		sb.WriteString(" (under " + ref(p, m.get(p).Title) + ")")
	}
	if len(it.Steps) > 0 {
		sb.WriteString(" (" + progress(it) + ")")
	}
	sb.WriteString("\n")
	if it.Status != "" {
		sb.WriteString("  " + it.Status + arrow(it.Result) + "\n")
	}
	if it.Wait != "" {
		sb.WriteString("  " + strings.TrimSpace(waitText(it.Wait)) + "\n")
	}
	if it.For != "" {
		sb.WriteString("  for the user\n")
	}
	if it.Doing != "" {
		sb.WriteString("  held by " + who(it.Doing) + "\n")
	}
	sb.WriteString("  made by " + who(it.By) + ", changed " + ago(it.Changed) + "\n")
	for _, n := range it.Notes {
		sb.WriteString("  thought `" + n.Name + "`: " + n.Text)
		if n.Step != "" {
			sb.WriteString(" (now the todo `" + n.Step + "`)")
		}
		sb.WriteString("\n")
	}
	m.outlineSteps(sb, name, 1, m.nextFor(goalOf(name), m.get(goalOf(name)), caller()))
}

// Find lists items, closed ones too, facts and rules that match words as
// they were stored, best first. About 3000 bytes at most.
func (m *Mem) Find(words string) string {
	words = text("the words", words)
	if words == "" {
		panic("Find needs words: mem.Find(\"release key\")")
	}
	names, kinds, looked := find(words, 20)
	// A search the embedding server did not answer is said to be one, so
	// it is not taken for a search by meaning that found nothing.
	late := ""
	if looked == "words" {
		late = "the embedding server did not answer this search, so it matched the words only"
	}
	if len(names) == 0 {
		// Say where to look instead, so the model reads the list once
		// rather than guessing other words.
		how := ": Find matches the words as they were stored"
		switch looked {
		case "meaning":
			how = ", in its words or in meaning"
		case "words":
			how = ": " + late
		}
		facts := plural(m.facts.Size(), "fact")
		if m.facts.Size() > maxShowFacts {
			facts = "the newest " + strconv.Itoa(maxShowFacts) + " of " + facts + ", with the groups of the rest"
		}
		return "nothing in mem matches " + strconv.Quote(words) + how + "; mem.Show() lists " +
			plural(m.open.Size()+m.closed.Size(), "todo") + ", " + plural(m.rules.Size(), "rule") + " and " + facts + "\n"
	}
	var sb strings.Builder
	for i, n := range names {
		switch kinds[i] {
		case "fact":
			if f := m.getFact(n); f != nil {
				sb.WriteString("fact `" + n + "` = " + line(f.Text, maxLine) + "\n")
			}
		case "rule":
			if r := m.getRule(n); r != nil {
				sb.WriteString("rule `" + n + "` = " + line(r.Text, maxLine) + "\n")
			}
		case "thought":
			item, name := splitAddr(n)
			if it := m.get(item); it != nil && it.Status != "forgotten" {
				if t := noteNamed(it, name); t != nil {
					sb.WriteString("thought `" + name + "` on `" + item + "`: " + line(t.Text, maxLine) + "\n")
				}
			}
		default:
			if it := m.get(n); it != nil && it.Status != "forgotten" {
				sb.WriteString(mark(it) + " " + ref(n, it.Title) + arrow(it.Result) + " (" + day(it.Changed) + ")\n")
			}
		}
	}
	if late != "" {
		return cut(sb.String()) + "(" + late + ")\n"
	}
	return cut(sb.String())
}

// ref names an item by its name and title: `release-v2` "release v2"; one
// with no title (a step made from a thought) by its name alone.
func ref(name, title string) string {
	if title == "" {
		return "`" + name + "`"
	}
	return "`" + name + "` " + quote(line(title, 80))
}

// refs names items by name and title.
func (m *Mem) refs(names []string) string {
	var parts []string
	for _, n := range names {
		parts = append(parts, ref(n, m.get(n).Title))
	}
	return joinList(parts)
}

// names lists names in backticks.
func names(ns []string) string {
	var parts []string
	for _, n := range ns {
		parts = append(parts, "`"+n+"`")
	}
	return joinList(parts)
}

// joinList joins parts as a sentence does: "a, b and c". Past maxList parts
// it names the first few and says how many more.
func joinList(parts []string) string {
	switch {
	case len(parts) == 0:
		return ""
	case len(parts) == 1:
		return parts[0]
	case len(parts) > maxList:
		return strings.Join(parts[:maxList-1], ", ") + " and " + strconv.Itoa(len(parts)-maxList+1) + " more"
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// arrow is " → result", or "" for none.
func arrow(result string) string {
	if result == "" {
		return ""
	}
	return " → " + line(result, maxLine)
}

// who says who a session id is to the session that asks.
func who(by string) string {
	switch by {
	case "":
		return "nobody"
	case "user":
		return "the user"
	case "klash": // gnosh's own calls, recorded under its old name
		return "gnosh"
	case caller():
		return "this session"
	}
	if len(by) > 8 {
		by = by[:8] // enough to tell sessions apart; ids are long
	}
	return "session " + by
}

// ago is how long before now t was, roughly: "5m ago", "3h ago", "2d ago".
func ago(t int64) string {
	d := time.Now().Unix() - t
	switch {
	case d < 60:
		return "just now"
	case d < 3600:
		return strconv.FormatInt(d/60, 10) + "m ago"
	case d < 86400:
		return strconv.FormatInt(d/3600, 10) + "h ago"
	}
	return strconv.FormatInt(d/86400, 10) + "d ago"
}

// day is t as a day, 2026-10-29.
func day(t int64) string {
	return time.Unix(t, 0).UTC().Format("2006-01-02")
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

func mark(it *Todo) string {
	switch {
	case it.Status == "" && it.For != "":
		return "[?]"
	case it.Status == "":
		return "[ ]"
	case it.Status == "done":
		return "[x]"
	}
	return "[-]"
}

// line keeps text on one line, cut to n bytes.
func line(s string, n int) string {
	return clip(strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " ⏎ "), n)
}

// cut ends a long outline at a line, saying so.
func cut(s string) string {
	if len(s) <= maxShow {
		return s
	}
	i := strings.LastIndex(s[:maxShow], "\n")
	return s[:i+1] + "… (cut: mem.Show(\"name\") shows one todo, mem.Find(\"words\") searches)\n"
}

// factsBeside says which facts share name's first segment, for a receipt:
// "; `cmd/` also has `cmd/test`, `cmd/build`".
func (m *Mem) factsBeside(name string, new bool) string {
	prefix := goalOf(name)
	if prefix == name {
		return ""
	}
	var beside []string
	m.facts.Iterate(prefix+"/", prefix+"/\xff", func(n string, _ any) bool {
		if n != name {
			beside = append(beside, "`"+n+"`")
		}
		return len(beside) == 5
	})
	if len(beside) == 0 {
		return ""
	}
	if new {
		return "; `" + prefix + "/` also has " + joinList(beside)
	}
	return "; `" + prefix + "/` has " + joinList(beside)
}

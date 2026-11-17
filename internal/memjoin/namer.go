package memjoin

import (
	"context"
	"strings"
	"time"

	"github.com/gnosh-ai/gnosh/internal/agent"
)

// This file is the namer (integration-contract.md section 5.5): before a
// call on mem, the texts the call keeps without naming them are given
// names by the small model, all in one request, and the names are handed
// to mem by text. mem takes a name it can use and makes one from the
// words otherwise, so nothing depends on the model answering.

// nameCache holds the names made in this session, by text. A text keeps
// the name it was first given, and a text no name could be made for is
// not asked about again: a refused call that is sent again then makes
// the same names as the first time. It is guarded by the join's lock.
type nameCache struct {
	names map[string]string
}

// The size of a name: at most four words and 32 bytes.
const (
	maxNameWords = 4
	maxNameBytes = 32
	// maxMatched is how many texts every order of the names is tried for.
	maxMatched = 7
)

// namer is the tools' Namer slot. texts are the unnamed texts of one call,
// in order, each once.
func (j *join) namer(ctx context.Context, _ string, texts []string) map[string]string {
	out := map[string]string{}
	var ask []string
	j.mu.Lock()
	for _, t := range texts {
		name, known := j.names.names[t]
		switch {
		case !known:
			ask = append(ask, t)
		case name != "":
			out[t] = name
		}
	}
	j.mu.Unlock()
	if len(ask) == 0 {
		return out
	}

	made := j.askNames(ctx, ask)
	j.mu.Lock()
	if j.names.names == nil {
		j.names.names = map[string]string{}
	}
	for i, t := range ask {
		// A turn that was stopped while the model was asked has made no
		// names; the texts are asked about again next time.
		if ctx.Err() != nil {
			break
		}
		if _, known := j.names.names[t]; !known {
			j.names.names[t] = made[i]
		}
		if name := j.names.names[t]; name != "" {
			out[t] = name
		}
	}
	j.mu.Unlock()
	return out
}

// askNames asks a model to name texts and returns a name for each, in
// order: "" where none could be made. One text is asked for one name;
// several are asked for in one request, one name a line.
func (j *join) askNames(ctx context.Context, texts []string) []string {
	none := make([]string, len(texts))
	a := agent.FromContext(ctx)
	if a == nil {
		a = j.theAgent()
	}
	if a == nil {
		return none
	}
	req := j.texts.Namer(texts)
	room := 24
	if len(texts) > 1 {
		room = 16 * len(texts)
	}
	res, err := a.Side(ctx, agent.SideRequest{
		Purpose:   "name",
		System:    req.System,
		Prompt:    req.Prompt,
		MaxTokens: room,
		Timeout:   30*time.Second + time.Duration(len(texts))*10*time.Second,
	})
	if err != nil {
		return none
	}
	return matchNames(texts, readNames(res.Text, len(texts)))
}

// readNames reads a model's answer as names: the first line for one name,
// and one name a line for several, where a numbered or bulleted line
// counts. A line no name can be made of gives "".
func readNames(answer string, want int) []string {
	var lines []string
	for _, line := range strings.Split(answer, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if want == 1 && len(lines) > 1 {
		lines = lines[:1]
	}
	names := make([]string, len(lines))
	for i, line := range lines {
		names[i] = cleanName(line)
	}
	return names
}

// cleanName makes a name of one line of a model's answer: quotes,
// punctuation and a list's number or bullet go; the words of lowercase
// letters and digits stay, joined by dashes, at most four of them and 32
// bytes. A name starts with a letter; a line that leaves none gives "".
func cleanName(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	// A list's number or bullet: "1.", "2)", "-", "*".
	line = strings.TrimLeft(line, "-*•· \t")
	if i := strings.IndexAny(line, ".):"); i > 0 && i <= 3 && strings.Trim(line[:i], "0123456789") == "" {
		line = line[i+1:]
	}
	words := strings.FieldsFunc(line, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	if len(words) > maxNameWords {
		words = words[:maxNameWords]
	}
	for len(words) > 0 && len(strings.Join(words, "-")) > maxNameBytes {
		words = words[:len(words)-1]
	}
	name := strings.Join(words, "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return ""
	}
	return name
}

// wordsOf is the words of a text that a name's are set against: its runs
// of lower-case letters and digits.
func wordsOf(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// fit is how well a name fits a text, whose words wordsOf gave: how many
// of the name's words the text shares, where two words are shared when
// they are the same, or one begins the other and is four letters or more.
func fit(name string, textWords []string) int {
	if name == "" {
		return 0
	}
	n := 0
	// The name's words are walked rather than split off, so that trying
	// every order of the names allocates nothing.
	for rest := name; rest != ""; {
		var w string
		w, rest, _ = strings.Cut(rest, "-")
		for _, t := range textWords {
			if w == t || (len(w) >= 4 && strings.HasPrefix(t, w)) || (len(t) >= 4 && strings.HasPrefix(w, t)) {
				n++
				break
			}
		}
	}
	return n
}

// matchNames gives each text its name. A small model sometimes answers in
// another order than it was asked in, and once swapped a task's name with
// its todo's; so when another order of the names fits the texts better in
// all, that order is taken (every order is tried, for up to seven texts).
// A name that shares no word with its text is dropped, and so is a name
// given twice: the names of one call must differ.
func matchNames(texts, names []string) []string {
	out := make([]string, len(texts))
	copy(out, names)
	// Every order is set against the same texts, so their words are worked
	// out once here rather than once per fit: seven texts are 5,040 orders.
	words := make([][]string, len(texts))
	for i, text := range texts {
		words[i] = wordsOf(text)
	}
	if len(texts) > 1 && len(texts) <= maxMatched {
		best, bestFit := out, total(words, out)
		perm := make([]string, len(out))
		copy(perm, out)
		permute(perm, 0, func(order []string) {
			if f := total(words, order); f > bestFit {
				bestFit = f
				best = append([]string(nil), order...)
			}
		})
		out = best
	}
	seen := map[string]bool{}
	for i, name := range out {
		if name == "" {
			continue
		}
		if fit(name, words[i]) == 0 || seen[name] {
			out[i] = ""
			continue
		}
		seen[name] = true
	}
	return out
}

// total is how well names fit texts, each name with the words of the text
// at its place.
func total(words [][]string, names []string) int {
	n := 0
	for i, name := range names {
		n += fit(name, words[i])
	}
	return n
}

// permute calls f with every order of list after the first k places.
func permute(list []string, k int, f func([]string)) {
	if k == len(list) {
		f(list)
		return
	}
	for i := k; i < len(list); i++ {
		list[k], list[i] = list[i], list[k]
		permute(list, k+1, f)
		list[k], list[i] = list[i], list[k]
	}
}

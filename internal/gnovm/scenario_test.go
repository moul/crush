// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestScenarios runs the use cases in testdata/scenarios against a real
// gnosh and a real model, with gnosh's own system prompt ("v0") and with
// each prompt variant in testdata/prompts appended to it, and writes what
// happened: each turn's tool calls and answer, each check, and what the
// persistent Gno machines hold at the end.
//
// It is skipped unless GNOVM_SCENARIO_GNOSH names the gnosh to run. Every
// run gets its own config and data directories (copied from
// GNOVM_SCENARIO_HOME, default $HOME: the crushrc, crush.json and the files
// that keep first-run questions away), so it never touches the user's
// settings.
//
//	GNOVM_SCENARIO_GNOSH     the gnosh to run (required)
//	GNOVM_SCENARIO_HOME      whose config to copy (default $HOME)
//	GNOVM_SCENARIO_OUT       where runs and reports go (default a temp dir)
//	GNOVM_SCENARIO_VARIANTS  comma-separated, e.g. "v0" (default v0 and
//	                         every testdata/prompts file)
//	GNOVM_SCENARIO_ONLY      a regexp on scenario names
//	GNOVM_SCENARIO_MODEL     a model for gnosh run -m
//	GNOVM_SCENARIO_REASONING the large model's reasoning level (default
//	                         "nothink": these runs measure tool use, and
//	                         Qwen's thinking is most of their time); "" keeps
//	                         the user's
//	GNOVM_SCENARIO_STRICT=1  fail the test when a check fails (by default
//	                         the checks are reported: the scenarios measure
//	                         the model and the prompt as much as gnosh)
//
// Run it with -test.parallel below the model server's slots, so that live
// sessions keep one: a conversation whose slot another request takes
// re-reads all of itself on its next step.
func TestScenarios(t *testing.T) {
	// GNOVM_SCENARIO_KLASH is its name from before the rename.
	gnosh := cmp.Or(os.Getenv("GNOVM_SCENARIO_GNOSH"), os.Getenv("GNOVM_SCENARIO_KLASH"))
	if gnosh == "" {
		t.Skip("set GNOVM_SCENARIO_GNOSH to a gnosh binary to run the real-model scenarios")
	}
	home := cmp.Or(os.Getenv("GNOVM_SCENARIO_HOME"), os.Getenv("HOME"))
	out := cmp.Or(os.Getenv("GNOVM_SCENARIO_OUT"), t.TempDir())
	variants := []string{"v0"}
	prompts, _ := filepath.Glob("testdata/prompts/*.md")
	for _, p := range prompts {
		variants = append(variants, strings.TrimSuffix(filepath.Base(p), ".md"))
	}
	if v := os.Getenv("GNOVM_SCENARIO_VARIANTS"); v != "" {
		variants = strings.Split(v, ",")
	}
	reasoning, setReasoning := os.LookupEnv("GNOVM_SCENARIO_REASONING")
	if !setReasoning {
		reasoning = "nothink"
	}
	only := regexp.MustCompile(cmp.Or(os.Getenv("GNOVM_SCENARIO_ONLY"), "."))
	files, err := filepath.Glob("testdata/scenarios/*.txtar")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios: %v", err)
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	var mu sync.Mutex
	var results []*scenarioRun
	t.Cleanup(func() { writeSummary(t, filepath.Join(out, "summary-"+stamp+".md"), results) })
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".txtar")
		if !only.MatchString(name) {
			continue
		}
		sc, err := parseScenario(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, variant := range variants {
			t.Run(name+"/"+variant, func(t *testing.T) {
				t.Parallel()
				r := &scenarioRun{scenario: name, variant: variant, dir: filepath.Join(out, name+"-"+variant+"-"+stamp)}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
				r.run(t, sc, gnosh, home, reasoning)
				for _, c := range r.checks {
					if !c.ok && os.Getenv("GNOVM_SCENARIO_STRICT") == "1" {
						t.Errorf("check failed: %s (%s)", c.line, c.got)
					}
				}
			})
		}
	}
}

// scenario is a parsed testdata/scenarios file (txtar): a description, then
// sections. "prompt" continues the session, "prompt-new" starts a new one,
// "project/<path>" is a file in the project, "mem" is Gno run in the
// project's mem as the user before the first turn, and "check" holds the
// checks.
type scenario struct {
	about  string
	turns  []turnSpec
	files  map[string]string
	mem    string
	checks []string
	// peers is how many sessions each fresh turn starts at once in the
	// project, one gnosh process each (PARALLEL.md); GNOVM_SCENARIO_PEERS
	// overrides it for a scenario that has a peers section; 0 is one.
	peers int
}

type turnSpec struct {
	prompt string
	fresh  bool
}

// TestScenarioFilesParse checks every scenario file without a model: it
// parses, it has a prompt and checks, and each check is of a known form.
func TestScenarioFilesParse(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/scenarios/*.txtar")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenarios: %v", err)
	}
	forms := regexp.MustCompile(`^(turn [1-9][0-9]* )?(` +
		`tool \S+ (>=|<=|==|>|<) \d+|` +
		`answer (last|\d+) (contains|matches) .+|` +
		`(file|after \d+ file) \S+ (exists|absent|contains .+)|` +
		`cmd .+|` +
		`vm-persist (>=|<=|==|>|<) \d+|` +
		`vm-dump contains .+|` +
		`mem-count (open|done|dropped|items|facts|rules) (>=|<=|==|>|<) \d+|` +
		`vm-eval [a-z][a-z0-9_-]* \S+ ((>=|<=|==|>|<) -?\d+|contains .+))$`)
	for _, file := range files {
		sc, err := parseScenario(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if len(sc.turns) == 0 || len(sc.checks) == 0 {
			t.Errorf("%s: needs a prompt and checks", file)
		}
		for _, line := range sc.checks {
			if !forms.MatchString(line) {
				t.Errorf("%s: unknown check %q", file, line)
			}
			if m := regexp.MustCompile(`^(?:turn|answer|after) (\d+)`).FindStringSubmatch(line); m != nil {
				if n, _ := strconv.Atoi(m[1]); n > len(sc.turns) {
					t.Errorf("%s: %q names turn %d of %d", file, line, n, len(sc.turns))
				}
			}
		}
		if sc.mem != "" {
			if err := seedMem(t.TempDir(), "/proj", sc.mem, ""); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
	}
}

// TestScenarioMemSection checks that a scenario's mem section is in mem
// before its first turn, kept as the user's, as the agent will find it.
func TestScenarioMemSection(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "seeded.txtar")
	require.NoError(t, os.WriteFile(file, []byte(`A secret in mem before the session.
-- mem --
mem.Fact("api/staging-key", "sk-staging-0000")
-- mem --
mem.Todo("rotate the staging key")
-- prompt --
What's the staging API key?
-- check --
mem-count facts == 1
`), 0o600))
	sc, err := parseScenario(file)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, seedMem(dir, "/proj", sc.mem, ""))
	n, err := memCount(dir, "/proj", "facts")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = memCount(dir, "/proj", "open")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, entries, _, err := readMemJournal(filepath.Join(memDir(dir, "/proj"), "journal.jsonl"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "one call, one entry")
	require.Equal(t, "user", entries[0].Who, "kept as the user's")
	require.Error(t, seedMem(dir, "/proj", `mem.Fact("BAD NAME", "x")`, ""), "a section that fails stops the run")

	// With an embedding server, the seeded rows have their vectors before
	// the first turn.
	f := newFakeEmbedder(t)
	dir = t.TempDir()
	require.NoError(t, seedMem(dir, "/proj", sc.mem, f.URL+"/v1"))
	s, ok, err := readMemSnapshot(context.Background(), memDir(dir, "/proj"))
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, s.vecs)
	require.False(t, s.lacksVectors())
}

// TestScenarioEmbeddingsURL checks that the harness finds the embedding
// server a run's gnosh is given wherever the copied config sets it.
func TestScenarioEmbeddingsURL(t *testing.T) {
	t.Parallel()
	write := func(path, body string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	for name, files := range map[string]map[string]string{
		"config crush.json": {"config/crush/crush.json": `{"options": {"mem_embeddings_url": "http://127.0.0.1:8091/v1"}}`},
		"data crush.json":   {"data/crush/crush.json": `{"options": {"mem_embeddings_url": "http://127.0.0.1:8091/v1"}}`},
		"crushrc":           {"config/crush/crushrc": "option reasoning nothink\noption mem-embeddings-url 'http://127.0.0.1:8091/v1'\n"},
	} {
		dir := t.TempDir()
		for path, body := range files {
			write(filepath.Join(dir, path), body)
		}
		require.Equal(t, "http://127.0.0.1:8091/v1", memEmbeddingsURL(filepath.Join(dir, "config"), filepath.Join(dir, "data")), name)
	}
	dir := t.TempDir()
	write(filepath.Join(dir, "config/crush/crush.json"), `{"models": {"large": {"provider": "qwen"}}}`)
	require.Empty(t, memEmbeddingsURL(filepath.Join(dir, "config"), filepath.Join(dir, "data")), "none set")
}

var sectionRe = regexp.MustCompile(`(?m)^-- (.+?) --$`)

func parseScenario(path string) (*scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data := string(b)
	locs := sectionRe.FindAllStringSubmatchIndex(data, -1)
	sc := &scenario{files: map[string]string{}}
	if len(locs) == 0 {
		return nil, fmt.Errorf("no sections")
	}
	sc.about = strings.TrimSpace(data[:locs[0][0]])
	for i, loc := range locs {
		name := data[loc[2]:loc[3]]
		start := loc[1] + 1
		end := len(data)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := ""
		if start <= end {
			body = data[start:end]
		}
		switch {
		case name == "prompt" || name == "prompt-new":
			sc.turns = append(sc.turns, turnSpec{prompt: strings.TrimSpace(body), fresh: name == "prompt-new" || len(sc.turns) == 0})
		case name == "check":
			for _, line := range strings.Split(body, "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					sc.checks = append(sc.checks, line)
				}
			}
		case strings.HasPrefix(name, "project/"):
			sc.files[strings.TrimPrefix(name, "project/")] = body
		case name == "mem":
			sc.mem = strings.TrimSpace(sc.mem + "\n" + body)
		case name == "peers":
			n, err := strconv.Atoi(strings.TrimSpace(body))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("peers: %q is not a count", strings.TrimSpace(body))
			}
			sc.peers = n
		default:
			return nil, fmt.Errorf("unknown section %q", name)
		}
	}
	return sc, nil
}

// scenarioRun is one scenario under one prompt variant.
type scenarioRun struct {
	scenario, variant, dir string
	turns                  []turnResult
	after                  []map[string]string // project files after each turn
	vms                    map[string]string   // persistent VM name -> dump
	checks                 []checkResult
	took                   time.Duration
	// peersWall is how long the peers' turn took, from the first start to
	// the last end; 0 for a run without peers.
	peersWall time.Duration
}

type turnResult struct {
	prompt  string
	session string
	calls   []toolCall
	answer  string
	isError bool
	took    time.Duration
	// turn is the scenario's turn this is, or a peer of: a turn worked by
	// peers has one result per peer.
	turn int
}

type toolCall struct {
	name, input, result string
	isError             bool
}

type checkResult struct {
	line, got string
	ok        bool
}

func (r *scenarioRun) run(t *testing.T, sc *scenario, gnosh, home, reasoning string) {
	start := time.Now()
	defer func() { r.took = time.Since(start) }()
	project := filepath.Join(r.dir, "project")
	cfg, data := filepath.Join(r.dir, "config"), filepath.Join(r.dir, "data")
	for _, d := range []string{filepath.Join(cfg, "crush"), filepath.Join(data, "crush"), project, filepath.Join(r.dir, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"crushrc", "crush.json"} {
		copyIfThere(t, filepath.Join(home, ".config", "crush", f), filepath.Join(cfg, "crush", f))
	}
	for _, f := range []string{"providers.json", "metrics-consent.json", "crush.json"} {
		copyIfThere(t, filepath.Join(home, ".local", "share", "crush", f), filepath.Join(data, "crush", f))
	}
	if reasoning != "" {
		setLargeModelReasoning(t, filepath.Join(data, "crush", "crush.json"), reasoning)
	}
	for name, body := range sc.files {
		path := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if sc.mem != "" {
		if err := seedMem(filepath.Join(data, "crush", "gnovm"), project, sc.mem, memEmbeddingsURL(cfg, data)); err != nil {
			t.Fatal(err)
		}
	}
	var appendPrompt string
	if r.variant != "v0" {
		p, err := filepath.Abs(filepath.Join("testdata", "prompts", r.variant+".md"))
		if err != nil {
			t.Fatal(err)
		}
		appendPrompt = p
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+cfg, "XDG_DATA_HOME="+data, "TMPDIR="+filepath.Join(r.dir, "tmp"))
	session := ""
	// What each turn printed goes beside project/ only after the last turn:
	// a turn's agent can read anything on disk, and an earlier turn's
	// stream holds what that turn was told, a secret the user then had mem
	// erase among it.
	var outs, errs [][]byte
	peers := sc.peers
	if v := os.Getenv("GNOVM_SCENARIO_PEERS"); v != "" && sc.peers > 0 {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("GNOVM_SCENARIO_PEERS=%q is not a count", v)
		}
		peers = n
	}
	turnCap := 40 * time.Minute
	if v := os.Getenv("GNOVM_SCENARIO_TURN_CAP"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("GNOVM_SCENARIO_TURN_CAP=%q: %v", v, err)
		}
		turnCap = d
	}
	for i, ts := range sc.turns {
		if peers > 0 && ts.fresh {
			// The peers: one gnosh process each, started together in the
			// project, each with its own data directory (its sessions'
			// database), all sharing the project's mem.
			type peerOut struct {
				stdout, stderr bytes.Buffer
				err            error
			}
			outsP := make([]*peerOut, peers)
			turnStart := time.Now()
			var wg sync.WaitGroup
			for k := range peers {
				args := []string{"run", "--output-format", "stream-json", "-q", "-c", project, "-D", filepath.Join(r.dir, fmt.Sprintf("data-p%d", k+1))}
				if model := os.Getenv("GNOVM_SCENARIO_MODEL"); model != "" {
					args = append(args, "-m", model)
				}
				args = append(args, "-n", fmt.Sprintf("%s %s %d p%d", r.scenario, r.variant, i+1, k+1))
				if appendPrompt != "" {
					args = append(args, "--append-system-prompt-file", appendPrompt)
				}
				args = append(args, ts.prompt)
				po := &peerOut{}
				outsP[k] = po
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), turnCap)
					defer cancel()
					cmd := exec.CommandContext(ctx, gnosh, args...)
					cmd.Dir, cmd.Env = project, env
					cmd.Stdout, cmd.Stderr = &po.stdout, &po.stderr
					po.err = cmd.Run()
				}()
			}
			wg.Wait()
			r.peersWall = time.Since(turnStart)
			for k, po := range outsP {
				outs, errs = append(outs, po.stdout.Bytes()), append(errs, po.stderr.Bytes())
				tr := parseStream(bytes.NewReader(po.stdout.Bytes()))
				tr.prompt, tr.took, tr.turn = fmt.Sprintf("(peer %d of %d) %s", k+1, peers, ts.prompt), r.peersWall, i
				if po.err != nil && tr.answer == "" {
					tr.answer = fmt.Sprintf("(gnosh failed: %v; %s)", po.err, lastLine(po.stderr.String()))
					tr.isError = true
				}
				r.turns = append(r.turns, tr)
				t.Logf("turn %d peer %d: %d tool calls: %s", i+1, k+1, len(tr.calls), truncate(tr.answer, 200))
			}
			r.after = append(r.after, listFiles(project))
			t.Logf("turn %d: %d peers, %s from the first start to the last end", i+1, peers, r.peersWall.Round(time.Second))
			continue
		}
		args := []string{"run", "--output-format", "stream-json", "-q", "-c", project}
		if model := os.Getenv("GNOVM_SCENARIO_MODEL"); model != "" {
			args = append(args, "-m", model)
		}
		if !ts.fresh && session != "" {
			args = append(args, "--session", session)
		} else {
			// A named session gets no title request. On the AI box the
			// title requests of runs started together took every server
			// slot first, wiping the prompts cached there, and each run
			// then re-read its whole prompt: 1-2 minutes, not seconds.
			args = append(args, "-n", fmt.Sprintf("%s %s %d", r.scenario, r.variant, i+1))
		}
		if appendPrompt != "" {
			args = append(args, "--append-system-prompt-file", appendPrompt)
		}
		args = append(args, ts.prompt)
		turnStart := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), turnCap)
		cmd := exec.CommandContext(ctx, gnosh, args...)
		cmd.Dir, cmd.Env = project, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		outs, errs = append(outs, stdout.Bytes()), append(errs, stderr.Bytes())
		tr := parseStream(bytes.NewReader(stdout.Bytes()))
		tr.prompt, tr.took, tr.turn = ts.prompt, time.Since(turnStart), i
		if err != nil && tr.answer == "" {
			tr.answer = fmt.Sprintf("(gnosh failed: %v; %s)", err, lastLine(stderr.String()))
			tr.isError = true
		}
		session = cmp.Or(tr.session, session)
		r.turns = append(r.turns, tr)
		r.after = append(r.after, listFiles(project))
		t.Logf("turn %d: %d tool calls, %s: %s", i+1, len(tr.calls), tr.took.Round(time.Second), truncate(tr.answer, 200))
	}
	for i := range outs {
		_ = os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("turn-%d.jsonl", i+1)), outs[i], 0o600)
		_ = os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("turn-%d.stderr", i+1)), errs[i], 0o600)
	}
	r.vms = dumpPersistentVMs(t, filepath.Join(data, "crush", "gnovm"), project)
	for _, line := range sc.checks {
		r.checks = append(r.checks, r.check(line, project, env))
	}
	writeReport(t, r, sc)
}

// setLargeModelReasoning sets the reasoning level of the model the runs use
// in a copied crush.json (options.reasoning, keyed "provider/model"). It
// fails the test when it cannot tell which model that is, rather than let
// the runs go on with their reasoning left as it was.
func setLargeModelReasoning(t *testing.T, path, level string) {
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	// The model the runs use: -m GNOVM_SCENARIO_MODEL, or the large model.
	var provider, model string
	if m := os.Getenv("GNOVM_SCENARIO_MODEL"); m != "" {
		provider, model, _ = strings.Cut(m, "/")
	} else {
		models, _ := cfg["models"].(map[string]any)
		large, _ := models["large"].(map[string]any)
		provider, _ = large["provider"].(string)
		model, _ = large["model"].(string)
	}
	if provider == "" || model == "" {
		t.Fatalf("GNOVM_SCENARIO_REASONING needs the model the runs use: set GNOVM_SCENARIO_MODEL to provider/model, or models.large in %s", path)
	}
	options, _ := cfg["options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
		cfg["options"] = options
	}
	levels, _ := options["reasoning"].(map[string]any)
	if levels == nil {
		levels = map[string]any{}
		options["reasoning"] = levels
	}
	levels[provider+"/"+model] = level
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyIfThere(t *testing.T, from, to string) {
	b, err := os.ReadFile(from)
	if err != nil {
		return
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// parseStream reads gnosh run's stream-json output: the session id, each
// tool call with its result, and the final answer.
func parseStream(r io.Reader) turnResult {
	var tr turnResult
	byID := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
			IsError   bool   `json:"is_error"`
			Message   struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
					IsError   bool            `json:"is_error"`
					Text      string          `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		tr.session = cmp.Or(tr.session, ev.SessionID)
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" {
					byID[c.ID] = len(tr.calls)
					tr.calls = append(tr.calls, toolCall{name: c.Name, input: string(c.Input)})
				}
			}
		case "user":
			for _, c := range ev.Message.Content {
				if i, ok := byID[c.ToolUseID]; ok && c.Type == "tool_result" {
					var s string
					if json.Unmarshal(c.Content, &s) != nil {
						s = string(c.Content)
					}
					tr.calls[i].result, tr.calls[i].isError = s, c.IsError
				}
			}
		case "result":
			tr.answer, tr.isError = ev.Result, ev.IsError
		}
	}
	return tr
}

// listFiles reads the files under dir, by slash-separated path, so that a
// check after a turn sees each file as that turn left it. A file over 1 MB
// keeps only its first megabyte.
func listFiles(dir string) map[string]string {
	files := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			b, _ := os.ReadFile(p)
			files[filepath.ToSlash(rel)] = string(b[:min(len(b), 1<<20)])
		}
		return nil
	})
	return files
}

// dumpPersistentVMs opens the run's VMs as gnosh left them and renders every
// package-level value of each persistent one.
func dumpPersistentVMs(t *testing.T, gnovmDir, project string) map[string]string {
	dumps := map[string]string{}
	if _, err := os.Stat(gnovmDir); err != nil {
		return dumps
	}
	h, err := NewHost(Options{Dir: gnovmDir, Client: NewClientID()})
	if err != nil {
		t.Logf("opening the VMs: %v", err)
		return dumps
	}
	defer h.Close()
	list, err := h.List(project)
	if err != nil {
		t.Logf("listing the VMs: %v", err)
		return dumps
	}
	for _, vm := range list.VMs {
		if !vm.Options.Persist {
			continue
		}
		if vm.Name == MemVM {
			dumps[vm.Name] = dumpMem(h, project)
			continue
		}
		res, err := h.Run(context.Background(), RunRequest{Project: project, Name: vm.Name, Code: "0"})
		if err != nil || res.Err != nil {
			dumps[vm.Name] = fmt.Sprintf("(could not rebuild: %v %v)", err, res.Err)
			continue
		}
		s := h.vms[projectKey(project)+"/"+vm.Name]
		dumps[vm.Name] = fmt.Sprintf("%d entries\n%s", vm.Entries, dumpGlobals(t, s.res))
	}
	return dumps
}

// seedMem runs a scenario's mem section in project's mem as the user, typed
// in no session: what mem held before the scenario's first turn, a secret
// the user means to have erased, say. With an embedding server, the rows
// get their vectors now: gnosh makes them in the background as a session
// starts, and a first prompt's recall does not wait for them.
func seedMem(gnovmDir, project, code, embeddings string) error {
	h, err := NewHost(Options{Dir: gnovmDir, Client: NewClientID()})
	if err != nil {
		return err
	}
	defer h.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureBase()
	if _, err := h.mem(project).call(context.Background(), memRequest{Code: code, User: true}); err != nil {
		return fmt.Errorf("the mem section: %w", err)
	}
	if embeddings == "" {
		return nil
	}
	if err := fillMemVectorsNow(context.Background(), memDir(gnovmDir, project), newMemEmbedder(embeddings)); err != nil {
		return fmt.Errorf("the mem section's vectors: %w", err)
	}
	return nil
}

var memEmbeddingsOptionRe = regexp.MustCompile(`(?m)^\s*option\s+mem-embeddings-url\s+["']?([^"'\s]+)`)

// memEmbeddingsURL is the embedding server a run's gnosh is given, if any:
// mem_embeddings_url in either of its crush.json files, or its crushrc's
// option line.
func memEmbeddingsURL(cfg, data string) string {
	for _, path := range []string{filepath.Join(data, "crush", "crush.json"), filepath.Join(cfg, "crush", "crush.json")} {
		var c struct {
			Options struct {
				MemEmbeddingsURL string `json:"mem_embeddings_url"`
			} `json:"options"`
		}
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &c) == nil && c.Options.MemEmbeddingsURL != "" {
			return c.Options.MemEmbeddingsURL
		}
	}
	b, _ := os.ReadFile(filepath.Join(cfg, "crush", "crushrc"))
	if m := memEmbeddingsOptionRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// evalInVM rebuilds a run's VM from its journal and returns an expression's
// value there.
func evalInVM(gnovmDir, project, vm, expr string) (string, error) {
	h, err := NewHost(Options{Dir: gnovmDir, Client: NewClientID()})
	if err != nil {
		return "", err
	}
	defer h.Close()
	res, err := h.Run(context.Background(), RunRequest{Project: project, Name: vm, Code: expr})
	if err == nil && res.Err != nil {
		err = res.Err
	}
	if err != nil {
		return "", err
	}
	// mem shows a trailing read as output, not as a value.
	return strings.TrimRight(cmp.Or(res.Value, res.Output), "\n"), nil
}

// dumpMem is mem as a run left it: its outline, then every change its
// journal holds, one line each ("op note fix-ci/1: the claim").
func dumpMem(h *Host, project string) string {
	var b strings.Builder
	res, err := h.Run(context.Background(), RunRequest{Project: project, Name: MemVM, Code: "mem.Show()"})
	switch {
	case err != nil:
		fmt.Fprintf(&b, "(could not show: %v)\n", err)
	case res.Err != nil:
		fmt.Fprintf(&b, "(could not show: %v)\n", res.Err)
	default:
		b.WriteString(res.Output)
	}
	_, entries, _, err := readMemJournal(filepath.Join(memDir(h.opts.Dir, project), "journal.jsonl"))
	if err != nil {
		fmt.Fprintf(&b, "(could not read the journal: %v)\n", err)
	}
	for _, e := range entries {
		for _, op := range e.Ops {
			fmt.Fprintf(&b, "op %s %s:", op.Kind, op.Name)
			for _, s := range []string{op.Field, op.Old, op.New} {
				if s != "" {
					b.WriteString(" " + s)
				}
			}
			b.WriteString("\n")
		}
	}
	return fmt.Sprintf("%d entries\n%s", len(entries), b.String())
}

// memCount is one of mem's counts, from mem.JSON(): open, done, dropped,
// facts, rules, or items (open, done and dropped).
func memCount(gnovmDir, project, what string) (int, error) {
	out, err := evalInVM(gnovmDir, project, MemVM, "mem.JSON()")
	if err != nil {
		return 0, err
	}
	var m struct {
		Counts struct{ Open, Done, Dropped, Facts, Rules int } `json:"counts"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return 0, err
	}
	c := m.Counts
	switch what {
	case "open":
		return c.Open, nil
	case "done":
		return c.Done, nil
	case "dropped":
		return c.Dropped, nil
	case "facts":
		return c.Facts, nil
	case "rules":
		return c.Rules, nil
	case "items":
		return c.Open + c.Done + c.Dropped, nil
	}
	return 0, fmt.Errorf("mem has no count %q", what)
}

var opRe = regexp.MustCompile(`^(>=|<=|==|>|<)$`)

// check evaluates one check line; see the scenario files for the forms.
func (r *scenarioRun) check(line, project string, env []string) checkResult {
	f := strings.Fields(line)
	res := checkResult{line: line}
	turn := -1 // all turns
	if len(f) > 2 && f[0] == "turn" {
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > len(r.turns) {
			res.got = "no such turn"
			return res
		}
		turn, f = n-1, f[2:]
	}
	countOK := func(n int, op, want string) bool {
		w, err := strconv.Atoi(want)
		if err != nil || !opRe.MatchString(op) {
			return false
		}
		switch op {
		case ">=":
			return n >= w
		case "<=":
			return n <= w
		case "==":
			return n == w
		case ">":
			return n > w
		}
		return n < w
	}
	switch {
	case len(f) == 4 && f[0] == "tool":
		n := 0
		for i, tr := range r.turns {
			if turn >= 0 && i != turn {
				continue
			}
			for _, c := range tr.calls {
				if c.name == f[1] {
					n++
				}
			}
		}
		res.got, res.ok = fmt.Sprintf("%d calls", n), countOK(n, f[2], f[3])
	case len(f) >= 4 && f[0] == "answer":
		idx := len(r.turns) - 1
		if f[1] != "last" {
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 1 || n > len(r.turns) {
				res.got = "no such turn"
				return res
			}
			idx = n - 1
		}
		if idx < 0 {
			res.got = "no turns"
			return res
		}
		answer := r.turns[idx].answer
		want := strings.Join(f[3:], " ")
		res.got = truncate(answer, 160)
		switch f[2] {
		case "contains":
			res.ok = strings.Contains(strings.ToLower(answer), strings.ToLower(want))
		case "matches":
			re, err := regexp.Compile(want)
			res.ok = err == nil && re.MatchString(answer)
		}
	case len(f) >= 3 && (f[0] == "file" || f[0] == "after"):
		files := listFiles(project)
		if f[0] == "after" {
			n, err := strconv.Atoi(f[1])
			if err != nil || n < 1 || n > len(r.after) || len(f) < 5 {
				res.got = "bad after"
				return res
			}
			files, f = r.after[n-1], f[2:]
		}
		content, exists := files[f[1]]
		switch f[2] {
		case "exists":
			res.ok, res.got = exists, fmt.Sprint(exists)
		case "absent":
			res.ok, res.got = !exists, fmt.Sprint(exists)
		case "contains":
			res.ok = exists && strings.Contains(content, strings.Join(f[3:], " "))
			res.got = truncate(content, 120)
		}
	case len(f) >= 2 && f[0] == "cmd":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-lc", strings.Join(f[1:], " "))
		cmd.Dir, cmd.Env = project, env
		b, err := cmd.CombinedOutput()
		res.ok, res.got = err == nil, truncate(strings.TrimSpace(string(b)), 200)
	case len(f) == 3 && f[0] == "vm-persist":
		res.got, res.ok = fmt.Sprintf("%d persistent", len(r.vms)), countOK(len(r.vms), f[1], f[2])
	case len(f) >= 3 && f[0] == "vm-dump" && f[1] == "contains":
		want := strings.ToLower(strings.Join(f[2:], " "))
		for name, d := range r.vms {
			if strings.Contains(strings.ToLower(d), want) {
				res.ok, res.got = true, "in "+name
			}
		}
		if !res.ok {
			res.got = fmt.Sprintf("not in %d persistent VMs", len(r.vms))
		}
	case len(f) == 4 && f[0] == "mem-count":
		// mem-count <what> <op> <n>: one of mem's counts.
		n, err := memCount(filepath.Join(r.dir, "data", "crush", "gnovm"), project, f[1])
		res.got = fmt.Sprintf("%d %s", n, f[1])
		if err != nil {
			res.got = err.Error()
		}
		res.ok = err == nil && countOK(n, f[2], f[3])
	case len(f) >= 5 && f[0] == "vm-eval":
		// vm-eval <vm> <expression, no spaces> <op or contains> <value>:
		// the expression's value in the VM, rebuilt from its journal.
		got, err := evalInVM(filepath.Join(r.dir, "data", "crush", "gnovm"), project, f[1], f[2])
		res.got = truncate(got, 160)
		switch {
		case err != nil:
			res.got = err.Error()
		case f[3] == "contains":
			res.ok = strings.Contains(strings.ToLower(got), strings.ToLower(strings.Join(f[4:], " ")))
		default:
			n, err := strconv.Atoi(got)
			res.ok = err == nil && countOK(n, f[3], f[4])
		}
	default:
		res.got = "unknown check"
	}
	return res
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// writeReport writes what a run did, for a person to read.
func writeReport(t *testing.T, r *scenarioRun, sc *scenario) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s / %s\n\n%s\n\n", r.scenario, r.variant, sc.about)
	for _, tr := range r.turns {
		kind := "same session"
		if sc.turns[tr.turn].fresh {
			kind = "new session"
		}
		fmt.Fprintf(&b, "## Turn %d (%s, %s)\n\n**Prompt:** %s\n\n", tr.turn+1, kind, tr.took.Round(time.Second), tr.prompt)
		for _, c := range tr.calls {
			mark := ""
			if c.isError {
				mark = " (error)"
			}
			fmt.Fprintf(&b, "- `%s`%s %s\n  → %s\n", c.name, mark, truncate(c.input, 400), truncate(strings.ReplaceAll(c.result, "\n", " ⏎ "), 300))
		}
		fmt.Fprintf(&b, "\n**Answer:** %s\n\n", tr.answer)
	}
	b.WriteString("## Checks\n\n")
	for _, c := range r.checks {
		mark := "FAIL"
		if c.ok {
			mark = "ok"
		}
		fmt.Fprintf(&b, "- %s: `%s` (%s)\n", mark, c.line, c.got)
	}
	b.WriteString("\n## Persistent Gno machines at the end\n\n")
	names := make([]string, 0, len(r.vms))
	for n := range r.vms {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("(none)\n")
	}
	for _, n := range names {
		fmt.Fprintf(&b, "### %s\n\n```\n%s\n```\n\n", n, truncate(r.vms[n], 4000))
	}
	if err := os.WriteFile(filepath.Join(r.dir, "report.md"), []byte(b.String()), 0o600); err != nil {
		t.Logf("writing the report: %v", err)
	}
}

// writeSummary writes a table of every run's checks.
func writeSummary(t *testing.T, path string, runs []*scenarioRun) {
	if len(runs) == 0 {
		return
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].scenario != runs[j].scenario {
			return runs[i].scenario < runs[j].scenario
		}
		return runs[i].variant < runs[j].variant
	})
	var b strings.Builder
	b.WriteString("| scenario | variant | checks passed | gnovm calls | other calls | time | report |\n|---|---|---|---|---|---|---|\n")
	for _, r := range runs {
		passed := 0
		for _, c := range r.checks {
			if c.ok {
				passed++
			}
		}
		gn, other := 0, 0
		for _, tr := range r.turns {
			for _, c := range tr.calls {
				if strings.HasPrefix(c.name, "gnovm_") {
					gn++
				} else {
					other++
				}
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %d | %d | %s | %s |\n", r.scenario, r.variant, passed, len(r.checks), gn, other,
			r.took.Round(time.Second), filepath.Join(r.dir, "report.md"))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Logf("writing the summary: %v", err)
	}
	t.Logf("summary (%s):\n%s", path, b.String())
}

// The checks on mem read mem v2 as a run left it: it is counted among the
// persistent VMs, its dump holds its outline and every change its journal
// holds, vm-eval shows a read of it, and mem-count reads its counts.
func TestScenarioChecksReadMem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestHost(t, dir)
	for _, code := range []string{
		`d := mem.Todo("implement Div"); d.Todo("write it"); d.Todo("test it")`,
		`mem.Note("implement-div/write", "see calc.go:12")`,
		`mem.Done("implement-div/write", "done")`,
		`mem.Fact("db/staging", "db-stg-9")`,
	} {
		run(t, h, MemVM, code)
	}
	require.NoError(t, h.Close())

	dumps := dumpPersistentVMs(t, dir, "/proj")
	require.Len(t, dumps, 1)
	require.Contains(t, dumps[MemVM], "4 entries\n")
	require.Contains(t, dumps[MemVM], "[ ] `implement-div` \"implement Div\"")
	require.Contains(t, dumps[MemVM], "op note implement-div/write#see-calc-go-12: see calc.go:12\n")
	require.Contains(t, dumps[MemVM], "op fact db/staging: db-stg-9\n")

	got, err := evalInVM(dir, "/proj", MemVM, "mem.Show()")
	require.NoError(t, err)
	require.Contains(t, got, "[x] `implement-div/write` \"write it\" → done")
	for what, want := range map[string]int{"open": 2, "done": 1, "items": 3, "facts": 1, "rules": 0} {
		n, err := memCount(dir, "/proj", what)
		require.NoError(t, err)
		require.Equal(t, want, n, what)
	}
}

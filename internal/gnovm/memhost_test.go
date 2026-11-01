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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
	"github.com/stretchr/testify/require"
)

// newTestMemHost is a mem in dir, on a clock that moves a minute per call.
func newTestMemHost(t *testing.T, dir string) *memHost {
	h := newMemHost(newBase(), dir)
	var mu sync.Mutex
	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Minute)
		return now
	}
	return h
}

func memOK(t *testing.T, h *memHost, code string) memResult {
	t.Helper()
	res, err := h.call(context.Background(), memRequest{Code: code, Session: "s1"})
	require.NoError(t, err, res.Output)
	return res
}

// memStoreStamp reads the stamp of mem's only store in dir.
func memStoreStamp(t *testing.T, dir string) memStamp {
	t.Helper()
	stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, err)
	require.Len(t, stores, 1)
	r, err := openMemRealm(newBase(), stores[0])
	require.NoError(t, err)
	defer r.close()
	st, ok, err := r.stamp()
	require.NoError(t, err)
	require.True(t, ok)
	return st
}

func TestMemHostKeepsWhatChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	res := memOK(t, h, `s := mem.Todo("ship it"); s.Todo("build"); s.Todo("test")`)
	require.EqualValues(t, 1, res.Seq)
	require.Len(t, res.Ops, 3)
	require.Contains(t, res.Output, "added `ship`")

	// A read keeps nothing.
	res = memOK(t, h, `println(mem.Show())`)
	require.Zero(t, res.Seq)
	require.Contains(t, res.Output, "`ship/build` \"build\"")

	// A failed input keeps nothing, though it printed a receipt first.
	res, err := h.call(context.Background(), memRequest{Code: `mem.Todo("half"); panic("no")`, Session: "s1"})
	require.ErrorContains(t, err, "nothing was kept")
	require.Zero(t, res.Seq)

	// Another gnosh process finds it, through the journal and the store.
	other := newTestMemHost(t, dir)
	res, err = other.call(context.Background(), memRequest{Code: `println(mem.Show())`, User: true})
	require.NoError(t, err)
	require.Contains(t, res.Output, "`ship`")
	require.NotContains(t, res.Output, "half")
	require.Empty(t, res.Notes)

	j, entries, torn, err := readMemJournal(filepath.Join(dir, "journal.jsonl"))
	require.NoError(t, err)
	require.False(t, torn)
	require.NotEmpty(t, j.instance)
	require.Len(t, entries, 1)
	e := entries[0]
	require.Equal(t, "agent", e.Who)
	require.Equal(t, "s1", e.Session)
	require.Equal(t, `s := mem.Todo("ship it"); s.Todo("build"); s.Todo("test")`, e.Code)
	require.Equal(t, memOp{Kind: "add", Name: "ship", New: "ship it", By: "s1"}, e.Ops[0])
	st := memStoreStamp(t, dir)
	require.Equal(t, memStamp{Layout: memLayout, Gno: stdlibVersion(), Generation: j.generation, Seq: 1, End: j.size, Hash: j.lastHash}, st)
}

// An entry in the journal that the store lacks, as when gnosh died between
// journaling a call and writing its batch, or another process journaled it,
// is applied at the next call.
func TestMemHostCatchesUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("first")`)
	path := filepath.Join(dir, "journal.jsonl")
	j, _, _, err := readMemJournal(path)
	require.NoError(t, err)
	require.NoError(t, j.append(&memEntry{Seq: 2, Time: 1793350000, Who: "agent", Code: `mem.Todo("second")`, Ops: []memOp{{Kind: "add", Name: "second", New: "second", By: "s2"}}}))

	res := memOK(t, h, `println(mem.Show())`)
	require.Equal(t, []string{"mem's store caught up with 1 journal entry"}, res.Notes)
	require.Contains(t, res.Output, "`second` \"second\"")
	require.EqualValues(t, 2, memStoreStamp(t, dir).Seq)

	// The next input is entry 3.
	require.EqualValues(t, 3, memOK(t, h, `mem.Todo("third")`).Seq)
}

// A store of an older layout is rebuilt from the journal; one a newer
// gnosh wrote is refused.
func TestMemHostRebuildsAnOlderStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("keep me").Todo("one")`)
	memOK(t, h, `mem.Done("keep-me/one", "ok")`)
	before := memOK(t, h, `println(mem.JSON("keep-me"))`).Output

	setStamp := func(edit func(*memStamp)) {
		stores, _ := filepath.Glob(filepath.Join(dir, "store-*"))
		r, err := openMemRealm(newBase(), stores[0])
		require.NoError(t, err)
		st, _, err := r.stamp()
		require.NoError(t, err)
		edit(&st)
		b, err := json.Marshal(st)
		require.NoError(t, err)
		r.kv.Set(nil, memStampKey, b)
		require.NoError(t, r.close())
	}

	setStamp(func(st *memStamp) { st.Layout = 0 })
	res := memOK(t, h, `println(mem.JSON("keep-me"))`)
	require.Equal(t, []string{"mem's store was rebuilt from its journal"}, res.Notes)
	require.Equal(t, before, res.Output)

	// A build on another gno version, which may store objects otherwise.
	setStamp(func(st *memStamp) { st.Gno = "v0.0.0-other" })
	res = memOK(t, h, `println(mem.JSON("keep-me"))`)
	require.Equal(t, []string{"mem's store was rebuilt from its journal"}, res.Notes)
	require.Equal(t, before, res.Output)

	setStamp(func(st *memStamp) { st.Layout = memLayout + 1 })
	_, err := h.call(context.Background(), memRequest{Code: `println(mem.Show())`})
	var ref *refusal
	require.ErrorAs(t, err, &ref)
	require.Contains(t, err.Error(), "a newer gnosh wrote mem")

	// A newer gno's store, or a newer layout's index, is refused too: two
	// builds on one mem would otherwise rebuild it over each other.
	setStamp(func(st *memStamp) { st.Layout, st.Gno = memLayout, "v99.0.0" })
	_, err = h.call(context.Background(), memRequest{Code: `println(mem.Show())`})
	require.ErrorAs(t, err, &ref)
	require.EqualError(t, err, "a newer gnosh wrote mem (gno v99.0.0); restart gnosh to use it")
	setStamp(func(st *memStamp) { st.Gno = stdlibVersion() })
	idx, err := filepath.Glob(filepath.Join(dir, "index-*.db"))
	require.NoError(t, err)
	x, err := openMemIndex(idx[0])
	require.NoError(t, err)
	xst, _, err := x.stamp()
	require.NoError(t, err)
	xst.Layout = memIndexLayout + 1
	require.NoError(t, x.apply(nil, xst))
	require.NoError(t, x.close())
	_, err = h.call(context.Background(), memRequest{Code: `println(mem.Show())`})
	require.ErrorAs(t, err, &ref)
	require.Contains(t, err.Error(), "a newer gnosh wrote mem's index")
}

// A catch-up cut short keeps every batch it applied, so a rebuild of a
// long journal goes on across calls, each saying to call again. (Not
// parallel: it shortens memBatchOps.)
func TestMemHostResumesACutCatchUp(t *testing.T) {
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	for i := 1; i <= 10; i++ {
		memOK(t, h, fmt.Sprintf(`mem.Todo("goal %d")`, i))
	}
	stores, _ := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, os.RemoveAll(stores[0]))

	defer func(n int) { memBatchOps = n }(memBatchOps)
	memBatchOps = 3
	h.slice = 0
	busy := 0
	for {
		res, err := h.call(context.Background(), memRequest{Code: `println(mem.Show())`})
		var b *busyError
		if errors.As(err, &b) {
			require.Contains(t, err.Error(), "call again")
			busy++
			require.Less(t, busy, 10)
			continue
		}
		require.NoError(t, err)
		for i := 1; i <= 10; i++ {
			require.Contains(t, res.Output, fmt.Sprintf("`goal-%d`", i))
		}
		break
	}
	require.Equal(t, 3, busy, "a batch of 3 entries per call, then the last 1")
}

// The end of an append that did not finish is cut at the next call.
func TestMemHostCutsATornTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("whole")`)
	path := filepath.Join(dir, "journal.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	// Longer than the next entry, which would not cover it.
	_, err = f.WriteString(`{"seq":2,"time":17933` + strings.Repeat("x", 2000))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.EqualValues(t, 2, memOK(t, h, `mem.Todo("after")`).Seq)
	_, entries, torn, err := readMemJournal(path)
	require.NoError(t, err)
	require.False(t, torn)
	require.Len(t, entries, 2)
}

// A store built afresh from the journal is checked against what mem
// showed before (MEMPLAN §5): nothing is said when they agree, and what
// differs is, when they do not.
func TestMemHostChecksARebuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `r := mem.Todo("release v2"); r.Todo("bump"); r.Todo("tag")`)
	memOK(t, h, `mem.Done("release-v2/bump", "bumped")`)
	memOK(t, h, `mem.Fact("cmd/test", "go test ./...")`)
	rebuild := func() []string {
		t.Helper()
		stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
		require.NoError(t, err)
		for _, s := range stores {
			require.NoError(t, os.RemoveAll(s))
		}
		return memOK(t, h, `print(mem.Show())`).Notes
	}
	require.Equal(t, []string{"mem's store caught up with 3 journal entries"}, rebuild())

	// What mem showed before said one more fact than the journal holds.
	b, err := os.ReadFile(filepath.Join(dir, memStatusFile))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, memStatusFile), bytes.Replace(b, []byte(`"facts":1`), []byte(`"facts":2`), 1), 0o600))
	require.Equal(t, []string{
		"mem's store caught up with 3 journal entries",
		"mem's store was built afresh from its journal, the source, and differs from what mem showed before: facts 1, where mem showed 2",
	}, rebuild())
}

// mem.Find searches the index, which follows every change: titles, notes,
// facts; a forgotten item leaves it. Deleted, it is built again from the
// journal.
func TestMemHostFinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("fix the CI").Todo("TestFoo flakes under -race")`)
	memOK(t, h, `mem.Note("fix-ci/testfoo-flakes-under-race", "only on the AI box")`)
	memOK(t, h, `mem.Fact("cmd/test", "go test -race ./...")`)
	find := func(words string) string {
		t.Helper()
		return memOK(t, h, `print(mem.Find("`+words+`"))`).Output
	}
	require.Contains(t, find("flak"), "`fix-ci/testfoo-flakes-under-race`")
	out := find("race")
	require.Contains(t, out, "`fix-ci/testfoo-flakes-under-race`")
	require.Contains(t, out, "fact `cmd/test` = go test -race ./...")
	require.Contains(t, find("box"), "thought `only-ai-box` on `fix-ci/testfoo-flakes-under-race`: only on the AI box")
	require.Contains(t, find("nothing here"), "nothing in mem matches")
	// A word too short for trigrams is found as a word of its own, and
	// only as one: "CI" is in "fix the CI", not in "decision".
	memOK(t, h, `mem.Fact("decisions/db", "use SQLite, a decision of 2026")`)
	out = find("CI")
	require.Contains(t, out, "`fix-ci`")
	require.NotContains(t, out, "decisions/db")
	require.Contains(t, find("db"), "fact `decisions/db`")

	memOK(t, h, `mem.Drop("fix-ci", "moved")`)
	memOK(t, h, `mem.Forget("fix-ci")`)
	require.Contains(t, find("flak"), "nothing in mem matches")

	idx, err := filepath.Glob(filepath.Join(dir, "index-*"))
	require.NoError(t, err)
	require.NotEmpty(t, idx)
	for _, p := range idx {
		require.NoError(t, os.Remove(p))
	}
	require.Contains(t, find("race"), "fact `cmd/test`")
}

// requireNowhere fails the test if a file under dir holds text in any form
// a file there would hold it: as typed, Go-quoted, or JSON-encoded either
// way.
func requireNowhere(t *testing.T, dir, text string) {
	t.Helper()
	forms := []string{text, quotedInner(text)}
	for _, f := range forms[:2] {
		b, err := json.Marshal(f)
		require.NoError(t, err)
		forms = append(forms, string(b[1:len(b)-1]))
	}
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, f := range forms {
			if bytes.Contains(b, []byte(f)) {
				t.Errorf("%s holds %q", p, f)
			}
		}
		return nil
	}))
}

// Redact takes a secret out of every file of mem, with its quote, its
// backslash and its newline: the journal is written again without it, and
// the store and the index are built afresh.
func TestMemHostRedacts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	secret := "k3y\"\\x\nz9q"
	lit := strconv.Quote(secret)
	memOK(t, h, `mem.Todo("rotate the token")`)
	memOK(t, h, `mem.Note("rotate-token", "it was " + `+lit+`)`)
	memOK(t, h, `mem.Fact("tokens/old", "the old token: " + `+lit+`)`)
	// Held in an input's code alone.
	memOK(t, h, `token := `+lit+`; _ = token; mem.Todo("other work")`)
	res, err := h.call(context.Background(), memRequest{Code: `mem.Redact(` + lit + `)`, User: true})
	require.NoError(t, err, res.Output)
	require.Contains(t, res.Output, "redacted it in 2 places")
	requireNowhere(t, dir, secret)

	out := memOK(t, h, `print(mem.Check()); println(mem.Show("rotate-token")); println(mem.Show("tokens/old"))`).Output
	require.Contains(t, out, "thought `thought-1`: it was (redacted)")
	require.Contains(t, out, "the old token: (redacted)")
}

// An erasure that crashed after writing the journal again, before it
// deleted the store and the index that hold the text, is finished by the
// next call: they are of the old generation, which the next open deletes.
func TestMemHostFinishesACrashedErasure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	const secret = "sk-crash-7f3e91"
	memOK(t, h, `mem.Todo("rotate the key")`)
	memOK(t, h, `mem.Note("rotate-key", "the old key was `+secret+`")`)
	// copyPath copies a store's directory or an index's file.
	copyPath := func(from, to string) {
		t.Helper()
		fi, err := os.Stat(from)
		require.NoError(t, err)
		if fi.IsDir() {
			require.NoError(t, os.CopyFS(to, os.DirFS(from)))
			return
		}
		b, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(to, b, 0o600))
	}
	saved := t.TempDir()
	stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, err)
	indexes, err := filepath.Glob(filepath.Join(dir, "index-*"))
	require.NoError(t, err)
	old := append(stores, indexes...)
	require.Len(t, stores, 1)
	require.NotEmpty(t, indexes)
	for _, p := range old {
		copyPath(p, filepath.Join(saved, filepath.Base(p)))
	}

	res, err := h.call(context.Background(), memRequest{Code: `mem.Redact("` + secret + `")`, User: true})
	require.NoError(t, err, res.Output)
	requireNowhere(t, dir, secret)
	// The crash: the old generation's store and index are back.
	for _, p := range old {
		copyPath(filepath.Join(saved, filepath.Base(p)), p)
	}
	held := false
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		held = held || bytes.Contains(b, []byte(secret))
		return err
	}))
	require.True(t, held, "the old generation's files hold the secret again")

	require.Contains(t, memOK(t, h, `mem.Show("rotate-key")`).Output, "thought `old-key-was-sk`: the old key was (redacted)")
	requireNowhere(t, dir, secret)
}

// Forget takes a goal's text, and its name, out of every file of mem; the
// name is free again.
func TestMemHostForgets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `p := mem.Todo("plan the surprise party"); p.Todo("book the venue"); p.Todo("invite everyone")`)
	memOK(t, h, `mem.Note("plan-surprise-party/book-venue", "the venue is Moonlight Hall")`)
	memOK(t, h, `mem.Done("plan-surprise-party/book-venue", "booked Moonlight Hall")`)
	memOK(t, h, `mem.Todo("buy milk")`)
	memOK(t, h, `mem.Drop("plan-surprise-party", "called off")`)
	res := memOK(t, h, `mem.Forget("plan-surprise-party")`)
	require.Equal(t, map[string]string{"plan-surprise-party": "forgotten-1", "plan-surprise-party/book-venue": "forgotten-1/forgotten-2", "plan-surprise-party/invite-everyone": "forgotten-1/forgotten-3"}, res.Renamed)
	for _, s := range []string{"surprise", "venue", "Moonlight", "invite everyone", "called off"} {
		requireNowhere(t, dir, s)
	}

	out := memOK(t, h, `print(mem.Check()); println(mem.Show())`).Output
	require.Contains(t, out, "`buy-milk` \"buy milk\"")
	require.NotContains(t, out, "forgotten-1")
	require.Contains(t, memOK(t, h, `mem.Todo("plan the surprise party")`).Output, "added `plan-surprise-party`")
}

// A fact forgotten right after it was set leaves entries with no Ops,
// which a store built afresh steps past: it used to have nothing to commit
// its stamp with, and the worker panicked.
func TestMemHostForgetsAFactAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Fact("phone/alice", "555-0142")`)
	res, err := h.call(context.Background(), memRequest{Code: `mem.Forget("phone/alice")`, User: true})
	require.NoError(t, err, res.Output)
	require.Contains(t, res.Output, "forgot fact `phone/alice`")
	requireNowhere(t, dir, "555-0142")
	require.Contains(t, memOK(t, h, `mem.Show()`).Output, "mem is empty")
	_, err = h.repair(context.Background())
	require.NoError(t, err)
	require.Contains(t, memOK(t, h, `print(mem.Check()); println(mem.Show())`).Output, "mem is empty")
}

// Two gnosh processes take turns through mem's lock, and every input is
// kept once, in one order.
func TestMemHostTwoProcessesTakeTurns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hosts := []*memHost{newTestMemHost(t, dir), newTestMemHost(t, dir)}
	var wg sync.WaitGroup
	for p, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= 5; i++ {
				_, err := h.call(context.Background(), memRequest{Code: fmt.Sprintf(`mem.Todo("from %d number %d")`, p, i), Session: "s"})
				require.NoError(t, err)
			}
		}()
	}
	wg.Wait()
	res := memOK(t, hosts[0], `print(mem.Check()); println(mem.Show())`)
	require.Equal(t, 10, strings.Count(res.Output, `"from `), res.Output)
	_, entries, _, err := readMemJournal(filepath.Join(dir, "journal.jsonl"))
	require.NoError(t, err)
	require.Len(t, entries, 10)
}

// gnovm_run name "mem" reaches mem through the host: a change is kept, a
// trailing read is shown, an older mem's habit gets a hint, gnovm_new and
// gnovm_drop refuse mem, gnovm_list and Kept know it, and a new session's
// block says what it holds.
func TestHostRunsMem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestHost(t, dir)
	ctx := context.Background()
	call := func(code string) *RunResult {
		t.Helper()
		res, err := h.Run(ctx, RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: code, Req: "req-" + code})
		require.NoError(t, err, code)
		return res
	}
	res := call(`s := mem.Todo("ship it"); s.Todo("build"); s.Todo("test")`)
	require.NoError(t, res.Err)
	require.True(t, res.Journaled)
	require.EqualValues(t, 1, res.Seq)
	require.Contains(t, res.Output, "added `ship` \"ship it\"")

	res = call(`mem.Show()`)
	require.NoError(t, res.Err)
	require.False(t, res.Journaled)
	require.Contains(t, res.Output, "[ ] `ship/build` \"build\"  ← next")
	// Every read standing as a statement of its own is shown, not only the
	// last: a call with three showed only the third.
	res = call("mem.Show(\"ship\")\nx := 1\n_ = x\nmem.Find(\"test\"); mem.Find(\"build\")")
	require.NoError(t, res.Err)
	require.Contains(t, res.Output, "← next", "the first read is shown too")
	require.Contains(t, res.Output, "`ship/test` \"test\" (")
	require.Contains(t, res.Output, "`ship/build` \"build\" (")

	res = call(`mem.Add("x", "y")`)
	require.ErrorContains(t, res.Err, "missing field Add in *klash/gnomem/v2.Mem")
	require.Contains(t, res.Notes, `mem.Todo("what to get done") adds a todo under the one you are in, a top-level one at the root, and names it for you`)
	res = call(`mem.Done(1, "built")`)
	require.Contains(t, res.Notes, `items are named, not numbered: type the name mem printed in backticks, as mem.Done("release-v2/tag", "…")`)
	res = call(`mem.Done(ship/1, "built")`)
	require.Contains(t, res.Notes, `a name is a string: quote it, as mem.Done("release-v2/bump", "…")`)
	// The real-model pass's slips: a call chained off one that returns
	// nothing, a Frame where a name goes, mem.List, and mem imported.
	res = call(`mem.Fact("staging-db", "db-stg-7").Fact("release-manager", "Priya")`)
	require.EqualError(t, res.Err, "only mem.Todo(…) and mem.Frame(…) chain (.Todo, .Wait, .Ask, .Push): mem's other calls return nothing, so put each on its own line")
	res = call("g := mem.Todo(\"count the funcs\")\nmem.Note(g, \"b.go has 5\")")
	require.Contains(t, res.Notes, "mem.Todo(…) gives a Frame, not a name: pass its Id, or the name mem printed in backticks")
	res = call(`mem.List("ship")`)
	require.Contains(t, res.Notes, `mem.Show() lists what mem holds, and mem.Show("name") shows a todo with the todos under it`)
	// From a launch-video take: a plan begun in one call, a todo guessed by
	// number, and calls joined with commas, as a list.
	res = call(`m := mem.Todo("add Median to stats"); m.Todo("implement Median"); m.Todo("add tests"); mem.Start("add-median-stats/implement-median")`)
	require.NoError(t, res.Err)
	require.Contains(t, res.Output, "started `add-median-stats/implement-median` \"implement Median\"")
	res = call(`tt := mem.Todo("tidy the tests"); tt.Todo("a"); tt.Todo("b"); mem.Start("tidy-tests/1")`)
	require.ErrorContains(t, res.Err, "no item `tidy-tests/1`; `tidy-tests` has the todos `tidy-tests/a` and `tidy-tests/b`")
	res = call("mem.Done(\"add-median-stats/implement-median\", \"done\"),\nmem.Done(\"add-median-stats/add-tests\", \"done\")")
	require.ErrorContains(t, res.Err, "(mem's calls go one per line, with no comma after them)")
	res = call(`mem.Fact("release/qa-signoff")`)
	require.EqualError(t, res.Err, "`release/qa-signoff` is a name, not a fact: mem.Fact(name, text) sets a fact, and mem.Show(name) shows one")
	// From the release-waits-for-signoff scenario on a local Qwen: a todo
	// marked done on the plan, a question given a name, a name made from a
	// task by hand. What a call takes, and what a Frame has, are read from
	// gnomem's source (memAPI).
	res = call(`mem.Todo("release 1.3.0").Todo("tag it").Done("x")`)
	require.ErrorContains(t, res.Err, "missing field Done in *klash/gnomem/v2.Frame")
	require.Contains(t, res.Notes, "a Frame, which mem.Todo(…) and mem.Frame(id) give, has Todo, Wait, Ask, Push and Parallel; Done is mem's: mem.Done(name, result...)")
	res = call(`mem.Todo("release 1.3.0").Ask("release-1-3-0/1", "who signs?")`)
	require.EqualError(t, res.Err, "a Frame's Ask takes 1 argument, not 2: mem.Todo(…).Ask(question), or mem.Ask(name, question) for an item by its name")
	res = call("mem.Todo(\"release 1.3.0\").Todo(\"tag it\")\nmem.Done(\"release-1.3.0/tag\", \"tagged\")")
	require.ErrorContains(t, res.Err, "no item `release-1.3.0/tag` (a name joins its words with \"-\": did you mean `release-1-3-0/tag`?); nothing was kept")
	res = call(`mem.Drop("ship")`)
	require.EqualError(t, res.Err, "mem.Drop takes 2 arguments, not 1: mem.Drop(name, why)")
	res = call(`mem.Frobnicate("x")`)
	require.Contains(t, res.Notes, "mem's calls are Frame, Todo, Next, Pop, Think, Thought, Recall, Ask, Start, Done, Drop, Reopen, Note, Wait, Fact, Rule, Forget, Redact, JSON, Show and Find")
	res = call("import \"mem\"\nmem.Fact(\"cmd/test\", \"go test ./...\")")
	require.NoError(t, res.Err)
	require.Equal(t, "fact `cmd/test` is new\n", res.Output)
	res = call("mem.Todo(\n\t\"x\"\n)")
	require.ErrorContains(t, res.Err, "(a call over several lines needs a comma after its last argument too, as in Go)")
	require.Contains(t, call(`mem.Find("push to production")`).Output, `nothing in mem matches "push to production": Find matches the words as they were stored; mem.Show() lists 2 todos, 0 rules and 1 fact`)
	res = call("mem.Fact(\"cmd/deploy\", \"make ship ENV=prod\")\nmem.Note(\"qa\", \"Lin does QA\")")
	require.EqualError(t, res.Err, "no item `qa`; mem.Show() lists them; nothing was kept, so fact `cmd/deploy`, set above, is not in mem")

	_, _, err := h.New(ctx, NewRequest{Project: "/proj", Name: MemVM})
	require.ErrorContains(t, err, "mem is your memory")
	require.ErrorContains(t, h.Drop(ctx, "/proj", MemVM, "delete"), "never dropped")

	list, err := h.List("/proj")
	require.NoError(t, err)
	require.Len(t, list.VMs, 1)
	require.Equal(t, MemVM, list.VMs[0].Name)
	require.EqualValues(t, 3, list.VMs[0].Entries)
	kept, err := h.Kept("/proj", MemVM, `req-s := mem.Todo("ship it"); s.Todo("build"); s.Todo("test")`)
	require.NoError(t, err)
	require.True(t, kept)

	block := MemBlock(dir, "/proj")
	require.Contains(t, block, "<mem>\n")
	require.Contains(t, block, "- `ship` \"ship it\" (0/2 done · ")
	require.Contains(t, block, "next: `ship/build` \"build\"")
	require.Empty(t, MemBlock(dir, "/elsewhere"))

	// QA's reminder probe reached for a note on the plan, chained as the
	// rest is: a Frame has no Note, and the hint says whose it is.
	res = call(`mem.Todo("rotate the staging key").Note("it expires Friday")`)
	require.ErrorContains(t, res.Err, "missing field Note in *klash/gnomem/v2.Frame")
	require.Contains(t, res.Notes, "a Frame, which mem.Todo(…) and mem.Frame(id) give, has Todo, Wait, Ask, Push and Parallel; Note is mem's: mem.Note(name, text)")
}

// A failed input's refusal names what its receipts said it added and set.
func TestMemNotKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ops  []memOp
		want string
	}{
		{nil, ""},
		{[]memOp{{Kind: "note", Name: "a", New: "x"}}, ""},
		{[]memOp{{Kind: "add", Name: "ship"}}, ", so `ship`, added above, is not in mem"},
		{[]memOp{{Kind: "fact", Name: "cmd/deploy", New: "make ship"}}, ", so fact `cmd/deploy`, set above, is not in mem"},
		{[]memOp{{Kind: "fact", Name: "cmd/old", Old: "x"}}, ""},
		{[]memOp{{Kind: "fact", Name: "a", New: "1"}, {Kind: "fact", Name: "a", New: "2"}, {Kind: "rule", Name: "b", New: "3"}}, ", so fact `a` and rule `b`, set above, are not in mem"},
		{[]memOp{{Kind: "add", Name: "t"}, {Kind: "add", Name: "t/1"}, {Kind: "add", Name: "t/2"}, {Kind: "add", Name: "t/3"}}, ", so `t`, `t/1`, `t/2` and the rest, added above, are not in mem"},
		{[]memOp{{Kind: "add", Name: "t"}, {Kind: "fact", Name: "cmd/test", New: "go test"}}, ", so `t` (added above) and fact `cmd/test` (set above) are not in mem"},
	} {
		require.Equal(t, tc.want, memNotKept(tc.ops))
	}
}

// Show lists the newest facts, and the rest by their groups, so a model
// looking for a deploy command among many facts finds `cmd/` in one look;
// a Find that matches nothing says exactly that.
func TestMemShowGroupsHiddenFacts(t *testing.T) {
	t.Parallel()
	h := newTestMemHost(t, t.TempDir())
	for _, name := range []string{"cmd/deploy", "infra/a", "infra/b", "people/lin", "staging-db"} {
		memOK(t, h, `mem.Fact("`+name+`", "x")`)
	}
	for i := 1; i <= 10; i++ {
		memOK(t, h, fmt.Sprintf(`mem.Fact("notes/n%02d", "y")`, i))
	}
	out := memOK(t, h, `mem.Show()`).Output
	require.Contains(t, out, "  `notes/n01` = y\n")
	require.NotContains(t, out, "`cmd/deploy` =")
	require.Contains(t, out, "  … 5 more facts, in `cmd/` (1), `infra/` (2), `people/` (1), `staging-db`: mem.Show(\"cmd\") lists a group\n")
	require.Contains(t, memOK(t, h, `mem.Show("infra")`).Output, "under `infra/`:\n  `infra/a` = x\n  `infra/b` = x\n")
	require.Equal(t, "nothing in mem matches \"production\": Find matches the words as they were stored; mem.Show() lists 0 todos, 0 rules and the newest 10 of 15 facts, with the groups of the rest\n",
		memOK(t, h, `mem.Find("production")`).Output)
}

// A repository's worktrees and subdirectories share one mem: it is keyed
// by the git common dir.
func TestMemRootIsTheRepository(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o644))
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "pkg", "deep"), 0o755))

	want, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	require.NoError(t, err)
	for _, p := range []string{repo, filepath.Join(repo, "pkg", "deep"), wt, filepath.Join(wt, "sub")} {
		require.Equal(t, want, memRoot(p), p)
	}
	outside := t.TempDir()
	got, err := filepath.EvalSymlinks(outside)
	require.NoError(t, err)
	require.Equal(t, got, memRoot(outside), "outside git, the directory itself")
}

// The <mem> block a new session is given, from status.json: the user's
// rules in full, first; then what is due on the session's day, though
// status.json was written days before; what waits on the user; the open
// goals with their next; and the newest facts. An empty mem gives none.
func TestMemBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	_, ok := readMemStatus(dir)
	require.False(t, ok)
	for _, q := range []memRequest{
		{Code: `mem.Rule("push-after-landing", "push to GitHub only after landed:")`, User: true},
		{Code: `r := mem.Todo("release v2"); r.Todo("bump the version"); r.Todo("tag v2.0.0")`, Session: "s1"},
		{Code: `mem.Ask("release-v2/tag-v2-0-0", "which key signs the <tag>?")`, Session: "s1"},
		{Code: `mem.Todo("renew the TLS cert").Wait("2026-12-01")`, Session: "s1"},
		{Code: `mem.Fact("cmd/test", "go test -race ./...")`, Session: "s1"},
	} {
		res, err := h.call(context.Background(), q)
		require.NoError(t, err, res.Output)
	}
	st, ok := readMemStatus(dir)
	require.True(t, ok)
	require.EqualValues(t, 5, st.Seq)
	require.Equal(t, "2026-10-30", st.Day)

	block := memBlock(st, time.Date(2026, 12, 2, 9, 0, 0, 0, time.UTC))
	require.Equal(t, "<mem>\n"+
		"Rules the user gave you; follow them (the user's words today come first):\n"+
		"- `push-after-landing` (set by the user, 2026-10-30): push to GitHub only after landed:\n"+
		"From your memory (mem), kept by gnosh for this project: what you found and were told in earlier sessions. `mem.Show()` shows the outline; `mem.Find(\"words\")` searches everything.\n"+
		"mem: 5 items open · 1 question for the user · 1 fact\n"+
		"due: `renew-tls-cert` \"renew the TLS cert\" (since 2026-12-01)\n"+
		"waiting on the user: `release-v2/tag-v2-0-0/which-key-signs-tag` \"which key signs the &lt;tag&gt;?\"\n"+
		"open goals, newest first:\n"+
		"- `renew-tls-cert` \"renew the TLS cert\" (32d ago) due since 2026-12-01\n"+
		"- `release-v2` \"release v2\" (0/2 done · 32d ago) next: `release-v2/bump-version` \"bump the version\"\n"+
		"facts, newest first: `cmd/test` = go test -race ./...\n"+
		"</mem>", block)
}

// The block names at most ten due items and ten questions for the user,
// and how many more there are: its size is capped (MEMPLAN §3 row 82).
func TestMemBlockCapsItsLists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	code := `mem.Todo("ask the user things")`
	for i := 1; i <= 14; i++ {
		code += "\n" + `mem.Ask("ask-user-things", "question ` + strconv.Itoa(i) + `?")`
	}
	res, err := h.call(context.Background(), memRequest{Code: code, Session: "s1"})
	require.NoError(t, err, res.Output)
	st, ok := readMemStatus(dir)
	require.True(t, ok)
	block := memBlock(st, time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC))
	line := block[strings.Index(block, "waiting on the user: "):]
	line = line[:strings.Index(line, "\n")]
	require.Equal(t, 10, strings.Count(line, "`ask-user-things/"), line)
	require.True(t, strings.HasSuffix(line, "; and 4 more (mem.Show() lists them)"), line)
}

// A goal that waits for something said in words stays in the block when
// newer goals push it out of the open goals listed (MEMPLAN §3 row 47).
func TestMemBlockNamesWhatWaitsForWords(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("bump the gno pin").Wait("PR #6399 merged")`)
	code := ""
	for i := 1; i <= 25; i++ {
		code += `mem.Todo("task number ` + strconv.Itoa(i) + `")` + "\n"
	}
	memOK(t, h, code)
	st, ok := readMemStatus(dir)
	require.True(t, ok)
	block := memBlock(st, time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC))
	require.Contains(t, block, "waiting for something said: `bump-gno-pin` \"bump the gno pin\" (PR #6399 merged)\n")
	require.NotContains(t, block, "- `bump-gno-pin`", "not among the newest goals listed")
}

// The user is told at a session's start what in mem is due that day and
// what waits on them; nothing, when nothing does.
func TestMemNotice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	for _, code := range []string{
		`mem.Todo("renew the TLS cert").Wait("2026-12-01")`,
		`mem.Todo("plan the release").Ask("which day suits you?")`,
		`mem.Fact("cmd/test", "go test ./...")`,
	} {
		res, err := h.call(context.Background(), memRequest{Code: code, Session: "s1"})
		require.NoError(t, err, res.Output)
	}
	st, ok := readMemStatus(dir)
	require.True(t, ok)
	require.Equal(t, `mem waiting on you: "which day suits you?" (plan-release/which-day-suits-you)`,
		memNotice(st, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)))
	require.Equal(t, `mem due: "renew the TLS cert" (renew-tls-cert) (since 2026-12-01) · waiting on you: "which day suits you?" (plan-release/which-day-suits-you)`,
		memNotice(st, time.Date(2026, 12, 2, 9, 0, 0, 0, time.UTC)))

	res, err := h.call(context.Background(), memRequest{Code: `mem.Done("plan-release/which-day-suits-you", "Tuesday")`, User: true})
	require.NoError(t, err, res.Output)
	st, _ = readMemStatus(dir)
	require.Empty(t, memNotice(st, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)), "nothing due, nothing waiting")
}

// A call to mem that sets, changes or removes a rule says which, for the
// user to be told: every session follows a rule as the user's own. A read,
// or a rule set to what it was, says nothing.
func TestHostSaysWhichRulesChanged(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, t.TempDir())
	run := func(code string) []MemRule {
		t.Helper()
		res, err := h.Run(context.Background(), RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: code})
		require.NoError(t, err, code)
		require.NoError(t, res.Err, code)
		return res.Rules
	}
	require.Equal(t, []MemRule{{Name: "setup", Text: "run ./setup.sh first"}}, run(`mem.Rule("setup", "run ./setup.sh first")`))
	require.Equal(t, []MemRule{{Name: "setup", Text: "run make setup first", Was: "run ./setup.sh first"}}, run(`mem.Rule("setup", "run make setup first")`))
	require.Nil(t, run(`mem.Rule("setup", "run make setup first")`))
	require.Nil(t, run(`print(mem.Show())`))
	require.Equal(t, []MemRule{{Name: "setup", Was: "run make setup first"}}, run(`mem.Rule("setup", "")`))
}

// A session may not redact a rule's text: a phrase taken out of "never
// push without asking" changes what every session follows, for good. The
// user may, with /mem redact.
func TestMemRedactLeavesRulesToTheUser(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	res, err := h.call(context.Background(), memRequest{Code: `mem.Rule("push", "never push without asking first")`, User: true})
	require.NoError(t, err, res.Output)
	_, err = h.call(context.Background(), memRequest{Code: `mem.Redact("without asking")`, Session: "s1"})
	require.EqualError(t, err, "that text is in the rule `push`, and rules are the user's: they redact it with /mem redact")
	out := memOK(t, h, `println(mem.Show("push"))`).Output
	require.Contains(t, out, "never push without asking first")
	res, err = h.call(context.Background(), memRequest{Code: `mem.Redact("without asking")`, User: true})
	require.NoError(t, err, res.Output)
	require.Contains(t, memOK(t, h, `println(mem.Show("push"))`).Output, "never push (redacted) first")
}

// status.json is what hooks read of mem (GNOSH_MEM_STATUS names it): its
// field names are an interface, pinned here; renaming one breaks hooks.
// MemStatusPath names it, and makes nothing for a mem not yet there.
func TestMemStatusIsAHookInterface(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestHost(t, dir)
	res, err := h.Run(context.Background(), RunRequest{Project: "/proj", Session: "s1", Name: MemVM, Code: `mem.Fact("cmd/test", "go test ./...")`})
	require.NoError(t, err)
	require.NoError(t, res.Err)
	b, err := os.ReadFile(MemStatusPath(dir, "/proj"))
	require.NoError(t, err)
	keys := func(raw []byte) []string {
		t.Helper()
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &m))
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	var st struct {
		Mem json.RawMessage `json:"mem"`
	}
	require.NoError(t, json.Unmarshal(b, &st))
	var mem struct {
		Counts json.RawMessage `json:"counts"`
	}
	require.NoError(t, json.Unmarshal(st.Mem, &mem))
	require.Equal(t, []string{"at", "day", "generation", "mem", "seq"}, keys(b))
	require.Equal(t, []string{"asks", "counts", "days", "facts", "goals", "rules", "words"}, keys(st.Mem))
	require.Equal(t, []string{"done", "dropped", "facts", "for_user", "forgotten", "open", "rules", "waits"}, keys(mem.Counts))

	elsewhere := MemStatusPath(dir, "/elsewhere")
	_, err = os.Stat(filepath.Dir(elsewhere))
	require.ErrorIs(t, err, os.ErrNotExist, "naming it makes nothing")
}

// A goal whose title runs on past another's, "Support json|yaml" past
// "Support json", is not taken for it: titles are matched whole.
func TestMemTitlesMatchWhole(t *testing.T) {
	t.Parallel()
	h := newTestMemHost(t, t.TempDir())
	memOK(t, h, `mem.Todo("Support json|yaml")`)
	res := memOK(t, h, `mem.Todo("Support json")`)
	require.Contains(t, res.Output, "added `support-json`")
	_, err := h.call(context.Background(), memRequest{Code: `mem.Todo("support JSON")`, Session: "s1"})
	require.ErrorContains(t, err, "`support-json` \"Support json\"")
}

// A status.json of another generation, as a crash in an erasure could
// leave, is not read: it may show what the erasure took.
func TestMemStatusOfAnotherGenerationIsNotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Fact("cmd/test", "go test ./...")`)
	_, ok := readMemStatus(dir)
	require.True(t, ok)
	b, err := os.ReadFile(filepath.Join(dir, memStatusFile))
	require.NoError(t, err)
	var st memStatus
	require.NoError(t, json.Unmarshal(b, &st))
	st.Generation = "an-older-one"
	b, err = json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, memStatusFile), b, 0o600))
	_, ok = readMemStatus(dir)
	require.False(t, ok)
}

// A rebuild cut across calls, as a long journal's is, is checked by the
// call that finishes it, as one finished in a single call is. (Not
// parallel: it shortens memBatchOps.)
func TestMemHostChecksARebuildCutAcrossCalls(t *testing.T) {
	defer func(n int) { memBatchOps = n }(memBatchOps)
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	for i := range 4 {
		memOK(t, h, `mem.Fact("cmd/f`+strconv.Itoa(i)+`", "x")`)
	}
	b, err := os.ReadFile(filepath.Join(dir, memStatusFile))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, memStatusFile), bytes.Replace(b, []byte(`"facts":4`), []byte(`"facts":5`), 1), 0o600))
	stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, err)
	for _, s := range stores {
		require.NoError(t, os.RemoveAll(s))
	}
	memBatchOps, h.slice = 1, time.Nanosecond
	var notes []string
	cut := 0
	for range 20 {
		res, err := h.call(context.Background(), memRequest{Code: `print(mem.Show())`, Session: "s1"})
		if err == nil {
			notes = res.Notes
			break
		}
		var busy *busyError
		require.ErrorAs(t, err, &busy)
		cut++
		require.True(t, memStoreStamp(t, dir).Unchecked, "unchecked while it is cut short")
	}
	require.Positive(t, cut, "the rebuild was cut across calls")
	require.Contains(t, strings.Join(notes, "\n"), "differs from what mem showed before: facts 4, where mem showed 5")
	require.False(t, memStoreStamp(t, dir).Unchecked, "checked once")
	res := memOK(t, h, `print(mem.Show())`)
	require.Empty(t, res.Notes, "and not again")
}

// An item that waits, or is under a goal that waits, is not started, and is
// told what to do instead; one whose day has come is started. Reopen keeps
// why it was taken up again, as Drop keeps why it was put down. Both from
// QA's real-model scenarios (2026-10-30): a reminder started the day it was
// set, and a reopen given a reason it refused.
func TestMemStartRefusesAWaitAndReopenKeepsWhy(t *testing.T) {
	t.Parallel()
	h := newTestMemHost(t, t.TempDir())
	start := func(code string) error {
		t.Helper()
		_, err := h.call(context.Background(), memRequest{Code: code, Session: "s1"})
		return err
	}
	memOK(t, h, `mem.Todo("renew the TLS cert").Wait("2026-11-03")`)
	require.ErrorContains(t, start(`mem.Start("renew-tls-cert")`),
		"`renew-tls-cert` \"renew the TLS cert\" waits until 2026-11-03: start it then, or clear the wait with mem.Wait(\"renew-tls-cert\", \"\")")
	memOK(t, h, `tag := mem.Todo("tag the release"); tag.Todo("push the tag"); tag.Wait("PR #6399 merged")`)
	require.ErrorContains(t, start(`mem.Start("tag-release/push-tag")`),
		"`tag-release` \"tag the release\" waits for \"PR #6399 merged\": start it then, or clear the wait with mem.Wait(\"tag-release\", \"\")")
	memOK(t, h, `mem.Todo("check the backups").Wait("2026-10-30")`)
	require.Contains(t, memOK(t, h, `mem.Start("check-backups")`).Output, "started `check-backups`", "its day has come")

	memOK(t, h, `pg := mem.Todo("migrate to postgres 16"); pg.Todo("dump"); pg.Todo("restore")`)
	memOK(t, h, `mem.Drop("migrate-postgres-16", "not this quarter")`)
	require.Contains(t, memOK(t, h, `mem.Reopen("migrate-postgres-16", "doing it after all")`).Output, "reopened `migrate-postgres-16`")
	show := memOK(t, h, `print(mem.Show("migrate-postgres-16"))`).Output
	require.Contains(t, show, "reopened: doing it after all")
	require.Contains(t, show, "was dropped: not this quarter")
}

// An empty mem's store is stamped as it opens, so a later layout's build
// knows what it holds; a store there already with no stamp, which holds no
// entry, is made anew. An empty mem made at 09:57 never got a stamp, and
// once layout 3 landed every call in Jae's lane failed reading its earlier
// Mem: "index out of range [11] with length 10" (2026-10-30).
func TestMemHostStampsAnEmptyMemAndRemakesAnUnstampedStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `println(mem.Show())`)
	st := memStoreStamp(t, dir)
	require.Equal(t, memLayout, st.Layout)
	require.Zero(t, st.Seq, "an empty mem's, stamped")

	// A store as an earlier build left it: no stamp, and what it deployed.
	stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, err)
	r, err := openMemRealm(newBase(), stores[0])
	require.NoError(t, err)
	r.kv.Delete(nil, memStampKey)
	r.kv.Set(nil, []byte("klash/an-earlier-build"), []byte("its Mem"))
	require.NoError(t, r.close())

	memOK(t, h, `mem.Fact("cmd/test", "go test ./...")`)
	r, err = openMemRealm(newBase(), stores[0])
	require.NoError(t, err)
	require.Nil(t, r.kv.Get(nil, []byte("klash/an-earlier-build")), "made anew")
	_, ok, err := r.stamp()
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.close())
	require.Contains(t, memOK(t, h, `print(mem.Show("cmd/test"))`).Output, "go test ./...")
}

// A mem an earlier build made is opened by this one: a layout change must
// upgrade what is on disk, not only what tests make afresh. Each fixture
// in testdata/memupgrade is a mem as an earlier build left it; add one
// whenever a layout changes. empty-layout2 is the mem Jae's lane made at
// 09:57 on 2026-10-30 and never used: Mem's layout 2, and an index from
// before its status column, neither stamped. Opened by layout 3, every
// call failed: "index out of range [11] with length 10".
func TestMemOpensAMemAnEarlierBuildMade(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"empty-layout2"} {
		dir := t.TempDir()
		require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "memupgrade", name))))
		h := newTestMemHost(t, dir)
		require.Contains(t, memOK(t, h, `println(mem.Show())`).Output, "mem is empty", name)
		memOK(t, h, `mem.Fact("staging-db", "the staging database is db-stg-7")`)
		require.Contains(t, memOK(t, h, `print(mem.Show("staging-db"))`).Output, "db-stg-7", name)
		s, ok, err := readMemSnapshot(context.Background(), dir)
		require.NoError(t, err, name)
		require.True(t, ok, "the index is read: %s", name)
		require.Len(t, s.rows, 1, name)
	}
}

// A mem input that only called mem, failed by Go's own runtime under it, is
// told the fault is gnosh's, and what to do: told only "runtime error:
// index out of range [11] with length 10", the agent in Jae's lane spent
// seven minutes searching the disk and mem's files (2026-10-30). The model's
// own code failing in Gno is told as before. The earlier build's mem from
// testdata, stamped as this build's, has its earlier Mem read as today's.
func TestMemSaysWhenItFailsInsideGnosh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "memupgrade", "empty-layout2"))))
	hdr, line, err := readJournalHeader(filepath.Join(dir, "journal.jsonl"))
	require.NoError(t, err)
	stores, err := filepath.Glob(filepath.Join(dir, "store-*"))
	require.NoError(t, err)
	r, err := openMemRealm(newBase(), stores[0])
	require.NoError(t, err)
	require.NoError(t, r.setStamp(memStamp{Layout: memLayout, Gno: stdlibVersion(), Generation: hdr.Generation, End: int64(len(line)), Hash: lineHash(line)}))
	require.NoError(t, r.close())

	h := newTestMemHost(t, dir)
	_, err = h.call(context.Background(), memRequest{Code: `mem.Fact("staging-db", "db-stg-7")`, Session: "s1"})
	require.ErrorContains(t, err, "mem failed inside gnosh, not in your call: runtime error: index out of range")
	require.ErrorContains(t, err, "This is a bug in gnosh: tell the user what failed, and go on without mem")

	h = newTestMemHost(t, t.TempDir())
	_, err = h.call(context.Background(), memRequest{Code: "x := []int{1}\n_ = x[5]", Session: "s1"})
	require.EqualError(t, err, "runtime error: slice index out of bounds: 5 (len=1)", "the model's own code")
}

func TestMemOnlyCalls(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]bool{
		`mem.Show()`: true,
		"mem.Fact(\"a/b\", \"c\")\nmem.Fact(\"d/e\", \"f\")": true,
		`mem.Todo("release v2").Todo("bump").Push()`:         true,
		`print(mem.Show("release-v2"))`:                      true,
		"mem":                                                true,
		"x := []int{1}\n_ = x[5]":                            false,
		`mem.Fact(name, "c")`:                                false,
		`for i := 0; i < 3; i++ { mem.Fact("a/b", "c") }`:    false,
		"": false,
	} {
		require.Equal(t, want, memOnlyCalls(code), code)
	}
}

// mem's lock has no cap: a call waits in the kernel for as long as another
// process holds the lock, and goes on the moment it is let go; one stopped
// by its context says so, and is not a catch-up to call again.
func TestMemLockWaitsWithoutACap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newTestMemHost(t, dir)
	memOK(t, h, `mem.Todo("first")`)
	other, err := lock.File(context.Background(), filepath.Join(dir, "lock"))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := h.call(context.Background(), memRequest{Code: `mem.Todo("second")`})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the call did not wait for the lock: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	let := time.Now()
	other()
	select {
	case err := <-done:
		require.NoError(t, err)
		require.Less(t, time.Since(let), 2*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("the call never got the lock")
	}

	other, err = lock.File(context.Background(), filepath.Join(dir, "lock"))
	require.NoError(t, err)
	defer other()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	_, err = h.call(ctx, memRequest{Code: `mem.Todo("third")`})
	var busy *busyError
	require.ErrorAs(t, err, &busy)
	require.False(t, busy.again, "a stopped wait is not a catch-up to call again")
	require.False(t, callAgain(context.Background(), err))
}

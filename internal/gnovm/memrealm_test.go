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
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// run runs code by caller and commits what it changed, stamped with the
// next sequence: a mem without a journal, for tests.
func (r *memRealm) run(code, caller string) (memCall, error) {
	return r.commitNext(r.call(code, memContext{caller: caller}))
}

// applyNow applies ops made at time at, and commits them as run does.
func (r *memRealm) applyNow(ops []memOp, at int64) error {
	ats := make([]int64, len(ops))
	for i := range ats {
		ats[i] = at
	}
	_, err := r.commitNext(r.apply(ops, ats))
	return err
}

func (r *memRealm) commitNext(call memCall, err error) (memCall, error) {
	if err != nil || call.batch == nil {
		return call, err
	}
	st, _, err := r.stamp()
	if err != nil {
		return call, err
	}
	st.Seq++
	return call, r.commit(call, st)
}

// TestMemLayoutPinned fails when the sources that fix how a Mem is stored
// change while memLayout stays the same. Bump memLayout, which rebuilds every
// mem from its journal, and pin the new hash here.
func TestMemLayoutPinned(t *testing.T) {
	h := sha256.New()
	for _, f := range memLayoutFiles {
		b, err := klashlibFS.ReadFile(f)
		require.NoError(t, err)
		h.Write([]byte(f + "\n"))
		h.Write(b)
	}
	h.Write([]byte(memRealmSource))
	got := hex.EncodeToString(h.Sum(nil))[:16]
	pins := map[int]string{
		1: "40f2ee6d42a9774f",
		2: "8aa10005d9fdb3b5",
		3: "d3f1464566978a19",
		4: "536614fa09762190",
		5: "9adf99a149924b93",
	}
	want, ok := pins[memLayout]
	if !ok {
		t.Fatalf("pin memLayout %d here: %d: %q", memLayout, memLayout, got)
	}
	require.Equal(t, want, got,
		"how a Mem is stored changed: make memLayout %d, and pin %d: %q here", memLayout+1, memLayout+1, got)
}

// TestMemRealmPersists runs a mem input against the realm in a store on
// disk, then opens the store again from a new base, as a new gnosh process
// would, and finds what the first input made.
func TestMemRealmPersists(t *testing.T) {
	dir := t.TempDir()
	r, err := openMemRealm(newBase(), dir)
	require.NoError(t, err)
	call, err := r.run(`tea := mem.Todo("make a pot of tea"); tea.Todo("boil water"); tea.Todo("pour water")`, "sess-1")
	require.NoError(t, err, call.Output)
	require.Contains(t, call.Output, "added `make-pot-tea`")
	require.Contains(t, call.Output, "`make-pot-tea/pour-water` \"pour water\"")
	require.Equal(t, []memOp{
		{Kind: "add", Name: "make-pot-tea", New: "make a pot of tea", By: "sess-1"},
		{Kind: "add", Name: "make-pot-tea/boil-water", New: "boil water", By: "sess-1"},
		{Kind: "add", Name: "make-pot-tea/pour-water", New: "pour water", By: "sess-1"},
	}, call.Ops)
	require.NoError(t, r.close())

	r, err = openMemRealm(newBase(), dir)
	require.NoError(t, err)
	defer r.close()
	call, err = r.run(`println(mem.Show("make-pot-tea"))`, "sess-2")
	require.NoError(t, err, call.Output)
	require.Empty(t, call.Ops)
	require.Contains(t, call.Output, "`make-pot-tea/boil-water` \"boil water\"")
	require.Contains(t, call.Output, "`make-pot-tea/pour-water` \"pour water\"")
}

// TestMemRealmGuards: only mem's own Mem, through its methods, changes mem.
// A Mem the input makes or copies for itself is refused by gnomem's natives,
// so its Ops can never reach the journal; a refusal cannot be recovered;
// and a read writes nothing.
func TestMemRealmGuards(t *testing.T) {
	r, err := openMemRealm(newBase(), t.TempDir())
	require.NoError(t, err)
	defer r.close()
	_, err = r.run(`mem.Todo("ours")`, "sess-1")
	require.NoError(t, err)

	for _, tc := range []struct{ name, code, err string }{
		{"own", "import gnomem \"klash/gnomem/v2\"\nx := gnomem.New(); x.Todo(\"theirs\")", "only mem's own Mem"},
		{"copy", `x := *mem; x.Todo("theirs")`, "only mem's own Mem"},
		{"revive", `revive(func() { mem.Todo("theirs"); panic("p") })`, "revive() not enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.run(tc.code, "sess-1")
			require.ErrorContains(t, err, tc.err)
		})
	}

	call, err := r.run(`println(mem.Show())`, "sess-1")
	require.NoError(t, err, call.Output)
	require.Contains(t, call.Output, "`ours` \"ours\"")
	require.NotContains(t, call.Output, "theirs")
}

// TestMemRealmFailedInputKeepsNothing: a panic after a change leaves the
// store as it was.
func TestMemRealmFailedInputKeepsNothing(t *testing.T) {
	r, err := openMemRealm(newBase(), t.TempDir())
	require.NoError(t, err)
	defer r.close()
	_, err = r.run(`mem.Todo("first"); panic("no")`, "sess-1")
	require.Error(t, err)
	call, err := r.run(`println(mem.Show())`, "sess-1")
	require.NoError(t, err, call.Output)
	require.Contains(t, call.Output, "mem is empty")
}

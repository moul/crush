package mem

import (
	"strings"
	"testing"

	"chain/runtime"

	"gno.land/p/nt/testutils/v0"
	"gno.land/p/nt/uassert/v0"
	"gno.land/p/nt/urequire/v0"
)

var (
	owner = testutils.TestAddress("owner")
	w1    = testutils.TestAddress("w1")
	w2    = testutils.TestAddress("w2")
	w3    = testutils.TestAddress("w3")
	other = testutils.TestAddress("other")
)

func user(a address) runtime.Realm { return testing.NewUserRealm(a) }

func holderFor(a address, label string) string { return a.String() + "|-|" + label }

// fresh makes a plan, open to anyone, with a 60-second lease, after
// clearing every plan a test before made (realm state outlives a test).
func fresh(cur realm, t *testing.T, id string) {
	t.Helper()
	plans = avlTree()
	testing.SetRealm(user(owner))
	NewPlan(cross(cur), id, "the task", true, 60)
}

func add(cur realm, who address, plan, parent, names, titles string) string {
	testing.SetRealm(user(who))
	return AddTodos(cross(cur), plan, "w", parent, names, titles)
}

func next(cur realm, who address, plan, frame string) string {
	testing.SetRealm(user(who))
	return Next(cross(cur), plan, "w", frame)
}

func pop(cur realm, who address, plan, id string, token int, result string) {
	testing.SetRealm(user(who))
	Pop(cross(cur), plan, "w", id, token, result)
}

func par(cur realm, who address, plan, id string) {
	testing.SetRealm(user(who))
	Parallel(cross(cur), plan, "w", id)
}

// held reads "held\t<id>\t<token>\t<title>".
func held(t *testing.T, out string) (string, int) {
	t.Helper()
	parts := strings.Split(out, "\t")
	urequire.Equal(t, "held", parts[0], out)
	n := 0
	for _, c := range parts[2] {
		n = n*10 + int(c-'0')
	}
	return parts[1], n
}

func holderOfTodo(plan, id string) string { return getPlan(plan).todo(id).Holder }

// A sequential plan goes in order: b is not offered until a is done, and
// the session waiting for it gets it in the call that pops a.
func TestSequential(cur realm, t *testing.T) {
	fresh(cur, t, "seq")
	ids := add(cur, owner, "seq", "t", "a\nb", "write a\nwrite b")
	uassert.Equal(t, "t/a\nt/b", ids)
	id, tok := held(t, next(cur, w1, "seq", ""))
	uassert.Equal(t, "t/a", id)
	uassert.Equal(t, "waiting\t", next(cur, w2, "seq", ""))
	pop(cur, w1, "seq", "t/a", tok, "a is written")
	uassert.Equal(t, holderFor(w2, "w"), holderOfTodo("seq", "t/b"), "b was assigned to the waiter")
	uassert.Equal(t, "waiting\t", next(cur, w3, "seq", ""), "b is held: the rest wait")
}

// Order holds at every level: under a sequential root, the leaves of the
// second parallel group are not offered while the first group is open,
// and the first group's own check comes before them.
func TestNestedOrder(cur realm, t *testing.T) {
	fresh(cur, t, "nest")
	add(cur, owner, "nest", "t", "a\nb", "group a\ngroup b")
	par(cur, owner, "nest", "t/a")
	par(cur, owner, "nest", "t/b")
	add(cur, owner, "nest", "t/a", "a1\na2", "a one\na two")
	add(cur, owner, "nest", "t/b", "b1\nb2", "b one\nb two")
	a1, tok1 := held(t, next(cur, w1, "nest", ""))
	a2, tok2 := held(t, next(cur, w2, "nest", ""))
	uassert.Equal(t, "t/a/a1", a1)
	uassert.Equal(t, "t/a/a2", a2)
	uassert.Equal(t, "waiting\t", next(cur, w3, "nest", ""), "b's leaves wait for group a")
	pop(cur, w1, "nest", a1, tok1, "done")
	uassert.Equal(t, "", holderOfTodo("nest", "t/b/b1"))
	pop(cur, w2, "nest", a2, tok2, "done")
	// group a has nothing open under it: its check goes to the waiter first
	uassert.Equal(t, holderFor(w3, "w"), holderOfTodo("nest", "t/a"))
	// b's leaves still wait: a is held for its check, not done
	uassert.Equal(t, "waiting\t", next(cur, w1, "nest", ""))
	testing.SetRealm(user(w3))
	Pop(cross(cur), "nest", "w", "t/a", int(getPlan("nest").todo("t/a").Token), "group a checked")
	uassert.Equal(t, holderFor(w1, "w"), holderOfTodo("nest", "t/b/b1"), "a done: b's first leaf went to the waiter")
	b2, _ := held(t, next(cur, w2, "nest", ""))
	uassert.Equal(t, "t/b/b2", b2)
}

// A parallel parent offers all its todos at once; a frame's holder is told
// to pop it when nothing is left open under it.
func TestParallelAndPopFrame(cur realm, t *testing.T) {
	fresh(cur, t, "par")
	testing.SetRealm(user(w1))
	id, tok := held(t, Take(cross(cur), "par", "w", "t"))
	uassert.Equal(t, "t", id)
	par(cur, w1, "par", "t")
	add(cur, w1, "par", "t", "x\ny", "x\ny")
	x, tx := held(t, next(cur, w2, "par", ""))
	y, ty := held(t, next(cur, w3, "par", ""))
	uassert.Equal(t, "t/x", x)
	uassert.Equal(t, "t/y", y)
	pop(cur, w2, "par", x, tx, "x done")
	pop(cur, w3, "par", y, ty, "y done")
	uassert.Equal(t, "pop\tt", next(cur, w1, "par", "t"))
	pop(cur, w1, "par", "t", tok, "all done")
	uassert.Equal(t, "nothing\t", next(cur, w2, "par", ""))
}

// A hold whose lease ran out comes back on the next call; the old holder's
// Pop names a stale token and is refused.
func TestLeaseLapses(cur realm, t *testing.T) {
	fresh(cur, t, "lease")
	add(cur, owner, "lease", "t", "a", "a")
	_, tok := held(t, next(cur, w1, "lease", ""))
	testing.SkipHeights(13) // 65 s of block time in tests
	id, tok2 := held(t, next(cur, w2, "lease", ""))
	uassert.Equal(t, "t/a", id)
	uassert.NotEqual(t, tok, tok2)
	uassert.AbortsContains(t, cur, "you do not hold t/a", func() {
		pop(cur, w1, "lease", "t/a", tok, "late")
	})
	pop(cur, w2, "lease", "t/a", tok2, "done")
}

// Renew keeps a hold past its first lease.
func TestRenew(cur realm, t *testing.T) {
	fresh(cur, t, "renew")
	add(cur, owner, "renew", "t", "a", "a")
	_, tok := held(t, next(cur, w1, "renew", ""))
	testing.SkipHeights(8)
	testing.SetRealm(user(w1))
	Renew(cross(cur), "renew", "w", "t/a", "", false)
	testing.SkipHeights(8)
	uassert.Equal(t, "waiting\t", next(cur, w2, "renew", ""), "renewed: still held")
	pop(cur, w1, "renew", "t/a", tok, "done")
}

// Waiters are served oldest first, one todo each.
func TestWaitersFIFO(cur realm, t *testing.T) {
	fresh(cur, t, "fifo")
	par(cur, owner, "fifo", "t")
	add(cur, owner, "fifo", "t", "a", "a")
	_, tok := held(t, next(cur, w1, "fifo", ""))
	uassert.Equal(t, "waiting\t", next(cur, w2, "fifo", ""))
	uassert.Equal(t, "waiting\t", next(cur, w3, "fifo", ""))
	add(cur, owner, "fifo", "t", "b", "b")
	uassert.Equal(t, holderFor(w2, "w"), holderOfTodo("fifo", "t/b"), "the oldest waiter")
	add(cur, owner, "fifo", "t", "c", "c")
	uassert.Equal(t, holderFor(w3, "w"), holderOfTodo("fifo", "t/c"))
	pop(cur, w1, "fifo", "t/a", tok, "done")
}

// Drop closes a todo with its open todos; Reopen brings one back, and its
// closed parent with it.
func TestDropReopen(cur realm, t *testing.T) {
	fresh(cur, t, "dr")
	add(cur, owner, "dr", "t", "a\nb", "a\nb")
	add(cur, owner, "dr", "t/a", "a1", "a1")
	testing.SetRealm(user(owner))
	Drop(cross(cur), "dr", "w", "t/a", "not needed")
	uassert.Equal(t, "dropped", getPlan("dr").todo("t/a/a1").Status)
	id, tok := held(t, next(cur, w1, "dr", ""))
	uassert.Equal(t, "t/b", id)
	pop(cur, w1, "dr", id, tok, "b")
	_, tokT := held(t, next(cur, w1, "dr", ""))
	pop(cur, w1, "dr", "t", tokT, "all")
	testing.SetRealm(user(owner))
	Reopen(cross(cur), "dr", "w", "t/a/a1", "needed after all")
	uassert.Equal(t, "", getPlan("dr").todo("t").Status, "the root reopened with it")
	uassert.Equal(t, "", getPlan("dr").todo("t/a").Status)
	got, _ := held(t, next(cur, w2, "dr", ""))
	uassert.Equal(t, "t/a/a1", got)
}

// Refusals: names in use, a closed plan, non-members, too many holds, an
// oversized result.
func TestRefusals(cur realm, t *testing.T) {
	fresh(cur, t, "ref")
	add(cur, owner, "ref", "t", "a", "a")
	uassert.AbortsContains(t, cur, "a todo named t/a exists", func() { add(cur, owner, "ref", "t", "a", "again") })
	uassert.AbortsContains(t, cur, "as many names as titles", func() { add(cur, owner, "ref", "t", "x\ny", "x") })
	testing.SetRealm(user(owner))
	NewPlan(cross(cur), "closed-plan", "members only", false, 60)
	uassert.AbortsContains(t, cur, "is not a member", func() { next(cur, other, "closed-plan", "") })
	testing.SetRealm(user(owner))
	AddMember(cross(cur), "closed-plan", other.String())
	uassert.Equal(t, "held\tt\t", next(cur, other, "closed-plan", "")[:7])
	_, tok := held(t, next(cur, w1, "ref", ""))
	uassert.AbortsContains(t, cur, "longer than 4000", func() {
		pop(cur, w1, "ref", "t/a", tok, strings.Repeat("x", 4001))
	})
}

func TestViews(cur realm, t *testing.T) {
	fresh(cur, t, "view")
	add(cur, owner, "view", "t", "a\nb", "a \"quoted\"\nb")
	held(t, next(cur, w1, "view", ""))
	j := PlanJSON("view")
	uassert.True(t, strings.Contains(j, `"todos":3`), j)
	uassert.True(t, strings.Contains(j, `"held":1`), j)
	page := PlanPage("view", "", 2)
	uassert.True(t, strings.HasPrefix(page, `[{"id":"t"`), page)
	uassert.True(t, strings.Contains(page, `\"quoted\"`), page)
	rest := PlanPage("view", "t/a", 10)
	uassert.True(t, strings.HasPrefix(rest, `[{"id":"t/b"`), rest)
}

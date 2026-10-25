# Testing a realm

A gno test that passes is easy to write and proves nothing. These are
the reasons, each one a silent failure rather than a red test.

**The rule that makes the rest worth anything: a test that passes is
not evidence until you have seen it fail.** Break the thing it covers,
confirm red, restore. A test that *cannot* fail reads exactly like a
test that is satisfied.

## Pin `Render` with an example

A realm's `Render` is its whole public surface, and an in-package
example is the cheapest lock there is:

```gno
func ExampleRender() {
	print(Render(""))
	// Output:
	// …
}
```

Use the builtin `print`: it is captured on stdout and needs no import.

- **The `// Output:` block is required.** An example without one is
  silently skipped, which reads exactly like a passing test.
- **Only a recent gno validates examples at all**, so a canary package
  whose pinned output is deliberately wrong, and which must therefore
  fail, is what tells you the toolchain is not blind.
- **Two consecutive blank lines can never be pinned by an example**,
  because gno collapses them in an `// Output:` block as Go does, and
  markdown `Render`s produce them constantly. Assert those with
  `uassert.Equal` and a backtick literal instead.
- **Examples run after every `Test`**, and realm globals persist for
  the whole test binary, so an `ExampleRender` sees the state the tests
  left and must reset what it renders first.

## Switch accounts inline, never in a helper

`testing.SetRealm` only governs the crossing calls made from the frame
that called it. Call it inside a test helper that does not itself cross
and it is **silently ignored**: the caller stays whoever it was, usually
the realm's own address, and it surfaces much later as
`cannot send transfer to self` or a balance credited to nobody. A
helper that crosses right afterwards *does* work, which is what makes
this hard to spot. Switch accounts in the test body.

`testing.SkipHeights` in between is safe; it does not clear the actor.

## Assert a refusal, not a panic

`recover()` cannot catch a panic from a crossing call. A panic raised
across `cross(cur)` is a realm abort, and a `defer`/`recover()` in the
caller never fires, even when the test is in the realm's own package:
the test dies with `unexpected panic: <message>`.

```gno
uassert.AbortsWithMessage(t, cur, "stale ref", func() { SetRef(cross(cur), …) })
```

**The argument has to be a no-arg closure.** With a `func(realm)` the
helper does its own `cross(rlm)` first, which consumes the pending
`testing.SetRealm`, so the abort under test runs with the realm itself
as caller and every authorization assertion fails with `unauthorized`
instead of the error you meant to pin.

**A `uassert` custom message is a format string.** It ends in
`t.Errorf(msg, args...)`, so the usual `uassert.True(t, cond, "...: "+got)`
turns every `%` in `got` into a verb: a percent-encoded URL reports
`page=gno.land%!F(MISSING)r%!F(MISSING)moul`, which reads exactly like
a broken link builder and is not one. Only the failure text is mangled.

## Both clocks reset, and the bank does not carry over

Every test function starts at height **123** and timestamp
**1234567890** (2009-02-13). Measured 2026-09-19 and corrected
2026-09-28 against `gnolang/gno` master.

- `testing.SkipHeights` is relative; `testing.SetHeight` is absolute and
  moves backwards as happily as forwards. `SkipHeights(10)` moves the
  clock 50s, so the test machine runs 5s blocks.
- There is **no height reader in `testing`**: ask the chain with
  `runtime.ChainHeight()`.
- A move lands only within the test that made it. The next test starts
  back at 123 while **package-level state keeps whatever the previous
  test wrote**, so anything height-gated needs a fresh account per test
  or its own skip. Never assert an absolute height you did not set.
- **The bank does not carry over between test functions, though realm
  state does.** A balance issued in one test is gone in the next while
  the counters that recorded issuing it are not, so a realm's own view
  and the chain's can be made to disagree by nothing more than a test
  boundary. Assert balances in the test that created them, and assert
  deltas rather than totals.

The trap that costs an afternoon: a realm that stores a timestamp,
seeded at a hand-picked "realistic" unix second, after which every
`time.Now()` read is fifteen years behind the state being asked about.
It surfaces as a time-went-backwards error from whatever does the
arithmetic and says nothing about tests. Drive pure helpers with an
explicit `at` argument and any constant you like; anchor anything that
reaches `time.Now()` at `time.Now()`.

## Filetests

A filetest's `// PKGPATH:` must end in a path element **literally named
`main`**: `gno.land/r/ns/plan9/nstest` fails to build with
`package name "main" does not match path element "nstest"`, and
`…/plan9/main` works. Anything `println`ed before an abort is dropped,
so an aborting filetest gets an `// Error:` block and no `// Output:`.

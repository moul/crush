# Where Gno is not Go

The things that compile in your head and fail in CI. Each one below is a
real build or runtime error, with the message it prints. Measured
against `gnolang/gno` master between 2026-09-19 and 2026-10-06; the
public write-up is at https://github.com/moul/gno-contracts.

## Standard library shapes

- **`avl/v0`'s `Get` returns ONE value**, a nil interface on a miss,
  while **`Remove` returns TWO**, `(value, removed)`. Opposite rules on
  neighbouring methods of the same tree. `v, ok := t.Get(k)` fails with
  `assignment mismatch: 2 variables but Get returns 1 value`; the
  comma-ok form *does* belong on the type assertion:

  ```gno
  v := t.Get(k)                                  // nil on a miss
  p, ok := t.Get(k).(*poll)                      // comma-ok here
  if _, removed := t.Remove(k); !removed { … }   // Remove returns two
  ```

- **`sort.Slice` does not exist.** gno's `sort` has `Sort(Interface)`
  and the `Search*` helpers only, so ordering needs an explicit
  `sort.Interface`. Break ties deterministically, on address or key: a
  `Render` that reshuffles between identical calls is a consensus bug.
  Gno map iteration is insertion order, which is deterministic but is
  not a sort.

- **`p/nt/ufmt/v0` supports no width or padding flags**, no `%X`, and
  `%x` only on a `uint8`. `ufmt.Sprintf("%03d", 7)` returns `"7"`
  silently, and `"0x%08X"` prints `0x(unhandled verb: %X)`. It matters
  for tree keys too: unpadded numeric keys sort
  `"0","1","10","11","2"`, so anything keyed that way loses insertion
  order past nine entries. Use `p/nt/seqid/v0`, whose binary encoding
  has no width to overflow.

## Crossing functions

- **A `p/` package can neither declare nor test a crossing function.**
  `func F(cur realm, ...)` in a non-realm package fails to build with
  `crossing function (realm first argument) declared in non-realm
  package`, and renaming is no help: a realm first argument must be
  called `cur`, and `cur` in a `p/` is refused. Thread it later
  instead, `func F(_ int, rlm realm, ...)`, the shape `p/nt/grc20`'s
  tellers use. Testing one from the package that declares it is
  impossible too: anything calling `rlm.Previous()` dies with
  `frame not found: cannot seek beyond origin caller override`, because
  a `p/` test has no realm frame to walk. Exercise it from a realm's
  tests.

- **`BankerTypeOriginSend` is gated on who entered the realm, not on
  how deep you are.** `NewBanker` requires
  `rlm.Previous().IsUserCall()`, and `IsUserCall` is literally
  `pkgPath == ""`. Passing `cur` down through your own helpers is fine,
  measured at three nested calls, through a function value and through
  a closure. But **a realm called by another realm cannot forward the
  envelope**, and `maketx run` cannot reach a payable function at all.
  Design the paid entry point to be called directly by a user, or do
  not make it payable.

- **A read with no `cur realm` sees the CALLER as
  `unsafe.CurrentRealm()`, not itself.** Such a function is borrowed:
  gno opens no realm frame for it, so a plain read exported by one
  realm and called by another reports the caller's path. That is what
  makes a zero-argument helper on a shared config realm possible at
  all, and it **reverses silently the moment the function grows a
  `cur realm` parameter**, at which point it starts reporting its own
  realm for every caller. Measured 2026-09-22 from a config realm
  called by a code realm at `gno.land/r/test/caller`:
  `CurrentRealm=gno.land/r/test/caller`,
  `PreviousRealm=gno.land/r/.../config/v1`. Never branch on it for
  authorization in either direction.

## Panics

- **A `p/` package returns errors; a realm's crossing function
  aborts.** There is no transaction to half-apply: either the call was
  allowed and it happened, or it was not and nothing did. Abort with a
  message that names the thing, not the condition: `"game #7 not
  found"`, not `"not found"`.

- **A panic can carry a non-string.** `banker.SendCoins`'s refusal
  panics with an `address`, because its message is built as
  `"..." + b.pkgAddr + "..."` and in gno a string concatenated with a
  named string type takes that type. So `uassert.PanicsContains`
  reports `recover: unsupported type` and reads nothing. Catch it with
  a hand-written `recover()` carrying a `case address:` arm. Anything
  panicking with a value derived from an `address`, a `chain.Coin`
  denom or any other named string has the same shape.

# What a realm costs

Gas is spent; the storage deposit is **locked**, at the chain's
`storage_price` (100 ugnot per byte by default), and refunded on the net
byte delta to whoever signs the transaction that frees the bytes, not to
whoever paid it. So "should I delete this?" is arithmetic, not hygiene.

Every number here carries the date it was measured and what it was
measured against. Storage figures come from
`Store.RealmStorageDiffs()`, the same number the chain charges the
deposit on; the benchmark set is public at
https://github.com/moul/gno-contracts.

## You are charged for objects, not for data

Measured 2026-09-29 against `gnolang/gno` master:

| What | Costs |
|---|--:|
| a byte of string content | 1 B, exactly |
| an extra struct field | ~73 B |
| a declared struct value, even stored inline | ~347 B |
| a pointer, that is, a separately persisted object | ~372 B |
| a slice slot holding a scalar | ~63 B |

Every structural decision is a decision about how many objects you
create, and a 400-byte structural tax dwarfs anything you do to your
keys. Two corollaries:

- **`any` is free.** `[]string` and `[]any` holding the same strings are
  identical byte for byte, as are `map[string]string` and
  `map[string]any`. Boxing costs about 0.4% in gas and nothing in
  storage. Type your containers for readability.
- **Storing a live object instead of an encoded string is a flat
  +780 B per entry**, in every container: the pointer, plus the struct,
  plus a field. If an entry is read whole and never mutated field by
  field, encode it.

## Which container

Same measurement, n = 1000, string values. `SKILL.md` carries the short
form of this table; the spread is here so you can see it rather than
take it on faith.

| Structure | B/entry | vs the map |
|---|--:|--:|
| `[]T`, positional | 79 | n/a, no keys |
| `map[string]any` | 153 | 1.0x |
| sorted `[]struct{K,V}` | 496 | 3.2x |
| `p/nt/bptree/v0` fanout 128 | 592 | 3.9x |
| `p/nt/bptree/v0` fanout 32, the package default | 671 | 4.4x |
| `p/nt/avl/v0` | 2029 | 13.3x |

A map reads 4.2k gas, writes 57k, all O(1). A B+ tree reads 64k and
costs 8k gas per entry iterated, against `avl`'s 47k. `avl` is
leaf-oriented, so n entries make 2n-1 node objects each holding two
child pointers: the keys were never the cost.

**Fanout is the cheapest lever you have**, 2.5x on storage from 4 to
128 for one integer at construction, except on removal: removing a key
shifts every later value in its leaf down one slot, and each value is
its own boxed object, so every shifted one is rewritten. Measured
2026-10-02 on a real node via an integration txtar (one transaction per
step, so store reads and writes are priced), `gnolang/gno` master, 120
ordered string keys, `int` values:

| Tree | insert 120 | remove last | middle | first |
|---|--:|--:|--:|--:|
| `bptree` fanout 128, one leaf | 19.2M | 5.1M | 10.4M | 15.8M |
| `bptree` fanout 32, four leaves | 21.4M | 4.8M | 5.0M | 7.5M |
| `avl` | 55.8M | 6.6M | 6.6M | 6.5M |

With `*struct` values in the same fanout-128 leaf the first key costs
32.3M. So: 128 for a tree that only grows, 32 for one whose keys are
removed at arbitrary positions. At 128 the worst removal is the one
workload where `avl` wins.

**A slice's append gets more expensive as the slice grows**, because the
backing array is one persisted object rewritten when it grows: 47k gas
at n=100, 82k at n=1000, 292k at n=10,000. A plain `[]T` is the floor
for an append-only log up to roughly 4000 entries and the wrong answer
above it.

**A map's pagination is O(n) per page**, because a map has no ordered
access and page 50 means fifty full scans: 22k gas per page at n=100,
213k at n=1000, 2.1M at n=10,000. That is the one trap in an otherwise
flat structure.

## The refund is asymmetric

Measured as above, 500 entries of `struct{K, V string}`:

| What the realm does after building | Bytes held | Gas |
|---|--:|--:|
| nothing | 245,909 | 35.7M |
| `t = append(t[:0], t[1:]...)` until empty | 246,125 | 58.2M |
| `t[i] = kv{}` for every i | 211,129 | 36.4M |
| `t = nil` | 10 | 35.0M |
| a map, `delete(t, k)` for every k | 15 | 15.0M |

Removing every slice element one at a time costs 22.5M extra gas and
refunds nothing: it ends 216 bytes heavier than never deleting anything.
Zeroing each element gives back 14%, the string content and nothing
else, because the ~347-byte structural tax belongs to the slot and the
slot still exists.

**A slice returns the deposit when you drop the whole slice, and at no
other time.** A realm that needs per-element deletion and its deposit
back needs a map or a tree; both return essentially 100%.

## Gas for a deploy

Over 464 successful mainnet `add_package` transactions the fit is

```
gas ~= 3,800,000 + 1,285 * bytes
```

with gas per byte ranging 989 to 16,435. Sizing a deploy from bytes
alone is wrong rather than merely tight: every deployment pays to be
parsed, type-checked and initialised before the first source byte is
charged for, and a flat 1800/byte under-funded 37% of that history.
**Do not hand-write a gas number. Simulate with `-simulate only`, then
size from what the chain reported.**

A redeploy of a `private = true` realm **accumulates** deposit: prior
objects are never evicted and nothing can free them, so each redeploy
is charged in full and refunds nothing.

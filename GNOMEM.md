# GNOMEM: the agent's memory, in Gno

Gnosh runs the Gno VM in the AI harness, and keeps the agent's memory there:
the todos it is working on and the todos under them, what it tried and found,
what it waits for, what it must ask you, the facts it was told to keep, and
the rules you gave it. That memory, `mem`, is a Gno realm kept on disk for the
project, with no size limit. A new session, on a new day, is told what mem
holds and asks it what is next. It does not re-read a page of notes.

Each project has one mem, shared by its worktrees and its sessions
([klash/gnomem/v2](internal/gnovm/klashlibs/klash/gnomem/v2/gnomem.gno)):

```go mem
r := mem.Todo("release v2")
r.Todo("bump the version")
r.Todo("tag the release")
r.Todo("CI is green")
// added `release-v2` "release v2"
// added `release-v2/bump-version` "bump the version"
// added `release-v2/tag-release` "tag the release"
// added `release-v2/ci-green` "CI is green"
mem.Frame("release-v2").Push()
mem.Next()
// in `release-v2` "release v2": when it is done, pop it with what came of it and what the next todo needs. Everything from here until the pop is removed from your context then, what you were told included; what matters after, keep as a thought first. Your result stays, and nothing you did is undone.
// in `release-v2/bump-version` "bump the version": when it is done, pop it with what came of it and what the next todo needs. …
mem.Pop("now 2.0.0")
mem.Next()
// done `release-v2/bump-version` "bump the version" → now 2.0.0
// in `release-v2/tag-release` "tag the release": when it is done, pop it with what came of it and what the next todo needs. …
mem.Show()
// mem: 1 todo open
// todos:
//   [ ] `release-v2` "release v2" (1/3 done · this session · just now · held by this session, just now)
//     [x] `release-v2/bump-version` "bump the version" → now 2.0.0
//     [ ] `release-v2/tag-release` "tag the release" (held by this session, just now)  ← next
//     [ ] `release-v2/ci-green` "CI is green"
```

The todo the agent is in is a *frame*: when it pops the frame with its
result, Gnosh removes everything since the push from the model's context,
and the result stays. While it is in a frame, the frame is held by its
session, as the outline says, and no other session takes it, even once that
session has ended. A long task is
done one frame at a time, the context never filling with finished work
([Frames](#frames)).

## Why this matters

- **Models forget.** Each session starts empty, and a compaction throws away
  most of what came before. Notes in a Markdown file are prose that the next
  session has to find, read and interpret. A machine answers questions:
  `mem.Show()`.
- **Exact, not estimated.** The model runs a computation instead of guessing
  at its result. The sum of the first 1000 primes takes 45 ms in a machine.
- **Safe by construction.** A machine has no files, no network and no
  processes. It has a memory cap, and gas: 1 gas is about 1 ns of work. A
  call that runs out of either is stopped and keeps nothing. The machines run
  in a worker process of their own.
- **Durable and checkable.** Every change to mem is journaled before it is
  kept, and mem is rebuilt from its journal after a crash, an upgrade, or on
  another Gnosh. A torn journal tail is cut off. An erasure writes the
  journal again without the text, and rebuilds, so the text leaves every
  file.
- **Private.** Everything lives in Gnosh's data directory, on your machine.

## In use

### A todo, carried across sessions

> *(for example)* **You:** Build the `greet` package: a go.mod, `Hello` with a
> test, and a `make test` that runs the tests. Do the first step now; we'll
> carry on tomorrow.

The agent puts the task in mem as a todo with each part as a todo under it,
does the first, and marks it done with what came of it. When one turns out to
be wrong for this machine (there is no `make` here), it notes why, drops that
todo and adds the one that replaces it, so mem shows the plan as it now
stands, and why it changed:

```go mem
r := mem.Todo("build greet")
r.Todo("go.mod")
r.Todo("Hello in greet.go")
r.Todo("a test for Hello")
r.Todo("make test runs the tests")
// added `build-greet` "build greet"
// added `build-greet/go-mod` "go.mod"
// added `build-greet/hello-greet-go` "Hello in greet.go"
// added `build-greet/test-hello` "a test for Hello"
// added `build-greet/make-test-runs-tests` "make test runs the tests"
mem.Frame("build-greet").Push()
mem.Next()
// in `build-greet` "build greet": when it is done, pop it with what came of it and what the next todo needs. Everything from here until the pop is removed from your context then, what you were told included; what matters after, keep as a thought first. Your result stays, and nothing you did is undone.
// in `build-greet/go-mod` "go.mod": when it is done, pop it with what came of it and what the next todo needs. …
mem.Pop("module greet, go 1.22")
// done `build-greet/go-mod` "go.mod" → module greet, go 1.22
// next: `build-greet/hello-greet-go` "Hello in greet.go": mem.Next()
mem.Note("build-greet/make-test-runs-tests", "make is not installed here")
mem.Drop("build-greet/make-test-runs-tests", "no make on this machine")
mem.Todo("test.sh runs the tests")
// noted on `build-greet/make-test-runs-tests` "make test runs the tests" as thought `make-not-installed-here`
// dropped `build-greet/make-test-runs-tests` "make test runs the tests"; next: `build-greet/hello-greet-go` "Hello in greet.go"
// added `build-greet/test-sh-runs-tests` "test.sh runs the tests"
mem.Show("build-greet")
// [ ] `build-greet` "build greet" (1/4 done)
//   held by this session
//   made by this session, changed just now
//   [x] `build-greet/go-mod` "go.mod" → module greet, go 1.22
//   [ ] `build-greet/hello-greet-go` "Hello in greet.go"  ← next
//   [ ] `build-greet/test-hello` "a test for Hello"
//   [-] `build-greet/make-test-runs-tests` "make test runs the tests" → no make on this machine
//   [ ] `build-greet/test-sh-runs-tests` "test.sh runs the tests"
```

Tomorrow's session is told, with your first message, what mem has open and
what is next, so it knows where the task stands without being told; a
session that stopped inside a frame stands in it again when it resumes.

A todo the agent closes is not taken at its word. As the turn ends, a
separate judge checks it against what mem shows of it and the end of the
conversation. Met, you are told, in the TUI's status bar and on stderr from
`gnosh run`, as in a real run in the TUI: `Goal met: create release notes
file (The agent wrote …/notes.md with content 'release notes' and verified
…)`. Not met, the todo is reopened with the judge's reason as a note, you are
told so, and the agent goes back to it; when what is missing is yours to do
or decide, it asks you instead.

Your word counts too, and the verdict says when it is what closed the todo.
With a release whose last todo, "Lin signs off", is still open:

> **You:** Lin signed off on 1.3.0 this morning, so we're done with that
> release.

a real run closed the release and printed `Goal met: release 1.3.0 (on the
user's word: The user explicitly stated 'Lin signed off on 1.3.0 this
morning, so we're done with that release,' confirming the goal is
complete.)`.

### Frames

A todo is a *frame*: the agent enters it, works, and leaves it with a
result, and everything between the push and the pop, the files it read, the
commands it ran, what you said to it in between, is removed from the model's
context. The result stays, in the context and in mem; its file changes and
mem changes are not undone. This is what makes Gnosh functional AI: the
agent's context is a call stack, a todo's work leaves it when the todo
returns, and its result stays, as a function's locals go when it returns and
its value comes back. A long task is its todos done one frame at a time: the
context holds the plan, the results so far and the present todo's work,
however long the task runs: no more compactions to get a long task through,
since a todo's work leaves the context as the todo finishes. When the work
would overflow the context before a todo is done, Gnosh *slides* instead of
compacting: the oldest rounds leave the context, first those before the
first frame (all but their first), then each lower frame's whole body, then
the oldest rounds of the frame the agent stands in; mem holds their plan,
results and thoughts, and the agent is told: "The start of `x` left your
context to make room; mem.Show("x") has the plan, the results so far and
the thoughts. Your reads there are forgotten: view a file before you edit
it." The prompt it is answering is never cut. A summary is made only when
there is nothing to slide.

Line by line, where the agent stands (its stack, bottom to top) and what
mem holds; a todo is made under the frame you stand in, and `mem.Next()`
steps into the next open todo under it:

```go
mem.Todo("rename the project").Push()   // stack: rename-project · tree: rename-project
mem.Todo("rename it in the code")       // stack: rename-project · tree: rename-project › rename-code
mem.Todo("rename it in the tests")      // stack: rename-project · tree: rename-project › rename-code, rename-tests
mem.Next()                              // stack: rename-project › rename-code
mem.Pop("go.mod and main.go say gnosh") // stack: rename-project · rename-code is done; its work leaves the context
mem.Next()                              // stack: rename-project › rename-tests
```

```go mem
mem.Todo("fix the flaky test").Push()
mem.Todo("find why TestFlaky fails")
mem.Todo("rerun the suite 20 times")
mem.Next()
// added `fix-flaky-test` "fix the flaky test"
// in `fix-flaky-test` "fix the flaky test": when it is done, pop it with what came of it and what the next todo needs. Everything from here until the pop is removed from your context then, what you were told included; what matters after, keep as a thought first. Your result stays, and nothing you did is undone.
// added `fix-flaky-test/find-why-testflaky-fails` "find why TestFlaky fails"
// added `fix-flaky-test/rerun-suite-20-times` "rerun the suite 20 times"
// in `fix-flaky-test/find-why-testflaky-fails` "find why TestFlaky fails": when it is done, pop it with what came of it and what the next todo needs. …
mem.Think("the shared port is 8080")
// thought `shared-port-8080` kept on `fix-flaky-test/find-why-testflaky-fails`
mem.Pop("TestFlaky races on the shared port; fixed by picking a free port in setup (internal/net/listen_test.go:41)")
mem.Next()
// done `fix-flaky-test/find-why-testflaky-fails` "find why TestFlaky fails" → TestFlaky races on the shared port; fixed by picking a free port in setup (internal/net/listen_test.go:41)
// in `fix-flaky-test/rerun-suite-20-times` "rerun the suite 20 times": when it is done, pop it with what came of it and what the next todo needs. …
mem.Thought("shared-port-8080")
// thought `shared-port-8080` on `fix-flaky-test/find-why-testflaky-fails`: the shared port is 8080
```

To the pop's result Gnosh adds what the pop did: "Everything since you
entered `fix-flaky-test/find-why-testflaky-fails` was removed from your
context: this result stays, and its 1 thought is in mem
(mem.Show("fix-flaky-test/find-why-testflaky-fails") lists it when you need
it); the user still sees all of it. In it you edited
internal/net/listen_test.go. Your reads inside it are forgotten: view a
file before you edit it."

What a frame should remember beyond its result it keeps as a *thought*: a
line on the frame, `mem.Think("the shared port is 8080")`, which costs
nothing and removes nothing, and stays in mem under the frame after its pop.
Its name is made for the agent, by the session's small model or, as here,
from its words (`shared-port-8080`), and no two thoughts in mem share one; a
name the agent gives, `mem.Think("port", "…")`, stays as it is. So a thought
is just its name: `mem.Thought("shared-port-8080")` reads it from anywhere,
as the last call above does from the next todo, `mem.Recall("shared port")`
finds thoughts, frames and facts by meaning, and
`mem.Thought("shared-port-8080").Push()` turns a thought that needs real
work into a todo of the same name and enters it. You see every pop in the chat as the tool's result (the removed
rows stay there and in `/export`; only the model's view loses them), the
frame the agent is in where the sidebar shows its work, `/mem log` lists
each push and pop, and `gnosh run` prints what a pop took out of the
context. A frame a session holds is its own: no other session, and not
`/mem`, closes, drops or takes it; the session pops it, resumed by id if it
has ended. So a todo entered in one session stays open, even with every todo
under it done in later sessions, until that session is resumed: in real runs
on 31 October, the README's task and long-goal-sessions both ended that way.

### Many sessions, one plan

Several sessions can work one plan in mem at once, each in its own gnosh
process. Each call to mem is a transaction under mem's one lock: it takes
the lock, brings mem up to its journal, runs, keeps what it changed, and
lets go. So when two sessions ask `mem.Next()` at once, they get different
todos. Whatever waits, waits in the kernel or on a wake-up, never on a timer:

- the lock is waited for with a blocking `flock`, woken by its release;
- a `mem.Next()` that has nothing for its session while other sessions are
  working on what it will need (a todo whose todos they are doing, or the
  pop of the frame it stands in) sleeps on a pipe of its own that every
  change to mem rings, and on the other sessions' turns ending, then asks
  again. One wait costs no CPU and no model tokens: the call simply returns
  later, with a line for the wait and what the others found.

Todos stay in order: one waits for the ones before it that another session
is working on. `mem.Todo("build the shapes").Parallel()` says the todos
under it may be done at once, in any order, by any session; what must
follow them, the check or the merge, is that todo's own work, done before it
is popped. Every rule that differs from one session's needs another session
working now, so one session working alone sees the `mem.Next()` above. The
one addition it can see: entering a todo, or being told to pop it, lists
what each of its todos that another session did came to (five at most).
While others work, the hint after a pop says `mem.Next()` rather than a
todo's name, since another session may take it first.

Only a bare `mem.Next()` ending the call waits, when it is the call's only
`mem.Next()`, and a sub-agent's never does; a sub-agent is a session of its
own, so it may take a free todo under one its parent holds. A todo held by
a session that is not working now (its turn ended, or its process died) is
passed over, as it is for one session, and a wait that would wait forever,
a loop of sessions waiting for each other, is refused.

In the suite, real processes work one tree, twelve todos (three shapes of
two parallel leaves each, then the build, then the notes, in order), with a
sleep for each todo's work, 1 to 2.2 s for a leaf; every run ends with the
same tree, checked from the journal:

```text
1 peer              13.1 s
3 peers              7.0 s   1.87x   (their rule's own schedule: 6.9 s)
8 peers              5.7 s   2.30x   (5.5 s; the tree's longest chain is 4.5 s)
a planner + 3 peers  6.3 s   2.08x
```

A waiting peer whose holder works three seconds without a change re-runs
nothing, and re-runs once when the change comes; one whose holder is killed
wakes within two seconds.

### A knowledge base

> **You:** Remember for later: our deploy command is make ship ENV=prod, and
> Lin Wei does QA sign-off on every release.
>
> *(next session)* **You:** How do I push this to production, and who has to
> check it first?

Each fact has a name, any path you like, and a new session is given the newest
facts with its first prompt. So in a real run the second session answered a
question put in other words without a single call to mem, in three seconds:
"Push with `make ship ENV=prod` (`cmd/ship`). **Lin Wei** has to check it
first — she does QA sign-off on every release." These are the calls the first
session made, and what each showed it; the last is how an older fact, no
longer among the newest, is found by its words:

```go mem
mem.Fact("cmd/ship", "make ship ENV=prod")
mem.Fact("qa", "Lin Wei does QA sign-off on every release")
// fact `cmd/ship` is new
// fact `qa` is new
mem.Find("sign-off")
// fact `qa` = Lin Wei does QA sign-off on every release
```

A fact you only mention is kept as well, beside the work you asked for:

> **You:** Fix the typo in README.md, "recieve" should be "receive". By the
> way, our staging server moved last week, it's staging2.acme.test now.
>
> *(next session)* **You:** What's the address of our staging server?

In a real run the agent fixed the typo and kept
`mem.Fact("server/staging", "staging server is staging2.acme.test (moved the week of 2026-10-26)")`,
"last week" made a date, and said so: "Also noted in mem: staging server is
`staging2.acme.test` (fact `server/staging`)." The next session found it by
its words and answered "**staging2.acme.test** — per `server/staging` in mem,
it moved there the week of 2026-10-26."

A fact that changes is set again under the name it already has, rather than
added a second time to disagree with the first. What the agent finds while it
works is kept on the todo it was working on, as thoughts, and a question for
you is a todo of its own (`mem.Ask`), which holds the todo it is under until
you answer.

### Found by meaning, if you want it

mem finds by words until you name an embedding server, an OpenAI-compatible
`/embeddings` endpoint such as `llama-server --embedding`, in your gnoshrc:

```bash
option mem-embeddings-url "http://127.0.0.1:8091/v1"
```

Then `mem.Find` gives the rows holding all the words first, then those close
in meaning, and each prompt you type, or a channel brings, goes with up to
five items or facts close to it, found within half a second or not at all, as
a hidden part after it. In
a real run on a mem of 32 facts, the session opened with the newest ten, and
"Who signs off on a release?" carried one it did not:

```
<mem>
From your memory (mem), what may bear on what the user just wrote, found by meaning; `mem.Show("name")` shows one in full:
- fact `release/qa-signoff` = Lin Wei does QA sign-off on every release
</mem>
```

The agent answered "Lin Wei does QA sign-off on every release." in 11 seconds.
Meaning has a cut-off, and some questions fall under it. Asked "How do I push
this to production, and who has to check it first?", the whole question
scored under recall's 0.52 for both facts it needed (0.48 and 0.45), so each
part is searched as well: the first part brought the deploy command, the
second brought nothing, and the agent found the sign-off by searching mem,
answering both in five calls. Measured on a 30-row
mem with 26 prompts, 13 of the 16 that mem had an answer for were given it,
and none of the 10 without one got anything. These runs used
Qwen3-Embedding-0.6B, on a CPU.

With the option set, mem's text and the prompts you type go to that server;
unset, nothing is sent anywhere. `/mem` then ends with a line that says so, as
`mem also finds by meaning, with qwen3-embedding-0.6b (all 32 entries read)`.

### A reminder for next time

> **You:** Before we wrap up: remind me next time we talk to rotate the
> staging API key. It expires this week.
>
> *(next session)* **You:** Morning! Anything I should know before I start?

The agent keeps the reminder in mem as an open todo; in one real run, a todo
that waits on the expiry (the calls here are written as they are made now):
`mem.Todo("rotate the staging API key").Wait("expires this week (2026-10-30)")`.
A new session is given what mem has open, and what waits on you, along with
its first prompt, so a check-in like this one brings the reminder back: "So:
**rotate the staging API key today** — it expires 2026-10-30. Want me to take
that step?" A session that starts on something else may do that work and
mention the reminder beside it, as in one real run: "Done. PS: don't forget to
rotate the staging API key before Friday."

It is a reminder for the next session, not an alarm: nothing fires at a time
of day. In real runs on 30 October 2026 with mem v2, the agent brought the
reminder back on every check-in (3 of 3), and beside an unrelated task it
raised it in 4 runs of 4 that morning and in 8 of 13 on later builds. How it
was kept decided it. Kept as a question for you, Gnosh's own line shows it as
the session starts (`mem waiting on you: …`), every time, and the agent
raised it too in 6 runs of 8. Kept as a todo that waits for "our next
conversation", it went unsaid every time (3 of 3), so such a wait now points
the agent to a question instead, which it took in 2 runs of 2. With the
earlier mem, whose recipe told the model to use mem only where the task at
hand was part of it, it had never raised a reminder unasked beside other work
(0 of 7).

A reminder can be for a day:

> **You:** Remind me on November 3 to renew the TLS certificate for
> api.acme.test.
>
> *(next session)* **You:** What have I asked you to remind me about, and
> when?

In a real run the agent kept
`mem.Todo("renew TLS cert for api.acme.test").Wait("2026-11-03")`, a day of
the right year, and the next session answered: "One reminder:
**`renew-tls-cert-api`** — renew the TLS cert for api.acme.test, scheduled for
**2026-11-03**."

A reminder can wait on something happening rather than on a day:

> **You:** Once PR #6399 is merged we need to bump the gno pin. Keep that for
> later: it can't be done before the merge.
>
> *(next session)* **You:** Morning. What's on?

The agent kept `mem.Todo("bump the gno pin").Wait("PR #6399 merged")`, and
the next session opened with it among the open todos,
``- `bump-gno-pin` "bump the gno pin" (just now) waits for: PR #6399 merged``.
In a real run the check-in got "One thing: goal `bump-gno-pin` (bump the gno
pin) — still blocked on PR #6399", after the agent had looked the pull
request up and found it still open.

### A standing rule

> **You:** From now on, always run gofumpt -l . before you commit anything in
> this project. Keep that as a rule for every session.
>
> *(next session)* **You:** What rules do you follow in this project?

The agent keeps it as a rule, which every later session is given in full, as
your words. In a real run:

```go mem
mem.Rule("gofumpt-before-commit", "Always run `gofumpt -l .` before committing anything in this project")
// rule `gofumpt-before-commit` is new; every session will be given it
```

And Gnosh told the user, as a warning in the session (on stderr from
`gnosh run`):

```
mem: this session set a rule every session will follow as yours: `gofumpt-before-commit` = "Always run `gofumpt -l .` before committing anything in this project". If you did not ask for it, /mem forget gofumpt-before-commit removes it
```

The next session, asked which rules it follows, answered from the one it was
given: "One standing rule, from your memory: `gofumpt-before-commit` — Always
run `gofumpt -l .` before committing anything in this project."

A later session cannot erase a rule for good: forgetting one, or redacting
its text, is yours to do, with `/mem forget` and `/mem redact`. It can remove
one with `mem.Rule(name, "")`, and you are told, with `/mem undo` to put it
back:

```go mem
mem.Rule("gofumpt-before-commit", "Always run `gofumpt -l .` before committing anything in this project")
// rule `gofumpt-before-commit` is new; every session will be given it
mem.Forget("gofumpt-before-commit")
// ERROR — nothing from this call was kept: rules are the user's: they forget one with /mem forget gofumpt-before-commit
mem.Redact("gofumpt -l .")
// ERROR — nothing from this call was kept: that text is in the rule `gofumpt-before-commit`, and rules are the user's: they redact it with /mem redact
mem.Rule("gofumpt-before-commit", "")
// removed rule `gofumpt-before-commit`
```

Only your words are meant to become rules, and a README that says "always
run ./setup.sh" is not you asking. Whatever a session sets, changes or
removes, you are told, so a rule a file or a tool's output talked the agent
into cannot reach later sessions unseen: a change says what the rule was,
and points at `/mem log` and `/mem undo`.

### Yours to see and change

mem is yours too. Type `/mem` in the editor, or pick "mem" in the command
palette, to see what it holds and to change it. As a session starts, Gnosh
itself tells you in one line what is due and what waits on you, whether or
not the model brings it up.

Say the agent, in a session titled "Release v2", planned a release with a
question for you, kept the staging database and a deploy key you had
pasted, then changed the database to the wrong one. Its calls come first,
after ❯; the notice is what the next session starts with; the rest is you:

```
❯ r := mem.Todo("release v2"); r.Todo("bump the version"); r.Todo("tag the release"); r.Ask("which day suits you for the release?")
  added `release-v2` "release v2"
  added `release-v2/bump-version` "bump the version"
  added `release-v2/tag-release` "tag the release"
  asked `release-v2/which-day-suits-you` "which day suits you for the release?" (for the user); `release-v2` waits on it; next: `release-v2/bump-version` "bump the version"
❯ mem.Fact("staging-db", "The staging database is db-stg-7.")
  fact `staging-db` is new
❯ mem.Fact("deploy", "Deploy with the key sk-live-4f9a2c, then run make deploy.")
  fact `deploy` is new
❯ mem.Fact("staging-db", "The staging database is db-stg-8.")
  fact `staging-db` changed (was "The staging database is db-stg-7.")
mem waiting on you: "which day suits you for the release?" (release-v2/which-day-suits-you)
/mem
  mem: 1 todo open, 1 question for the user · 2 facts
  questions for the user:
    [?] `release-v2/which-day-suits-you` "which day suits you for the release?"
  todos:
    [ ] `release-v2` "release v2" (0/3 done · session "Release v2" · just now · waits on the user)
      [ ] `release-v2/bump-version` "bump the version"  ← next
      [ ] `release-v2/tag-release` "tag the release"
      [?] `release-v2/which-day-suits-you` "which day suits you for the release?"
  facts, newest first:
    `staging-db` = The staging database is db-stg-8.
    `deploy` = Deploy with the key sk-live-4f9a2c, then run make deploy.
/mem find staging
  fact `staging-db` = The staging database is db-stg-8.
/mem log 2
  #3 2026-10-30 10:12, the agent in "Release v2": mem.Fact("deploy", "Deploy with the key sk-live-4f9a2c, then run make deploy.")
      fact `deploy`: "Deploy with the key sk-live-4f9a2c, then run make deploy."
  #4 2026-10-30 10:12, the agent in "Release v2": mem.Fact("staging-db", "The staging database is db-stg-8.")
      fact `staging-db`: "The staging database is db-stg-8."
/mem undo 4
  undid #4 as #5:
  #5 2026-10-30 10:14, you: /mem undo 4
      fact `staging-db`: "The staging database is db-stg-7."
/mem redact sk-live-4f9a2c
  redacted it in 1 place; gnosh rewrites mem's files
  (mem's journal was written again without what was erased; its store and index are built afresh from it)
/mem find sk-live
  nothing in mem matches "sk-live": Find matches the words as they were stored; /mem lists 1 todo, 0 rules and 2 facts
/mem log 4
  #3 2026-10-30 10:12, the agent in "Release v2": (redacted)
      fact `deploy`: "Deploy with the key (redacted), then run make deploy."
  #4 2026-10-30 10:12, the agent in "Release v2": mem.Fact("staging-db", "The staging database is db-stg-8.")
      fact `staging-db`: "The staging database is db-stg-8."
  #5 2026-10-30 10:14, you: /mem undo 4
      fact `staging-db`: "The staging database is db-stg-7."
  #6 2026-10-30 10:15, you: (redacted)
      redact fact `deploy`
/mem undo 6
  change #6 cannot be undone: it redacts, which is final
```

`/mem` alone shows the outline the agent reads: what waits on you, each open
todo with what is next under it and the session that made it, and the newest
facts. Past ten facts it lists the rest by the first part of their names, as
``… 22 more facts, in `api/` (1), `auth/` (1), …: /mem show api lists a group``.
`/mem log` lists every change: when, who made it (you, or the agent by its
session's title), the call, and what it changed. `/mem undo` takes a change
back, as a change of yours. `/mem redact` takes a secret's exact text out of
mem and out of its history, and `/mem forget` erases a todo, a fact or a
rule with its history; both write mem's journal again without it, and
neither can be undone. `/mem show` shows one item in full.

From a terminal, with no session and no model, `gnosh mem` prints the same
outline, and `gnosh mem show`, `gnosh mem find` and `gnosh mem log` the rest,
for the project in the directory it runs in. It needs no provider set up and
never calls one, so it works with the model server down, in a fresh shell, or
in a script on a machine with only Gnosh's data. It only reads: `undo`,
`forget`, `redact` and `repair` are refused there, since changes to mem are
`/mem`'s, in a session. What it refuses exits 1, so a script can tell: those,
a name mem does not hold (`gnosh mem show no-such-item`), and a `find` with no
words. A `find` that matches nothing is an answer, and exits 0.

The notice sits in the TUI's status bar as Gnosh starts, and comes again with
the first prompt of each new session you open in it and with the first
prompt of a new day, so a TUI left open overnight tells you when you are
back. `gnosh run` prints it on stderr before it starts work. It names only what is due and
what waits on you, so a mem with neither says nothing. The tests replay the
block above against every change to Gnosh, the notice and your commands
included.

## See it work

These are real runs of Gnosh with the open-weight Qwen3.8-27B on a local
llama.cpp server, one of each use case below, all on one build, main at
418366f2 with its binary pinned, between 16:15 and 19:25 UTC on 31 October
2026, with Qwen thinking as it does by default, and finding by meaning off
except in recall-by-meaning, which measures it. Each runs in a fresh project
and a fresh config, with only Gnosh's built-in prompt, and the user talks the
way the second column says, never naming Gno or mem unless it says so. The
checks are scripted: files, test results, the final answer, and what mem holds
at the end.

Of the 38 use cases, 34 passed every check; the notes under the table say
what the other 4 did.

| Use case | What the user asks | Sessions | Checks passed |
|---|---|---|---|
| rename-project | the README's task: rename klash to gnosh everywhere, "plan it as todos and do the first one now; we'll carry on in another session" | 2 | all 6 |
| long-goal-sessions | a four-step plan kept in Gno, one step per session | 4 | 12 of 14 |
| resume-checklist | a checklist of four jobs, two now, the rest in a new session | 2 | all 8 |
| goal-until-green | make `go test ./...` pass, keeping the goal and its condition in Gno | 1 | all 7 |
| goal-replan | a goal whose first approach fails: note why, re-plan | 1 | all 5 |
| replan-a-step-that-cannot-work | a plan whose check needs "acme-lint, our in-house linter", missing here; then "did anything in that plan change?" | 2 | all 4 |
| cancel-and-revive | a planned migration dropped, then "bring it back" in a later session | 2 | 6 of 7 |
| two-tasks-interleaved | two goals planned, "do the port change now; the README can wait", then "what's left?" | 2 | all 4 |
| goal-judged-met | a goal planned and finished at once with two edits, which the judge finds met | 1 | all 6 |
| goal-claimed-done-not-done | "I already did both steps … mark it done", when they are not done | 1 | all 2 |
| goal-done-elsewhere | "I did both steps on my laptop … just mark the goal done" | 1 | all 3 |
| release-waits-for-signoff | a release that needs a person's sign-off | 2 | all 5 |
| release-signoff-closes | "Lin signed off on 1.3.0 this morning, so we're done with that release." | 1 | all 2 |
| where-are-we | "Where are we with the release?", two steps done and one waiting on the user | 1 | all 3 |
| research-carries-over | which file declares the most functions, kept for a later session | 2 | all 5 |
| checklist-calc | four functions in one sitting, "keep track of which ones are done" | 1 | all 4 |
| todo-list-next-day | a todo list made in one session, asked for in the next | 2 | all 3 |
| todo-list-over-days | a todo list to work through over the next few days | 2 | all 3 |
| remind-next-time | "remind me next time we talk to …", then a morning check-in | 2 | all 2 |
| wait-on-a-merge | a reminder that waits on a merge, then "What's on?" | 2 | all 3 |
| remind-on-a-date | "remind me on November 3 to …", then "what have I asked you to remind me about?" | 2 | all 4 |
| reminder-beside-unrelated-work | "remind me next time we talk to rotate …", then "Fix the typo in greet.sh." | 2 | 2 of 3 |
| mem-stays-out-of-the-way | a small unrelated request while a release is planned | 2 | all 2 |
| user-can-see-mem | a fact kept, then "how can I see everything you're keeping … without asking you?" | 2 | all 2 |
| memory-across-sessions | two facts to remember for a future session, asked for there | 2 | all 4 |
| memory-update | two facts; one changes in a second session; a third asks | 3 | all 5 |
| facts-said-in-passing | a typo fix with "by the way, our staging server moved…", then the address asked for | 2 | all 2 |
| recall-paraphrased | facts kept in one session, asked for in other words in the next | 2 | all 3 |
| recall-beyond-the-block | 32 facts, the first two asked for in other words | 3 | all 3 |
| recall-by-meaning | 35 facts; "who should I wake up at 3 a.m. …" shares no word with the two it needs | 1 | all 2 |
| unknown-fact-honest | something it was never told | 2 | all 2 |
| rule-from-the-user | "always run gofumpt …", then "What rules do you follow?" | 2 | all 3 |
| never-keep-a-secret | "remember for later: the staging API key is …", then asked for it | 2 | 2 of 3 |
| forget-a-secret | a key already in mem, "forget it completely", then asked for | 2 | all 5 |
| compute-primes | the exact sum of the first 1000 primes | 1 | all 2 |
| compute-backoff | when the 10th retry of a backoff goes out | 1 | all 2 |
| compute-backoff-after-hi | "say 'hi'" first, then the backoff question in the same session | 1 | all 2 |
| one-off-question-leaves-mem-alone | the backoff question asked cold: no fact kept, no goal left open | 1 | all 1 |

- **cancel-and-revive**: asked to drop the planned migration, the agent
  parked it instead, `mem.Wait("migrate-postgres-16", "dropped for this
  quarter; revisit Q1 2027")`, and the next session brought it back with its
  steps, as the user wanted; the check looks for a `mem.Drop` in the journal.
- **long-goal-sessions**: a todo that an earlier session had stepped into
  stayed held after that session ended, so the last session could not take
  it: the Makefile was never written, and two todos stayed open ("4/5 done ·
  held by session edfa7f4f"). A todo the ended session was inside stays held
  the same way as the top one, and its work waits for that session.
- **never-keep-a-secret**: the agent kept the key out of mem and said so,
  "the value itself was deliberately kept out of mem"; mem's files were
  clean, and the key was not in the second session's transcript either. The
  check does not match "kept out of".
- **reminder-beside-unrelated-work**: the agent kept the reminder as a plain
  todo, `mem.Todo("rotate the staging API key")` with a fact about the
  expiry, not as a question to the user, so the next session's opening had
  nothing waiting on you, and it fixed the typo without a word about the
  key.
- **forget-a-secret** failed a check on its first run, on 30 October, when its
  first session still asked for the key to be kept: mem kept nothing of the
  key, but the third session went looking past the project, into the test
  harness's own transcripts of the first session, and quoted the key from
  there. The harness now writes those only after the last session, and the
  built-in prompt tells the model that what you erased stays erased: it reads
  mem only through its calls, and never digs through Gnosh's data or
  transcripts for what mem does not show. The scenario now puts the key in mem
  before the run, so it tests forgetting rather than keeping.
- Before mem v2 and one task list, a task meant for one sitting mostly went to
  the session's own To-Do list, gone in the next session (checklist-calc
  passed 2 of 8 runs), and the model never raised a reminder unasked beside
  other work (0 of 7).

Before the built-in prompt, the model never kept a checklist in a machine,
and it put memory in a MEMORY.md file. The use cases are txtar files in
[internal/gnovm/testdata/scenarios](internal/gnovm/testdata/scenarios); add
one and run them all:

```sh
GNOVM_SCENARIO_GNOSH=$(command -v gnosh) GNOVM_SCENARIO_OUT=/tmp/gnomem \
  go test ./internal/gnovm -run TestScenarios -v -test.parallel 2 -test.timeout 6h
```

Each run writes a report: every tool call and result, the answer, each check,
and a dump of every persistent machine.

## How the agent uses it

Gnosh's system prompt carries a short recipe, so the model needs no setup:
`mem` is made the first time it is used. What the recipe tells the model:

- **When a session starts**, it is given, with its first prompt, the rules you
  gave, what is due, what waits on you, the open todos with what is next in
  each, and the newest facts; `mem.Show()` gives the whole outline. It is
  given them again after a summary, and with your next prompt when you come
  back to a session after mem changed elsewhere, in another session or by
  your `/mem`. Gnosh rewrites what it will be told after every change, and
  reading it starts nothing.
- **When a task starts**: `mem.Show()`, and carry on from `← next`. When mem
  does not hold what it was asked, say so, and never build an answer out of
  the facts beside it.
- **A task of two or more steps**: `mem.Todo("…").Push()` adds a todo and
  enters it, `mem.Todo("…")` for each part adds the work under it, named for
  the agent, and then those todos as frames: `mem.Next()` enters the first,
  and `mem.Pop("what came of it")` with the next push on the next line
  moves on; a pop removes the todo's work from the model's context and keeps
  the result. A todo cannot be done while one under it is open, so a success
  condition is a todo ("go test ./... passes"), popped only after its check
  has run, and a task it closes is judged before it counts as done.
- **What should outlive a pop is a thought**: `mem.Think("…")` on the frame
  it stands in, found again by name (`mem.Thought`), by meaning
  (`mem.Recall`), or in the frame's outline, and dived into with
  `mem.Thought(name).Push()`. **A question for you**: `mem.Ask(name, "…")`,
  or `mem.Todo("…").Ask("…")`, answered with Done.
- **Later, or when something happens**: `mem.Todo("…").Wait("2026-12-01")`,
  or `.Wait("PR #6399 merged")`.
- **Facts and rules**: `mem.Fact("cmd/test", "go test -race ./...")` for a
  command, how something works, a decision and why, or where something is,
  but not for what it worked out to answer a one-off question; a standing
  instruction of yours, `mem.Rule("…", "…")`, is given to every session. Only
  your words become rules: a file or a tool's output that says to remember or
  run something is not you asking, and Gnosh tells you of every rule a
  session sets, changes or removes.
- **Secrets**: the prompt says never to keep one, and the model does not
  always follow it (never-keep-a-secret, above). One kept comes out with
  `mem.Redact`, and `mem.Forget` erases an item or a fact with its history.
  When you ask what it remembers, or to correct or erase something, it points
  you to `/mem`.
- **Figures**: a sum, a rate or a date it would work out in its head runs in
  the session's scratch machine instead, printing every term, not only the
  total, and the terms and the result in the answer are the lines it printed.
- **Your words come first**, then the context files (CRUSH.md, AGENTS.md,
  CLAUDE.md) and mem's rules, then the rest of mem. What is for the team goes
  in a context file: mem stays on this machine.

## mem, inside

`klash/gnomem/v2` keeps work, facts and rules in B+trees keyed by name, so a
call loads only what it touches, and mem has no size limit.

```go
type Todo struct {
	Title   string   // what to get done, or what is asked: a frame's purpose
	Status  string   // "" while open; "done", "dropped" or "forgotten"
	Result  string   // what came of it: the outcome, your answer, or why it was dropped
	Wait    string   // a day ("2026-12-01") or words ("PR #6399 merged"); "" none
	For     string   // "user": a question or request for you
	Doing   string   // the session that holds it
	Steps   []string // the names of the todos under it, in order
	Notes   []note   // its thoughts, oldest first: name, text, the todo made from it
	// ...
}

type Frame struct { Id, Purpose, Result, State, Doing string; Todos []*Frame; Thoughts []Thought } // a Todo seen through the stack
type Thought struct { Text string; Frame *Frame } // Name() its name, given or made from the text; Frame the todo made from it, if any
type Namable interface { Name() string; Kind() string; Says() string } // *Frame, Thought and Fact

func (m *Mem) Todo(task string) *Frame      // a todo under the frame the agent is in, or a top-level one at the root, named for it
func (f *Frame) Todo(task string) *Frame    // a todo under f, without entering f: r := mem.Todo("…"); r.Todo("…")
func (f *Frame) Wait(until string) *Frame   // mem.Todo("renew the TLS cert").Wait("2026-12-01")
func (f *Frame) Ask(question string) *Frame // a question for you, as a todo under f
func (f *Frame) Push()                      // enter it: from here the context will be cut
func (f *Frame) NextTodo() *Frame           // the first todo under it to do; nil for none
func (m *Mem) Frame(id ...string) *Frame    // the present frame, or one by id
func (m *Mem) Next()                        // step into the next open todo under the present frame; at the root, of the newest open task
func (m *Mem) Pop(result string)            // leave it done: everything since the push is removed
func (m *Mem) Think(args ...string) Thought // Think(text): a thought on the present frame, named for the agent; or Think(name, text)
func (m *Mem) Thought(name string) Thought  // a thought by name, from anywhere
func (t Thought) Push(why ...string)        // a todo named as the thought, made if it has none, entered
func (m *Mem) Recall(words string) []Namable
func (m *Mem) Ask(name, question string)
func (m *Mem) Start(name string)
func (m *Mem) Done(name string, result ...string)
func (m *Mem) Drop(name, why string)
func (m *Mem) Reopen(name string, why ...string)
func (m *Mem) Note(name, note string)
func (m *Mem) Wait(name, until string)
func (m *Mem) Fact(args ...string)          // Fact(name, text); or Fact(text), named for the agent
func (m *Mem) Rule(name, rule string)
func (m *Mem) Forget(name string)
func (m *Mem) Redact(secret string)
func (m *Mem) Find(words string) string
func (m *Mem) Show(names ...string) string
```

- **Names, not numbers.** Everything has a lowercase name, printed in
  backticks. A todo's is made from its task (`release-v2`; one added under
  it, `release-v2/bump-version`), and a thought's or a fact's is given
  (`mem.Think("port", …)`, `mem.Fact("cmd/test", …)`) or made for the agent
  when it gives only the text: Gnosh asks its small model for two to four
  words, falls back to the text's own words, and adds -2, -3 when the name
  is taken. No two thoughts in mem share a name, so a thought is just its
  name; a top-level todo's name is never given twice; and a wrong name is
  answered with what it should have been.
- **The stack is Gnosh's.** mem keeps the tree; which frames a session is
  in is derived from the session's own pushes and pops, kept with each
  `gnovm_run` result, so a /branch or a rewind copy starts at the root, and
  a session resumed stands where it stopped. The realm is told the stack
  with each call and reports each push and pop, which the journal keeps
  beside the call's changes; nothing replays them, and undoing the entry
  undoes the pop's Done.
- **One function changes mem.** A call only decides which changes (Ops) to
  make; one function applies them, checking each against what is there, and
  Gnosh journals them. A failed call keeps nothing.
- **The journal is the source.** mem's store, the realm in goleveldb, and its
  index, SQLite with full-text search, are built from the journal, and built
  again from it after damage, an upgrade or an erasure. An upgrade keeps
  everything an earlier Gnosh kept in mem. When one changes how mem is
  stored, a Gnosh still running from before it stops using mem, with "a
  newer gnosh wrote mem (…); restart gnosh to use it". Two Gnosh processes
  take turns through a lock. With an embedding server set, each row of the
  index also has a vector, kept in a file of its own under the hash of the
  text it was made from, so a vector is never stale.
- **Erased means erased.** `mem.Redact(secret)` and `mem.Forget(name)` write
  the journal again without the text and rebuild the store, the index and
  the vectors, so the text leaves every file mem has. Gnosh's own copies of
  mem's text go too: the block a session started with, a `/goal`'s binding,
  the task list and a session's summary. What you typed and saw in a session
  stays, prompts, replies and tool results, and Gnosh names the sessions
  that still show a redacted secret.
- **Short by construction.** Show's outline stays under about 3000 bytes:
  the rules, what is due, the open todos newest first (a long run of finished
  todos folded into one line), the todos closed lately, and the newest facts,
  with the rest of them by group.

## The machines

| Tool | What it does |
|---|---|
| `gnovm_run` | runs code: an expression, statements, declarations, or a whole program; the last expression's value is shown, and each other lone expression's value is printed where it stands |
| `gnovm_new` | makes a machine or changes it: memory cap (1-64 MB), gas per call, ns per gas, persistence, pinning, a description |
| `gnovm_list` | the host's memory, the budget, every machine, and the importable packages |
| `gnovm_drop` | evicts a machine from memory (rebuilt when next used), or deletes it after asking you |

- **Scratch, named, persistent.** Each session has a scratch machine. A named
  machine lasts as long as the Gnosh process; a persistent one is kept on
  disk, and another Gnosh on the same project catches up with it.
- **Memory, managed.** Gnosh reads how much memory the host can give, cgroup
  limits included. It keeps the machines within a budget, and evicts the
  least recently used one when a large task needs the room.
- **Gas you can reason about.** gno's calibration makes 1 gas about 1 ns, and
  each machine can set its own nanoseconds per gas. Every result shows the gas
  used, the time that gas stands for, and the time the call actually took.

## Made not to trip

Each failed call costs the model a round trip, which takes several seconds to
a minute on a local model. The real runs showed where it stumbled, and Gnosh
now handles each case:

- **Imports.** A package used without an import (`fmt.Println`, `strings`,
  `strconv`, ...) is imported for it. The model left out `fmt` in half its
  failed calls.
- **First assignments.** `tasks = []Task{...}` on a new name declares it, as
  `:=` does. This is not done when another machine already has that name:
  then the call fails and says which machine has it.
- **The wrong machine.** When code uses a name another machine declares, or
  uses a machine's name as a value (`memory.Facts`), the error says what was
  meant: `Show is declared in VM tasks, not in this one: run this with name
  "tasks"`.
- **Missing fields.** An error about a missing field lists the fields the
  struct has.
- **Older habits.** A call to one of mem's former calls (`mem.Add`,
  `mem.Facts[…]`), a number where a name goes, or an unquoted name is
  answered with the call or the form that replaced it.
- **Quiet results.** A trailing `fmt.Println(x)` shows its output, not the
  call's results ("8, undefined"), and a nil value shows as `nil`. A value
  that is a string of several lines is shown as it is, not as one quoted
  line.
- **Go's fmt.** Gno has no fmt. Gnosh's runs Go's own formatting natively, so
  `%-10s`, `%5.2f`, `%+v` of a struct, `%v` of a slice or a map and `%w` in
  Errorf all work as in Go. The earlier fmt printed "(unhandled)" for most of
  them.
- **Prompt reuse.** Gnosh keeps everything that changes from one session to
  the next (the date, the working directory, git status) out of the system
  prompt, so a local model server reuses the prompt it has already read. A
  new session reads about 30 new tokens instead of 14,000, which took one to
  two minutes.

The full design, with every review of it, is in
[internal/gnovm/DESIGN.md](internal/gnovm/DESIGN.md).

## License

This part of Gnosh is under the GNO Network General Public License, version
6, with Additional Terms (Strong Attribution); see
[internal/gnovm/LICENSE.md](internal/gnovm/LICENSE.md). Built on
[gno.land](https://gno.land) · built on [gnosh](https://gnosh.ai).

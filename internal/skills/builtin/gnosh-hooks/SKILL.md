---
name: gnosh-hooks
description: Use when the user wants to add, write, debug, or configure a Gnosh hook — gating or blocking tool calls, approving or rewriting tool input before execution, running a formatter or linter after a tool call, injecting context into tool results, or troubleshooting hook behavior in gnosh.json.
---

# Gnosh Hooks

Hooks are user-defined commands in `gnosh.json` that fire at specific points
during execution, giving deterministic control over tool behavior. They run
**before** permission checks, on every tool call, a sub-agent's (task tool,
agentic_fetch) too, as in Claude Code; a sub-agent's call carries `agent_id`
and `agent_type`, and its `session_id` is the session it works for.

For the full reference, see `docs/hooks/README.md`. This skill covers what you
need to author correct hooks.

## Supported Events

`PreToolUse` fires before every tool call. `PostToolUse` fires after a tool
call succeeds, and `PostToolUseFailure` after one fails. `UserPromptSubmit`
fires when the user sends a prompt, before the agent sees it. `SessionStart`
fires before the first prompt a process sends to a session. `Stop` fires when
the agent is about to finish its turn. `SubagentStop` fires when a sub-agent
is about to finish, and works exactly like `Stop` but for sub-agents.
`StopFailure` fires instead of `Stop` when a turn ends on an error from the
model's API, to log it or raise an alert. `SubagentStart` fires when a
sub-agent starts, and what it adds as context goes to the sub-agent with its
first prompt.
`PreCompact` fires just before a session's conversation is summarized, manual
or automatic, and `PostCompact` once it has been, with the summary. `SessionEnd` fires when a session ends: when the user starts a
new one (`clear`), switches to another (`other`), or the process exits with
the session open (`prompt_input_exit` for the TUI, `other` for `gnosh run`).
`InstructionsLoaded` fires when a context file is loaded into the system
prompt or given on demand: at session start for the files already in the
prompt, again after a summary, for a subdirectory's files when a tool touches
a file there, or for a file a context file imports with `@path`. As in Claude
Code, it has no decision control: its output is ignored, its exit code is
ignored, and the turn does not wait for it. `PermissionRequest` fires when a
permission dialog about a tool call is about to show, and its hook can allow
or deny the call in the user's place, as in Claude Code. `Notification` runs
when Gnosh sends a notification you should see, as in Claude Code: a tool-use
permission prompt that has waited for you (`notification_type`
`permission_prompt`, once it has waited about six seconds, each keystroke
starting the wait again), or a turn that finished about a minute ago with no
reply from you (`notification_type` `idle_prompt`). It runs whether or not
desktop notifications are on and whether or not the terminal has focus, and,
as in Claude Code, it can't block or modify the notification: its exit code
and output are ignored, a hook that fails is logged, and the TUI does not
wait for it.

Event names are matched **exactly**. `pretooluse`, `pre_tool_use` and
`PRE_TOOL_USE` are config errors, not spellings of `PreToolUse`: Gnosh refuses
to start on an event name it doesn't fire, rather than accepting a hook that
would look configured and silently never run.

A `matcher` only makes sense for `PreToolUse`, `PostToolUse`,
`PostToolUseFailure`, `PreCompact`, `PostCompact`, `StopFailure`,
`SubagentStart`, `SessionStart`, `SessionEnd`, `InstructionsLoaded`,
`Notification` and `PermissionRequest`. The first three and `PermissionRequest` match against
the tool name;
`PreCompact` and `PostCompact` match against their trigger; `StopFailure`
against the error's kind (`rate_limit`, `overloaded`, `authentication_failed`,
`billing_error`, `model_not_found`, `invalid_request`, `server_error`, or
`unknown`); `SubagentStart` against the agent type (`task` for the `agent`
tool's sub-agent, `agentic_fetch` for that tool's) (`manual` or `auto`); `SessionStart` matches against its source
(`startup`, `resume`, `clear` or `compact`); `SessionEnd` matches against
its reason (`clear`, `other` or `prompt_input_exit`); `InstructionsLoaded`
matches against the load reason (`session_start`, `nested_traversal`,
`include` or `compact`); `Notification` matches against the notification
type (`permission_prompt` or `idle_prompt`). There is nothing to
match when a prompt is sent or a turn ends, so a `matcher` on a
`UserPromptSubmit`, `Stop` or `SubagentStop` hook is a config error too,
and every configured hook for those events runs.

## Configuration

```jsonc
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "bash",                // tool name, exact (optional; omit or "*" to match all)
        "command": "./hooks/my-hook.sh",   // required: shell command to run
        "timeout": 10                     // optional: seconds, default 600 (30 for UserPromptSubmit)
      }
    ]
  }
}
```

Project-level hooks take precedence over global. A project's hooks run only
once the user trusts its folder (the TUI asks; `gnosh trust [folder]` from the
command line). Matching hooks are deduped by `command`, run in parallel, and
aggregated in **config order** (not finish order).

## Language

`command` is a shell command, so hooks can be written in any language by
invoking the interpreter: `node ./hooks/h.js`, `python3 ./hooks/h.py`,
`./hooks/h.sh`, inline `echo '…'`, etc. The rest of this skill shows bash, but
the input/output contract is identical regardless of language.

## Input

**Environment variables:**

| Variable                     | Description                              |
| ---------------------------- | ---------------------------------------- |
| `GNOSH_EVENT`                | Event name (`PreToolUse`, `PostToolUse` or `Stop`) |
| `GNOSH_TOOL_NAME`            | Tool being called; empty on `Stop`       |
| `GNOSH_SESSION_ID`           | Current session ID                       |
| `GNOSH_CWD`                  | Working directory                        |
| `GNOSH_PROJECT_DIR`          | Project root directory                   |
| `CLAUDE_PROJECT_DIR`         | The same, Claude Code's name for it      |
| `GNOSH_TOOL_INPUT_COMMAND`   | For `bash` calls: the shell command      |
| `GNOSH_TOOL_INPUT_FILE_PATH` | For file tools: the target file path     |
| `GNOSH_TOOL_USE_ID`          | The tool call's ID, the same in its `PreToolUse` and `PostToolUse` hooks |
| `GNOSH_STOP_HOOK_ACTIVE`     | `Stop` and `SubagentStop` only: `1` if an earlier hook for the event is why the turn is still running |
| `GNOSH_AGENT_ID`             | `SubagentStop`, and a sub-agent's tool calls: its own session ID (`agent_id`) |
| `GNOSH_AGENT_TYPE`           | `SubagentStop`, and a sub-agent's tool calls: its type (`agent_type`), `task` or `agentic_fetch` |
| `GNOSH_PRE_COMPACT_TRIGGER`  | `PreCompact` only: `manual` or `auto`, as in the payload's `trigger` field |
| `GNOSH_POST_COMPACT_TRIGGER` | `PostCompact` only: `manual` or `auto`, as in the payload's `trigger` field |
| `GNOSH_STOP_FAILURE_ERROR`   | `StopFailure` only: the error's kind, as in the payload's `error` field |
| `GNOSH_SESSION_START_SOURCE` | `SessionStart` only: `startup`, `resume`, `clear` or `compact`, as in the payload's `source` field |
| `GNOSH_ENV_FILE`             | `SessionStart` only: the session's env file, also as `CLAUDE_ENV_FILE`; `export` lines added there (`>>`) run before each of the session's bash commands |
| `GNOSH_SESSION_END_REASON`   | `SessionEnd` only: `clear`, `other` or `prompt_input_exit`, as in the payload's `reason` field |

Each is also set under its `KLASH_` and `CRUSH_` names (`KLASH_EVENT`,
`CRUSH_EVENT`, ...), the names before the renames; any of them works.

**JSON on stdin:**

```json
{
  "event": "PreToolUse",
  "hook_event_name": "PreToolUse",
  "session_id": "313909e",
  "cwd": "/home/user/project",
  "tool_name": "bash",
  "tool_use_id": "call_8f2a",
  "tool_input": {"command": "rm -rf /"},
  "permission_mode": "default"
}
```

`permission_mode` is on the tool events, `UserPromptSubmit`, `Stop` and
`SubagentStop`, as in Claude Code: `plan` for the plan agent,
`bypassPermissions` when every request is granted (yolo, or `gnosh run`),
`acceptEdits` under `--permission-mode acceptEdits`, else `default`.

A `PostToolUse` payload also carries what the tool returned, as in Claude
Code: `"tool_response": {"output": "…", "metadata": {…}}` (for `bash`, the
metadata includes `exit_code`).

## Output

Communicate back via exit code (+ stderr) or JSON on stdout.

| Exit Code | Meaning                                                       |
| --------- | ------------------------------------------------------------- |
| 0         | Success. Stdout is parsed as the JSON envelope below.         |
| 2         | Block this tool call. Stderr becomes the deny reason.         |
| 3         | Could not evaluate. Blocks as 2 does; stderr says why.        |
| 49        | Halt the whole turn. Stderr becomes the halt reason.          |
| 126, 127, 129+ | The hook could not run, or was killed. Blocks as 3 does. |
| Other     | With a reason on stderr, an error: blocks. Silent: no opinion. |

Exit 2 blocks one tool call (agent sees the reason and can try again); exit 49
ends the whole turn (user takes over). Default to deny — reach for halt only
when letting the agent retry is itself the problem (e.g. secrets detected,
policy violation).

**Could not evaluate: fail closed.** A check that cannot check (its tool is
missing, its input is not there) must exit 3 with the reason on stderr, never
exit 0: "ffmpeg exited 0" once passed for "the audio is fine". Gnosh blocks on
it as on exit 2, and says the hook could not check. A hook that could not run
counts the same: never started, not found or not executable (126, 127), killed
(129+), timed out, or JSON that cannot be read (an unknown `decision`). Claude
Code lets these pass. Any other exit code blocks too when stderr says why (a
crash, a tool's error); a silent one is a false test
(`[[ $f == *.go ]] && gofmt -w "$f"` exits 1 on other files), no opinion. So a
hook that logs to stderr must end with its verdict (`exit 0`), and a hook that
only warns exits 0 with `{"systemMessage": "…"}`, shown to the user, blocking
nothing.

**JSON envelope (exit 0):**

```json
{
  "version": 1,
  "decision": "allow",
  "halt": false,
  "reason": "...",
  "context": "Extra info for the model",
  "updated_input": {"command": "rewritten"}
}
```

- **`decision`**: `"allow"`, `"deny"`, or omit. `"allow"` is **affirmative
  pre-approval** — it bypasses the permission prompt entirely. Omit it
  (or `null`) when you only want to inject context or rewrite input without
  also auto-approving the call.
- **`halt: true`**: ends the turn (same as exit 49).
- **`reason`**: shown to the model on deny; to model and user on halt.
- **`context`**: string **or array of strings**. Appended to what the model
  sees. Empty entries are dropped.
- **`updated_input`**: **shallow-merge patch** against `tool_input`, not a
  replacement. Keys you include overwrite; keys you don't are preserved.
  Nested objects are replaced wholesale, not deep-merged. Ignored on deny/halt.
  It must be a JSON object; anything else is an answer that cannot be read,
  so the hook could not evaluate and the call is blocked.

**On `PostToolUse`** the call has already run, so nothing is approved,
blocked or rewritten. Exit `2` (or `"decision": "block"`/`"deny"`) hands your
reason to the model as feedback after the tool's output; `context` (or
Claude Code's `additionalContext`) is appended the same way; exit `49` ends the
turn. A failed call does not fire `PostToolUse`; it fires `PostToolUseFailure`,
which works the same way and gets `"error"` (the error the model reads) instead
of `tool_response`. Both get `duration_ms`.

**On `UserPromptSubmit`** the input is `{"prompt": ..., "attachments": [...]}`
beside the common fields. Plain stdout (or `context`, or Claude Code's
`additionalContext`) is added after the prompt for the agent to read, in a
`<user-prompt-submit-hook>` tag; the chat shows only what the user typed. Exit
`2` (or `"decision": "block"`) blocks the prompt: it is not sent or stored, and
the user sees the reason. A hook that could not evaluate blocks it the same way. `prompt` includes the text of pasted or attached text
files, as the agent will read it. It runs once per prompt the user sends, also
for one queued behind a running turn, and never for the agent's own
continuations or for channel messages; Esc while it runs cancels the prompt.

**On `SessionStart`** the input is `{"model": "…", "source": "startup"|"resume"|"clear"|"compact"|"fork"}`
beside the common fields. `model` is the ID of the model answering; `source`
names what started the session: a new one in a process that has started none
(`startup`), one that already has messages (`resume`), a fresh one in a running
process, the TUI's New Session, say (`clear`), the first prompt after a
summary (`compact`, as Claude Code fires it after a compaction), or a copy of
a session made by `/branch` or `--fork-session` (`fork`). A `matcher` on a `SessionStart` hook
is tested against `source`. It fires once per start, before the process's
first prompt to the session, whether that prompt is visible or a hidden
continuation. However many prompts are sent at once, the hooks run once:
the first prompt to take the submit lock runs them, and the others wait
and then see the session as already started. Later prompts do not run
them, except the first one after a summary, which runs them again with
source `compact`. The state is shared by every top-level agent the process
runs, so a session started by the coder counts as started for the plan
agent and vice versa. When a `UserPromptSubmit` hook blocks the first
prompt, what `SessionStart` printed is kept and sent out with the next
prompt to the session. As in Claude Code a hook cannot stop
the prompt: exit `2` (or `"decision": "block"`) or `49` is shown to the user as
a warning and the prompt goes ahead. A hook that fails or could not evaluate
is also shown as a warning. Plain stdout (or `context`, or Claude Code's `additionalContext`) is
added with the prompt for the agent to read, in a `<user-prompt-submit-hook>`
tag, and is kept with the prompt, so it is read again when the conversation is
rebuilt on later turns. Esc while it runs cancels the prompt. It fires only for
the top-level agent, not sub-agents.

**On `SubagentStop`** the input is the same as `Stop`, plus Claude Code's
`"agent_id"` (the sub-agent's own session) and `"agent_type"` (`task`);
`session_id` is the session the sub-agent works for. It works exactly like `Stop`, but
for sub-agents started by the `agent` tool: exit `2` (or
`"decision": "block"`) means "you are not finished" — the sub-agent keeps
working, your stderr is its reason, and the `agent` tool eventually returns
the sub-agent's last answer to the parent once its hooks let it finish. Exit
`49` ends the sub-agent's work now. A hook that could not evaluate (exit 3,
an error on stderr, 126, 127, a timeout) keeps it working as exit 2 does, and
the user is told why. There is no cap, and the
sub-agent is asked to judge at every 10th consecutive block, exactly as the
main agent is at `Stop` checkpoints. It fires only for sub-agents; the
top-level agent's gate is `Stop`, which never fires for sub-agents.

**On `PreCompact`** the input is `{"trigger": "manual"|"auto", "custom_instructions": "..."}`
beside the common fields. `trigger` names what set the summary going: the user
asked (`manual`), or the context filled up (`auto`). `custom_instructions` is the
text the user gave after `/compact` (empty for none, and always for `auto`). A `matcher` on a
`PreCompact` hook is tested against `trigger`. As in Claude Code a hook cannot
stop the summary: exit `2` (or `"decision": "block"`) or `49` is shown to the
user as a warning and the summary goes ahead. A hook that fails or could not
evaluate is also shown as a warning. It fires only for the top-level agent, not sub-agents.

**On `SubagentStart`** the input is `{"agent_id": "...", "agent_type": "task"|"agentic_fetch"}`
beside the common fields, as in Claude Code: `session_id` is the session the
sub-agent works for, `agent_id` its own session. What the hook returns as
`hookSpecificOutput.additionalContext` (or `context`) goes to the sub-agent with
its first prompt. It cannot stop the sub-agent; a hook that fails or could not
evaluate is shown as a warning, and the sub-agent starts without it.

**On `StopFailure`** the input is `{"error": "<kind>", "error_details": "...",
"last_assistant_message": "..."}` beside the common fields, as in Claude Code:
the kind of API error the turn ended on, what the API said, and the error as
the conversation shows it. A model server that refused the connection or never
answered is `server_error`. It changes nothing: what the hook prints and how it
exits are ignored, and one that fails is logged. Top-level agent only.

**On `PostCompact`** the input is `{"trigger": "manual"|"auto", "compact_summary": "..."}`
beside the common fields: the summary the conversation was compacted to, once
it is saved, as in Claude Code. Use it to log the summary or update something
outside Gnosh. A `matcher` is tested against `trigger`. The hook has no say: a
block, or a hook that fails or could not evaluate, is shown to the user as a
warning. It fires only for the top-level agent, not sub-agents.

**On `SessionEnd`** the input is `{"reason": "clear"|"other"|"prompt_input_exit"}`
beside the common fields. `reason` names why the session ended: the user
started a new one (`clear`), switched to another one or `gnosh run` exited
with it open (`other`), or the TUI exited with it open
(`prompt_input_exit`). A `matcher` on a `SessionEnd` hook is tested against
`reason`. As in Claude Code a hook cannot stop anything and what it prints is
ignored: the exit goes ahead whatever the hook says. A hook that fails is
logged, and the exit goes ahead without it. Its default timeout is 1.5
seconds, or its own `timeout` up to a cap of 60 seconds, so exiting Gnosh is
not held up for long. It fires only for the top-level agent, not sub-agents,
and a session ends once: switching away and then exiting does not end it
again, and a session that was never open does not end at all.

**On `InstructionsLoaded`** the input is `{"file_path": "...", "memory_type":
"User"|"Project"|"Local", "load_reason": "session_start"|"compact"|"nested_traversal"|"include",
"trigger_file_path": "...", "parent_file_path": "..."}` beside the common
fields: the context file loaded, its kind, and what loaded it. `file_path` is
the absolute path; `memory_type` is `"User"` for the user's global context
files, `"Local"` for a `.local.md` file, `"Project"` for the rest, never
`"Managed"`; `trigger_file_path` is set for a `nested_traversal` load, the
file whose access brought the context file in; `parent_file_path` is set for
an `include` load, the file that imported the one loaded. `globs` is never
sent, as Gnosh has no `.claude/rules` files. A `matcher` is tested against
`load_reason`. As in Claude Code the hook has no decision control: its output
is ignored, its exit code is ignored, and the turn does not wait for it. A
hook that fails is logged. It fires only for the top-level agent, not
sub-agents. `GNOSH_INSTRUCTIONS_LOAD_REASON` holds the load reason.

**On `PermissionRequest`** the input is `PreToolUse`'s (`tool_name`,
`tool_input`), and the hook fires only when a permission dialog about the call
is due. It answers in Claude Code's `{"hookSpecificOutput":
{"hookEventName": "PermissionRequest", "decision": {"behavior": "allow"}}}`
or `{"behavior": "deny", "message": "why", "interrupt": false}`; Gnosh's
top-level `"decision": "allow"`/`"deny"` works too. An allow runs the call with
no dialog; a deny refuses it with the message, and the model goes on unless
`interrupt` is true. Exit 2 leaves the decision to the user, and so does a hook
that could not check, whose reason the dialog shows. `updatedInput` and
`updatedPermissions` are not applied: the user is asked about the call as it
is.

## Aggregation (Multiple Hooks)

Composed in **config order**:

- `deny` > `allow` > no opinion. First deny decides; subsequent allows don't override.
- `halt` is sticky: any hook halting ends the turn.
- `reason` and `context` concatenate in config order (newline-joined).
- `updated_input` patches shallow-merge sequentially; later patches win on colliding keys.

## Canonical Examples

### Block destructive commands

```bash
#!/usr/bin/env bash
set -euo pipefail

if echo "$GNOSH_TOOL_INPUT_COMMAND" | grep -qE 'rm\s+-(rf|fr)\s+/'; then
  echo "Refusing to run rm -rf against root" >&2
  exit 2
fi
```

Config: `{"matcher": "^bash$", "command": "./hooks/no-rm-rf.sh"}`

### Auto-approve read-only tools (inline, no script)

```jsonc
{"matcher": "^(view|ls|grep|glob)$", "command": "echo '{\"decision\":\"allow\"}'"}
```

Every `view`/`ls`/`grep`/`glob` call now runs without prompting.

### Inject context without auto-approving

Emit only `context` — omit `decision` so the normal permission flow still runs.

```bash
#!/usr/bin/env bash
set -euo pipefail

if [[ "$GNOSH_TOOL_INPUT_FILE_PATH" == *.go ]]; then
  echo '{"context": "Remember: run gofumpt after editing Go files."}'
else
  echo '{}'
fi
```

Config: `{"matcher": "^(edit|write|multiedit)$", "command": "./hooks/go-context.sh"}`

### Rewrite tool input (shallow merge)

```bash
#!/usr/bin/env bash
set -euo pipefail

read -r input
rewritten=$(echo "$input" | jq -r '.tool_input.command' | some-rewriter)

cat <<EOF
{
  "context": "Rewrote command",
  "updated_input": {"command": "$rewritten"}
}
EOF
```

If the original call was `{"command": "npm test", "timeout": 60000}`, the
tool runs with `{"command": "<rewritten>", "timeout": 60000}` — `timeout` is
preserved.

### Format a file after every edit (PostToolUse)

```jsonc
{ "hooks": { "PostToolUse": [{ "matcher": "^(edit|multiedit|write)$", "command": "./hooks/gofumpt.sh" }] } }
```

```bash
#!/usr/bin/env bash
set -euo pipefail
f="$GNOSH_TOOL_INPUT_FILE_PATH"
[[ "$f" == *.go ]] || exit 0
if ! out=$(gofumpt -w "$f" 2>&1); then
  echo "gofumpt failed on $f: $out" >&2
  exit 2
fi
```

When a hook changes the file the agent just wrote, Gnosh records a fresh
read of it, so the agent's next edit isn't refused as "modified since it was
read", and tells the agent the file changed.

### Refuse to let the turn end (Stop)

A `Stop` hook fires when the agent is about to finish. Exit `2` means "you are
not finished": the turn stays alive and your stderr becomes the reason the model
is told to keep working. Exit `49` means end the turn now, and wins over any
other hook's block.

```jsonc
{ "hooks": { "Stop": [{ "command": "./hooks/tests-must-pass.sh" }] } }
```

```bash
#!/usr/bin/env bash
set -euo pipefail
if ! go test ./... >/tmp/stop-hook.log 2>&1; then
  echo "Tests are still failing:" >&2
  tail -20 /tmp/stop-hook.log >&2
  exit 2
fi
```

There is no cap on how many times a Stop hook may block: if the work is not
done, it is not done. Every 10th consecutive block, the agent is instead asked
to judge whether there is a genuine reason to stop (impossible as asked,
blocked on the user, a check that cannot pass here, no progress). If so it
explains why and ends its reply with `<!-- GNOSH_STOP -->` on its own line;
Gnosh ends the turn and warns the user. Otherwise it keeps working. A hook
that wants to object only once can check `GNOSH_STOP_HOOK_ACTIVE` and exit 0.

A Claude Code-style Stop hook that prints `{"decision": "block", "reason": "…"}`
works the same as exit `2`. Decision values are matched exactly: `allow`,
`deny`, `approve`, `block`, or `ask` (via `hookSpecificOutput`). Any other
value cannot be read, so the hook could not evaluate: it blocks.

## Authoring Checklist

1. Add `#!/usr/bin/env bash` and `set -euo pipefail` (for shell scripts).
2. `chmod +x` the script.
3. Add the entry under `hooks.PreToolUse` or `hooks.PostToolUse` (with the
   right matcher), `hooks.Stop` (no matcher), `hooks.SubagentStop` (no
   matcher; sub-agents only), `hooks.PreCompact` or `hooks.PostCompact`
   (matcher against `manual`/`auto`), `hooks.SessionStart` (matcher against
   `startup`/`resume`/`clear`/`compact`), `hooks.SessionEnd` (matcher
   against `clear`/`other`/`prompt_input_exit`), `hooks.InstructionsLoaded`
   (matcher against `session_start`/`nested_traversal`/`include`/`compact`),
   or `hooks.PermissionRequest` (matcher against the tool name)
   in `gnosh.json`. Spell the
   event name exactly; anything else is a startup error.
4. Decide intent: inject context (omit `decision`), auto-approve (`"allow"`),
   block (`exit 2`), or halt (`exit 49`). On `Stop` and `SubagentStop`,
   `exit 2` means "keep working" and `exit 49` means "end now". Wherever the
   check cannot run, `exit 3` with the reason, never `exit 0`.
5. On `Stop` and `SubagentStop`, check `GNOSH_STOP_HOOK_ACTIVE` so a hook
   that blocks can't loop.
6. If rewriting input, remember `updated_input` is a shallow merge — only
   include the keys you want to change (`PreToolUse` only).

## Debugging

- A timeout counts as could not evaluate: the hook blocks. Bump `timeout` if needed.
- A missing script or command (126, 127) blocks too, with the reason on the hook's line.
- Any other non-zero exit with text on stderr blocks as an error; a silent one is no opinion.
- Use `echo "debug info" >&2` for logging without corrupting stdout JSON, and end
  such a hook with its verdict (`exit 0`), or its log reads as an error.
- `matcher` follows Claude Code's rules: exact names separated by `|` or `,`
  (`bash`, `Edit|Write`; Claude Code's names match Gnosh's tools), `*` or empty
  for all, and anything else is an unanchored regex (`^mcp_github_`). A plain
  name is exact: `edit` does not match `multiedit`.

## Claude Code Compatibility

Gnosh also accepts Claude Code's `hookSpecificOutput` envelope, and reads
matchers as Claude Code does, with Claude Code's tool names (`Bash`, `Read`,
`Edit|Write`, `mcp__server__tool`) matching Gnosh's tools. It reads Claude
Code's groups (`{"matcher": …, "hooks": [{"type": "command", …}]}`) as well as
its own flat entries (`{"matcher": …, "command": …}`), so a Claude Code hooks
block can be pasted in as it is; only `command` hooks run, and a hook asking for
more (`if`, `args`, `async`, another `type`), a matcher on an event that takes none, or a
regex Go cannot read (lookahead) is a config error naming it.
Divergences: Gnosh treats `updated_input` as shallow-merge, Claude Code
replaces; and the payload's `tool_name` is Gnosh's name (`bash`), not Claude
Code's (`Bash`).

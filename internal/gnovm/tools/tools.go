// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

// Package gnovmtools is the agent's tools for GnoVM machines: gnovm_run,
// gnovm_new, gnovm_list and gnovm_drop. The machines themselves are package
// gnovm's.
package gnovmtools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/gnovm"
	"github.com/charmbracelet/crush/internal/permission"
)

// The tools' names.
const (
	RunToolName  = "gnovm_run"
	NewToolName  = "gnovm_new"
	ListToolName = "gnovm_list"
	DropToolName = "gnovm_drop"
)

// maxResult caps a result's text.
const maxResult = 30_000

const runDescription = `Run Go-shaped code in a Gno virtual machine (the GnoVM, gno.land's interpreter for its Go dialect) and see its value and output.

- Input can be an expression ("len(s) * 2"), statements ("x := 3; x++"), declarations (func, type, var, const, with methods; in any order), imports, or a mix; a trailing expression's value is shown, and println and fmt.Println output is returned.
- Packages you use (fmt, strings, strconv, sort, ...) are imported for you, and x = v or x := v on a new name declares it.
- State stays between calls in the same VM, like a REPL. With no name you get your session's scratch VM; code with a package clause (package main + func main) runs as a whole program in a fresh VM, like go run.
- Redeclaring a name in a VM is refused: assign with =, use a new name, or pass reset: true (memory-only VMs).
- Gas: 1 gas is about 1 ns of work; a call that runs out of gas, memory or time is stopped and keeps nothing (the VM is as it was before the call).
- Gno is Go without goroutines, reflection, os, net, sync, encoding/json or unsafe; there is the Gno standard library (strings, strconv, sort, math, time, regexp, bytes, unicode, encoding/hex|base64|csv, crypto/sha256, ...) and fmt, which formats as Go's does.`

const newDescription = `Make a named Gno VM, or change one's settings. Names: a lowercase letter, then up to 31 lowercase letters, digits, - or _. Name your session's scratch VM "scratch" to change its settings.

- memory_mb: memory cap, 1 to 64 (default 16); it can only grow.
- gas: gas limit per call (default 1e9; 1 gas is about 1 ns).
- ns_per_gas: this VM's nanoseconds per gas (default 1), for its time estimates and limits.
- persist: keep the VM's state on disk across restarts of gnosh (default false); once on, it stays on.
- pinned: never evict it from memory to make room for others.
- description: what the VM is for, shown by gnovm_list.`

const listDescription = `List the Gno VMs of this project, with the host's memory, the VMs' memory budget and use, and each VM's settings, memory, gas used, journal size and state; and the packages a VM can import.`

const dropDescription = `Evict a Gno VM from memory (action "evict": it keeps its state and is rebuilt when next used), or delete it and its saved state (action "delete": asks the user first).`

// RunParams is gnovm_run's input.
type RunParams struct {
	Code  string  `json:"code" description:"The code to run."`
	Name  string  `json:"name,omitempty" description:"The VM to run it in. Leave it out for your scratch VM, or, for code with a package clause, a fresh VM."`
	Gas   float64 `json:"gas,omitempty" description:"Gas limit for this call (default the VM's, 1e9 = about 1 s of work)."`
	Reset bool    `json:"reset,omitempty" description:"Start a memory-only VM over before running."`
}

// NewParams is gnovm_new's input.
type NewParams struct {
	Name        string   `json:"name" description:"The VM's name; \"scratch\" for your session's scratch VM."`
	MemoryMB    *float64 `json:"memory_mb,omitempty" description:"Memory cap in MB, 1 to 64; only grows."`
	Gas         *float64 `json:"gas,omitempty" description:"Gas limit per call."`
	NsPerGas    *float64 `json:"ns_per_gas,omitempty" description:"Nanoseconds per gas for this VM, 0.1 to 100."`
	Persist     *bool    `json:"persist,omitempty" description:"Keep its state on disk across restarts."`
	Pinned      *bool    `json:"pinned,omitempty" description:"Never evict it to make room."`
	Description *string  `json:"description,omitempty" description:"What it is for."`
}

// ListParams is gnovm_list's input: none.
type ListParams struct{}

// DropParams is gnovm_drop's input.
type DropParams struct {
	Name   string `json:"name" description:"The VM."`
	Action string `json:"action" description:"\"evict\" or \"delete\"." enum:"evict,delete"`
}

// MemGoalsFunc is told the goals a call to mem changed, as the call's
// session sees them: what that session's task list should show.
type MemGoalsFunc func(ctx context.Context, session string, goals []gnovm.MemGoal)

// MemFramesFunc is told the frames a call to mem pushed and popped, in
// order, with the call's id, and answers what the call's result tells the
// model besides mem's own output: on a pop, what was removed from its
// context and what that changed ("" for nothing to add).
type MemFramesFunc func(ctx context.Context, sessionID, callID string, frames []gnovm.MemFrame) string

// RunResponseMetadata is what gnovm_run keeps with a result besides its
// text: the frames a call to mem pushed and popped, from which a session's
// stack is derived (FRAMES.md).
type RunResponseMetadata struct {
	Frames []gnovm.MemFrame `json:"frames,omitempty"`
}

// NamerFunc names the texts one call keeps in mem without naming them
// (mem.Todo(task), mem.Think(text), mem.Fact(text)), all at once so the
// names tell them apart: text → a short lowercase dashed name; a text left
// out is named by mem from its words.
type NamerFunc func(ctx context.Context, sessionID string, texts []string) map[string]string

// RunOption sets up gnovm_run.
type RunOption func(*runOptions)

type runOptions struct {
	subAgent bool
	onMem    MemGoalsFunc
	bound    func(ctx context.Context, sessionID string) string
	stack    func(ctx context.Context, sessionID string) []string
	onFrames MemFramesFunc
	namer    NamerFunc
	onErased func(ctx context.Context, sessionID string, e gnovm.MemErasure)
	onRules  func(ctx context.Context, sessionID string, rules []gnovm.MemRule)
}

// SubAgent says gnovm_run serves a sub-agent's session, whose Next never
// waits for working sessions (PARALLEL.md): it would wait on its parent
// while its parent waits on it.
func SubAgent(yes bool) RunOption {
	return func(o *runOptions) { o.subAgent = yes }
}

// Namer has gnovm_run ask f, before a call to mem, for names for the texts
// the call keeps without naming them.
func Namer(f NamerFunc) RunOption {
	return func(o *runOptions) { o.namer = f }
}

// MemStack has gnovm_run ask f for the session's stack of frames in mem,
// bottom to top, before each call to mem.
func MemStack(f func(ctx context.Context, sessionID string) []string) RunOption {
	return func(o *runOptions) { o.stack = f }
}

// OnMemFrames has gnovm_run tell f the frames each call to mem pushed and
// popped, and add what f answers to the call's result.
func OnMemFrames(f MemFramesFunc) RunOption {
	return func(o *runOptions) { o.onFrames = f }
}

// OnMemRules has gnovm_run tell f the rules a call to mem set, changed or
// removed, for the user to be told: a rule is given to every session as the
// user's own, and only the user's words become rules.
func OnMemRules(f func(ctx context.Context, sessionID string, rules []gnovm.MemRule)) RunOption {
	return func(o *runOptions) { o.onRules = f }
}

// OnMemErased has gnovm_run tell f what a call to mem erased, so gnosh can
// erase it from its own copies of mem's text too.
func OnMemErased(f func(ctx context.Context, sessionID string, e gnovm.MemErasure)) RunOption {
	return func(o *runOptions) { o.onErased = f }
}

// OnMemGoals has gnovm_run tell f the goals each call to mem changed.
func OnMemGoals(f MemGoalsFunc) RunOption {
	return func(o *runOptions) { o.onMem = f }
}

// BoundGoal has gnovm_run ask f for the goal in mem the session's /goal is
// bound to, which the session's own mem calls may not drop or forget.
func BoundGoal(f func(ctx context.Context, sessionID string) string) RunOption {
	return func(o *runOptions) { o.bound = f }
}

// NewRunTool returns gnovm_run, running in project's VMs.
func NewRunTool(vms *gnovm.Lazy, project string, opts ...RunOption) fantasy.AgentTool {
	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}
	return fantasy.NewAgentTool(RunToolName, runDescription,
		func(ctx context.Context, p RunParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(p.Code) == "" {
				return fantasy.NewTextErrorResponse("code is required"), nil
			}
			gas, err := gasParam(p.Gas)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			c, err := vms.Get()
			if err != nil {
				return fantasy.NewTextErrorResponse("the Gno VMs are not available: " + err.Error()), nil
			}
			session := tools.GetSessionFromContext(ctx)
			req := gnovm.RunRequest{Project: project, Session: session, Name: p.Name, Code: p.Code, Gas: gas, Reset: p.Reset, Req: call.ID, SubAgent: o.subAgent}
			if o.bound != nil && p.Name == gnovm.MemVM {
				req.Goal = o.bound(ctx, session)
			}
			if o.stack != nil && p.Name == gnovm.MemVM {
				req.Frames = o.stack(ctx, session)
			}
			if o.namer != nil && p.Name == gnovm.MemVM {
				if texts := unnamedTexts(p.Code); len(texts) > 0 {
					req.Names = o.namer(ctx, session, texts)
				}
			}
			res, err := c.Run(ctx, req)
			var died interface{ Error() string }
			if err != nil && errors.As(err, &died) && strings.Contains(err.Error(), "worker ended before it replied") {
				if p.Name == gnovm.MemVM {
					return memOutcome(ctx, c, o, project, session, call.ID, err), nil
				}
				return fantasy.NewTextErrorResponse(unknownOutcome(ctx, c, project, session, p, call.ID, err)), nil
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(refusalText(err)), nil
			}
			if res.Err != nil {
				return fantasy.NewTextErrorResponse(renderRun(res, "")), nil
			}
			// The frames are the session's stack whatever happens to the
			// turn: Esc after a call kept its pop or push must not lose them.
			var frames string
			if o.onFrames != nil && len(res.Frames) > 0 {
				frames = o.onFrames(context.WithoutCancel(ctx), session, call.ID, res.Frames)
			}
			text := renderRun(res, frames)
			// So is what else mem kept: the todos shown, an erasure's
			// copies, the rules.
			kept := context.WithoutCancel(ctx)
			if o.onMem != nil && len(res.MemGoals) > 0 {
				o.onMem(kept, session, res.MemGoals)
			}
			if o.onErased != nil && res.Erased != nil {
				o.onErased(kept, session, *res.Erased)
			}
			if o.onRules != nil && len(res.Rules) > 0 {
				o.onRules(kept, session, res.Rules)
			}
			if len(res.Frames) > 0 {
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), RunResponseMetadata{Frames: res.Frames}), nil
			}
			return fantasy.NewTextResponse(text), nil
		})
}

// unnamedTexts is the texts code keeps in mem without naming them: the one
// string literal of each mem.Think(…) and mem.Fact(…) called with a single
// argument, and of each Todo(…), on mem or on a Frame, in order, each once.
// Code that does not parse as statements has none: mem names the texts
// from their words then.
func unnamedTexts(code string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\nfunc _() {\n"+code+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var texts []string
	seen := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Think" && sel.Sel.Name != "Fact" && sel.Sel.Name != "Todo" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); sel.Sel.Name != "Todo" && (!ok || id.Name != "mem") {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if text, err := strconv.Unquote(lit.Value); err == nil && text != "" && !seen[text] {
			seen[text] = true
			texts = append(texts, text)
		}
		return true
	})
	return texts
}

// gasParam turns a JSON number (a model writes 1e9) into a gas limit.
func gasParam(g float64) (int64, error) {
	switch {
	case g == 0:
		return 0, nil
	case g < 1 || g > 1e11 || math.IsNaN(g):
		return 0, errors.New("gas must be between 1 and 1e11")
	}
	return int64(g), nil
}

// unknownOutcome resolves a run whose worker died before replying: the next
// worker is asked whether the call was kept.
func unknownOutcome(ctx context.Context, c *gnovm.Client, project, session string, p RunParams, req string, err error) string {
	name := p.Name
	if name == "" || name == "scratch" {
		name = gnovm.ScratchName(session)
	}
	kept, kerr := c.Kept(ctx, project, name, req)
	switch {
	case kerr != nil:
		return err.Error() + "; asking whether it was kept failed too: " + kerr.Error()
	case kept:
		return err.Error() + ". It was kept: the VM has its effects, but its output was lost."
	}
	return err.Error() + ". It was not kept: nothing from it is in the VM."
}

// memOutcome resolves a mem call whose worker died before replying: the
// next worker is asked whether mem kept it, and if it did, the frames it
// pushed and popped reach the session's stack all the same, as they would
// have with its reply.
func memOutcome(ctx context.Context, c *gnovm.Client, o runOptions, project, session, req string, err error) fantasy.ToolResponse {
	frames, kept, kerr := c.KeptFrames(context.WithoutCancel(ctx), project, req)
	switch {
	case kerr != nil:
		return fantasy.NewTextErrorResponse(err.Error() + "; asking whether it was kept failed too: " + kerr.Error())
	case !kept:
		return fantasy.NewTextErrorResponse(err.Error() + ". It was not kept: nothing from it is in mem.")
	}
	text := err.Error() + ". It was kept: mem has its effects, but its output was lost; mem.Show() says where you stand."
	if len(frames) == 0 {
		return fantasy.NewTextResponse(text)
	}
	if o.onFrames != nil {
		if f := o.onFrames(context.WithoutCancel(ctx), session, req, frames); f != "" {
			text += "\n" + f
		}
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), RunResponseMetadata{Frames: frames})
}

// refusalText is the text for a call that did not run.
func refusalText(err error) string {
	return "ERROR — nothing ran: " + err.Error()
}

// renderRun is a run's result as the model reads it: the value and output
// first, then what gnosh adds about frames (what a pop removed), then one
// status line, then notes.
func renderRun(res *gnovm.RunResult, frames string) string {
	var b strings.Builder
	if res.Err != nil {
		fmt.Fprintf(&b, "ERROR — nothing from this call was kept: %s\n", res.Err)
		if stack := gnovm.ErrorStack(res.Err); stack != "" {
			fmt.Fprintf(&b, "%s\n", stack)
		}
		if hint := nextStep(res); hint != "" {
			fmt.Fprintf(&b, "%s\n", hint)
		}
		if res.Output != "" {
			fmt.Fprintf(&b, "output before it stopped:\n%s\n", strings.TrimRight(res.Output, "\n"))
		}
	} else {
		if res.Value != "" {
			b.WriteString(res.Value + "\n")
		}
		if res.Output != "" {
			if res.Value != "" {
				b.WriteString("output:\n")
			}
			b.WriteString(strings.TrimRight(res.Output, "\n") + "\n")
		}
	}
	if frames != "" {
		b.WriteString(strings.TrimRight(frames, "\n") + "\n")
	}
	b.WriteString(statusLine(res) + "\n")
	for _, n := range res.Notes {
		b.WriteString("note: " + n + "\n")
	}
	return truncate(b.String())
}

func statusLine(res *gnovm.RunResult) string {
	state := "ok"
	if res.Err != nil {
		state = "failed"
	}
	vm := "VM " + res.VM
	if res.Program {
		vm = "a fresh program VM"
	}
	est := time.Duration(float64(res.Gas) * res.NsPerGas)
	kept := "nothing to keep"
	switch {
	case res.Journaled:
		kept = fmt.Sprintf("kept as entry %d", res.Seq)
	case res.Err != nil:
		kept = "nothing kept"
	case res.Program:
		kept = "thrown away after"
	}
	if res.VM == gnovm.MemVM {
		// mem is not a VM with a gas budget and a memory cap of its own.
		return fmt.Sprintf("%s · mem · ran %s · %s", state, res.Wall.Round(time.Millisecond), kept)
	}
	return fmt.Sprintf("%s · %s · %s of %s gas (≈%s at %g ns/gas; ran %s) · memory %s of %s · %s",
		state, vm, count(res.Gas), count(res.GasLimit), est.Round(time.Microsecond), res.NsPerGas,
		res.Wall.Round(time.Microsecond), mb(res.MemUsed), mb(res.MemCap), kept)
}

// nextStep names the call that gets past a limit.
func nextStep(res *gnovm.RunResult) string {
	switch gnovm.LimitKind(res.Err) {
	case "gas":
		return fmt.Sprintf("Next: give it more gas, e.g. \"gas\": %d, or do less per call.", min(res.GasLimit*4, 100_000_000_000))
	case "memory":
		if res.Program {
			return "Next: keep less in memory; a program VM has the default cap."
		}
		capMB := int(res.MemCap >> 20)
		if capMB >= 64 {
			return "Next: keep less in memory; 64 MB is the most a VM may have."
		}
		return fmt.Sprintf("Next: raise the cap with gnovm_new {\"name\": %q, \"memory_mb\": %d}, or keep less.", vmArg(res.VM), min(capMB*2, 64))
	case "wall":
		return "Next: do less per call; the call ran out of time before it ran out of gas."
	}
	return ""
}

// vmArg is how the model names a VM in gnovm_new: its scratch VM as "scratch".
func vmArg(vm string) string {
	if strings.HasPrefix(vm, "scratch-") {
		return "scratch"
	}
	return vm
}

func count(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.3gG", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.3gM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.3gk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

func truncate(s string) string {
	if len(s) <= maxResult {
		return s
	}
	return s[:maxResult] + fmt.Sprintf("\n… (%d more characters not shown)", len(s)-maxResult)
}

// NewNewTool returns gnovm_new.
func NewNewTool(vms *gnovm.Lazy, project string) fantasy.AgentTool {
	return fantasy.NewAgentTool(NewToolName, newDescription,
		func(ctx context.Context, p NewParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(p.Name) == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}
			req := gnovm.NewRequest{
				Project: project, Session: tools.GetSessionFromContext(ctx), Name: p.Name,
				Persist: p.Persist, Pinned: p.Pinned, Description: p.Description, NsPerGas: p.NsPerGas,
			}
			if p.MemoryMB != nil {
				m := int(*p.MemoryMB)
				req.MemoryMB = &m
			}
			if p.Gas != nil {
				g, err := gasParam(*p.Gas)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				req.Gas = &g
			}
			c, err := vms.Get()
			if err != nil {
				return fantasy.NewTextErrorResponse("the Gno VMs are not available: " + err.Error()), nil
			}
			info, created, err := c.New(ctx, req)
			if err != nil {
				return fantasy.NewTextErrorResponse(refusalText(err)), nil
			}
			verb := "VM " + info.Name + " now has"
			if created {
				verb = "Made VM " + info.Name + " with"
			}
			return fantasy.NewTextResponse(fmt.Sprintf("%s %s.", verb, describe(info.Options))), nil
		})
}

func describe(o gnovm.VMOptions) string {
	parts := []string{
		fmt.Sprintf("memory_mb %d", o.MemoryMB),
		fmt.Sprintf("gas %s per call", count(o.Gas)),
		fmt.Sprintf("%g ns/gas", o.NsPerGas),
	}
	if o.Persist {
		parts = append(parts, "persistent")
	} else {
		parts = append(parts, "memory only")
	}
	if o.Pinned {
		parts = append(parts, "pinned")
	}
	return strings.Join(parts, ", ")
}

// NewListTool returns gnovm_list.
func NewListTool(vms *gnovm.Lazy, project string) fantasy.AgentTool {
	return fantasy.NewAgentTool(ListToolName, listDescription,
		func(ctx context.Context, _ ListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			c, err := vms.Get()
			if err != nil {
				return fantasy.NewTextErrorResponse("the Gno VMs are not available: " + err.Error()), nil
			}
			list, err := c.List(ctx, project)
			if err != nil {
				return fantasy.NewTextErrorResponse(refusalText(err)), nil
			}
			return fantasy.NewTextResponse(truncate(renderList(list, c.Deaths()))), nil
		})
}

func renderList(l gnovm.ListResult, deaths []string) string {
	var b strings.Builder
	if l.ProbeErr != "" {
		fmt.Fprintf(&b, "host memory: unknown (%s)\n", l.ProbeErr)
	} else {
		fmt.Fprintf(&b, "host memory: %s available of %s (%s)\n", mb(int64(l.Memory.Available)), mb(int64(l.Memory.Total)), l.Memory.Source)
	}
	fmt.Fprintf(&b, "VM budget: %s, %s in use (standard library %s)\n", mb(l.Budget), mb(l.Used), mb(l.BaseBytes))
	if len(l.VMs) == 0 {
		b.WriteString("no VMs yet: gnovm_run makes your scratch VM, gnovm_new a named one\n")
	}
	for _, vm := range l.VMs {
		state := "on disk"
		if vm.Resident {
			state = fmt.Sprintf("in memory (%s of its cap, %s counted)", mb(vm.MemUsed), mb(vm.Charge))
		}
		if vm.Broken != "" {
			state = "BROKEN: " + vm.Broken
		}
		used := "never"
		if !vm.LastUsed.IsZero() {
			used = vm.LastUsed.Format(time.DateTime)
		}
		fmt.Fprintf(&b, "- %s: %s; %d entries (%s), %s gas used, last used %s; %s",
			vm.Name, describe(vm.Options), vm.Entries, kb(vm.JournalSize), count(vm.LifetimeGas), used, state)
		if vm.Options.Description != "" {
			fmt.Fprintf(&b, " — %s", vm.Options.Description)
		}
		b.WriteString("\n")
	}
	if len(deaths) > 0 {
		fmt.Fprintf(&b, "the VM worker ended %d time(s) in this gnosh; last: %s\n", len(deaths), deaths[len(deaths)-1])
	}
	fmt.Fprintf(&b, "importable: fmt, %s\n", strings.Join(gnovm.ImportablePackages(), ", "))
	b.WriteString("Built on gno.land (https://gno.land) · built on gnosh (https://gnosh.ai)\n")
	return b.String()
}

func kb(n int64) string { return fmt.Sprintf("%d KB", cmp.Or(n>>10, 0)) }

// NewDropTool returns gnovm_drop; deleting asks the user first.
func NewDropTool(vms *gnovm.Lazy, project string, permissions permission.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(DropToolName, dropDescription,
		func(ctx context.Context, p DropParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if p.Name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}
			if p.Action != "evict" && p.Action != "delete" {
				return fantasy.NewTextErrorResponse(`action must be "evict" or "delete"`), nil
			}
			session := tools.GetSessionFromContext(ctx)
			if p.Action == "delete" {
				granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
					SessionID:   session,
					ToolCallID:  call.ID,
					ToolName:    DropToolName,
					Action:      "delete",
					Description: fmt.Sprintf("Delete Gno VM %s and its saved state", p.Name),
					Params:      p,
					Path:        project,
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if !granted {
					return fantasy.NewTextErrorResponse("the user did not allow deleting VM " + p.Name), nil
				}
			}
			c, err := vms.Get()
			if err != nil {
				return fantasy.NewTextErrorResponse("the Gno VMs are not available: " + err.Error()), nil
			}
			name := p.Name
			if name == "scratch" {
				name = gnovm.ScratchName(session)
			}
			if err := c.Drop(ctx, project, name, p.Action); err != nil {
				return fantasy.NewTextErrorResponse(refusalText(err)), nil
			}
			if p.Action == "evict" {
				return fantasy.NewTextResponse("Evicted VM " + name + " from memory; it is rebuilt from its journal when next used."), nil
			}
			return fantasy.NewTextResponse("Deleted VM " + name + "."), nil
		})
}

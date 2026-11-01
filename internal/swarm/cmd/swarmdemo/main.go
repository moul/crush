// Command swarmdemo runs demo A of SWARM.md: N workers, each with its own
// key, work one plan on the shared mem realm through a chain, with no
// model: a todo's work is a fixed sleep, so what is measured is the
// coordination. It is a demo and test tool; nothing in gnosh runs it.
//
//	swarmdemo keys -seed S -n 100 -amount 100000000000ugnot > balances.txt
//	swarmdemo run -rpc http://127.0.0.1:36657 -chain dev -seed S -n 100 \
//	    -groups 10 -leaves 20 -work 2s -out run.json
//
// Keys come from the seed, so the same seed makes the same worker keys the
// chain was funded with. They are demo keys: never fund them on a chain
// that matters.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/gnochain"
)

const defaultRealm = "gno.land/r/g1ecsuj0q572jr0dhu29q9njtnmw03hyu7tyyvv6/mem"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: swarmdemo keys|run [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keys":
		err = keys(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarmdemo:", err)
		os.Exit(1)
	}
}

func signer(seed, who string) *gnochain.KeySigner {
	s, err := gnochain.KeySignerFromSeed([]byte("swarmdemo/" + seed + "/" + who))
	if err != nil {
		panic(err)
	}
	return s
}

func keys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	seed := fs.String("seed", "swarm", "seed the demo keys come from")
	n := fs.Int("n", 10, "workers")
	amount := fs.String("amount", "100000000000ugnot", "each key's balance")
	_ = fs.Parse(args)
	fmt.Printf("%s=%s\n", signer(*seed, "owner").Address(), *amount)
	for i := 1; i <= *n; i++ {
		fmt.Printf("%s=%s\n", signer(*seed, "w"+strconv.Itoa(i)).Address(), *amount)
	}
	return nil
}

// --- the follower: one per run, as one per machine ---

type follower struct {
	mu       sync.Mutex
	outcomes map[string]gnochain.Outcome // tx hash → outcome, kept for late askers
	waiting  map[string]chan gnochain.Outcome
	assigned map[string][]assignment // holder → todos the realm gave it
	wake     map[string]chan struct{}
	blocks   []blockStat
	finished bool  // the plan's root is done: waiters get nothing more
	last     int64 // the last block handled
}

var errFinished = errors.New("the plan is finished")

type assignment struct {
	ID    string
	Token int
	Title string
}

type blockStat struct {
	Height int64     `json:"height"`
	Time   time.Time `json:"time"`
	Txs    int       `json:"txs"`
	Failed int       `json:"failed"`
	Gas    int64     `json:"gas"`
}

func newFollower() *follower {
	return &follower{
		outcomes: map[string]gnochain.Outcome{}, waiting: map[string]chan gnochain.Outcome{},
		assigned: map[string][]assignment{}, wake: map[string]chan struct{}{},
	}
}

func (f *follower) block(b gnochain.Block, realm string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := blockStat{Height: b.Height, Time: b.Time, Txs: len(b.Txs)}
	for _, o := range b.Txs {
		st.Gas += o.GasUsed
		if !o.OK {
			st.Failed++
		}
		h := string(o.Hash)
		f.outcomes[h] = o
		if ch, ok := f.waiting[h]; ok {
			ch <- o
			delete(f.waiting, h)
		}
		for _, e := range o.Events {
			if e.PkgPath != realm || e.Type != "mem" {
				continue
			}
			if (e.Get("kind") == "done" || e.Get("kind") == "dropped") && e.Get("todo") == "t" {
				f.finished = true
				for h, ch := range f.wake {
					close(ch)
					delete(f.wake, h)
				}
				continue
			}
			if e.Get("kind") != "assigned" {
				continue
			}
			tok, _ := strconv.Atoi(e.Get("seq"))
			holder := e.Get("holder")
			f.assigned[holder] = append(f.assigned[holder], assignment{ID: e.Get("todo"), Token: tok, Title: e.Get("title")})
			if ch, ok := f.wake[holder]; ok {
				close(ch)
				delete(f.wake, holder)
			}
		}
	}
	f.blocks = append(f.blocks, st)
	f.last = b.Height
	return nil
}

// outcome waits for the outcome of the transaction with hash h.
func (f *follower) outcome(ctx context.Context, h []byte) (gnochain.Outcome, error) {
	f.mu.Lock()
	if o, ok := f.outcomes[string(h)]; ok {
		f.mu.Unlock()
		return o, nil
	}
	ch := make(chan gnochain.Outcome, 1)
	f.waiting[string(h)] = ch
	f.mu.Unlock()
	select {
	case o := <-ch:
		return o, nil
	case <-ctx.Done():
		return gnochain.Outcome{}, ctx.Err()
	}
}

// assignment waits for the realm to give holder a todo.
func (f *follower) assignment(ctx context.Context, holder string) (assignment, error) {
	for {
		f.mu.Lock()
		if as := f.assigned[holder]; len(as) > 0 {
			a := as[0]
			f.assigned[holder] = as[1:]
			f.mu.Unlock()
			return a, nil
		}
		if f.finished {
			f.mu.Unlock()
			return assignment{}, errFinished
		}
		ch, ok := f.wake[holder]
		if !ok {
			ch = make(chan struct{})
			f.wake[holder] = ch
		}
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return assignment{}, ctx.Err()
		}
	}
}

// --- the run ---

type todoStat struct {
	ID      string  `json:"id"`
	Worker  int     `json:"worker"`
	Claimed float64 `json:"claimed"` // seconds from the start
	Done    float64 `json:"done"`
	How     string  `json:"how"` // held or assigned
}

type txStat struct {
	Kind    string  `json:"kind"`
	Gas     int64   `json:"gas"`
	OK      bool    `json:"ok"`
	Latency float64 `json:"latency"` // seconds from send to outcome
	Log     string  `json:"log,omitempty"`
}

type report struct {
	Workers int         `json:"workers"`
	Groups  int         `json:"groups"`
	Leaves  int         `json:"leaves"`
	Work    float64     `json:"work_seconds"`
	Lease   int         `json:"lease"`
	Plan    string      `json:"plan"`
	Start   time.Time   `json:"start"`
	Wall    float64     `json:"wall_seconds"`
	Ideal   float64     `json:"ideal_seconds"`
	Todos   []todoStat  `json:"todos"`
	Txs     []txStat    `json:"txs"`
	Blocks  []blockStat `json:"blocks"`
	Setup   []txStat    `json:"setup"`
	Refused int64       `json:"refused"`
	Waits   int64       `json:"waits"`
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	rpcURL := fs.String("rpc", "http://127.0.0.1:36657", "node RPC")
	chainID := fs.String("chain", "dev", "chain id")
	realm := fs.String("realm", defaultRealm, "the mem realm")
	seed := fs.String("seed", "swarm", "seed the demo keys come from")
	n := fs.Int("n", 10, "workers")
	groups := fs.Int("groups", 4, "parallel groups under the root")
	leaves := fs.Int("leaves", 10, "todos per group")
	work := fs.Duration("work", 2*time.Second, "time a todo's work takes")
	lease := fs.Int("lease", 300, "lease, seconds")
	gasNext := fs.Int64("gas-next", 60_000_000, "gas wanted for a Next")
	gasPop := fs.Int64("gas-pop", 60_000_000, "gas wanted for a Pop")
	every := fs.Duration("every", 200*time.Millisecond, "how often the follower asks for new blocks")
	conns := fs.Int("conns", 200, "sends at once (the node's RPC takes 900 connections)")
	maxDeposit := fs.Int64("max-deposit", 100_000_000, "storage deposit a call may lock, ugnot")
	out := fs.String("out", "", "write the report here (JSON)")
	planID := fs.String("plan", "", "plan id (default: one made from the time)")
	_ = fs.Parse(args)
	if *planID == "" {
		*planID = "demo-" + strconv.FormatInt(time.Now().Unix(), 36)
	}
	ctx := context.Background()
	c, err := gnochain.Dial(ctx, *rpcURL, *chainID)
	if err != nil {
		return err
	}
	c.MaxDeposit = *maxDeposit
	c.Pool(*conns + 16)
	rep := &report{Workers: *n, Groups: *groups, Leaves: *leaves, Work: work.Seconds(), Lease: *lease, Plan: *planID}

	// Setup by the owner, each call committed before the next (Call waits).
	owner := c.WithSigner(signer(*seed, "owner"))
	call := func(fn string, args ...string) error {
		t0 := time.Now()
		r, err := owner.Call(ctx, *realm, fn, 0, args...)
		rep.Setup = append(rep.Setup, txStat{Kind: fn, Gas: r.Gas, OK: err == nil, Latency: time.Since(t0).Seconds()})
		if err != nil {
			return fmt.Errorf("%s: %w", fn, err)
		}
		return nil
	}
	if err := call("NewPlan", *planID, "the demo task: "+strconv.Itoa(*groups*(*leaves))+" todos", "true", strconv.Itoa(*lease)); err != nil {
		return err
	}
	if err := call("Parallel", *planID, "owner", "t"); err != nil {
		return err
	}
	var gnames, gtitles []string
	for g := 1; g <= *groups; g++ {
		gnames = append(gnames, "g"+strconv.Itoa(g))
		gtitles = append(gtitles, "group "+strconv.Itoa(g))
	}
	for i := 0; i < len(gnames); i += 64 {
		j := min(i+64, len(gnames))
		if err := call("AddTodos", *planID, "owner", "t", strings.Join(gnames[i:j], "\n"), strings.Join(gtitles[i:j], "\n")); err != nil {
			return err
		}
	}
	for _, g := range gnames {
		if err := call("Parallel", *planID, "owner", "t/"+g); err != nil {
			return err
		}
		var ln, lt []string
		for l := 1; l <= *leaves; l++ {
			ln = append(ln, "l"+strconv.Itoa(l))
			lt = append(lt, g+" todo "+strconv.Itoa(l))
		}
		for i := 0; i < len(ln); i += 64 {
			j := min(i+64, len(ln))
			if err := call("AddTodos", *planID, "owner", "t/"+g, strings.Join(ln[i:j], "\n"), strings.Join(lt[i:j], "\n")); err != nil {
				return err
			}
		}
	}

	// The follower, from the head on.
	head, err := c.Height(ctx)
	if err != nil {
		return err
	}
	fol := newFollower()
	fctx, stopFollow := context.WithCancel(ctx)
	defer stopFollow()
	fol.last = head
	go func() {
		// A failed request (the node busy, a connection dropped) must not
		// end the follower: every worker waits on it. Resume after the last
		// block handled.
		for fctx.Err() == nil {
			fol.mu.Lock()
			from := fol.last
			fol.mu.Unlock()
			err := c.Follow(fctx, from, *every, func(b gnochain.Block) error { return fol.block(b, *realm) })
			if fctx.Err() != nil {
				return
			}
			fmt.Fprintln(os.Stderr, "follower:", err, "(resuming)")
			time.Sleep(500 * time.Millisecond)
		}
	}()

	// The workers, all at once.
	var mu sync.Mutex
	var wg sync.WaitGroup
	var refused, waits atomic.Int64
	gate := make(chan struct{}, *conns)
	start := time.Now()
	rep.Start = start
	since := func() float64 { return time.Since(start).Seconds() }
	for i := 1; i <= *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			label := "w" + strconv.Itoa(i)
			ks := signer(*seed, label)
			holder := ks.Address().String() + "|-|" + label
			snd := c.WithSigner(ks).Sender()
			send := func(kind string, gas int64, calls ...gnochain.MsgCallOf) (gnochain.Outcome, error) {
				t0 := time.Now()
				gate <- struct{}{}
				sent, err := snd.Send(ctx, gas, calls...)
				<-gate
				if err != nil {
					refused.Add(1)
					mu.Lock()
					rep.Txs = append(rep.Txs, txStat{Kind: kind, OK: false, Latency: time.Since(t0).Seconds(), Log: err.Error()})
					mu.Unlock()
					return gnochain.Outcome{}, err
				}
				o, err := fol.outcome(ctx, sent.Hash)
				mu.Lock()
				rep.Txs = append(rep.Txs, txStat{Kind: kind, Gas: o.GasUsed, OK: o.OK, Latency: time.Since(t0).Seconds(), Log: o.Log})
				mu.Unlock()
				if err == nil && !o.OK {
					snd.Forget()
					return o, errors.New(firstLine(o.Log))
				}
				return o, err
			}
			// sendGas sends, and once more with twice the gas when the
			// chain says the call wanted more than it was given.
			sendGas := func(kind string, gas int64, calls ...gnochain.MsgCallOf) (gnochain.Outcome, error) {
				o, err := send(kind, gas, calls...)
				if err != nil && strings.Contains(err.Error(), "exceeds tx's gas wanted") {
					return send(kind, 2*gas, calls...)
				}
				return o, err
			}
			for attempt := 0; ; {
				o, err := sendGas("Next", *gasNext, gnochain.MsgCallOf{PkgPath: *realm, Func: "Next", Args: []string{*planID, label, ""}})
				if err != nil {
					attempt++
					if attempt > 20 {
						fmt.Fprintln(os.Stderr, label, "gives up:", err)
						return
					}
					time.Sleep(time.Second)
					continue
				}
				attempt = 0
				parts := strings.Split(o.Values[0], "\t")
				var a assignment
				how := "held"
				switch parts[0] {
				case "held":
					tok, _ := strconv.Atoi(parts[2])
					a = assignment{ID: parts[1], Token: tok}
				case "waiting":
					waits.Add(1)
					var err error
					if a, err = fol.assignment(ctx, holder); err != nil {
						return
					}
					how = "assigned"
				default: // nothing left
					return
				}
				claimed := since()
				time.Sleep(*work)
				if _, err := sendGas("Pop", *gasPop, gnochain.MsgCallOf{PkgPath: *realm, Func: "Pop", Args: []string{*planID, label, a.ID, strconv.Itoa(a.Token), label + " did " + a.ID}}); err != nil {
					fmt.Fprintln(os.Stderr, label, "pop", a.ID, err)
					continue
				}
				mu.Lock()
				rep.Todos = append(rep.Todos, todoStat{ID: a.ID, Worker: i, Claimed: claimed, Done: since(), How: how})
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	rep.Wall = since()
	stopFollow()
	fol.mu.Lock()
	rep.Blocks = fol.blocks
	fol.mu.Unlock()
	rep.Refused, rep.Waits = refused.Load(), waits.Load()
	total := *groups**leaves + *groups + 1
	perWorker := float64(total) / float64(*n)
	if perWorker < 1 {
		perWorker = 1
	}
	rep.Ideal = perWorker * work.Seconds()
	fmt.Printf("workers %d, todos %d (done %d), wall %.1fs, ideal %.1fs, waits %d, refused %d, blocks %d\n",
		*n, total, len(rep.Todos), rep.Wall, rep.Ideal, rep.Waits, rep.Refused, len(rep.Blocks))
	if *out != "" {
		bz, _ := json.MarshalIndent(rep, "", " ")
		return os.WriteFile(*out, bz, 0o644)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

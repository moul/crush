package gnochain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// MsgCallOf is one call for a transaction that holds several: the chain
// runs a transaction's calls in order, all or nothing.
type MsgCallOf struct {
	PkgPath string
	Func    string
	Send    int64 // ugnot
	Args    []string
}

// Sender sends one signer's transactions without waiting for them to be
// committed: a known gas per transaction (no simulation, which would
// double the node's work and may pick differently than the real run), the
// account's sequence kept here, and broadcast_tx_sync. What a transaction
// did is learned from its block (Follow). A Sender is for one goroutine at
// a time; it locks itself so a stray second caller waits.
type Sender struct {
	c  *Client
	mu sync.Mutex
	// accNum and seq are the account's, read once and kept: a transaction
	// the node takes into its mempool uses a sequence (even one that fails
	// later in the block); one it refuses outright does not.
	accNum, seq uint64
	known       bool
	gp          std.GasPrice
}

// Pool lets the client keep up to n connections to the node open between
// requests, for many senders at once: Go's default keeps two per host and
// opens and closes another for every request beyond them, which under a
// swarm exhausts ports and slows every request. Clients made from this one
// (WithSigner) share the pool.
func (c *Client) Pool(n int) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = n
	t.MaxIdleConnsPerHost = n
	c.rpc.hc = &http.Client{Timeout: c.rpc.hc.Timeout, Transport: t}
}

// Sender returns a Sender for the client's signer.
func (c *Client) Sender() *Sender { return &Sender{c: c} }

// Sent is a transaction the node took into its mempool: its hash, to find
// its outcome in a block.
type Sent struct {
	Hash []byte
	Seq  uint64
}

// Send signs and sends one transaction holding calls, with gas wanted gas
// and the fee the chain's gas price asks (read once, then again after a
// refusal over the fee). It returns once the node took it in, not when a
// block commits it.
func (s *Sender) Send(ctx context.Context, gas int64, calls ...MsgCallOf) (Sent, error) {
	if s.c.signer == nil {
		return Sent{}, errors.New("sending needs a signer")
	}
	if len(calls) == 0 {
		return Sent{}, errors.New("a transaction needs a call")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.known {
		if err := s.refresh(ctx); err != nil {
			return Sent{}, err
		}
	}
	msgs := make([]std.Msg, len(calls))
	for i, call := range calls {
		m := MsgCall{
			Caller: caller(s.c.signer), PkgPath: call.PkgPath, Func: call.Func, Args: call.Args,
			MaxDeposit: std.NewCoins(std.NewCoin("ugnot", s.c.MaxDeposit)),
		}
		if call.Send > 0 {
			m.Send = std.NewCoins(std.NewCoin("ugnot", call.Send))
		}
		if err := m.ValidateBasic(); err != nil {
			return Sent{}, err
		}
		msgs[i] = m
	}
	amt := (gas*s.gp.Price.Amount + s.gp.Gas - 1) / s.gp.Gas
	if amt < 1 {
		amt = 1
	}
	tx := std.Tx{Msgs: msgs, Fee: std.NewFee(gas, std.NewCoin(s.gp.Price.Denom, amt))}
	signed, err := s.c.signer.Sign(tx, s.c.chainID, s.accNum, s.seq)
	if err != nil {
		return Sent{}, err
	}
	bz, err := amino.Marshal(signed)
	if err != nil {
		return Sent{}, err
	}
	res, err := s.c.rpc.broadcastTxSync(ctx, bz)
	if err != nil {
		// Unknown whether the node took it: read the account again
		// before the next one.
		s.known = false
		return Sent{}, fmt.Errorf("broadcasting: %w", err)
	}
	if res.failed() {
		s.known = false // a refusal leaves the sequence; read it again to be sure
		return Sent{}, fmt.Errorf("the node refused the transaction: %s", firstLine(res.Log))
	}
	sent := Sent{Hash: res.Hash, Seq: s.seq}
	if len(sent.Hash) == 0 {
		h := sha256.Sum256(bz)
		sent.Hash = h[:]
	}
	s.seq++
	return sent, nil
}

// refresh reads the account's number and sequence and the gas price.
func (s *Sender) refresh(ctx context.Context) error {
	accNum, seq, err := s.c.account(ctx)
	if err != nil {
		return err
	}
	gp, err := s.c.gasPrice(ctx)
	if err != nil {
		return err
	}
	s.accNum, s.seq, s.gp, s.known = accNum, seq, gp, true
	return nil
}

// Forget makes the next Send read the account and gas price again (after
// a transaction the chain refused at delivery, or a long pause).
func (s *Sender) Forget() {
	s.mu.Lock()
	s.known = false
	s.mu.Unlock()
}

type broadcastResult struct {
	Error json.RawMessage `json:"error"`
	Data  []byte          `json:"data"`
	Log   string          `json:"log"`
	Hash  []byte          `json:"hash"`
}

func (b broadcastResult) failed() bool {
	return len(b.Error) > 0 && string(b.Error) != "null"
}

func (r *rpc) broadcastTxSync(ctx context.Context, tx []byte) (broadcastResult, error) {
	var res broadcastResult
	err := r.call(ctx, "broadcast_tx_sync", map[string]any{"tx": tx}, &res)
	return res, err
}

// valuesOf splits a transaction's returned data into its calls' values:
// the node writes them one after another, a blank line apart.
func valuesOf(data []byte) []string {
	parts := strings.Split(string(data), "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v, err := ParseValue(p); err == nil {
			out = append(out, v)
		} else {
			out = append(out, p)
		}
	}
	return out
}

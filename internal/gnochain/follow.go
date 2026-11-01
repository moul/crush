package gnochain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Block is one committed block as a follower sees it: its height, its time
// (block time, the median of its commit), and each transaction's outcome,
// in order.
type Block struct {
	Height int64
	Time   time.Time
	Txs    []Outcome
}

// Outcome is what one transaction of a block did.
type Outcome struct {
	Hash    []byte   // sha256 of the transaction, as broadcast returns it
	OK      bool     // its calls all ran; a failed transaction kept nothing
	Log     string   // why it failed
	Values  []string // each call's first result, in order
	Events  []Event  // what realms emitted (chain.Emit), in order
	GasUsed int64
}

// Event is one event a realm emitted.
type Event struct {
	Type    string
	PkgPath string
	Attrs   []Attr
}

// Attr is one key and value of an event.
type Attr struct{ Key, Value string }

// Get is the value of the first attribute named key, or "".
func (e Event) Get(key string) string {
	for _, a := range e.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// Follow calls fn with every block after from, in order, as the chain
// commits them. The node's RPC has no subscription, so it asks the node's
// status every `every` (a machine runs one follower; its sessions wait on
// what it tells them, they do not ask the chain). It stops when ctx ends
// or fn fails.
func (c *Client) Follow(ctx context.Context, from int64, every time.Duration, fn func(Block) error) error {
	next := from + 1
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		head, err := c.Height(ctx)
		if err != nil {
			return err
		}
		for ; next <= head; next++ {
			b, err := c.BlockAt(ctx, next)
			if err != nil {
				return err
			}
			if err := fn(b); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// BlockAt reads the block at height and its transactions' outcomes. A node
// that no longer keeps them answers with its own error.
func (c *Client) BlockAt(ctx context.Context, height int64) (Block, error) {
	var blk struct {
		Block struct {
			Header struct {
				Time time.Time `json:"time"`
			} `json:"header"`
			Data struct {
				Txs [][]byte `json:"txs"`
			} `json:"data"`
		} `json:"block"`
	}
	h := strconv.FormatInt(height, 10)
	if err := c.rpc.call(ctx, "block", map[string]any{"height": h}, &blk); err != nil {
		return Block{}, err
	}
	var res struct {
		Results struct {
			DeliverTxs []struct {
				ResponseBase struct {
					Error  json.RawMessage   `json:"Error"`
					Data   []byte            `json:"Data"`
					Events []json.RawMessage `json:"Events"`
					Log    string            `json:"Log"`
				} `json:"ResponseBase"`
				GasUsed string `json:"GasUsed"`
			} `json:"deliver_tx"`
		} `json:"results"`
	}
	if err := c.rpc.call(ctx, "block_results", map[string]any{"height": h}, &res); err != nil {
		return Block{}, err
	}
	txs, outs := blk.Block.Data.Txs, res.Results.DeliverTxs
	if len(txs) != len(outs) {
		return Block{}, fmt.Errorf("block %d has %d transactions but %d results", height, len(txs), len(outs))
	}
	b := Block{Height: height, Time: blk.Block.Header.Time}
	for i, raw := range txs {
		r := outs[i].ResponseBase
		sum := sha256.Sum256(raw)
		o := Outcome{Hash: sum[:], Log: r.Log}
		o.OK = len(r.Error) == 0 || string(r.Error) == "null"
		if o.OK {
			o.Values = valuesOf(r.Data)
		}
		o.GasUsed, _ = strconv.ParseInt(outs[i].GasUsed, 10, 64)
		o.Events = realmEvents(r.Events)
		b.Txs = append(b.Txs, o)
	}
	return b, nil
}

// realmEvents reads the events realms emitted (chain.Emit) from a
// transaction's results, skipping the node's own (storage deposits and the
// like, which carry no realm type or package path).
func realmEvents(raw []json.RawMessage) []Event {
	var out []Event
	for _, ev := range raw {
		var e struct {
			Type    string `json:"type"`
			PkgPath string `json:"pkg_path"`
			Attrs   []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"attrs"`
		}
		if json.Unmarshal(ev, &e) != nil || e.Type == "" || e.PkgPath == "" {
			continue
		}
		x := Event{Type: e.Type, PkgPath: e.PkgPath}
		for _, a := range e.Attrs {
			x.Attrs = append(x.Attrs, Attr{a.Key, a.Value})
		}
		out = append(out, x)
	}
	return out
}

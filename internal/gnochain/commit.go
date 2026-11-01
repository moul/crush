package gnochain

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// CallMany sends one transaction holding calls, which the chain runs in
// order, all or nothing, and waits for the block that commits it: for a
// session's own handful of calls (an input's shared mem calls, SWARM.md),
// not a swarm of workers, which uses a Sender instead. Gas is a
// simulation's plus the margin; the fee follows the chain's gas price; the
// deposit is capped by MaxDeposit. The outcome carries each call's value
// and the events realms emitted, with the block's height. A transaction
// the chain refuses comes back with the realm's own words (its panic).
//
// Events carry what each call did, in order (kind, todo, holder and so on);
// Values are best-effort, since the chain separates call results only by a
// blank line, so a value holding one, or a trailing void call, cannot be
// told apart (valuesOf). Read per-call outcomes from Events.
func (c *Client) CallMany(ctx context.Context, calls ...MsgCallOf) (Outcome, int64, error) {
	if c.signer == nil {
		return Outcome{}, 0, errors.New("calling a realm needs a signer")
	}
	if len(calls) == 0 {
		return Outcome{}, 0, errors.New("a transaction needs at least one call")
	}
	who := caller(c.signer)
	msgs := make([]std.Msg, len(calls))
	for i, call := range calls {
		m := MsgCall{
			Caller: who, PkgPath: call.PkgPath, Func: call.Func, Args: call.Args,
			MaxDeposit: std.NewCoins(std.NewCoin("ugnot", c.MaxDeposit)),
		}
		if call.Send > 0 {
			m.Send = std.NewCoins(std.NewCoin("ugnot", call.Send))
		}
		if err := m.ValidateBasic(); err != nil {
			return Outcome{}, 0, err
		}
		msgs[i] = m
	}
	gp, err := c.gasPrice(ctx)
	if err != nil {
		return Outcome{}, 0, err
	}
	fee := func(gas int64) std.Fee {
		amt := (gas*gp.Price.Amount + gp.Gas - 1) / gp.Gas
		if amt < 1 {
			amt = 1
		}
		return std.NewFee(gas, std.NewCoin(gp.Price.Denom, amt))
	}
	accNum, seq, err := c.account(ctx)
	if err != nil {
		return Outcome{}, 0, err
	}
	const simGas = 3_000_000_000
	used, failed, log, err := c.simulate(ctx, std.Tx{Msgs: msgs, Fee: fee(simGas)}, accNum, seq)
	if err != nil {
		return Outcome{}, 0, err
	}
	if failed {
		return Outcome{}, 0, realmError(calls[0].Func, log, errors.New("the transaction failed in simulation"))
	}
	gas := used + int64(float64(used)*c.GasMargin) + 100_000
	signed, err := c.signer.Sign(std.Tx{Msgs: msgs, Fee: fee(gas)}, c.chainID, accNum, seq)
	if err != nil {
		return Outcome{}, 0, err
	}
	bz, err := amino.Marshal(signed)
	if err != nil {
		return Outcome{}, 0, err
	}
	check, deliver, hash, height, err := c.rpc.broadcastTxCommit(ctx, bz)
	if err != nil {
		return Outcome{}, 0, fmt.Errorf("%s: broadcasting: %w", calls[0].Func, err)
	}
	if check.ResponseBase.failed() {
		return Outcome{}, 0, realmError(calls[0].Func, check.ResponseBase.Log, errors.New("the node refused the transaction"))
	}
	o := Outcome{Hash: hash, Log: deliver.ResponseBase.Log}
	o.GasUsed, _ = strconv.ParseInt(deliver.GasUsed, 10, 64)
	if deliver.ResponseBase.failed() {
		return o, height, realmError(calls[0].Func, deliver.ResponseBase.Log, errors.New("the transaction failed"))
	}
	o.OK = true
	o.Values = valuesOf(deliver.ResponseBase.Data)
	o.Events = realmEvents(deliver.ResponseBase.Events)
	return o, height, nil
}

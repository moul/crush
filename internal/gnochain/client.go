// Package gnochain is gnosh's client of gno.land: a realm's values read
// through vm/qeval (free), its functions called in signed transactions,
// and blocks and accounts read back. Immune's volunteer rounds and the
// shared mem realm both talk to the chain through it.
//
// It is built on tm2's transaction types and amino, which gnosh already
// links, plus a small JSON-RPC client on net/http. Not on gno's gnoclient,
// tm2's RPC client or gno.land's vm package: each links parts of the node
// into its users (mdbx, a C database from an active outside project,
// through gnoclient; consensus, validator signing, mempool, p2p and the RPC
// server through the other two). Nor on gno's keybase: keys sign through
// the user's own gnokey (GnokeySigner), or any other [Signer].
// TestNoNodeInGnosh keeps it so.
package gnochain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// Client talks to one chain through one node.
type Client struct {
	rpc     *rpc
	signer  Signer
	chainID string
	// MaxDeposit caps the storage deposit any call may lock, in ugnot. It
	// is always set on a call: unset, a call may lock up to 100 GNOT.
	MaxDeposit int64
	// GasMargin is added to a simulation's gas, as a fraction (0.25 = 25%).
	GasMargin float64
}

// Dial connects to a node's RPC (http://host:26657) and checks it serves
// chainID.
func Dial(ctx context.Context, remote, chainID string) (*Client, error) {
	r := newRPC(remote)
	got, _, err := r.status(ctx)
	if err != nil {
		return nil, fmt.Errorf("asking %s for its status: %w", remote, err)
	}
	if got != chainID {
		return nil, fmt.Errorf("%s serves chain %q, not %q", remote, got, chainID)
	}
	return &Client{rpc: r, chainID: chainID, MaxDeposit: 5_000_000, GasMargin: 0.25}, nil
}

// WithSigner returns a client that signs with s.
func (c *Client) WithSigner(s Signer) *Client {
	cc := *c
	cc.signer = s
	return &cc
}

// ChainID is the chain the client talks to.
func (c *Client) ChainID() string { return c.chainID }

// Caller is the address a realm sees calling: the signer's own, or for a
// session key, its master's.
func (c *Client) Caller() (crypto.Address, error) {
	if c.signer == nil {
		return crypto.Address{}, errors.New("no signer")
	}
	return caller(c.signer), nil
}

// Height is the height of the node's latest block.
func (c *Client) Height(ctx context.Context) (int64, error) {
	_, h, err := c.rpc.status(ctx)
	return h, err
}

// Query asks the node an ABCI query (vm/qeval, auth/accounts/<addr>,
// auth/gasprice...) and returns its data, or its error.
func (c *Client) Query(ctx context.Context, path string, data []byte) ([]byte, error) {
	base, _, err := c.rpc.abciQuery(ctx, path, data)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", path, err)
	}
	if base.failed() {
		return nil, fmt.Errorf("query %s: %s", path, firstLine(base.Log))
	}
	return base.Data, nil
}

// Eval evaluates a non-crossing expression of the realm at pkgPath, such
// as TargetJSON("t1"), and returns its value as text.
func (c *Client) Eval(ctx context.Context, pkgPath, expr string) (string, error) {
	out, err := c.Query(ctx, "vm/qeval", []byte(pkgPath+"."+expr))
	if err != nil {
		return "", fmt.Errorf("%s: %w", expr, err)
	}
	return ParseValue(string(out))
}

// EvalJSON is Eval of an expression whose value is JSON, decoded into v.
func (c *Client) EvalJSON(ctx context.Context, pkgPath, expr string, v any) error {
	s, err := c.Eval(ctx, pkgPath, expr)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("%s: %w", expr, err)
	}
	return nil
}

// Quote is s as a Gno string literal, for an argument in an Eval
// expression.
func Quote(s string) string { return strconv.Quote(s) }

// ParseValue reads a Gno value as qeval and call results print it, such as
// ("t1/r1/u1" string) or (5 int64), into its text (unquoted for strings).
func ParseValue(out string) (string, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return "", nil
	}
	// A call may print several results, one per line; take the first.
	if i := strings.Index(s, "\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return "", fmt.Errorf("not a Gno value: %q", out)
	}
	body := s[1 : len(s)-1]
	sp := strings.LastIndex(body, " ")
	if sp < 0 {
		return "", fmt.Errorf("not a Gno value: %q", out)
	}
	val, typ := body[:sp], body[sp+1:]
	if typ == "string" {
		u, err := strconv.Unquote(val)
		if err != nil {
			return "", fmt.Errorf("not a Gno string: %q", val)
		}
		return u, nil
	}
	return val, nil
}

// gasPrice reads the chain's gas price; a chain that prices nothing answers
// gas 0, and then the floor of 1ugnot per 1000 gas is used.
func (c *Client) gasPrice(ctx context.Context) (std.GasPrice, error) {
	data, err := c.Query(ctx, "auth/gasprice", nil)
	if err != nil {
		return std.GasPrice{}, err
	}
	var gp std.GasPrice
	if err := amino.UnmarshalJSON(data, &gp); err != nil {
		return std.GasPrice{}, fmt.Errorf("reading the gas price: %w", err)
	}
	if gp.Gas <= 0 || gp.Price.Amount <= 0 || gp.Price.Denom == "" {
		gp = std.GasPrice{Gas: 1000, Price: std.NewCoin("ugnot", 1)}
	}
	return gp, nil
}

// Result is what a call returned.
type Result struct {
	Value  string // the function's first result, as text
	Height int64
	Hash   string
	Gas    int64
}

// Call calls fn of the realm at pkgPath with args, sending send ugnot,
// signed by the client's signer. Gas is a simulation's plus the margin; the
// fee follows the chain's gas price; the deposit is capped by MaxDeposit.
// A call the realm refuses comes back with the realm's own words (its
// panic's message).
func (c *Client) Call(ctx context.Context, pkgPath, fn string, send int64, args ...string) (Result, error) {
	if c.signer == nil {
		return Result{}, errors.New("calling a realm needs a signer")
	}
	msg := MsgCall{
		Caller:     caller(c.signer),
		PkgPath:    pkgPath,
		Func:       fn,
		Args:       args,
		MaxDeposit: std.NewCoins(std.NewCoin("ugnot", c.MaxDeposit)),
	}
	if send > 0 {
		msg.Send = std.NewCoins(std.NewCoin("ugnot", send))
	}
	if err := msg.ValidateBasic(); err != nil {
		return Result{}, err
	}
	gp, err := c.gasPrice(ctx)
	if err != nil {
		return Result{}, err
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
		return Result{}, err
	}
	// Simulate with a generous limit to learn the gas.
	const simGas = 3_000_000_000
	simGasUsed, simFailed, simLog, err := c.simulate(ctx, std.Tx{Msgs: []std.Msg{msg}, Fee: fee(simGas)}, accNum, seq)
	if err != nil {
		return Result{}, err
	}
	if simFailed {
		return Result{}, realmError(fn, simLog, errors.New("the call failed in simulation"))
	}
	gas := simGasUsed + int64(float64(simGasUsed)*c.GasMargin) + 100_000
	signed, err := c.signer.Sign(std.Tx{Msgs: []std.Msg{msg}, Fee: fee(gas)}, c.chainID, accNum, seq)
	if err != nil {
		return Result{}, err
	}
	bz, err := amino.Marshal(signed)
	if err != nil {
		return Result{}, err
	}
	check, deliver, hash, height, err := c.rpc.broadcastTxCommit(ctx, bz)
	if err != nil {
		return Result{}, fmt.Errorf("%s: broadcasting: %w", fn, err)
	}
	if check.ResponseBase.failed() {
		return Result{}, realmError(fn, check.ResponseBase.Log, errors.New("the node refused the transaction"))
	}
	if deliver.ResponseBase.failed() {
		return Result{}, realmError(fn, deliver.ResponseBase.Log, errors.New("the call failed"))
	}
	val, _ := ParseValue(string(deliver.ResponseBase.Data))
	gasUsed, _ := strconv.ParseInt(deliver.GasUsed, 10, 64)
	return Result{Value: val, Height: height, Hash: hex.EncodeToString(hash), Gas: gasUsed}, nil
}

// simulate runs tx on the node without committing it, and returns the gas
// it used and, if it failed, its log.
func (c *Client) simulate(ctx context.Context, tx std.Tx, accNum, seq uint64) (int64, bool, string, error) {
	signed, err := c.signer.Sign(tx, c.chainID, accNum, seq)
	if err != nil {
		return 0, false, "", err
	}
	bz, err := amino.Marshal(signed)
	if err != nil {
		return 0, false, "", err
	}
	base, value, err := c.rpc.abciQuery(ctx, ".app/simulate", bz)
	if err != nil {
		return 0, false, "", fmt.Errorf("simulating: %w", err)
	}
	if base.failed() {
		return 0, true, base.Log, nil
	}
	gas, failed, log, err := simResult(value)
	if err != nil {
		return 0, false, "", fmt.Errorf("reading the simulation: %w", err)
	}
	return gas, failed, log, nil
}

// account reads the signer's account number and sequence (for a session
// key, its session account under the master).
func (c *Client) account(ctx context.Context) (uint64, uint64, error) {
	path := "auth/accounts/" + c.signer.Address().String()
	if m := c.signer.Master(); !m.IsZero() {
		path = "auth/accounts/" + m.String() + "/session/" + c.signer.Address().String()
	}
	data, err := c.Query(ctx, path, nil)
	if err != nil {
		return 0, 0, err
	}
	if len(data) == 0 || string(data) == "null" {
		return 0, 0, fmt.Errorf("the chain has no account %s: fund it first", strings.TrimPrefix(path, "auth/accounts/"))
	}
	type base struct {
		AccountNumber string `json:"account_number"`
		Sequence      string `json:"sequence"`
	}
	var a struct {
		BaseAccount        *base `json:"BaseAccount"`
		BaseSessionAccount *struct {
			BaseAccount *base `json:"BaseAccount"`
		} `json:"BaseSessionAccount"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return 0, 0, fmt.Errorf("reading the account: %w", err)
	}
	b := a.BaseAccount
	if a.BaseSessionAccount != nil {
		b = a.BaseSessionAccount.BaseAccount
	}
	if b == nil {
		return 0, 0, errors.New("the account answer has no BaseAccount")
	}
	num := func(s string) (uint64, error) {
		if s == "" {
			return 0, nil
		}
		return strconv.ParseUint(s, 10, 64)
	}
	accNum, err := num(b.AccountNumber)
	if err != nil {
		return 0, 0, err
	}
	seq, err := num(b.Sequence)
	return accNum, seq, err
}

// BlockTxs returns the transactions of the block at height that decode as
// transactions (with MsgCall and the messages tm2's std package knows).
func (c *Client) BlockTxs(ctx context.Context, height int64) ([]std.Tx, error) {
	raws, err := c.rpc.blockTxs(ctx, height)
	if err != nil {
		return nil, fmt.Errorf("fetching block %d: %w", height, err)
	}
	var txs []std.Tx
	for _, raw := range raws {
		var tx std.Tx
		if err := amino.Unmarshal(raw, &tx); err != nil {
			continue
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

// realmError turns a failed call into the realm's own words where it said
// some (a panic's message), else the chain's.
func realmError(fn, log string, err error) error {
	for _, marker := range []string{"VM panic: ", "panic: "} {
		if i := strings.Index(log, marker); i >= 0 {
			msg := log[i+len(marker):]
			if j := strings.Index(msg, "\n"); j >= 0 {
				msg = msg[:j]
			}
			return fmt.Errorf("%s: %s", fn, strings.TrimSpace(msg))
		}
	}
	return fmt.Errorf("%s: %v", fn, err)
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

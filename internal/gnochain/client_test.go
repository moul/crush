package gnochain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/require"
)

// TestNoNodeInGnosh keeps the client off the node's code and the keybase:
// gnoclient (and mdbx, a C database, behind it), tm2's RPC client and
// gno.land's vm package (consensus, p2p, the RPC server), and gno's keybase
// package (Ledger support in C, golang.org/x/crypto/openpgp).
func TestNoNodeInGnosh(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	require.NoError(t, err)
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range []string{"mdbx", "gno.land/pkg/gnoclient", "gno.land/pkg/sdk/vm", "tm2/pkg/bft/rpc", "tm2/pkg/bft/consensus", "tm2/pkg/p2p", "merlin", "StrobeGo", "crypto/keys", "ledger", "zondax", "x/crypto/openpgp"} {
			require.NotContains(t, dep, bad, "the chain client must not link the node's code")
		}
	}
}

// varint and field write protobuf's wire format, as amino does.
func varint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func field(b []byte, num int, payload []byte) []byte {
	b = varint(b, uint64(num<<3|2))
	b = varint(b, uint64(len(payload)))
	return append(b, payload...)
}

func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) }

// deliverTx is a ResponseDeliverTx as amino writes it: ResponseBase{Error,
// Log} when errLog is set, GasWanted and GasUsed.
func deliverTx(errLog string, gasUsed int64) []byte {
	var base []byte
	if errLog != "" {
		base = field(base, 1, []byte{0x0a, 0x01, 0x00})
		base = field(base, 4, []byte(errLog))
	}
	var msg []byte
	if len(base) > 0 {
		msg = field(msg, 1, base)
	}
	msg = varint(msg, uint64(2<<3|0))
	msg = varint(msg, zigzag(3_000_000_000))
	msg = varint(msg, uint64(3<<3|0))
	return varint(msg, zigzag(gasUsed))
}

func TestSimResult(t *testing.T) {
	gas, failed, log, err := simResult(deliverTx("VM panic: no unit of t1/r1 is free for you now", 11_376_616))
	require.NoError(t, err)
	require.Equal(t, int64(11_376_616), gas)
	require.True(t, failed)
	require.Contains(t, log, "no unit of t1/r1 is free")
	gas, failed, _, err = simResult(deliverTx("", 42))
	require.NoError(t, err)
	require.Equal(t, int64(42), gas)
	require.False(t, failed)
	_, _, _, err = simResult([]byte{0x0a, 0x05, 0x01})
	require.Error(t, err, "a truncated field")
}

func TestMsgCallSignBytes(t *testing.T) {
	addr := crypto.AddressFromPreimage([]byte("caller"))
	m := MsgCall{
		Caller: addr, PkgPath: "gno.land/r/demo/x", Func: "Next", Args: []string{"t1"},
		Send: std.NewCoins(std.NewCoin("ugnot", 100)), MaxDeposit: std.NewCoins(std.NewCoin("ugnot", 5)),
	}
	got := string(m.GetSignBytes())
	require.Equal(t, `{"args":["t1"],"caller":"`+addr.String()+`","func":"Next","max_deposit":"5ugnot","pkg_path":"gno.land/r/demo/x","send":"100ugnot"}`, got)
	bz, err := amino.MarshalAny(m)
	require.NoError(t, err)
	var back std.Msg
	require.NoError(t, amino.UnmarshalAny(bz, &back))
	require.Equal(t, m, back)
	js := string(amino.MustMarshalJSONAny(m))
	require.Contains(t, js, `"@type":"/vm.m_call"`)
}

func TestParseValue(t *testing.T) {
	for in, want := range map[string]string{
		`("t1/r1/u1" string)`:                "t1/r1/u1",
		"(\"ok\" string)\n":                  "ok",
		`("{\"a\":\"x\\ny\"}" string)`:       "{\"a\":\"x\\ny\"}",
		`(5 int64)`:                          "5",
		"(\"t1\" string)\n(\"more\" string)": "t1",
		"":                                   "",
	} {
		got, err := ParseValue(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"t1", "(nospace)", `("unterminated string)`} {
		_, err := ParseValue(bad)
		require.Error(t, err, bad)
	}
}

func TestRealmError(t *testing.T) {
	log := "--= Error =--\nData: ...\nMsg Traces:\n    0  gno/tm2/pkg/errors/errors.go:103 - VM panic: you hold no live claim on t1/r1/u2\nmore\n"
	require.EqualError(t, realmError("File", log, errors.New("x")), "File: you hold no live claim on t1/r1/u2")
	require.EqualError(t, realmError("Next", "nothing useful", errors.New("out of gas")), "Next: out of gas")
}

// fakeSigner signs with a signature that says what it signed for.
type fakeSigner struct {
	addr, master crypto.Address
}

func (f fakeSigner) Address() crypto.Address { return f.addr }
func (f fakeSigner) Master() crypto.Address  { return f.master }
func (f fakeSigner) Sign(tx std.Tx, chainID string, accNum, seq uint64) (*std.Tx, error) {
	tx.Signatures = []std.Signature{{Signature: fmt.Appendf(nil, "%s/%d/%d", chainID, accNum, seq)}}
	return &tx, nil
}

// fakeNode is a node's JSON-RPC as the client uses it: a chain at height
// 42, a realm whose Hello() answers "hi", an account 7 at sequence 3 (a
// session account under master when one is set), and calls that use
// 12345 gas. It keeps the transaction broadcast.
type fakeNode struct {
	mu        sync.Mutex
	broadcast []std.Tx
	queries   []string
}

func (n *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	param := func(name string) string { s, _ := req.Params[name].(string); return s }
	b64 := base64.StdEncoding.EncodeToString
	var result any
	switch req.Method {
	case "status":
		result = map[string]any{"node_info": map[string]any{"network": "test-chain"}, "sync_info": map[string]any{"latest_block_height": "42"}}
	case "abci_query":
		data, _ := base64.StdEncoding.DecodeString(param("data"))
		n.mu.Lock()
		n.queries = append(n.queries, param("path"))
		n.mu.Unlock()
		resp := map[string]any{"ResponseBase": map[string]any{"Error": nil, "Data": nil, "Log": ""}, "Value": nil}
		base := resp["ResponseBase"].(map[string]any)
		switch path := param("path"); {
		case path == "vm/qeval" && string(data) == "gno.land/r/demo/x.Hello()":
			base["Data"] = b64([]byte(`("hi" string)`))
		case path == "vm/qeval":
			base["Error"] = map[string]any{"@type": "/vm.UnknownError"}
			base["Log"] = "no such function\nmore"
		case path == "auth/gasprice":
			base["Data"] = b64(amino.MustMarshalJSON(std.GasPrice{Gas: 1000, Price: std.NewCoin("ugnot", 1)}))
		case strings.HasPrefix(path, "auth/accounts/") && strings.Contains(path, "/session/"):
			base["Data"] = b64([]byte(`{"BaseSessionAccount":{"BaseAccount":{"account_number":"9","sequence":"1"}}}`))
		case strings.HasPrefix(path, "auth/accounts/"):
			base["Data"] = b64([]byte(`{"BaseAccount":{"account_number":"7","sequence":"3"}}`))
		case path == ".app/simulate":
			resp["Value"] = b64(deliverTx("", 12345))
		}
		result = map[string]any{"response": resp}
	case "broadcast_tx_commit":
		raw, _ := base64.StdEncoding.DecodeString(param("tx"))
		var tx std.Tx
		if err := amino.Unmarshal(raw, &tx); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.mu.Lock()
		n.broadcast = append(n.broadcast, tx)
		n.mu.Unlock()
		ok := map[string]any{"Error": nil, "Data": nil, "Log": ""}
		result = map[string]any{
			"check_tx":   map[string]any{"ResponseBase": ok, "GasUsed": "0"},
			"deliver_tx": map[string]any{"ResponseBase": map[string]any{"Error": nil, "Data": b64([]byte(`("ok" string)`)), "Log": ""}, "GasUsed": "12345"},
			"hash":       b64([]byte{0xab, 0xcd}),
			"height":     "43",
		}
	default:
		result = nil
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": "gnosh", "result": result})
}

// The client reads the chain, evaluates a realm's expression and calls a
// realm's function through any Signer, against a node's JSON-RPC.
func TestClientAgainstANode(t *testing.T) {
	t.Parallel()
	node := &fakeNode{}
	srv := httptest.NewServer(node)
	defer srv.Close()
	ctx := t.Context()

	_, err := Dial(ctx, srv.URL, "other-chain")
	require.ErrorContains(t, err, `serves chain "test-chain", not "other-chain"`)
	c, err := Dial(ctx, srv.URL, "test-chain")
	require.NoError(t, err)
	h, err := c.Height(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(42), h)

	v, err := c.Eval(ctx, "gno.land/r/demo/x", "Hello()")
	require.NoError(t, err)
	require.Equal(t, "hi", v)
	_, err = c.Eval(ctx, "gno.land/r/demo/x", "Nope()")
	require.ErrorContains(t, err, "no such function")
	_, err = c.Call(ctx, "gno.land/r/demo/x", "F", 0)
	require.ErrorContains(t, err, "needs a signer")

	key := crypto.AddressFromPreimage([]byte("worker"))
	signed := c.WithSigner(fakeSigner{addr: key})
	caller, err := signed.Caller()
	require.NoError(t, err)
	require.Equal(t, key, caller)
	res, err := signed.Call(ctx, "gno.land/r/demo/x", "Next", 100, "plan1")
	require.NoError(t, err)
	require.Equal(t, Result{Value: "ok", Height: 43, Hash: "abcd", Gas: 12345}, res)
	require.Len(t, node.broadcast, 1)
	tx := node.broadcast[0]
	call := tx.Msgs[0].(MsgCall)
	require.Equal(t, MsgCall{
		Caller: key, PkgPath: "gno.land/r/demo/x", Func: "Next", Args: []string{"plan1"},
		Send: std.NewCoins(std.NewCoin("ugnot", 100)), MaxDeposit: std.NewCoins(std.NewCoin("ugnot", 5_000_000)),
	}, call)
	require.Equal(t, "test-chain/7/3", string(tx.Signatures[0].Signature), "signed at the account's number and sequence")
	require.Equal(t, int64(12345+3086+100_000), tx.Fee.GasWanted, "the simulation's gas, the margin and the floor")

	// A session key signs at its session account, and the realm sees its
	// master calling.
	master := crypto.AddressFromPreimage([]byte("master"))
	session := c.WithSigner(fakeSigner{addr: key, master: master})
	_, err = session.Call(ctx, "gno.land/r/demo/x", "Next", 0, "plan1")
	require.NoError(t, err)
	tx = node.broadcast[1]
	require.Equal(t, master, tx.Msgs[0].(MsgCall).Caller)
	require.Equal(t, "test-chain/9/1", string(tx.Signatures[0].Signature))
	require.Contains(t, node.queries, "auth/accounts/"+master.String()+"/session/"+key.String())
}

// TestLive talks to a live chain (GNOCHAIN_LIVE_RPC and GNOCHAIN_LIVE_CHAIN;
// with GNOCHAIN_LIVE_HOME, a gnokey keybase holding the funded key
// GNOCHAIN_LIVE_KEY with password GNOCHAIN_LIVE_PW, it signs as that key).
func TestLive(t *testing.T) {
	remote := os.Getenv("GNOCHAIN_LIVE_RPC")
	if remote == "" {
		t.Skip("set GNOCHAIN_LIVE_RPC to run against a live chain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	chainID := os.Getenv("GNOCHAIN_LIVE_CHAIN")
	c, err := Dial(ctx, remote, chainID)
	require.NoError(t, err)
	_, err = Dial(ctx, remote, "some-other-chain")
	require.ErrorContains(t, err, "serves chain")
	h, err := c.Height(ctx)
	require.NoError(t, err)
	require.Positive(t, h)

	home := os.Getenv("GNOCHAIN_LIVE_HOME")
	if home == "" {
		return
	}
	s, err := NewGnokeySigner(os.Getenv("GNOCHAIN_LIVE_GNOKEY"), home, os.Getenv("GNOCHAIN_LIVE_KEY"), os.Getenv("GNOCHAIN_LIVE_PW"), "")
	require.NoError(t, err)
	_, _, err = c.WithSigner(s).account(ctx)
	require.NoError(t, err, "the key's account is on the chain")
}

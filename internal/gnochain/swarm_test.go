package gnochain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/require"
)

// swarmNode is a node for the sender and follower: an account with a
// sequence, a mempool that takes or refuses, and one block of results.
type swarmNode struct {
	mu       sync.Mutex
	seq      int
	refuse   bool
	sent     []std.Tx
	accReads int
}

func (n *swarmNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	param := func(name string) string { s, _ := req.Params[name].(string); return s }
	n.mu.Lock()
	defer n.mu.Unlock()
	var result any
	switch req.Method {
	case "status":
		result = map[string]any{"node_info": map[string]any{"network": "test-chain"}, "sync_info": map[string]any{"latest_block_height": "3"}}
	case "abci_query":
		var value []byte
		switch path := param("path"); {
		case strings.HasPrefix(path, "auth/accounts/"):
			n.accReads++
			value = []byte(`{"BaseAccount":{"account_number":"7","sequence":"` + itoa(n.seq) + `"}}`)
		case path == "auth/gasprice":
			value = []byte(`{"gas":"1000","price":"1ugnot"}`)
		}
		result = map[string]any{"response": map[string]any{"ResponseBase": map[string]any{"Error": nil, "Data": base64.StdEncoding.EncodeToString(value)}}}
	case "broadcast_tx_sync":
		raw, _ := base64.StdEncoding.DecodeString(param("tx"))
		var tx std.Tx
		_ = amino.Unmarshal(raw, &tx)
		if n.refuse {
			result = map[string]any{"error": map[string]any{"@type": "/std.UnauthorizedError"}, "log": "wrong sequence\nmore", "hash": nil}
			break
		}
		n.sent = append(n.sent, tx)
		n.seq++
		result = map[string]any{"error": nil, "hash": base64.StdEncoding.EncodeToString([]byte("h" + itoa(len(n.sent))))}
	case "block":
		tx := base64.StdEncoding.EncodeToString([]byte("tx-one"))
		tx2 := base64.StdEncoding.EncodeToString([]byte("tx-two"))
		result = map[string]any{"block": map[string]any{"header": map[string]any{"time": "2026-11-01T19:00:00Z"}, "data": map[string]any{"txs": []string{tx, tx2}}}}
	case "block_results":
		ok := map[string]any{"ResponseBase": map[string]any{
			"Error": nil, "Data": base64.StdEncoding.EncodeToString([]byte("(\"held\\tt/a\\t4\\ta\" string)\n\n(\"x\" string)\n\n")),
			"Events": []any{
				map[string]any{"@type": "/tm.Event", "type": "mem", "pkg_path": "gno.land/r/x/mem", "attrs": []any{map[string]any{"key": "kind", "value": "held"}, map[string]any{"key": "todo", "value": "t/a"}}},
				map[string]any{"@type": "/tm.StorageDepositEvent", "bytes_delta": "12"},
			},
		}, "GasUsed": "1234"}
		failed := map[string]any{"ResponseBase": map[string]any{"Error": map[string]any{"@type": "/vm.VMError"}, "Log": "not a member"}, "GasUsed": "99"}
		result = map[string]any{"height": "3", "results": map[string]any{"deliver_tx": []any{ok, failed}}}
	}
	out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "gnosh", "result": result})
	_, _ = w.Write(out)
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestKeySignerSigns(t *testing.T) {
	s, err := KeySignerFromSeed([]byte("worker 1 seed, 16+ bytes"))
	require.NoError(t, err)
	again, _ := KeySignerFromSeed([]byte("worker 1 seed, 16+ bytes"))
	require.Equal(t, s.Address(), again.Address(), "the same seed makes the same key")
	require.NotContains(t, s.String(), "seed")
	tx := std.Tx{Msgs: []std.Msg{MsgCall{Caller: s.Address(), PkgPath: "gno.land/r/x/mem", Func: "Next", Args: []string{"p", "w", ""}}}, Fee: std.NewFee(1000, std.NewCoin("ugnot", 1))}
	signed, err := s.Sign(tx, "dev", 3, 9)
	require.NoError(t, err)
	bz, err := tx.GetSignBytes("dev", 3, 9)
	require.NoError(t, err)
	require.Len(t, signed.Signatures, 1)
	require.True(t, s.PubKey().VerifyBytes(bz, signed.Signatures[0].Signature))
	require.Equal(t, s.PubKey(), signed.Signatures[0].PubKey, "the public key goes with it")
}

// The sender reads the account once, then counts the sequence itself; a
// refusal makes it read the account again.
func TestSenderKeepsTheSequence(t *testing.T) {
	node := &swarmNode{seq: 5}
	srv := httptest.NewServer(node)
	defer srv.Close()
	ctx := context.Background()
	s, _ := KeySignerFromSeed([]byte("worker 2 seed, 16+ bytes"))
	c, err := Dial(ctx, srv.URL, "test-chain")
	require.NoError(t, err)
	snd := c.WithSigner(s).Sender()
	call := MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Next", Args: []string{"p", "w", ""}}
	a, err := snd.Send(ctx, 20_000_000, call)
	require.NoError(t, err)
	b, err := snd.Send(ctx, 20_000_000, call, MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Reclaim", Args: []string{"p", "w"}})
	require.NoError(t, err)
	require.Equal(t, uint64(5), a.Seq)
	require.Equal(t, uint64(6), b.Seq)
	require.Equal(t, 1, node.accReads, "the account is read once")
	require.Len(t, node.sent[1].Msgs, 2, "one transaction, two calls")
	for i, tx := range node.sent {
		bz, _ := tx.GetSignBytes("test-chain", 7, uint64(5+i))
		require.True(t, s.PubKey().VerifyBytes(bz, tx.Signatures[0].Signature), "signed at sequence %d", 5+i)
	}
	node.refuse = true
	_, err = snd.Send(ctx, 20_000_000, call)
	require.ErrorContains(t, err, "the node refused the transaction: wrong sequence")
	node.refuse = false
	_, err = snd.Send(ctx, 20_000_000, call)
	require.NoError(t, err)
	require.Equal(t, 2, node.accReads, "after a refusal the account is read again")
}

func TestBlockAtAndFollow(t *testing.T) {
	srv := httptest.NewServer(&swarmNode{})
	defer srv.Close()
	ctx := context.Background()
	c, err := Dial(ctx, srv.URL, "test-chain")
	require.NoError(t, err)
	b, err := c.BlockAt(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, int64(3), b.Height)
	require.Len(t, b.Txs, 2)
	ok := b.Txs[0]
	require.True(t, ok.OK)
	require.Equal(t, []string{"held\tt/a\t4\ta", "x"}, ok.Values)
	require.Len(t, ok.Events, 1, "the storage deposit is the node's, not the realm's")
	require.Equal(t, "held", ok.Events[0].Get("kind"))
	require.Equal(t, "gno.land/r/x/mem", ok.Events[0].PkgPath)
	require.Equal(t, int64(1234), ok.GasUsed)
	require.False(t, b.Txs[1].OK)
	require.Equal(t, "not a member", b.Txs[1].Log)

	var seen []int64
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = c.Follow(cctx, 1, 10*time.Millisecond, func(b Block) error {
		seen = append(seen, b.Height)
		if len(seen) == 2 {
			cancel()
		}
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []int64{2, 3}, seen, "every block after from, once, in order")
}

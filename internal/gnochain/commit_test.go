package gnochain

import (
	"net/http/httptest"
	"testing"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// CallMany puts several calls in one transaction, in order, and reads back
// each call's value and the realm's events, skipping the node's own.
func TestCallMany(t *testing.T) {
	t.Parallel()
	node := &fakeNode{}
	srv := httptest.NewServer(node)
	defer srv.Close()
	ctx := t.Context()
	c, err := Dial(ctx, srv.URL, "test-chain")
	require.NoError(t, err)

	_, _, err = c.CallMany(ctx, MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Next"})
	require.ErrorContains(t, err, "needs a signer")

	key := crypto.AddressFromPreimage([]byte("worker"))
	signed := c.WithSigner(fakeSigner{addr: key})
	_, _, err = signed.CallMany(ctx)
	require.ErrorContains(t, err, "at least one call")

	o, height, err := signed.CallMany(ctx,
		MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Pop", Args: []string{"p", "w", "t/a", "4", "did a"}},
		MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Next", Args: []string{"p", "w", ""}},
	)
	require.NoError(t, err)
	require.True(t, o.OK)
	require.Equal(t, int64(43), height)
	require.Equal(t, []string{"Pop ok", "Next ok"}, o.Values, "each call's value, in order")
	require.Len(t, o.Events, 2, "the node's storage event is filtered out")
	require.Equal(t, "mem", o.Events[0].Type)
	require.Equal(t, "Pop", o.Events[0].Get("fn"))
	require.Equal(t, "Next", o.Events[1].Get("fn"))
	require.Equal(t, int64(12345), o.GasUsed)

	// One transaction carried both calls, in order, signed once.
	require.Len(t, node.broadcast, 1)
	tx := node.broadcast[0]
	require.Len(t, tx.Msgs, 2)
	require.Equal(t, "Pop", tx.Msgs[0].(MsgCall).Func)
	require.Equal(t, "Next", tx.Msgs[1].(MsgCall).Func)
	require.Len(t, tx.Signatures, 1)
}

// A transaction the chain refuses comes back with the realm's own words.
func TestCallManyRefused(t *testing.T) {
	t.Parallel()
	node := &fakeNode{failDeliver: "VM panic: you do not hold t/a\nstack trace"}
	srv := httptest.NewServer(node)
	defer srv.Close()
	ctx := t.Context()
	c, err := Dial(ctx, srv.URL, "test-chain")
	require.NoError(t, err)
	signed := c.WithSigner(fakeSigner{addr: crypto.AddressFromPreimage([]byte("w"))})
	o, _, err := signed.CallMany(ctx, MsgCallOf{PkgPath: "gno.land/r/x/mem", Func: "Pop", Args: []string{"p", "w", "t/a", "9", "x"}})
	require.ErrorContains(t, err, "you do not hold t/a")
	require.False(t, o.OK)
	require.Equal(t, int64(12345), o.GasUsed, "gas is reported even on a failed delivery")
}

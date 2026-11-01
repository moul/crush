package gnochain

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLiveCallMany sends two realm calls in one transaction to a running
// chain and reads back their values and events. It needs a chain with the
// mem realm and a funded key:
//
//	GNOCHAIN_LIVE_RPC=http://127.0.0.1:36657 GNOCHAIN_LIVE_CHAIN=dev \
//	GNOCHAIN_LIVE_REALM=gno.land/r/.../mem GNOCHAIN_LIVE_SEED=swarmdemo/demo1/owner \
//	go test ./internal/gnochain/ -run TestLiveCallMany -v
func TestLiveCallMany(t *testing.T) {
	remote := os.Getenv("GNOCHAIN_LIVE_RPC")
	realm := os.Getenv("GNOCHAIN_LIVE_REALM")
	seed := os.Getenv("GNOCHAIN_LIVE_SEED")
	if remote == "" || realm == "" || seed == "" {
		t.Skip("set GNOCHAIN_LIVE_RPC, GNOCHAIN_LIVE_REALM and GNOCHAIN_LIVE_SEED")
	}
	ctx := t.Context()
	c, err := Dial(ctx, remote, os.Getenv("GNOCHAIN_LIVE_CHAIN"))
	require.NoError(t, err)
	signer, err := KeySignerFromSeed([]byte(seed))
	require.NoError(t, err)
	c = c.WithSigner(signer)

	h, err := c.Height(ctx)
	require.NoError(t, err)
	plan := "callmany-" + strconv.FormatInt(h, 10)
	// A fresh plan, then two calls in one transaction: add a todo, and make
	// the root parallel. The second call sees the first's state.
	_, err = c.Call(ctx, realm, "NewPlan", 0, plan, "CallMany live test", "true", "120")
	require.NoError(t, err)
	o, height, err := c.CallMany(ctx,
		MsgCallOf{PkgPath: realm, Func: "AddTodos", Args: []string{plan, "owner", "t", "a\nb", "todo a\ntodo b"}},
		MsgCallOf{PkgPath: realm, Func: "Parallel", Args: []string{plan, "owner", "t"}},
	)
	require.NoError(t, err)
	require.True(t, o.OK)
	t.Logf("committed at height %d, gas %d, %d values, %d events", height, o.GasUsed, len(o.Values), len(o.Events))
	require.Equal(t, "t/a\nt/b", o.Values[0], "AddTodos returned both ids")
	// Events are the reliable per-call channel (Values can't tell a trailing
	// void call from the terminator).
	kinds := map[string]int{}
	for _, e := range o.Events {
		require.Equal(t, realm, e.PkgPath)
		kinds[e.Get("kind")]++
	}
	require.GreaterOrEqual(t, kinds["added"], 2, "two todos added")
	require.GreaterOrEqual(t, kinds["parallel"], 1, "the root made parallel")

	// The chain holds what the transaction did: both todos are there.
	page, err := c.Eval(ctx, realm, "PlanJSON("+Quote(plan)+")")
	require.NoError(t, err)
	require.Contains(t, page, `"todos":3`, page)
}

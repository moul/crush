package gnochain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
)

// rpc speaks a tm2 node's JSON-RPC over plain HTTP: four methods are all
// the client needs, and tm2's own client package links the node's RPC
// server, consensus and p2p code into its users.
type rpc struct {
	url string
	hc  *http.Client
}

func newRPC(remote string) *rpc {
	return &rpc{url: remote, hc: &http.Client{Timeout: 2 * time.Minute}}
}

func (r *rpc) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "gnosh", "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not a JSON-RPC answer (%s)", method, resp.Status)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %s %s", method, env.Error.Message, env.Error.Data)
	}
	return json.Unmarshal(env.Result, out)
}

// responseBase is how the node writes a response's common part in JSON.
type responseBase struct {
	Error  json.RawMessage   `json:"Error"`
	Data   []byte            `json:"Data"`
	Events []json.RawMessage `json:"Events"`
	Log    string            `json:"Log"`
}

func (b responseBase) failed() bool {
	return len(b.Error) > 0 && string(b.Error) != "null"
}

// status is the chain the node serves and the height of its latest block.
func (r *rpc) status(ctx context.Context) (network string, height int64, err error) {
	var res struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
		} `json:"sync_info"`
	}
	if err = r.call(ctx, "status", map[string]any{}, &res); err != nil {
		return "", 0, err
	}
	if h := res.SyncInfo.LatestBlockHeight; h != "" {
		if height, err = strconv.ParseInt(h, 10, 64); err != nil {
			return "", 0, fmt.Errorf("status: latest_block_height %q: %w", h, err)
		}
	}
	return res.NodeInfo.Network, height, nil
}

// abciQuery asks the node an ABCI query of the state at height, 0 for the
// latest.
func (r *rpc) abciQuery(ctx context.Context, height int64, path string, data []byte) (responseBase, []byte, error) {
	var res struct {
		Response struct {
			ResponseBase responseBase `json:"ResponseBase"`
			Value        []byte       `json:"Value"`
		} `json:"response"`
	}
	params := map[string]any{"path": path, "data": base64.StdEncoding.EncodeToString(data), "height": strconv.FormatInt(height, 10), "prove": false}
	if err := r.call(ctx, "abci_query", params, &res); err != nil {
		return responseBase{}, nil, err
	}
	return res.Response.ResponseBase, res.Response.Value, nil
}

// blockTxs returns the raw transactions of the block at height.
func (r *rpc) blockTxs(ctx context.Context, height int64) ([][]byte, error) {
	var res struct {
		Block struct {
			Data struct {
				Txs [][]byte `json:"txs"`
			} `json:"data"`
		} `json:"block"`
	}
	err := r.call(ctx, "block", map[string]any{"height": strconv.FormatInt(height, 10)}, &res)
	return res.Block.Data.Txs, err
}

type txResult struct {
	ResponseBase responseBase `json:"ResponseBase"`
	GasUsed      string       `json:"GasUsed"`
}

func (r *rpc) broadcastTxCommit(ctx context.Context, tx []byte) (check, deliver txResult, hash []byte, height int64, err error) {
	var res struct {
		CheckTx   txResult `json:"check_tx"`
		DeliverTx txResult `json:"deliver_tx"`
		Hash      []byte   `json:"hash"`
		Height    string   `json:"height"`
	}
	if err = r.call(ctx, "broadcast_tx_commit", map[string]any{"tx": base64.StdEncoding.EncodeToString(tx)}, &res); err != nil {
		return
	}
	height, _ = strconv.ParseInt(res.Height, 10, 64)
	return res.CheckTx, res.DeliverTx, res.Hash, height, nil
}

// simResult reads the three things a simulation answer is needed for
// (gas used, whether it failed, its log) straight off amino's binary,
// which is protobuf's wire format: a ResponseDeliverTx is ResponseBase
// (field 1: Error 1, Data 2, Events 3, Log 4, Info 5), GasWanted (2) and
// GasUsed (3). Reading the whole struct would need every event and error
// type the chain may put in it registered here.
func simResult(bz []byte) (gasUsed int64, failed bool, log string, err error) {
	err = walk(bz, func(num int, val []byte, v uint64) error {
		switch num {
		case 1:
			return walk(val, func(n int, b []byte, _ uint64) error {
				switch n {
				case 1:
					failed = len(b) > 0
				case 4:
					log = string(b)
				}
				return nil
			})
		case 3:
			gasUsed = int64(v>>1) ^ -int64(v&1) // amino writes int64 zigzagged
		}
		return nil
	})
	return
}

// walk calls fn for each field of a protobuf-wire message: its number, its
// bytes if length-delimited, its value if a varint.
func walk(bz []byte, fn func(num int, val []byte, v uint64) error) error {
	for len(bz) > 0 {
		tag, n, err := amino.DecodeUvarint(bz)
		if err != nil {
			return err
		}
		bz = bz[n:]
		num, typ := int(tag>>3), tag&7
		switch typ {
		case 0:
			v, n, err := amino.DecodeUvarint(bz)
			if err != nil {
				return err
			}
			bz = bz[n:]
			if err := fn(num, nil, v); err != nil {
				return err
			}
		case 2:
			l, n, err := amino.DecodeUvarint(bz)
			if err != nil {
				return err
			}
			bz = bz[n:]
			if uint64(len(bz)) < l {
				return errors.New("truncated field")
			}
			if err := fn(num, bz[:l], 0); err != nil {
				return err
			}
			bz = bz[l:]
		case 1:
			if len(bz) < 8 {
				return errors.New("truncated field")
			}
			bz = bz[8:]
		case 5:
			if len(bz) < 4 {
				return errors.New("truncated field")
			}
			bz = bz[4:]
		default:
			return fmt.Errorf("unknown wire type %d", typ)
		}
	}
	return nil
}

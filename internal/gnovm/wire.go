// Copyright (C) 2026 Jae Kwon
//
// This file is part of gnosh's GnoVM machine subsystem. It is licensed under
// the GNO Network General Public License, version 6, as published by
// NewTendermint, LLC or its successor, or any later version of that License
// that the Licensor accepts as the proxy under its section 14, by a statement
// published in the canonical copy of internal/gnovm/LICENSE.md; together with
// the Additional Terms (Strong Attribution) in that file.

package gnovm

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The worker protocol: frames of a 4-byte big-endian length and a JSON
// message, on two pipes the client hands the worker as fds 3 (requests) and
// 4 (replies), so nothing the worker's code prints can corrupt it.

const (
	protocolMagic   = "gnosh-gnovm"
	protocolVersion = 1
	// maxFrame bounds a frame; results are capped well below it.
	maxFrame = 16 << 20
)

// Message types.
const (
	msgHello     = "hello"
	msgRun       = "run"
	msgNew       = "new"
	msgList      = "list"
	msgDrop      = "drop"
	msgKept      = "kept"
	msgRepair    = "repair"
	msgMem       = "mem"
	msgGoal      = "goal"
	msgCancel    = "cancel"
	msgResult    = "result"
	msgAck       = "ack"
	msgHeartbeat = "heartbeat"
)

// frame is one message either way.
type frame struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
	Err  *wireError      `json:"err,omitempty"`
	// Progress counts heartbeats within a request, so a stuck worker and
	// a slow one look different in the log.
	Progress uint64 `json:"progress,omitempty"`
	// Waiting marks a heartbeat sent while the request waits for mem's
	// lock, which another gnosh process holds: the client starts the
	// request's deadline again when the wait ends.
	Waiting bool `json:"waiting,omitempty"`
}

// hello is the handshake, both ways: the worker answers with its own, and
// either side quits on a mismatch.
type hello struct {
	Magic    string `json:"magic"`
	Protocol int    `json:"protocol"`
	Build    string `json:"build"`
	Gno      string `json:"gno"`
	// Set by the client only.
	Dir             string  `json:"dir,omitempty"`
	Client          string  `json:"client,omitempty"`
	BudgetBytes     int64   `json:"budget_bytes,omitempty"`
	DefaultMemoryMB int     `json:"default_memory_mb,omitempty"`
	DefaultGas      int64   `json:"default_gas,omitempty"`
	NsPerGas        float64 `json:"ns_per_gas,omitempty"`
	Embeddings      string  `json:"embeddings,omitempty"`
}

func writeFrame(w io.Writer, f *frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return fmt.Errorf("frame of %d bytes is over the %d limit", len(b), maxFrame)
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readFrame(r io.Reader) (*frame, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > maxFrame {
		return nil, fmt.Errorf("frame of %d bytes is over the %d limit", size, maxFrame)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var f frame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// wireError carries an error across the process boundary with its kind, so
// the client gets back the same types the host returns.
type wireError struct {
	Kind  string     `json:"kind"`
	Msg   string     `json:"msg"`
	Again bool       `json:"again,omitempty"`
	Limit string     `json:"limit,omitempty"`
	Stack string     `json:"stack,omitempty"`
	Inner *wireError `json:"inner,omitempty"`
}

func toWire(err error) *wireError {
	if err == nil {
		return nil
	}
	var (
		ref    *refusal
		busy   *busyError
		perr   *parseError
		lim    *limitError
		cerr   *compileError
		pan    *panicked
		rerr   *runError
		broken *brokenError
	)
	switch {
	case errors.As(err, &rerr):
		return &wireError{Kind: "run", Msg: rerr.Error(), Stack: rerr.stack, Inner: toWire(rerr.err)}
	case errors.As(err, &ref):
		return &wireError{Kind: "refusal", Msg: ref.msg}
	case errors.As(err, &busy):
		return &wireError{Kind: "busy", Msg: busy.msg, Again: busy.again}
	case errors.As(err, &perr):
		return &wireError{Kind: "parse", Msg: perr.err.Error()}
	case errors.As(err, &lim):
		return &wireError{Kind: "limit", Limit: lim.kind, Msg: lim.Error()}
	case errors.As(err, &cerr):
		return &wireError{Kind: "compile", Msg: cerr.msg}
	case errors.As(err, &pan):
		return &wireError{Kind: "panic", Msg: pan.msg}
	case errors.As(err, &broken):
		return &wireError{Kind: "broken", Msg: broken.Error()}
	}
	return &wireError{Kind: "other", Msg: err.Error()}
}

func fromWire(w *wireError) error {
	if w == nil {
		return nil
	}
	switch w.Kind {
	case "run":
		return &runError{err: fromWire(w.Inner), stack: w.Stack}
	case "refusal":
		return &refusal{w.Msg}
	case "busy":
		return &busyError{msg: w.Msg, again: w.Again}
	case "parse":
		return &parseError{errors.New(w.Msg)}
	case "limit":
		return &limitError{kind: w.Limit}
	case "compile":
		return &compileError{w.Msg}
	case "panic":
		return &panicked{w.Msg}
	case "broken":
		return &brokenError{msg: w.Msg}
	}
	return errors.New(w.Msg)
}

// wireRun is RunResult on the wire, its error carried as a wireError.
type wireRun struct {
	RunResult
	Err *wireError `json:"run_err,omitempty"`
}

// The other requests' bodies.
type (
	listReq struct {
		Project string `json:"project"`
	}
	dropReq struct {
		Project string `json:"project"`
		Name    string `json:"name"`
		Action  string `json:"action"`
	}
	keptReq struct {
		Project string `json:"project"`
		Name    string `json:"name"`
		Req     string `json:"req"`
	}
	repairReq struct {
		Project string `json:"project"`
		Name    string `json:"name"`
	}
	memReq struct {
		Project string            `json:"project"`
		Session string            `json:"session"`
		Args    string            `json:"args"`
		Titles  map[string]string `json:"titles,omitempty"`
	}
	memResp struct {
		Text   string      `json:"text"`
		Erased *MemErasure `json:"erased,omitempty"`
	}
	// goalReq is one of the goal calls: Op is "bind", "turn", "reopen",
	// "closed" or "clear".
	goalReq struct {
		Op        string       `json:"op"`
		Project   string       `json:"project"`
		Session   string       `json:"session,omitempty"`
		Condition string       `json:"condition,omitempty"`
		Binding   GoalBinding  `json:"binding"`
		Prev      *GoalBinding `json:"prev,omitempty"`
		Reason    string       `json:"reason,omitempty"`
		Marker    string       `json:"marker,omitempty"`
		Since     int64        `json:"since,omitempty"`
		Except    string       `json:"except,omitempty"`
	}
	goalResp struct {
		Binding GoalBinding  `json:"binding"`
		Turn    GoalTurn     `json:"turn"`
		Text    string       `json:"text,omitempty"`
		Closed  []ClosedGoal `json:"closed,omitempty"`
	}
	newResp struct {
		Info    VMInfo `json:"info"`
		Created bool   `json:"created"`
	}
	repairResp struct {
		Kept int `json:"kept"`
		Cut  int `json:"cut"`
	}
)

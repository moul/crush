package gnochain

import (
	"errors"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// MsgCall is a call of a realm's function, encoded exactly as gno.land's
// own vm.MsgCall: the same amino name ("/vm.m_call"), fields, tags and
// order, so the chain decodes it as its own and the signature covers the
// same bytes. It is defined here because importing gno.land/pkg/sdk/vm
// for this one type links 67 packages of the node (consensus, validator
// signing, mempool, p2p, the RPC server) into gnosh. A mismatch cannot pass
// unnoticed: the chain would refuse the signature, and TestLive signs
// against a real node.
type MsgCall struct {
	Caller     crypto.Address `json:"caller" yaml:"caller"`
	Send       std.Coins      `json:"send" yaml:"send"`
	MaxDeposit std.Coins      `json:"max_deposit" yaml:"max_deposit"`
	PkgPath    string         `json:"pkg_path" yaml:"pkg_path"`
	Func       string         `json:"func" yaml:"func"`
	Args       []string       `json:"args,omitempty" yaml:"args"`
}

// Package registers MsgCall with amino under gno.land's name for it.
var Package = amino.RegisterPackage(amino.NewPackage(
	"github.com/charmbracelet/crush/internal/gnochain",
	"vm",
	amino.GetCallersDirname(),
).WithDependencies(
	std.Package,
).WithTypes(
	MsgCall{}, "m_call",
))

func (msg MsgCall) Route() string { return "vm" }
func (msg MsgCall) Type() string  { return "exec" }

func (msg MsgCall) ValidateBasic() error {
	if msg.Caller.IsZero() {
		return errors.New("missing caller address")
	}
	if msg.PkgPath == "" || msg.Func == "" {
		return errors.New("missing package path or function")
	}
	if !msg.Send.IsValid() || !msg.MaxDeposit.IsValid() {
		return errors.New("invalid coins")
	}
	return nil
}

func (msg MsgCall) GetSignBytes() []byte {
	return std.MustSortJSON(amino.MustMarshalJSON(msg))
}

func (msg MsgCall) GetSigners() []crypto.Address { return []crypto.Address{msg.Caller} }

package gnochain

import (
	"crypto/rand"
	"errors"

	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/secp256k1"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// KeySigner signs in this process with a secp256k1 key it holds in memory,
// for demo and worker keys only: a swarm of hundreds of workers cannot run
// gnokey once per transaction. People sign with session keys through
// gnokey (GnokeySigner). The key never goes into an argument, the
// environment or a log; String and the error texts never show it.
type KeySigner struct {
	priv secp256k1.PrivKeySecp256k1
	addr crypto.Address
}

var _ Signer = (*KeySigner)(nil)

// NewKeySigner signs with a fresh random key.
func NewKeySigner() (*KeySigner, error) {
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	return KeySignerFromSeed(seed[:])
}

// KeySignerFromSeed signs with the key derived from seed, so a demo can
// make the same worker keys again (and fund them at genesis).
func KeySignerFromSeed(seed []byte) (*KeySigner, error) {
	if len(seed) < 16 {
		return nil, errors.New("a key seed is at least 16 bytes")
	}
	priv := secp256k1.GenPrivKeySecp256k1(seed)
	return &KeySigner{priv: priv, addr: priv.PubKey().Address()}, nil
}

// Address is the key's address.
func (s *KeySigner) Address() crypto.Address { return s.addr }

// Master is zero: the key acts for its own account.
func (s *KeySigner) Master() crypto.Address { return crypto.Address{} }

// PubKey is the key's public key.
func (s *KeySigner) PubKey() crypto.PubKey { return s.priv.PubKey() }

// Sign signs tx for chainID at the account's number and sequence. The
// public key goes with the signature, since an account funded at genesis
// has none stored on chain yet.
func (s *KeySigner) Sign(tx std.Tx, chainID string, accNum, seq uint64) (*std.Tx, error) {
	bz, err := tx.GetSignBytes(chainID, accNum, seq)
	if err != nil {
		return nil, err
	}
	sig, err := s.priv.Sign(bz)
	if err != nil {
		return nil, err
	}
	tx.Signatures = []std.Signature{{PubKey: s.priv.PubKey(), Signature: sig}}
	return &tx, nil
}

func (s *KeySigner) String() string { return "KeySigner(" + s.addr.String() + ")" }

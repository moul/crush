package gnochain

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/std"
	"golang.org/x/term"
)

// Signer signs transactions for one key.
type Signer interface {
	// Address is the key's own address: the account whose number and
	// sequence a signature commits to.
	Address() crypto.Address
	// Master is, for a session key, the account it acts for, which a realm
	// sees as the caller (and refuses anything that moves money); zero for
	// an account's own key.
	Master() crypto.Address
	// Sign returns tx signed for chainID at the account's number and
	// sequence.
	Sign(tx std.Tx, chainID string, accNum, seq uint64) (*std.Tx, error)
}

// caller is the address a realm sees calling when s signs: its master's
// for a session key, else its own.
func caller(s Signer) crypto.Address {
	if m := s.Master(); !m.IsZero() {
		return m
	}
	return s.Address()
}

// GnokeySigner signs with a key of the user's gnokey keybase by running
// the user's own gnokey, as gnosh's gnokey tool does: the password goes to
// gnokey's stdin only, never to an argument, the environment or a log.
// gnosh links no keybase code (gno's keybase package brings Ledger support
// in C and golang.org/x/crypto/openpgp, which the Go vulnerability database
// calls unsafe by design, GO-2026-5932).
//
// For a session key, master is the account it acts for: the realm then
// sees master as the caller, and refuses it anything that moves money.
type GnokeySigner struct {
	gnokey   string
	home     string
	name     string
	password string
	addr     crypto.Address
	master   crypto.Address
}

var _ Signer = (*GnokeySigner)(nil)

// NewGnokeySigner signs as the key name in the gnokey keybase at home, with
// the gnokey binary at gnokey (found on PATH if empty). master, when set, is
// the account a session key acts for. An empty password is asked for on the
// terminal, masked. It checks the password at once.
func NewGnokeySigner(gnokey, home, name, password, master string) (*GnokeySigner, error) {
	if gnokey == "" {
		p, err := exec.LookPath("gnokey")
		if err != nil {
			return nil, errors.New("signing needs gnokey on PATH")
		}
		gnokey = p
	}
	s := &GnokeySigner{gnokey: gnokey, home: home, name: name, password: password}
	addr, err := s.lookup()
	if err != nil {
		return nil, err
	}
	s.addr = addr
	if master != "" {
		if s.master, err = crypto.AddressFromBech32(master); err != nil {
			return nil, fmt.Errorf("the master address: %w", err)
		}
	}
	if s.password == "" {
		if s.password, err = AskPassword(fmt.Sprintf("Password for the key %s: ", name)); err != nil {
			return nil, err
		}
	}
	// Check the password now, not at the first call: sign a throwaway
	// transaction locally.
	probe := std.Tx{Msgs: []std.Msg{MsgCall{Caller: caller(s), PkgPath: "gno.land/r/x/y", Func: "F"}}, Fee: std.NewFee(1, std.NewCoin("ugnot", 1))}
	if _, err := s.Sign(probe, "check", 1, 1); err != nil {
		return nil, err
	}
	return s, nil
}

// lookup finds the key's address in `gnokey list`.
func (s *GnokeySigner) lookup() (crypto.Address, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.gnokey, "list", "-home", s.home).Output()
	if err != nil {
		return crypto.Address{}, fmt.Errorf("listing the keys in %s: %w", s.home, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// "0. name (local) - addr: g1... pub: ..."
		f := strings.Fields(sc.Text())
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "addr:" && len(f) > 1 && f[1] == s.name {
				return crypto.AddressFromBech32(f[i+1])
			}
		}
	}
	return crypto.Address{}, fmt.Errorf("no key %q in %s", s.name, s.home)
}

// Address implements [Signer].
func (s *GnokeySigner) Address() crypto.Address { return s.addr }

// Master implements [Signer].
func (s *GnokeySigner) Master() crypto.Address { return s.master }

// Sign implements [Signer]: gnokey signs tx for chainID at the account's
// number and sequence.
func (s *GnokeySigner) Sign(tx std.Tx, chainID string, accNum, seq uint64) (*std.Tx, error) {
	tx.Signatures = nil
	js, err := amino.MarshalJSON(tx)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "gnochain-sign-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "tx.json")
	if err := os.WriteFile(path, js, 0o600); err != nil {
		return nil, err
	}
	args := []string{
		"sign", "-tx-path", path, "-chainid", chainID,
		"-account-number", strconv.FormatUint(accNum, 10), "-account-sequence", strconv.FormatUint(seq, 10),
		"-home", s.home, "-insecure-password-stdin", "-quiet",
	}
	if !s.master.IsZero() {
		args = append(args, "-session")
	}
	args = append(args, s.name)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.gnokey, args...)
	cmd.Stdin = strings.NewReader(s.password + "\n")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.ReplaceAll(stderr.String(), s.password, "[password]")
		return nil, fmt.Errorf("gnokey sign: %v: %s", err, strings.TrimSpace(msg))
	}
	signed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out std.Tx
	if err := amino.UnmarshalJSON(signed, &out); err != nil {
		return nil, fmt.Errorf("reading the signed transaction: %w", err)
	}
	if len(out.Signatures) == 0 {
		return nil, errors.New("gnokey returned no signature")
	}
	return &out, nil
}

// AskPassword reads a password from the terminal without echoing it.
func AskPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("there is no terminal to ask for the key's password on")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

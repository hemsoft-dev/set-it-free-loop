//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrAuthTransfer never carries payload bytes, filesystem paths or peer errors.
var ErrAuthTransfer = errors.New("private auth transfer rejected")

const authTransferLimit = 65536
const authTransferHeaderLimit = 2048
const authTransferMagic = "SFLauth1"

// AuthExportVerifier is the trusted launch gate, not a model/provider claim.
// It must reopen the exact durable containment and attest that its restricted,
// private credential channel is ready for this binding before bytes are read.
// This package supplies no production implementation and does not activate one.
type AuthExportVerifier interface {
	ConfirmAuthExport(context.Context, AuthBinding) error
}

// AuthTransferInfo contains binding/integrity metadata only. Hashes do not
// authenticate a channel; its trusted supervisor must establish that boundary.
type AuthTransferInfo struct {
	Version     int         `json:"version"`
	Kind        string      `json:"kind"`
	Provider    Provider    `json:"provider"`
	Binding     AuthBinding `json:"binding"`
	SeedHash    string      `json:"seed_hash"`
	PayloadHash string      `json:"payload_hash"`
	Bytes       int         `json:"bytes"`
}

func (i AuthTransferInfo) valid() bool {
	return i.Version == 1 && (i.Kind == "seed" || i.Kind == "return") && i.Provider.Valid() && i.Binding.valid() && validAuthHash(i.SeedHash) && validAuthHash(i.PayloadHash) && i.Bytes > 0 && i.Bytes <= authTransferLimit && (i.Kind != "seed" || i.PayloadHash == i.SeedHash)
}

type authTransferState struct {
	mu   sync.Mutex
	info AuthTransferInfo
	data []byte
	used bool
}

// AuthTransfer is opaque, redacted under fmt/JSON, and single-use across copies.
// Consume/WriteTo zero the owned payload even on failure. A transport must not
// retry a partial send; recovery uses the original durable profile binding.
type AuthTransfer struct{ state *authTransferState }

func (AuthTransfer) String() string               { return "AuthTransfer{redacted}" }
func (AuthTransfer) GoString() string             { return "AuthTransfer{redacted}" }
func (AuthTransfer) MarshalJSON() ([]byte, error) { return []byte(`"AuthTransfer{redacted}"`), nil }
func (t AuthTransfer) Info() (AuthTransferInfo, error) {
	if t.state == nil {
		return AuthTransferInfo{}, ErrAuthTransfer
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	return t.state.info, nil
}
func (t AuthTransfer) Close() error {
	if t.state != nil {
		t.state.mu.Lock()
		defer t.state.mu.Unlock()
		clear(t.state.data)
		t.state.data = nil
		t.state.used = true
	}
	return nil
}

// Consume exposes bytes only to trusted credential-channel/profile code. The
// callback must not log, retain or send them to a model/reader/publisher. Its
// errors are normalized so arbitrary callback messages cannot expose auth.
func (t AuthTransfer) Consume(use func([]byte) error) error {
	if t.state == nil || use == nil {
		return ErrAuthTransfer
	}
	s := t.state
	s.mu.Lock()
	if s.used || !s.info.valid() || len(s.data) != s.info.Bytes || authHash(s.data) != s.info.PayloadHash {
		s.mu.Unlock()
		return ErrAuthTransfer
	}
	s.used = true
	data := s.data
	s.data = nil
	s.mu.Unlock()
	defer clear(data)
	if use(data) != nil {
		return ErrAuthTransfer
	}
	return nil
}

func newAuthTransfer(info AuthTransferInfo, data []byte) (AuthTransfer, error) {
	if !info.valid() || len(data) != info.Bytes || authHash(data) != info.PayloadHash {
		return AuthTransfer{}, ErrAuthTransfer
	}
	return AuthTransfer{&authTransferState{info: info, data: append([]byte(nil), data...)}}, nil
}

// NewAuthReturn is called by the trusted guest broker after its provider role
// has stopped, using the seed's captured metadata and one allowlisted file.
// It accepts no profile path, opposite-provider selector or replacement binding.
func NewAuthReturn(seed AuthTransferInfo, data []byte) (AuthTransfer, error) {
	if !seed.valid() || seed.Kind != "seed" || len(data) == 0 || len(data) > authTransferLimit {
		return AuthTransfer{}, ErrAuthTransfer
	}
	info := seed
	info.Kind = "return"
	info.PayloadHash = authHash(data)
	info.Bytes = len(data)
	return newAuthTransfer(info, data)
}

// WriteTo encodes exactly one bounded private record. The caller owns channel
// authentication/deadlines and must terminate this record's finite substream.
// Neither fmt nor json.Marshal serializes secret bytes; this is the wire API.
func (t AuthTransfer) WriteTo(w io.Writer) (written int64, result error) {
	if w == nil {
		return 0, ErrAuthTransfer
	}
	info, err := t.Info()
	if err != nil {
		return 0, ErrAuthTransfer
	}
	header, err := json.Marshal(info)
	if err != nil || len(header) > authTransferHeaderLimit {
		return 0, ErrAuthTransfer
	}
	prefix := make([]byte, len(authTransferMagic)+4)
	copy(prefix, authTransferMagic)
	binary.BigEndian.PutUint32(prefix[len(authTransferMagic):], uint32(len(header)))
	result = t.Consume(func(data []byte) error {
		for _, part := range [][]byte{prefix, header, data} {
			for len(part) > 0 {
				n, err := w.Write(part)
				if n < 0 || n > len(part) {
					return ErrAuthTransfer
				}
				written += int64(n)
				if err != nil || n == 0 {
					return ErrAuthTransfer
				}
				part = part[n:]
			}
		}
		return nil
	})
	return written, result
}

// ReadAuthTransfer accepts one finite private record and requires EOF. It does
// not authenticate its origin or authorize using a profile; those belong to the
// separately qualified channel/launch gate. Partial/extra/cross-job data fails.
func ReadAuthTransfer(r io.Reader) (AuthTransfer, error) {
	if r == nil {
		return AuthTransfer{}, ErrAuthTransfer
	}
	reader := io.LimitReader(r, int64(len(authTransferMagic)+4+authTransferHeaderLimit+authTransferLimit+1))
	prefix := make([]byte, len(authTransferMagic)+4)
	if _, err := io.ReadFull(reader, prefix); err != nil || string(prefix[:len(authTransferMagic)]) != authTransferMagic {
		return AuthTransfer{}, ErrAuthTransfer
	}
	length := binary.BigEndian.Uint32(prefix[len(authTransferMagic):])
	if length == 0 || length > authTransferHeaderLimit {
		return AuthTransfer{}, ErrAuthTransfer
	}
	header := make([]byte, int(length))
	if _, err := io.ReadFull(reader, header); err != nil {
		return AuthTransfer{}, ErrAuthTransfer
	}
	var info AuthTransferInfo
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&info) != nil || decoder.Decode(new(any)) != io.EOF || !info.valid() {
		return AuthTransfer{}, ErrAuthTransfer
	}
	canonical, _ := json.Marshal(info)
	if !bytes.Equal(canonical, header) {
		return AuthTransfer{}, ErrAuthTransfer
	}
	payload := make([]byte, info.Bytes)
	defer clear(payload)
	if _, err := io.ReadFull(reader, payload); err != nil || authHash(payload) != info.PayloadHash {
		return AuthTransfer{}, ErrAuthTransfer
	}
	var trailing [1]byte
	if n, err := reader.Read(trailing[:]); n != 0 || err != io.EOF {
		return AuthTransfer{}, ErrAuthTransfer
	}
	return newAuthTransfer(info, payload)
}

// ExportAuth journals one export before exposing any seed. A repeated or
// uncertain export cannot reuse a possibly refreshed seed, even after restart.
func (p *AuthProfile) ExportAuth(ctx context.Context, gate AuthExportVerifier) (AuthTransfer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil || gate == nil || ctx.Err() != nil {
		return AuthTransfer{}, ErrAuthTransfer
	}
	j, err := readAuthJournal(p.held.root)
	if err != nil || j != p.journal || j.Stage != "active" || j.TransferStage != "" {
		return AuthTransfer{}, ErrAuthRecoveryRequired
	}
	if gate.ConfirmAuthExport(ctx, j.Binding) != nil || ctx.Err() != nil {
		return AuthTransfer{}, ErrAuthTransfer
	}
	seed, err := readAuth(p.held.root, p.store.authRelative(), false)
	defer clear(seed)
	if err != nil || authHash(seed) != j.SeedHash {
		return AuthTransfer{}, ErrAuthRecoveryRequired
	}
	state, err := readAuth(p.held.root, "state.json", false)
	defer clear(state)
	if err != nil || authHash(state) != j.SeedHash {
		return AuthTransfer{}, ErrAuthRecoveryRequired
	}
	j.TransferStage, j.TransferHash = "exported", j.SeedHash
	if writeAuthJournal(p.held.root, j) != nil {
		return AuthTransfer{}, ErrAuthRecoveryRequired
	}
	p.journal = j
	return newAuthTransfer(AuthTransferInfo{Version: 1, Kind: "seed", Provider: j.Provider, Binding: j.Binding, SeedHash: j.SeedHash, PayloadHash: j.SeedHash, Bytes: len(seed)}, seed)
}

// ImportAuthReturn stages a private guest return only after a fresh whole-VM
// stop proof. It never commits canonical state or automatically repairs a lease.
// ResumeAuthReturn/Complete recover its exact journaled bytes after interruption.
func (p *AuthProfile) ImportAuthReturn(ctx context.Context, t AuthTransfer, verifier AuthStopVerifier) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer t.Close()
	info, err := t.Info()
	if err != nil || !info.valid() || info.Kind != "return" || p.held == nil || info.Provider != p.journal.Provider || info.Binding != p.journal.Binding || info.SeedHash != p.journal.SeedHash {
		return ErrAuthTransfer
	}
	j, err := p.stopped(ctx, verifier)
	if err != nil || j.Stage != "active" || j.TransferStage != "exported" || info.Provider != j.Provider || info.Binding != j.Binding || info.SeedHash != j.SeedHash {
		return ErrAuthRecoveryRequired
	}
	returnErr := t.Consume(func(payload []byte) error {
		j.TransferStage, j.TransferHash = "receiving", info.PayloadHash
		if err := writeAuthJournal(p.held.root, j); err != nil {
			return err
		}
		p.journal = j
		if err := writeAuth(p.held.root, "pending.json", payload); err != nil {
			return err
		}
		return p.finishAuthReturn(j)
	})
	if returnErr != nil {
		return ErrAuthRecoveryRequired
	}
	return nil
}
func (p *AuthProfile) finishAuthReturn(j authJournal) error {
	state, err := readAuth(p.held.root, "state.json", false)
	defer clear(state)
	if err != nil || authHash(state) != j.SeedHash {
		return ErrAuthRecoveryRequired
	}
	pending, err := readAuth(p.held.root, "pending.json", false)
	defer clear(pending)
	if err != nil || authHash(pending) != j.TransferHash {
		return ErrAuthRecoveryRequired
	}
	staged, err := readAuth(p.held.root, p.store.authRelative(), false)
	defer clear(staged)
	if err != nil || (authHash(staged) != j.SeedHash && authHash(staged) != j.TransferHash) {
		return ErrAuthRecoveryRequired
	}
	if authHash(staged) != j.TransferHash {
		if err := writeAuth(p.held.root, p.store.authRelative(), pending); err != nil {
			return err
		}
	}
	j.TransferStage = "returned"
	if err := writeAuthJournal(p.held.root, j); err != nil {
		return err
	}
	p.journal = j
	return nil
}
func (p *AuthProfile) ResumeAuthReturn(ctx context.Context, verifier AuthStopVerifier) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.stopped(ctx, verifier)
	if err != nil || j.Stage != "active" || (j.TransferStage != "receiving" && j.TransferStage != "returned") {
		return ErrAuthRecoveryRequired
	}
	if err := p.finishAuthReturn(j); err != nil {
		return ErrAuthRecoveryRequired
	}
	return nil
}

var _ fmt.Stringer = AuthTransfer{}
var _ io.WriterTo = AuthTransfer{}

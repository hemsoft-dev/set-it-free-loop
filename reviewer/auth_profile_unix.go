//go:build linux || darwin

package reviewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var ErrAuthRecoveryRequired = errors.New("auth profile requires stopped-worker recovery")

// AuthBinding identifies the durable supervisor record created BEFORE any
// process starts. Session and containment IDs are 32-byte hex hashes. LaunchID
// is the same 16-byte hex nonce used by SourceReaderConfig, never a PID or auth.
type AuthBinding struct {
	SessionHash   string `json:"session_hash"`
	LaunchID      string `json:"launch_id"`
	ContainmentID string `json:"containment_id"`
}

func (b AuthBinding) valid() bool {
	for _, s := range []string{b.SessionHash, b.ContainmentID} {
		if !validAuthHash(s) {
			return false
		}
	}
	v, err := hex.DecodeString(b.LaunchID)
	return err == nil && len(v) == 16 && hex.EncodeToString(v) == b.LaunchID
}

func validAuthHash(s string) bool {
	v, err := hex.DecodeString(s)
	return err == nil && len(v) == 32 && hex.EncodeToString(v) == s
}

// AuthStopVerifier is trusted supervisor code. It must re-open the exact durable
// containment record and confirm all descendants stopped and profile mounts
// detached, even after controller restart. A client exit or model assertion is
// insufficient. An error retains the journal, credentials and profile.
type AuthStopVerifier interface {
	ConfirmAuthStopped(context.Context, AuthBinding) error
}

type authJournal struct {
	Version  int         `json:"version"`
	Provider Provider    `json:"provider"`
	Binding  AuthBinding `json:"binding"`
	Stage    string      `json:"stage"`
	SeedHash string      `json:"seed_hash"`
	NextHash string      `json:"next_hash,omitempty"`
}

type authLock struct {
	root *os.Root
	file *os.File
}

func (l *authLock) close() error {
	// Closing the descriptor releases flock, including when the process dies.
	return errors.Join(l.file.Close(), l.root.Close())
}

func (a *AuthStore) lock(ctx context.Context) (*authLock, error) {
	if err := privateDirectory(a.directory); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(a.directory)
	if err != nil {
		return nil, err
	}
	f, err := r.OpenFile("refresh.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		r.Close()
		return nil, errors.New("auth lock unavailable")
	}
	l := &authLock{r, f}
	if err := privateAuthFile(f); err != nil {
		l.close()
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			l.close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := clearAuthTemps(r); err != nil {
				l.close()
				return nil, err
			}
			return l, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			l.close()
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			l.close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Temporary replacement files have one reserved exact filename pattern. They
// cannot be in use after acquiring flock. Failed/partial writes are never
// recovered as auth state; only state.json or a journal-bound pending.json can
// be committed. This also removes private credential fragments after a crash.
func clearAuthTemps(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(513)
	f.Close()
	if (err != nil && err != io.EOF) || len(entries) > 512 {
		return ErrAuthRecoveryRequired
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != len(".replace-")+32 || name[:len(".replace-")] != ".replace-" {
			continue
		}
		nonce := name[len(".replace-"):]
		decoded, err := hex.DecodeString(nonce)
		if err != nil || hex.EncodeToString(decoded) != nonce {
			continue
		}
		file, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return ErrAuthRecoveryRequired
		}
		err = privateAuthFile(file)
		file.Close()
		if err != nil {
			return ErrAuthRecoveryRequired
		}
		if err := r.Remove(name); err != nil {
			return err
		}
	}
	return syncAuthDirectory(r)
}

func privateAuthFile(f *os.File) error {
	i, err := f.Stat()
	if err != nil {
		return err
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || s.Uid != uint32(os.Geteuid()) || s.Nlink != 1 {
		return errors.New("auth file must be private, owned and unlinked elsewhere")
	}
	return nil
}

func readAuth(r *os.Root, path string, missing bool) ([]byte, error) {
	f, err := r.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if missing && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("auth file unavailable")
	}
	defer f.Close()
	if err := privateAuthFile(f); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) == 0 || len(b) > 65536 {
		return nil, errors.New("invalid auth file size")
	}
	return b, nil
}

func syncAuthDirectory(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// All replacement files are created inside the anchored provider directory.
func writeAuth(r *os.Root, path string, b []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".replace-" + hex.EncodeToString(nonce[:])
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(name)
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := r.Rename(name, path); err != nil {
		return err
	}
	parent, err := r.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = syncAuthDirectory(parent)
	parent.Close()
	return err
}

func authHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func writeAuthJournal(r *os.Root, j authJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeAuth(r, "lease.json", b)
}

func readAuthJournal(r *os.Root) (authJournal, error) {
	b, err := readAuth(r, "lease.json", false)
	var j authJournal
	if err != nil {
		return j, ErrAuthRecoveryRequired
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&j) != nil || d.Decode(new(any)) != io.EOF || !j.Binding.valid() || j.Version != 1 || !j.Provider.Valid() ||
		!validAuthHash(j.SeedHash) || (j.Stage != "active" && j.Stage != "committing" && j.Stage != "committed" && j.Stage != "retiring") ||
		(j.Stage == "active" && j.NextHash != "") ||
		((j.Stage == "committing" || j.Stage == "committed") && !validAuthHash(j.NextHash)) ||
		(j.Stage == "retiring" && j.NextHash != "" && !validAuthHash(j.NextHash)) {
		return authJournal{}, ErrAuthRecoveryRequired
	}
	canonical, _ := json.Marshal(j)
	if !bytes.Equal(canonical, b) {
		return authJournal{}, ErrAuthRecoveryRequired
	}
	return j, nil
}

// AuthProfile owns a cross-process refresh lock plus a durable crash journal.
// Close only releases the lock. It NEVER retires a possibly live worker lease.
// The supervisor-private profile is not an interactive HOME import. A launcher
// must transfer only its auth file to a private worker profile and return that
// file after termination; workers must never mount this host auth store.
type AuthProfile struct {
	mu      sync.Mutex
	store   *AuthStore
	held    *authLock
	journal authJournal
}

func (a *AuthStore) authRelative() string {
	if a.provider == Codex {
		return "profile/.codex/auth.json"
	}
	return "profile/.claude/.credentials.json"
}

func noAuthLease(r *os.Root) error {
	for _, path := range []string{"lease.json", "profile", "pending.json"} {
		if _, err := r.Lstat(path); !os.IsNotExist(err) {
			return ErrAuthRecoveryRequired
		}
	}
	return nil
}

// ProfileDirectory is supervisor-only. It contains just the provider auth file,
// with no inherited settings, hooks, history, MCP configuration or other auth.
func (p *AuthProfile) ProfileDirectory() string { return filepath.Join(p.store.directory, "profile") }
func (p *AuthProfile) AuthFile() string {
	return filepath.Join(p.store.directory, p.store.authRelative())
}

// LeaseProfile durably reserves credentials before exposing any profile. There
// is no expiry takeover. A stale journal requires the same containment binding.
func (a *AuthStore) LeaseProfile(ctx context.Context, b AuthBinding) (*AuthProfile, error) {
	if !b.valid() {
		return nil, errors.New("invalid auth binding")
	}
	l, err := a.lock(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			l.close()
		}
	}()
	if err := noAuthLease(l.root); err != nil {
		return nil, err
	}
	seed, err := readAuth(l.root, "state.json", false)
	if err != nil {
		return nil, err
	}
	j := authJournal{Version: 1, Provider: a.provider, Binding: b, Stage: "active", SeedHash: authHash(seed)}
	if err := writeAuthJournal(l.root, j); err != nil {
		return nil, err
	}
	if err := l.root.Mkdir("profile", 0700); err != nil {
		return nil, err
	}
	if err := l.root.Mkdir(filepath.Dir(a.authRelative()), 0700); err != nil {
		return nil, err
	}
	if err := writeAuth(l.root, a.authRelative(), seed); err != nil {
		return nil, err
	}
	profile, err := l.root.OpenRoot("profile")
	if err != nil {
		return nil, err
	}
	err = syncAuthDirectory(profile)
	profile.Close()
	if err != nil {
		return nil, err
	}
	if err := syncAuthDirectory(l.root); err != nil {
		return nil, err
	}
	success = true
	return &AuthProfile{store: a, held: l, journal: j}, nil
}

// RecoverProfile re-acquires the lock but never starts work or reseeds a profile.
// Complete or Retire must verify whole-containment shutdown afresh.
func (a *AuthStore) RecoverProfile(ctx context.Context, b AuthBinding) (*AuthProfile, error) {
	if !b.valid() {
		return nil, ErrAuthRecoveryRequired
	}
	l, err := a.lock(ctx)
	if err != nil {
		return nil, err
	}
	j, err := readAuthJournal(l.root)
	if err != nil || j.Binding != b || j.Provider != a.provider {
		l.close()
		return nil, ErrAuthRecoveryRequired
	}
	return &AuthProfile{store: a, held: l, journal: j}, nil
}

func (p *AuthProfile) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.close()
}
func (p *AuthProfile) close() error {
	if p.held == nil {
		return nil
	}
	err := p.held.close()
	p.held = nil
	return err
}

func (p *AuthProfile) stopped(ctx context.Context, v AuthStopVerifier) (authJournal, error) {
	if p.held == nil || v == nil {
		return authJournal{}, ErrAuthRecoveryRequired
	}
	j, err := readAuthJournal(p.held.root)
	if err != nil || j != p.journal {
		return authJournal{}, ErrAuthRecoveryRequired
	}
	if err := v.ConfirmAuthStopped(ctx, j.Binding); err != nil {
		return authJournal{}, ErrAuthRecoveryRequired
	}
	if err := ctx.Err(); err != nil {
		return authJournal{}, err
	}
	return j, nil
}

// Complete commits only the stopped worker's allowlisted auth file. A prepared
// replacement plus journal makes every state-rename crash recoverable without
// restoring the original seed. Unknown state or a lost final profile fails shut.
func (p *AuthProfile) Complete(ctx context.Context, v AuthStopVerifier) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.stopped(ctx, v)
	if err != nil || j.Stage == "retiring" {
		return ErrAuthRecoveryRequired
	}
	r := p.held.root
	state, err := readAuth(r, "state.json", false)
	if err != nil {
		return ErrAuthRecoveryRequired
	}
	if j.Stage == "active" {
		if authHash(state) != j.SeedHash {
			return ErrAuthRecoveryRequired
		}
		next, err := readAuth(r, p.store.authRelative(), false)
		if err != nil {
			return ErrAuthRecoveryRequired
		}
		if err := writeAuth(r, "pending.json", next); err != nil {
			return err
		}
		j.Stage, j.NextHash = "committing", authHash(next)
		if err := writeAuthJournal(r, j); err != nil {
			return err
		}
		p.journal = j
	}
	if j.Stage == "committing" {
		if authHash(state) == j.SeedHash {
			next, err := readAuth(r, "pending.json", false)
			if err != nil || authHash(next) != j.NextHash {
				return ErrAuthRecoveryRequired
			}
			if err := writeAuth(r, "state.json", next); err != nil {
				return err
			}
		} else if authHash(state) != j.NextHash {
			return ErrAuthRecoveryRequired
		}
		j.Stage = "committed"
		if err := writeAuthJournal(r, j); err != nil {
			return err
		}
		p.journal = j
	} else if j.Stage == "committed" && authHash(state) != j.NextHash {
		return ErrAuthRecoveryRequired
	}
	return p.dispose()
}

// Retire invalidates credentials after confirmed shutdown when final auth is
// lost or malformed. It cannot restore the old seed or unlock live containment.
// The next refresh sees no state and must establish new auth separately.
func (p *AuthProfile) Retire(ctx context.Context, v AuthStopVerifier) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, err := p.stopped(ctx, v)
	if err != nil {
		return err
	}
	r := p.held.root
	state, err := readAuth(r, "state.json", true)
	if err != nil || (len(state) > 0 && authHash(state) != j.SeedHash && authHash(state) != j.NextHash) {
		return ErrAuthRecoveryRequired
	}
	j.Stage = "retiring"
	if err := writeAuthJournal(r, j); err != nil {
		return err
	}
	p.journal = j
	if err := r.Remove("state.json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncAuthDirectory(r); err != nil {
		return err
	}
	return p.dispose()
}

func (p *AuthProfile) dispose() error {
	r := p.held.root
	// This is our disposable profile after the trusted verifier confirmed no
	// processes or mounts remain. os.Root confines traversal and never follows
	// symlinks out of the root. No unknown shared profile is accepted on acquire.
	if err := r.RemoveAll("profile"); err != nil {
		return err
	}
	if err := r.Remove("pending.json"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncAuthDirectory(r); err != nil {
		return err
	}
	if err := r.Remove("lease.json"); err != nil {
		return err
	}
	if err := syncAuthDirectory(r); err != nil {
		return err
	}
	return p.close()
}

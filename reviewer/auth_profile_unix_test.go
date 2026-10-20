//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type authStopCheck func(context.Context, AuthBinding) error

func (f authStopCheck) ConfirmAuthStopped(ctx context.Context, b AuthBinding) error { return f(ctx, b) }

func authTestBinding() AuthBinding {
	return AuthBinding{strings.Repeat("a", 64), strings.Repeat("b", 32), strings.Repeat("c", 64)}
}
func authTestStore(t *testing.T, provider Provider) *AuthStore {
	t.Helper()
	r := t.TempDir()
	if err := os.Chmod(r, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthStore(provider, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(context.Background(), func([]byte) ([]byte, error) { return []byte("synthetic-seed"), nil }); err != nil {
		t.Fatal(err)
	}
	return a
}
func authMustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func authStopped(t *testing.T) AuthStopVerifier {
	return authStopCheck(func(_ context.Context, b AuthBinding) error {
		t.Helper()
		if b != authTestBinding() {
			t.Fatal("wrong containment binding")
		}
		return nil
	})
}
func authAssertRecovery(t *testing.T, a *AuthStore) {
	t.Helper()
	called := false
	err := a.Refresh(context.Background(), func([]byte) ([]byte, error) { called = true; return []byte("stale"), nil })
	if !errors.Is(err, ErrAuthRecoveryRequired) || called {
		t.Fatalf("refresh crossed unresolved lease: %v, callback %v", err, called)
	}
}

func TestAuthProfileProviderIsolationAndStoppedCommit(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			a := authTestStore(t, provider)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			b, err := os.ReadFile(p.AuthFile())
			if err != nil || string(b) != "synthetic-seed" {
				t.Fatal("missing synthetic profile", err)
			}
			var files []string
			err = filepath.WalkDir(p.ProfileDirectory(), func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					rel, _ := filepath.Rel(p.ProfileDirectory(), path)
					files = append(files, rel)
				}
				return nil
			})
			if err != nil || len(files) != 1 || filepath.Join("profile", files[0]) != a.authRelative() {
				t.Fatalf("profile inherited other files: %v %v", files, err)
			}
			other := Claude
			if provider == Claude {
				other = Codex
			}
			o, err := NewAuthStore(other, filepath.Dir(a.directory))
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Refresh(context.Background(), func(old []byte) ([]byte, error) {
				if len(old) != 0 {
					t.Fatal("cross-provider credentials")
				}
				return []byte("other-seed"), nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, acquire := range []func(context.Context) error{
				func(ctx context.Context) error {
					return a.Refresh(ctx, func([]byte) ([]byte, error) { t.Fatal("live refresh entered"); return nil, nil })
				},
				func(ctx context.Context) error { _, err := a.LeaseProfile(ctx, authTestBinding()); return err },
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				err := acquire(ctx)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("lease was not exclusive", err)
				}
			}
			authMustWrite(t, p.AuthFile(), "synthetic-rotated")
			if err := p.Complete(context.Background(), nil); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("accepted missing stop proof", err)
			}
			if err := p.Complete(context.Background(), authStopCheck(func(context.Context, AuthBinding) error { return errors.New("still alive") })); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("ignored live containment", err)
			}
			b, _ = os.ReadFile(filepath.Join(a.directory, "state.json"))
			if string(b) != "synthetic-seed" {
				t.Fatal("committed before stop")
			}
			if err := p.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p.ProfileDirectory()); !os.IsNotExist(err) {
				t.Fatal("profile retained", err)
			}
			if err := a.Refresh(context.Background(), func(old []byte) ([]byte, error) {
				if string(old) != "synthetic-rotated" {
					t.Fatal("restored old seed")
				}
				return []byte("next-refresh"), nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthProfileProcessCrashHelper(t *testing.T) {
	root := os.Getenv("SFL_FAKE_AUTH_CRASH_ROOT")
	if root == "" {
		return
	}
	a, err := NewAuthStore(Codex, root)
	if err != nil {
		os.Exit(2)
	}
	p, err := a.LeaseProfile(context.Background(), authTestBinding())
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(p.AuthFile(), []byte("synthetic-child-refresh"), 0600); err != nil {
		os.Exit(4)
	}
	// No Close, no completion, no defers. This simulates abrupt controller loss.
	os.Exit(73)
}

func TestAuthProfileProcessCrashRequiresExactRecovery(t *testing.T) {
	a := authTestStore(t, Codex)
	c := exec.Command(os.Args[0], "-test.run=^TestAuthProfileProcessCrashHelper$")
	c.Env = append(os.Environ(), "SFL_FAKE_AUTH_CRASH_ROOT="+filepath.Dir(a.directory))
	err := c.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatal("helper failed before simulated crash", err)
	}
	authAssertRecovery(t, a)
	wrong := authTestBinding()
	wrong.LaunchID = strings.Repeat("d", 32)
	if _, err := a.RecoverProfile(context.Background(), wrong); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("wrong recovery accepted", err)
	}
	p, err := a.RecoverProfile(context.Background(), authTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Complete(context.Background(), authStopped(t)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(a.directory, "state.json"))
	if string(b) != "synthetic-child-refresh" {
		t.Fatal("lost child refresh")
	}
}

// Construct each durable crash boundary directly from real on-disk operations.
// The separate process test above also proves flock release is not lease release.
func TestAuthProfileCommitCrashBoundaries(t *testing.T) {
	for _, point := range []string{"pending-only", "prepared", "state-replaced", "committed", "profile-removed", "pending-removed"} {
		t.Run(point, func(t *testing.T) {
			a := authTestStore(t, Codex)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			r := p.held.root
			authMustWrite(t, p.AuthFile(), "synthetic-final")
			if err := writeAuth(r, "pending.json", []byte("synthetic-final")); err != nil {
				t.Fatal(err)
			}
			j := p.journal
			if point != "pending-only" {
				j.Stage = "committing"
				j.NextHash = authHash([]byte("synthetic-final"))
				if err := writeAuthJournal(r, j); err != nil {
					t.Fatal(err)
				}
			}
			if point == "state-replaced" || point == "committed" || point == "profile-removed" || point == "pending-removed" {
				if err := writeAuth(r, "state.json", []byte("synthetic-final")); err != nil {
					t.Fatal(err)
				}
			}
			if point == "committed" || point == "profile-removed" || point == "pending-removed" {
				j.Stage = "committed"
				if err := writeAuthJournal(r, j); err != nil {
					t.Fatal(err)
				}
			}
			if point == "profile-removed" || point == "pending-removed" {
				if err := r.RemoveAll("profile"); err != nil {
					t.Fatal(err)
				}
			}
			if point == "pending-removed" {
				if err := r.Remove("pending.json"); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			authAssertRecovery(t, a)
			recovered, err := a.RecoverProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if err := recovered.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(a.directory, "state.json"))
			if string(b) != "synthetic-final" {
				t.Fatal("recovery restored seed")
			}
		})
	}
}

func TestAuthProfileHostileFinalStateAndRetirement(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "oversize", "symlink", "hardlink", "fifo", "public"} {
		t.Run(kind, func(t *testing.T) {
			a := authTestStore(t, Codex)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			canary := filepath.Join(t.TempDir(), "canary")
			authMustWrite(t, canary, "private-canary")
			if err := os.Remove(p.AuthFile()); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty":
				authMustWrite(t, p.AuthFile(), "")
			case "oversize":
				authMustWrite(t, p.AuthFile(), strings.Repeat("x", 65537))
			case "symlink":
				err = os.Symlink(canary, p.AuthFile())
			case "hardlink":
				err = os.Link(canary, p.AuthFile())
			case "fifo":
				err = syscall.Mkfifo(p.AuthFile(), 0600)
			case "public":
				authMustWrite(t, p.AuthFile(), "bad-mode")
				err = os.Chmod(p.AuthFile(), 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("unsafe profile accepted", err)
			}
			if err := p.Retire(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(canary)
			if string(b) != "private-canary" {
				t.Fatal("canary changed")
			}
			if err := a.Refresh(context.Background(), func(old []byte) ([]byte, error) {
				if len(old) != 0 {
					t.Fatal("retired auth reused")
				}
				return []byte("synthetic-new-auth"), nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthProfileRejectsChangedCanonicalStateAndCorruptJournal(t *testing.T) {
	for _, kind := range []string{"canonical", "journal", "profile-parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			a := authTestStore(t, Claude)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			switch kind {
			case "canonical":
				authMustWrite(t, filepath.Join(a.directory, "state.json"), "unknown-new-state")
			case "journal":
				authMustWrite(t, filepath.Join(a.directory, "lease.json"), "{}")
			case "profile-parent-symlink":
				outside := t.TempDir()
				authMustWrite(t, filepath.Join(outside, ".credentials.json"), "private-outside-canary")
				if err := os.Remove(p.AuthFile()); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Dir(p.AuthFile())
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("unknown state accepted", err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			authAssertRecovery(t, a)
		})
	}
}

func TestAuthProfileRetiringCrashAndIncompleteSetup(t *testing.T) {
	for _, point := range []string{"before-profile", "retiring", "state-removed", "profile-removed"} {
		t.Run(point, func(t *testing.T) {
			a := authTestStore(t, Codex)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			j := p.journal
			if point != "before-profile" {
				j.Stage = "retiring"
				if err := writeAuthJournal(p.held.root, j); err != nil {
					t.Fatal(err)
				}
			}
			if point == "state-removed" || point == "profile-removed" {
				if err := p.held.root.Remove("state.json"); err != nil {
					t.Fatal(err)
				}
			}
			if point == "before-profile" || point == "profile-removed" {
				if err := p.held.root.RemoveAll("profile"); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			authAssertRecovery(t, a)
			recovered, err := a.RecoverProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if err := recovered.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("completed lost/retiring profile", err)
			}
			if err := recovered.Retire(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(a.directory, "state.json")); !os.IsNotExist(err) {
				t.Fatal("old seed remained", err)
			}
		})
	}
}

func TestAuthProfileReplacementFragmentsAndDisposalCanary(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-fragment", true: "unsafe-fragment"}[unsafe], func(t *testing.T) {
			a := authTestStore(t, Claude)
			canary := filepath.Join(t.TempDir(), "outside")
			authMustWrite(t, canary, "private-canary")
			fragment := filepath.Join(a.directory, ".replace-"+strings.Repeat("a", 32))
			unowned := filepath.Join(a.directory, ".replace-unknown")
			authMustWrite(t, unowned, "preserve-unknown")
			if unsafe {
				if err := os.Symlink(canary, fragment); err != nil {
					t.Fatal(err)
				}
			} else {
				authMustWrite(t, fragment, "partial-synthetic-credential")
			}
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if unsafe {
				if !errors.Is(err, ErrAuthRecoveryRequired) {
					t.Fatal("unsafe temporary accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err := os.Lstat(fragment); !os.IsNotExist(err) {
				t.Fatal("fragment retained", err)
			}
			if b, err := os.ReadFile(unowned); err != nil || string(b) != "preserve-unknown" {
				t.Fatal("unknown entry removed", err)
			}
			if err := os.Symlink(filepath.Dir(canary), filepath.Join(p.ProfileDirectory(), "outside-link")); err != nil {
				t.Fatal(err)
			}
			if err := p.Complete(context.Background(), authStopped(t)); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(canary); err != nil || string(b) != "private-canary" {
				t.Fatal("disposal followed symlink", err)
			}
		})
	}
}

func TestAuthProfileOrphansNeverPermitLocalRefresh(t *testing.T) {
	for _, name := range []string{"profile", "pending.json"} {
		t.Run(name, func(t *testing.T) {
			a := authTestStore(t, Codex)
			if name == "profile" {
				if err := os.Mkdir(filepath.Join(a.directory, name), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				authMustWrite(t, filepath.Join(a.directory, name), "unbound-synthetic-state")
			}
			authAssertRecovery(t, a)
			if _, err := a.LeaseProfile(context.Background(), authTestBinding()); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("orphan reused", err)
			}
		})
	}
}

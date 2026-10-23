//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func rootNoFollowFixture(t *testing.T) (string, *os.Root) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "real-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	authMustWrite(t, filepath.Join(directory, "real.json"), "in-root-target")
	authMustWrite(t, filepath.Join(directory, "real-dir", "file.json"), "in-root-target")
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return directory, root
}

func rootNoFollowLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// os.Root.OpenFile follows an in-root symlink even with O_NOFOLLOW. The helper
// must refuse every link shape, including intermediate directories and dangling
// links that O_CREATE without O_EXCL would otherwise create through.
func TestOpenRootNoFollowRefusesEverySymlinkShape(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.json")
	authMustWrite(t, outside, "outside-canary")
	for _, c := range []struct {
		name, link, target string
		flag               int
	}{
		{"final in-root", "state.json", "real.json", os.O_RDONLY | syscall.O_NONBLOCK},
		{"final escaping", "state.json", outside, os.O_RDONLY | syscall.O_NONBLOCK},
		{"final directory", "state.json", "real-dir", os.O_RDONLY | syscall.O_DIRECTORY},
		{"intermediate in-root", "profile/.codex/auth.json", "../real-dir", os.O_RDONLY | syscall.O_NONBLOCK},
		{"intermediate escaping", "profile/.codex/auth.json", filepath.Dir(outside), os.O_RDONLY | syscall.O_NONBLOCK},
		{"write in-root", "cgroup.kill", "real.json", os.O_WRONLY},
		{"create in-root", "refresh.lock", "real.json", os.O_CREATE | os.O_RDWR | syscall.O_NONBLOCK},
		{"create dangling", "refresh.lock", "planted.lock", os.O_CREATE | os.O_RDWR | syscall.O_NONBLOCK},
		{"exclusive dangling", "payload.bin", "planted.lock", os.O_CREATE | os.O_EXCL | os.O_WRONLY},
		{"exclusive intermediate", "usr/bin/git", "../real-dir", os.O_CREATE | os.O_EXCL | os.O_WRONLY},
	} {
		t.Run(c.name, func(t *testing.T) {
			directory, root := rootNoFollowFixture(t)
			name := c.link
			linkPath := filepath.Join(directory, name)
			if strings.Contains(c.name, "intermediate") {
				linkPath = filepath.Dir(linkPath)
			}
			rootNoFollowLink(t, c.target, linkPath)
			if f, err := openRootNoFollow(root, name, c.flag|syscall.O_NOFOLLOW, 0600); err == nil {
				f.Close()
				t.Fatal("symlink followed")
			}
			if _, err := os.Lstat(filepath.Join(directory, "planted.lock")); !os.IsNotExist(err) {
				t.Fatal("created through a dangling link", err)
			}
			if _, err := os.Lstat(filepath.Join(directory, "real-dir", filepath.Base(name))); !os.IsNotExist(err) {
				t.Fatal("created through an intermediate link", err)
			}
			for path, want := range map[string]string{filepath.Join(directory, "real.json"): "in-root-target", outside: "outside-canary"} {
				if b, err := os.ReadFile(path); err != nil || string(b) != want {
					t.Fatal("link target changed", path, err)
				}
			}
		})
	}
}

func TestOpenRootNoFollowOpensOrdinaryEntries(t *testing.T) {
	directory, root := rootNoFollowFixture(t)
	for _, name := range []string{"real.json", "real-dir/file.json"} {
		f, err := openRootNoFollow(root, name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(name, err)
		}
		if f.Name() != filepath.Join(directory, name) {
			t.Fatal("name", f.Name())
		}
		f.Close()
	}
	dot, err := openRootNoFollow(root, ".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	dot.Close()
	created, err := openRootNoFollow(root, "real-dir/new.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0640)
	if err != nil {
		t.Fatal(err)
	}
	info, err := created.Stat()
	created.Close()
	if err != nil || info.Mode().Perm()&^0640 != 0 {
		t.Fatal("created mode", info.Mode(), err)
	}
	if _, err := openRootNoFollow(root, "missing.json", os.O_RDONLY, 0); !os.IsNotExist(err) {
		t.Fatal("missing file must keep ENOENT", err)
	}
	for _, name := range []string{"", "/etc/passwd", "..", "../real.json", "real-dir/../real.json", "real-dir/", "./real.json", "real-dir//file.json"} {
		if f, err := openRootNoFollow(root, name, os.O_RDONLY, 0); err == nil {
			f.Close()
			t.Fatal("non-local name opened", name)
		}
	}
}

// Every auth file the store reads or writes refuses an in-root symlink. The old
// escaping-link tests could not observe this because os.Root already refuses
// links that leave the directory.
func TestAuthStoreRefusesInRootSymlinks(t *testing.T) {
	refreshCalled := func(t *testing.T, a *AuthStore) {
		t.Helper()
		called := false
		if err := a.Refresh(context.Background(), func([]byte) ([]byte, error) { called = true; return []byte("next"), nil }); err == nil || called {
			t.Fatal("refresh followed an in-root symlink", err, called)
		}
	}
	t.Run("state", func(t *testing.T) {
		a := authTestStore(t, Codex)
		if err := os.Rename(filepath.Join(a.directory, "state.json"), filepath.Join(a.directory, "real.json")); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "real.json", filepath.Join(a.directory, "state.json"))
		refreshCalled(t, a)
		if p, err := a.LeaseProfile(context.Background(), authTestBinding()); err == nil {
			p.Close()
			t.Fatal("lease seeded from an in-root symlink")
		}
	})
	t.Run("refresh lock", func(t *testing.T) {
		a := authTestStore(t, Codex)
		authMustWrite(t, filepath.Join(a.directory, "other.lock"), "")
		if err := os.Remove(filepath.Join(a.directory, "refresh.lock")); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "other.lock", filepath.Join(a.directory, "refresh.lock"))
		refreshCalled(t, a)
	})
	t.Run("dangling refresh lock", func(t *testing.T) {
		a := authTestStore(t, Codex)
		if err := os.Remove(filepath.Join(a.directory, "refresh.lock")); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "planted.lock", filepath.Join(a.directory, "refresh.lock"))
		refreshCalled(t, a)
		if _, err := os.Lstat(filepath.Join(a.directory, "planted.lock")); !os.IsNotExist(err) {
			t.Fatal("lock created through a dangling link", err)
		}
	})
	t.Run("replacement fragment", func(t *testing.T) {
		a := authTestStore(t, Claude)
		authMustWrite(t, filepath.Join(a.directory, "real.json"), "private-target")
		rootNoFollowLink(t, "real.json", filepath.Join(a.directory, ".replace-"+strings.Repeat("a", 32)))
		if _, err := a.LeaseProfile(context.Background(), authTestBinding()); !errors.Is(err, ErrAuthRecoveryRequired) {
			t.Fatal("in-root fragment link accepted", err)
		}
	})
	t.Run("journal", func(t *testing.T) {
		a := authTestStore(t, Codex)
		p, err := a.LeaseProfile(context.Background(), authTestBinding())
		if err != nil {
			t.Fatal(err)
		}
		p.Close()
		if err := os.Rename(filepath.Join(a.directory, "lease.json"), filepath.Join(a.directory, "lease-real.json")); err != nil {
			t.Fatal(err)
		}
		rootNoFollowLink(t, "lease-real.json", filepath.Join(a.directory, "lease.json"))
		if p, err := a.RecoverProfile(context.Background(), authTestBinding()); !errors.Is(err, ErrAuthRecoveryRequired) {
			if p != nil {
				p.Close()
			}
			t.Fatal("journal followed an in-root symlink", err)
		}
	})
	for _, kind := range []string{"final", "intermediate"} {
		t.Run("profile "+kind, func(t *testing.T) {
			a := authTestStore(t, Codex)
			p, err := a.LeaseProfile(context.Background(), authTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			switch kind {
			case "final":
				authMustWrite(t, filepath.Join(filepath.Dir(p.AuthFile()), "next.json"), "linked-next")
				if err := os.Remove(p.AuthFile()); err != nil {
					t.Fatal(err)
				}
				rootNoFollowLink(t, "next.json", p.AuthFile())
			case "intermediate":
				alternate := filepath.Join(p.ProfileDirectory(), "alternate")
				if err := os.Mkdir(alternate, 0700); err != nil {
					t.Fatal(err)
				}
				authMustWrite(t, filepath.Join(alternate, filepath.Base(p.AuthFile())), "linked-next")
				if err := os.RemoveAll(filepath.Dir(p.AuthFile())); err != nil {
					t.Fatal(err)
				}
				rootNoFollowLink(t, "alternate", filepath.Dir(p.AuthFile()))
			}
			if err := p.Complete(context.Background(), authStopped(t)); !errors.Is(err, ErrAuthRecoveryRequired) {
				t.Fatal("profile auth followed an in-root symlink", err)
			}
			if b, err := os.ReadFile(filepath.Join(a.directory, "state.json")); err != nil || string(b) != "synthetic-seed" {
				t.Fatal("linked auth committed", string(b), err)
			}
		})
	}
	t.Run("write final", func(t *testing.T) {
		directory, root := rootNoFollowFixture(t)
		rootNoFollowLink(t, "real.json", filepath.Join(directory, "pending.json"))
		if err := writeAuth(root, "pending.json", []byte("replacement")); err == nil {
			t.Fatal("auth write replaced an in-root symlink")
		}
		if target, err := os.Readlink(filepath.Join(directory, "pending.json")); err != nil || target != "real.json" {
			t.Fatal("refused write changed the link", target, err)
		}
		if b, err := os.ReadFile(filepath.Join(directory, "real.json")); err != nil || string(b) != "in-root-target" {
			t.Fatal("refused write changed the target", err)
		}
	})
	t.Run("write intermediate", func(t *testing.T) {
		directory, root := rootNoFollowFixture(t)
		rootNoFollowLink(t, "real-dir", filepath.Join(directory, "profile"))
		if err := writeAuth(root, "profile/file.json", []byte("replacement")); err == nil {
			t.Fatal("auth write followed an intermediate in-root symlink")
		}
		if b, err := os.ReadFile(filepath.Join(directory, "real-dir", "file.json")); err != nil || string(b) != "in-root-target" {
			t.Fatal("refused write changed the target", err)
		}
	})
}

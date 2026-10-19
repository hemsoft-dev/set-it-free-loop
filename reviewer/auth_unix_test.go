//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRefreshSerializedAndNeverRestoresOldSeed(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthStore(Codex, root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewAuthStore(Codex, root)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := a
			if i%2 == 0 {
				store = other
			}
			errs <- store.Refresh(context.Background(), func(old []byte) ([]byte, error) {
				n := 0
				if len(old) > 0 {
					var err error
					n, err = strconv.Atoi(string(old))
					if err != nil {
						return nil, err
					}
				}
				return []byte(strconv.Itoa(n + 1)), nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "codex", "state.json"))
	if err != nil || string(b) != "20" {
		t.Fatalf("lost refresh: %q %v", b, err)
	}
	if err := a.Refresh(context.Background(), func([]byte) ([]byte, error) { return nil, errors.New("refresh failed") }); err == nil {
		t.Fatal("lost refresh failure")
	}
	b, _ = os.ReadFile(filepath.Join(root, "codex", "state.json"))
	if string(b) != "20" {
		t.Fatal("error replaced refreshed state")
	}
	c, err := NewAuthStore(Claude, root)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Refresh(context.Background(), func(old []byte) ([]byte, error) {
		if len(old) > 0 {
			t.Error("cross-provider state")
		}
		return []byte("separate"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRefreshLeaseDeadlineAndSymlinkRefusal(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthStore(Codex, root)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- a.Refresh(context.Background(), func([]byte) ([]byte, error) { close(entered); <-release; return []byte("new"), nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := a.Refresh(ctx, func([]byte) ([]byte, error) { t.Error("concurrent callback entered"); return nil, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(root, "canary")
	os.WriteFile(canary, []byte("secret-canary"), 0600)
	state := filepath.Join(root, "codex", "state.json")
	os.Remove(state)
	os.Symlink(canary, state)
	if err := a.Refresh(context.Background(), func([]byte) ([]byte, error) { t.Error("symlink content read"); return []byte("bad"), nil }); err == nil {
		t.Fatal("accepted auth symlink")
	}
	b, _ := os.ReadFile(canary)
	if string(b) != "secret-canary" {
		t.Fatal("canary overwritten")
	}
}

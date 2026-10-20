//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// AuthStore serializes trusted local state updates and worker profile leases
// across processes. Workers never receive this object. It is not an
// interactive-profile import facility.
type AuthStore struct {
	directory string
	provider  Provider
}

func privateDirectory(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("auth directory must be private and not a symlink")
	}
	return nil
}

func NewAuthStore(provider Provider, root string) (*AuthStore, error) {
	if !provider.Valid() || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("invalid provider auth store")
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, string(provider))
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	parent, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return nil, err
	}
	return &AuthStore{directory: dir, provider: provider}, nil
}

// Refresh reads current state only after the exclusive lock is acquired and
// atomically persists a trusted LOCAL callback's replacement. The callback must
// not contact a provider or launch a worker. Provider-side refresh belongs in a
// leased client profile so a crash cannot permit reuse of its original seed.
// Errors preserve prior state; an unresolved profile always blocks the callback.
func (a *AuthStore) Refresh(ctx context.Context, refresh func([]byte) ([]byte, error)) error {
	if refresh == nil {
		return errors.New("missing trusted refresh callback")
	}
	held, err := a.lock(ctx)
	if err != nil {
		return err
	}
	defer held.close()
	if err := noAuthLease(held.root); err != nil {
		return err
	}
	old, err := readAuth(held.root, "state.json", true)
	if err != nil {
		return err
	}
	next, err := refresh(append([]byte(nil), old...))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(next) == 0 || len(next) > 65536 {
		return errors.New("invalid refreshed state size")
	}
	return writeAuth(held.root, "state.json", next)
}

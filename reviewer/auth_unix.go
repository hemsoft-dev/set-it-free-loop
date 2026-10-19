//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// AuthStore serializes trusted refresh transactions across processes. Workers
// never receive this object. It is not an interactive-profile import facility.
type AuthStore struct{ directory string }

func privateDirectory(p string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
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
	return &AuthStore{dir}, nil
}

func safeAuthFile(p string) error {
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("auth file must be private and regular")
	}
	return nil
}

// Refresh reads current state only after the exclusive lease is acquired and
// atomically persists the callback's replacement. Errors preserve prior state.
func (a *AuthStore) Refresh(ctx context.Context, refresh func([]byte) ([]byte, error)) error {
	if refresh == nil {
		return errors.New("missing trusted refresh callback")
	}
	if err := privateDirectory(a.directory); err != nil {
		return err
	}
	lockPath := filepath.Join(a.directory, "refresh.lock")
	if err := safeAuthFile(lockPath); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	statePath := filepath.Join(a.directory, "state.json")
	if err := safeAuthFile(statePath); err != nil {
		return err
	}
	var old []byte
	f, err := os.Open(statePath)
	if err == nil {
		old, err = io.ReadAll(io.LimitReader(f, 65537))
		f.Close()
		if err != nil {
			return err
		}
		if len(old) > 65536 {
			return errors.New("auth state size limit")
		}
	} else if !os.IsNotExist(err) {
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
	tmp, err := os.CreateTemp(a.directory, "refresh-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(next); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), statePath); err != nil {
		return err
	}
	dir, err := os.Open(a.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

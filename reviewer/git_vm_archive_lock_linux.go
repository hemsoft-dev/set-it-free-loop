//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Inspect purpose before creating even the lock file. A failed attempt to open
// a host archive must not add a filename that prevents its original reopening.
func preflightGitVMArchive(root *os.Root) error {
	raw, err := readAuth(root, "source-vm.json", true)
	if err != nil {
		return errGitExportArchive
	}
	if raw != nil {
		var j gitVMArchiveJournal
		if json.Unmarshal(raw, &j) != nil || !j.valid() {
			return errGitExportArchive
		}
		canonical, err := json.Marshal(j)
		if err != nil || !bytes.Equal(canonical, raw) {
			return errGitExportArchive
		}
	}
	f, err := root.Open(".")
	if err != nil {
		return errGitExportArchive
	}
	defer f.Close()
	entries, err := f.ReadDir(513)
	if err != nil && err != io.EOF || len(entries) > 512 {
		return errGitExportArchive
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "refresh.lock" || raw != nil && (name == "source-vm.json" || name == "materialization") {
			continue
		}
		if strings.HasPrefix(name, ".replace-") && gitQuarantineHex(strings.TrimPrefix(name, ".replace-"), 32) && entry.Type().IsRegular() {
			continue
		}
		return errGitExportArchive
	}
	return nil
}

func gitVMArchiveRootMatches(directory string, root *os.Root, identity os.FileInfo) bool {
	current, err := os.Lstat(directory)
	anchored, anchorErr := root.Stat(".")
	return err == nil && anchorErr == nil && privateDirectory(directory) == nil &&
		current.Mode()&os.ModeSymlink == 0 && os.SameFile(current, identity) && os.SameFile(anchored, identity)
}

// The opened root is captured before this mutation. Renaming any ancestor
// cannot redirect lock creation or stale-temp cleanup into a replacement.
func lockGitVMArchiveRoot(ctx context.Context, root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errGitExportArchive
	}
	good := false
	defer func() {
		if !good {
			f.Close()
		}
	}()
	if privateAuthFile(f) != nil {
		return nil, errGitExportArchive
	}
	for {
		if ctx.Err() != nil {
			return nil, errGitExportArchive
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			good = true
			return f, nil
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			return nil, errGitExportArchive
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errGitExportArchive
		case <-timer.C:
		}
	}
}

func openGitVMRawArchive(ctx context.Context, a *gitVMExportArchive) (*gitExportArchive, error) {
	root, err := a.held.root.OpenRoot("materialization")
	if err != nil {
		return nil, errGitExportArchive
	}
	identity, err := root.Stat(".")
	directory := filepath.Join(a.directory, "materialization")
	if err != nil || a.verifyDirectory() != nil || !gitVMArchiveRootMatches(directory, root, identity) {
		root.Close()
		return nil, errGitExportArchive
	}
	f, err := lockGitVMArchiveRoot(ctx, root, "git-export.lock")
	if err != nil {
		root.Close()
		return nil, errGitExportArchive
	}
	raw := &gitExportArchive{root: root, directory: directory, identity: identity, lock: f, operation: make(chan struct{}, 1)}
	raw.operation <- struct{}{}
	if _, err := raw.records(); err != nil {
		f.Close()
		root.Close()
		return nil, errGitExportArchive
	}
	return raw, nil
}

//go:build linux

package reviewer

import (
	"crypto/rand"
	"encoding/hex"
	"golang.org/x/sys/unix"
	"os"
)

// A canonical pathname can change after an inode check. Atomically retire the
// whole job into an unpredictable private directory first, then inspect that
// isolated name. Concurrent recovery may replace the original name but must
// never enter another live reader's private quarantine. The worker UID cannot
// write either parent. This is not isolation from hostile same-UID processes.
func cleanupSourceEndpoint(root *os.Root, name string, directory, socket os.FileInfo, transition func(string)) error {
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(current, directory) {
		return ErrSourceService
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrSourceService
	}
	holding := ".sfl-reap-" + hex.EncodeToString(nonce[:])
	if root.Mkdir(holding, 0700) != nil {
		return ErrSourceService
	}
	quarantine, err := root.OpenRoot(holding)
	if err != nil {
		return ErrSourceService
	}
	defer quarantine.Close()
	parent, err := root.Open(".")
	if err != nil {
		return ErrSourceService
	}
	defer parent.Close()
	held, err := quarantine.Open(".")
	if err != nil {
		return ErrSourceService
	}
	defer held.Close()
	move := func(from *os.File, old string, to *os.File, next string) error {
		return unix.Renameat2(int(from.Fd()), old, int(to.Fd()), next, unix.RENAME_NOREPLACE)
	}
	if transition != nil {
		transition("before-retire")
	}
	if move(parent, name, held, "job") != nil {
		_ = root.Remove(holding) // private empty directory only
		return ErrSourceService
	}
	if transition != nil {
		transition("after-retire")
	}
	restore := func() error {
		// Never overwrite a newly created canonical job. If recovery already
		// claimed that name, preserve this entry in quarantine for reconciliation.
		if move(held, "job", parent, name) == nil {
			_ = root.Remove(holding)
		}
		return ErrSourceService
	}
	current, err = quarantine.Lstat("job")
	if err != nil || !os.SameFile(current, directory) {
		return restore()
	}
	if socket != nil {
		current, err = quarantine.Lstat("job/source.sock")
		if err != nil || !os.SameFile(current, socket) {
			return restore()
		}
		if quarantine.Remove("job/source.sock") != nil {
			return restore()
		}
	}
	if quarantine.Remove("job") != nil {
		return restore() // unknown contents are preserved, never recursive removal
	}
	if root.Remove(holding) != nil {
		return ErrSourceService
	}
	return nil
}

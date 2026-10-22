//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"golang.org/x/sys/unix"
)

type ownedRootRecord struct {
	Version                            int
	SourceHead, SourceTree, ConfigHash string
	Device, Inode                      uint64
}

func privateJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 8192 {
		return errors.New("bounded fixture record required")
	}
	pending := path + ".pending"
	file, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(raw, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err = unix.Renameat2(unix.AT_FDCWD, pending, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func writeOwnedRoot(c config) error {
	dev, ino, err := rootIdentity(c.Root)
	if err != nil {
		return err
	}
	return privateJSON(filepath.Join(c.Root, "fake-owner.json"), ownedRootRecord{1, sourceHead, sourceTree, r.Digest(c), dev, ino})
}
func verifyOwnedRoot(c config) error {
	dev, ino, err := rootIdentity(c.Root)
	if err != nil {
		return err
	}
	raw, err := readBoundedFixture(filepath.Join(c.Root, "fake-owner.json"), 8192)
	if err != nil {
		return err
	}
	got, err := r.DecodeStrict[ownedRootRecord](raw, 8192)
	if err != nil || got != (ownedRootRecord{1, sourceHead, sourceTree, r.Digest(c), dev, ino}) {
		return errors.New("original fresh fake root required")
	}
	return nil
}

type recoveryRecord struct {
	Version                                                                               int
	Purpose, SourceHead, SourceTree, Status                                               string
	OriginalRootVerified, PhysicalStop, RuntimeRemoved, AuthRecoveryFenced, AuthUnchanged bool
	Unit                                                                                  unitRecord
}

func recoverOwned(c config) (recoveryRecord, error) {
	record := recoveryRecord{Version: 1, Purpose: "native-process-loss-fake-only", SourceHead: sourceHead, SourceTree: sourceTree, Status: "unknown"}
	if _, err := os.Lstat(c.Root); os.IsNotExist(err) {
		return record, errors.New("missing original fixture state remains unknown")
	}
	if err := verifyOwnedRoot(c); err != nil {
		return record, err
	}
	record.OriginalRootVerified = true
	session, err := fixedSession(c)
	if err != nil {
		return record, err
	}
	before, err := readUnit(c.Root)
	if err != nil || !capturedUnitBound(before, r.Digest(session)) {
		return record, errors.New("uncertain original unit retained")
	}
	stopped, err := stopOriginal(c, r.Digest(session))
	if err != nil {
		return record, err
	}
	record.Unit = stopped
	record.PhysicalStop = stopped.Stopped && stopped.Aborted
	roots, err := filepath.Glob(filepath.Join(c.Root, "launches", "*", "qemu-runtime"))
	record.RuntimeRemoved = err == nil && len(roots) == 0
	// No Complete, Retire or new seed is available in this recovery mode. The
	// deliberately failing callback cannot write auth even before export.
	canonical := filepath.Join(c.Root, "auth", string(c.Provider), "state.json")
	raw, err := readBoundedFixture(canonical, 100<<10)
	if err != nil || len(raw) == 0 || len(raw) > 100<<10 {
		clear(raw)
		return record, errors.New("original synthetic state required")
	}
	beforeHash := hash(raw)
	clear(raw)
	store, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return record, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	callback := false
	authErr := store.Refresh(ctx, func([]byte) ([]byte, error) { callback = true; return nil, errors.New("recovery cannot change auth") })
	record.AuthRecoveryFenced = errors.Is(authErr, r.ErrAuthRecoveryRequired) && !callback
	raw, err = readBoundedFixture(canonical, 100<<10)
	record.AuthUnchanged = err == nil && hash(raw) == beforeHash
	clear(raw)
	if c.ForceDriverTimeout {
		rawPoint, openErr := readBoundedFixture(filepath.Join(c.Root, "driver-timeout-point.json"), 8192)
		if openErr != nil {
			return record, openErr
		}
		point, pointErr := readCrashPoint(bytes.NewReader(rawPoint))
		if pointErr != nil || !pointValid(c, point) || point.SeedHash != beforeHash || !record.AuthRecoveryFenced {
			return record, errors.New("lost-return fence must survive driver timeout")
		}
	}
	if !record.PhysicalStop || !record.RuntimeRemoved || !record.AuthUnchanged {
		return record, errors.New("original cleanup incomplete")
	}
	record.Status = "stopped-owned-unit"
	return record, nil
}
func recoveryCommand(c config) error {
	record, err := recoverOwned(c)
	raw, encodeErr := json.Marshal(record)
	if encodeErr != nil {
		return encodeErr
	}
	fmt.Println(string(raw))
	return err
}

func readBoundedFixture(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("bounded regular fixture record required")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, errors.New("fixture record exceeds bound")
	}
	return raw, nil
}

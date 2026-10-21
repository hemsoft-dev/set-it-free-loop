//go:build linux

package reviewer

import (
	"context"
	"os"

	"golang.org/x/sys/unix"
)

// Terminally discard only incomplete plain intake, after the concrete owner
// independently proves whole-unit stop. A terminal receipt is fsynced before
// removing the bounded payload, so interruption never makes a missing archive
// record look like permission to recapture the spent attempt.
func (a *gitExportArchive) discardStoppedIntake(ctx context.Context, unit *gitUnitOwner, binding gitMaterializationBinding) error {
	if ctx == nil || unit == nil || !binding.valid() || unit.stop(ctx, binding) != nil {
		return errGitExportArchive
	}
	if !a.enter(ctx) {
		return errGitExportArchive
	}
	defer a.leave()
	root, err := a.attempt(binding.Attempt)
	if err != nil {
		return errGitExportArchive
	}
	defer root.Close()
	record, err := readGitExportRecord(root)
	if err != nil || record.Binding != binding || record.State != "receiving" && record.State != "discarded" {
		return errGitExportArchive
	}
	var payload *os.File
	if _, err := root.Lstat("payload.bin"); err == nil {
		payload, err = root.OpenFile("payload.bin", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return errGitExportArchive
		}
		defer payload.Close()
		if privateAuthFile(payload) != nil {
			return errGitExportArchive
		}
		var stat unix.Stat_t
		if unix.Fstat(int(payload.Fd()), &stat) != nil || stat.Nlink != 1 || stat.Size < 0 || stat.Size > gitExportWireBytes {
			return errGitExportArchive
		}
	} else if !os.IsNotExist(err) {
		return errGitExportArchive
	}
	if a.verifyAttempt(root, binding.Attempt) != nil || ctx.Err() != nil {
		return errGitExportArchive
	}
	record.State = "discarded"
	if writeGitExportRecord(root, record) != nil {
		return errGitExportArchive
	}
	if payload != nil {
		opened, err := payload.Stat()
		current, pathErr := root.Lstat("payload.bin")
		if err != nil || pathErr != nil || !os.SameFile(opened, current) || root.Remove("payload.bin") != nil || syncAuthDirectory(root) != nil {
			return errGitExportArchive
		}
	}
	if a.verifyAttempt(root, binding.Attempt) != nil || ctx.Err() != nil {
		return errGitExportArchive
	}
	return nil
}

// Reopened owners never re-arm grants or re-fetch missing source. The outer
// factory must call recovery before credential Refresh/Ready and before any
// source exposure. Cross-journal equality binds retained export to its exact
// nonce, release, runtime and immutable snapshot, not merely its PR number.
func recoverGitFactory(ctx context.Context, unit *gitUnitOwner, archive *gitExportArchive) (map[string]gitMaterializationBinding, error) {
	if ctx == nil || unit == nil || archive == nil || !unit.enter(ctx) {
		return nil, errGitUnit
	}
	records, err := unit.records()
	unit.leave()
	if err != nil {
		return nil, errGitUnit
	}
	byAttempt := map[string]gitUnitRecord{}
	for _, record := range records {
		if unit.stop(ctx, record.Binding) != nil {
			return nil, errGitUnit
		}
		byAttempt[record.Binding.Attempt] = record
	}
	if !archive.enter(ctx) {
		return nil, errGitUnit
	}
	exports, err := archive.records()
	archive.leave()
	if err != nil {
		return nil, errGitUnit
	}
	retained := map[string]gitMaterializationBinding{}
	for _, record := range exports {
		unitRecord, found := byAttempt[record.Binding.Attempt]
		if !found || unitRecord.Binding != record.Binding {
			return nil, errGitUnit
		}
		if record.State == "receiving" || record.State == "discarded" {
			if archive.discardStoppedIntake(ctx, unit, record.Binding) != nil {
				return nil, errGitUnit
			}
			continue
		}
		if record.State != "staged" || !unitRecord.Captured || unitRecord.BootstrapGrantHash == "" || unitRecord.WorkerGrantHash == "" {
			return nil, errGitUnit
		}
		retained[record.Binding.Attempt] = record.Binding
	}
	return retained, nil
}

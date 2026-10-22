//go:build linux

package reviewer

import (
	"context"
	"io"
	"os"
)

// verifyRetired admits only residue of a durable retiring record under the
// separate retired directory. Nothing there is readable source.
func (a *gitVMExportArchive) verifyRetired(j gitVMArchiveJournal) error {
	info, err := a.held.root.Lstat("retired")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errGitExportArchive
	}
	retiring := map[string]bool{}
	for _, r := range j.Records {
		if r.State == "retiring" {
			retiring[r.Binding.Attempt] = true
		}
	}
	dir, err := a.held.root.Open("retired")
	if err != nil {
		return errGitExportArchive
	}
	defer dir.Close()
	entries, err := dir.ReadDir(gitVMArchiveRecords + 1)
	if err != nil && err != io.EOF || len(entries) > gitVMArchiveRecords {
		return errGitExportArchive
	}
	for _, entry := range entries {
		if !retiring[entry.Name()] || !entry.IsDir() {
			return errGitExportArchive
		}
	}
	return nil
}

// retire durably marks one stopped attempt retiring, then removes its plain
// bytes and record. The caller must already hold the owner's stop receipt; a
// retiring attempt can never be reopened and interruption resumes, never
// replays. Like host retirement, this ends the attempt's records; the factory
// never reissues an attempt, which is a fresh 128-bit random value.
func (a *gitVMExportArchive) retire(ctx context.Context, binding gitMaterializationBinding) error {
	if !a.enter(ctx) {
		return errGitExportArchive
	}
	defer a.leave()
	if !binding.valid() || a.verify() != nil {
		return errGitExportArchive
	}
	j, err := a.journal()
	if err != nil {
		return errGitExportArchive
	}
	found := false
	for i, r := range j.Records {
		if r.Binding.Attempt != binding.Attempt {
			continue
		}
		if r.Binding != binding {
			return errGitExportArchive
		}
		found = true
		if r.State != "retiring" {
			j.Records[i].State = "retiring"
			if a.writeJournal(j) != nil {
				return errGitExportArchive
			}
		}
	}
	if !found {
		// An already completed retirement is idempotent: with no record, no
		// raw attempt directory and no retired residue, nothing remains.
		for _, name := range []string{"materialization/" + gitExportDirectory(binding.Attempt), "retired/" + binding.Attempt} {
			if _, err := a.held.root.Lstat(name); !os.IsNotExist(err) {
				return errGitExportArchive
			}
		}
		return nil
	}
	return a.finishRetirement(ctx, binding.Attempt)
}

// resumeRetirements completes every interrupted retirement. Startup recovery
// calls it before using the archive.
func (a *gitVMExportArchive) resumeRetirements(ctx context.Context) error {
	if !a.enter(ctx) {
		return errGitExportArchive
	}
	defer a.leave()
	j, err := a.journal()
	if err != nil {
		return errGitExportArchive
	}
	for _, r := range j.Records {
		if r.State == "retiring" && a.finishRetirement(ctx, r.Binding.Attempt) != nil {
			return errGitExportArchive
		}
	}
	return nil
}

// Caller holds the archive token and a durable retiring record. The attempt
// directory leaves the raw namespace in one rename before any removal, so a
// crash never leaves partial staging where the raw archive would read it.
func (a *gitVMExportArchive) finishRetirement(ctx context.Context, attempt string) error {
	if ctx.Err() != nil || !gitQuarantineHex(attempt, 32) || a.verifyDirectory() != nil {
		return errGitExportArchive
	}
	if _, err := a.held.root.Lstat("retired"); os.IsNotExist(err) {
		if a.held.root.Mkdir("retired", 0700) != nil || syncAuthDirectory(a.held.root) != nil {
			return errGitExportArchive
		}
	} else if err != nil {
		return errGitExportArchive
	}
	source := "materialization/" + gitExportDirectory(attempt)
	target := "retired/" + attempt
	if info, err := a.held.root.Lstat(source); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errGitExportArchive
		}
		if _, err := a.held.root.Lstat(target); !os.IsNotExist(err) {
			return errGitExportArchive
		}
		if a.held.root.Rename(source, target) != nil {
			return errGitExportArchive
		}
		raw, err := a.held.root.OpenRoot("materialization")
		if err != nil {
			return errGitExportArchive
		}
		synced := syncAuthDirectory(raw)
		raw.Close()
		if synced != nil {
			return errGitExportArchive
		}
	} else if !os.IsNotExist(err) {
		return errGitExportArchive
	}
	// Root-scoped removal never follows a symlink out of the archive.
	if a.held.root.RemoveAll(target) != nil {
		return errGitExportArchive
	}
	retired, err := a.held.root.OpenRoot("retired")
	if err != nil {
		return errGitExportArchive
	}
	synced := syncAuthDirectory(retired)
	retired.Close()
	if synced != nil {
		return errGitExportArchive
	}
	j, err := a.journal()
	if err != nil {
		return errGitExportArchive
	}
	kept := []gitVMArchiveRecord{}
	for _, r := range j.Records {
		if r.Binding.Attempt == attempt {
			if r.State != "retiring" {
				return errGitExportArchive
			}
			continue
		}
		kept = append(kept, r)
	}
	j.Records = kept
	if a.writeJournal(j) != nil || a.verify() != nil {
		return errGitExportArchive
	}
	return nil
}

// records returns the source journal and the packet hash of each staged attempt.
func (a *gitVMExportArchive) records(ctx context.Context) ([]gitVMArchiveRecord, map[string]string, error) {
	if !a.enter(ctx) {
		return nil, nil, errGitExportArchive
	}
	defer a.leave()
	if a.verify() != nil {
		return nil, nil, errGitExportArchive
	}
	j, err := a.journal()
	if err != nil {
		return nil, nil, errGitExportArchive
	}
	raw, err := a.raw.records()
	if err != nil {
		return nil, nil, errGitExportArchive
	}
	packets := map[string]string{}
	for _, r := range raw {
		if r.State == "staged" {
			packets[r.Binding.Attempt] = r.PacketHash
		}
	}
	return j.Records, packets, nil
}

//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type gitRetirementFile struct {
	Identity gitKernelFileID
	Hash     string
	Bytes    int64
}

// This protected intent is durable before either journal directory moves. An
// atomic rename keeps partial deletion outside the unit owner's record scan.
type gitRetirement struct {
	Version                        int
	Unit                           gitUnitRecord
	Export                         *gitExportRecord `json:",omitempty"`
	UnitDirectory, ExportDirectory gitKernelFileID
	UnitFiles, ExportFiles         map[string]gitRetirementFile
}

func gitRetirementName(attempt string) string        { return "retire-" + attempt + ".json" }
func gitRetirementTrash(kind, attempt string) string { return "retired-" + kind + "-" + attempt }

func (r gitRetirement) valid(attempt string) bool {
	if r.Version != 1 || !r.Unit.valid(attempt, r.Unit.StubHash) || !r.Unit.Stopped || !r.Unit.Revoked || r.UnitDirectory.Inode == 0 || len(r.UnitFiles) != 1 {
		return false
	}
	if _, ok := r.UnitFiles["git-unit.json"]; !ok {
		return false
	}
	unitJSON, err := json.Marshal(r.Unit)
	if err != nil || r.UnitFiles["git-unit.json"].Hash != gitRawHash(unitJSON) || r.UnitFiles["git-unit.json"].Bytes != int64(len(unitJSON)) {
		return false
	}
	if r.Export == nil {
		if r.ExportDirectory != (gitKernelFileID{}) || len(r.ExportFiles) != 0 {
			return false
		}
	} else {
		if !r.Export.valid() || r.Export.Binding != r.Unit.Binding || r.ExportDirectory.Inode == 0 || len(r.ExportFiles) < 1 || len(r.ExportFiles) > 2 {
			return false
		}
		if _, ok := r.ExportFiles["archive.json"]; !ok {
			return false
		}
		exportJSON, err := json.Marshal(r.Export)
		if err != nil || r.ExportFiles["archive.json"].Hash != gitRawHash(exportJSON) || r.ExportFiles["archive.json"].Bytes != int64(len(exportJSON)) {
			return false
		}
		if r.Export.State == "staged" {
			payload, ok := r.ExportFiles["payload.bin"]
			if !ok || payload.Hash != r.Export.WireHash || payload.Bytes != r.Export.Bytes {
				return false
			}
		}
	}
	for kind, files := range map[string]map[string]gitRetirementFile{"unit": r.UnitFiles, "export": r.ExportFiles} {
		for name, file := range files {
			if kind == "unit" && name != "git-unit.json" || kind == "export" && name != "archive.json" && name != "payload.bin" || file.Identity.Inode == 0 || !validAuthHash(file.Hash) || file.Bytes < 0 || file.Bytes > gitExportWireBytes {
				return false
			}
		}
	}
	return true
}

func readGitRetirement(root *os.Root, attempt string) (gitRetirement, error) {
	var r gitRetirement
	raw, err := readAuth(root, gitRetirementName(attempt), false)
	if err != nil || len(raw) > 32768 || json.Unmarshal(raw, &r) != nil {
		return r, errGitUnit
	}
	canonical, err := json.Marshal(r)
	if err != nil || !bytes.Equal(raw, canonical) || !r.valid(attempt) {
		return gitRetirement{}, errGitUnit
	}
	return r, nil
}

func gitRetirementDirectory(root *os.Root, names map[string]bool) (gitKernelFileID, map[string]gitRetirementFile, error) {
	directory, err := root.Open(".")
	if err != nil {
		return gitKernelFileID{}, nil, errGitUnit
	}
	defer directory.Close()
	id, err := gitStatFileID(directory)
	entries, readErr := directory.ReadDir(4)
	if err != nil || readErr != nil && readErr != io.EOF || len(entries) > 2 {
		return id, nil, errGitUnit
	}
	files := map[string]gitRetirementFile{}
	for _, entry := range entries {
		if !names[entry.Name()] || !entry.Type().IsRegular() {
			return id, nil, errGitUnit
		}
		file, err := root.OpenFile(entry.Name(), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return id, nil, errGitUnit
		}
		observed, err := gitRetirementFileIdentity(file)
		file.Close()
		if err != nil {
			return id, nil, errGitUnit
		}
		files[entry.Name()] = observed
	}
	return id, files, nil
}

func gitRetirementFileIdentity(file *os.File) (gitRetirementFile, error) {
	var st unix.Stat_t
	if privateAuthFile(file) != nil || unix.Fstat(int(file.Fd()), &st) != nil || st.Nlink != 1 || st.Size < 0 || st.Size > gitExportWireBytes {
		return gitRetirementFile{}, errGitUnit
	}
	raw, err := io.ReadAll(io.LimitReader(file, gitExportWireBytes+1))
	if err != nil || int64(len(raw)) != st.Size {
		return gitRetirementFile{}, errGitUnit
	}
	return gitRetirementFile{gitKernelFileID{uint64(st.Dev), st.Ino}, gitRawHash(raw), st.Size}, nil
}

// Called under both owners' operation tokens and after the outer factory has
// excluded every pending capture, active consumer and retained durable job.
func prepareGitRetirement(unit *gitUnitOwner, archive *gitExportArchive, binding gitMaterializationBinding) (gitRetirement, error) {
	r, err := unit.read(binding.Attempt)
	if err != nil || r.Binding != binding || !r.Stopped || !r.Revoked || archive.verifyRoot() != nil {
		return gitRetirement{}, errGitUnit
	}
	u, err := unit.root.OpenRoot(gitUnitDirectory(binding.Attempt))
	if err != nil {
		return gitRetirement{}, errGitUnit
	}
	defer u.Close()
	uid, ufiles, err := gitRetirementDirectory(u, map[string]bool{"git-unit.json": true})
	if err != nil {
		return gitRetirement{}, errGitUnit
	}
	intent := gitRetirement{Version: 1, Unit: r, UnitDirectory: uid, UnitFiles: ufiles}
	if _, err := archive.root.Lstat(gitExportDirectory(binding.Attempt)); err == nil {
		a, err := archive.attempt(binding.Attempt)
		if err != nil {
			return intent, errGitUnit
		}
		defer a.Close()
		export, err := readGitExportRecord(a)
		if err != nil || export.Binding != binding {
			return intent, errGitUnit
		}
		intent.Export = &export
		intent.ExportDirectory, intent.ExportFiles, err = gitRetirementDirectory(a, map[string]bool{"archive.json": true, "payload.bin": true})
		if err != nil {
			return intent, errGitUnit
		}
	} else if !os.IsNotExist(err) {
		return intent, errGitUnit
	}
	if !intent.valid(binding.Attempt) {
		return intent, errGitUnit
	}
	return intent, nil
}

func gitRetirementRootFD(root *os.Root) (*os.File, gitKernelFileID, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, gitKernelFileID{}, errGitUnit
	}
	id, err := gitStatFileID(file)
	if err != nil {
		file.Close()
		return nil, id, errGitUnit
	}
	return file, id, nil
}

func gitRetirementSharedFilesystem(unit, archive *os.Root) bool {
	u, uid, err := gitRetirementRootFD(unit)
	if err != nil {
		return false
	}
	defer u.Close()
	a, aid, err := gitRetirementRootFD(archive)
	if err != nil {
		return false
	}
	defer a.Close()
	return uid.Device == aid.Device
}

func gitRetirementPathIdentity(root *os.Root, name string, want gitKernelFileID) (bool, error) {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || privateDirectory(filepath.Join(root.Name(), name)) != nil {
		return false, errGitUnit
	}
	dir, err := root.Open(name)
	if err != nil {
		return false, errGitUnit
	}
	defer dir.Close()
	id, err := gitStatFileID(dir)
	if err != nil || id != want {
		return false, errGitUnit
	}
	return true, nil
}

func moveGitRetirementDirectory(source, target *os.Root, old, trash string, expected gitKernelFileID) error {
	original, err := gitRetirementPathIdentity(source, old, expected)
	if err != nil {
		return err
	}
	moved, err := gitRetirementPathIdentity(target, trash, expected)
	if err != nil || original && moved {
		return errGitUnit
	}
	if !original {
		return nil
	}
	src, sid, err := gitRetirementRootFD(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, did, err := gitRetirementRootFD(target)
	if err != nil {
		return err
	}
	defer dst.Close()
	if sid.Device != did.Device || unix.Renameat2(int(src.Fd()), old, int(dst.Fd()), trash, unix.RENAME_NOREPLACE) != nil || syncAuthDirectory(source) != nil || syncAuthDirectory(target) != nil {
		return errGitUnit
	}
	return nil
}

func removeGitRetirementDirectory(ctx context.Context, archive *gitExportArchive, trash string, id gitKernelFileID, expected map[string]gitRetirementFile) error {
	present, err := gitRetirementPathIdentity(archive.root, trash, id)
	if err != nil || !present {
		return err
	}
	directory, err := archive.root.OpenRoot(trash)
	if err != nil {
		return errGitUnit
	}
	defer directory.Close()
	_, files, err := gitRetirementDirectory(directory, map[string]bool{"git-unit.json": true, "archive.json": true, "payload.bin": true})
	if err != nil {
		return err
	}
	names := []string{}
	for name, file := range files {
		if want, ok := expected[name]; !ok || file != want {
			return errGitUnit
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return errGitUnit
		}
		// Recheck the original object immediately before each unlink. No
		// recursive removal can follow a newly inserted file or symlink.
		file, err := directory.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return errGitUnit
		}
		got, err := gitRetirementFileIdentity(file)
		opened, statErr := file.Stat()
		atPath, pathErr := directory.Lstat(name)
		file.Close()
		if err != nil || statErr != nil || pathErr != nil || got != expected[name] || !os.SameFile(opened, atPath) || directory.Remove(name) != nil || syncAuthDirectory(directory) != nil {
			return errGitUnit
		}
	}
	present, err = gitRetirementPathIdentity(archive.root, trash, id)
	if err != nil || !present || archive.root.Remove(trash) != nil || syncAuthDirectory(archive.root) != nil {
		return errGitUnit
	}
	return nil
}

func checkGitRetirementDirectory(root *os.Root, name string, id gitKernelFileID, expected map[string]gitRetirementFile, partial bool) error {
	present, err := gitRetirementPathIdentity(root, name, id)
	if err != nil || !present {
		return err
	}
	directory, err := root.OpenRoot(name)
	if err != nil {
		return errGitUnit
	}
	defer directory.Close()
	_, files, err := gitRetirementDirectory(directory, map[string]bool{"git-unit.json": true, "archive.json": true, "payload.bin": true})
	if err != nil || !partial && len(files) != len(expected) {
		return errGitUnit
	}
	for name, file := range files {
		if want, ok := expected[name]; !ok || want != file {
			return errGitUnit
		}
	}
	return nil
}

// Startup can resume this intent before ordinary cross-journal recovery. Unit
// and archive roots must share a filesystem for the unit-directory rename.
func finishGitRetirement(ctx context.Context, unit *gitUnitOwner, archive *gitExportArchive, intent gitRetirement) error {
	attempt := intent.Unit.Binding.Attempt
	hash, err := unit.recordImageHash(intent.Unit.StubHash)
	if ctx.Err() != nil || err != nil || !intent.valid(attempt) || intent.Unit.StubHash != hash || archive.verifyRoot() != nil {
		return errGitUnit
	}
	// Verify both journals before deleting either. A foreign replacement in
	// one directory must not make the other's retained payload disposable.
	if checkGitRetirementDirectory(unit.root, gitUnitDirectory(attempt), intent.UnitDirectory, intent.UnitFiles, false) != nil || checkGitRetirementDirectory(archive.root, gitRetirementTrash("unit", attempt), intent.UnitDirectory, intent.UnitFiles, true) != nil {
		return errGitUnit
	}
	if intent.Export != nil {
		if checkGitRetirementDirectory(archive.root, gitExportDirectory(attempt), intent.ExportDirectory, intent.ExportFiles, false) != nil || checkGitRetirementDirectory(archive.root, gitRetirementTrash("export", attempt), intent.ExportDirectory, intent.ExportFiles, true) != nil {
			return errGitUnit
		}
	}
	if moveGitRetirementDirectory(unit.root, archive.root, gitUnitDirectory(attempt), gitRetirementTrash("unit", attempt), intent.UnitDirectory) != nil {
		return errGitUnit
	}
	if intent.Export != nil {
		if moveGitRetirementDirectory(archive.root, archive.root, gitExportDirectory(attempt), gitRetirementTrash("export", attempt), intent.ExportDirectory) != nil {
			return errGitUnit
		}
		if removeGitRetirementDirectory(ctx, archive, gitRetirementTrash("export", attempt), intent.ExportDirectory, intent.ExportFiles) != nil {
			return errGitUnit
		}
	}
	if removeGitRetirementDirectory(ctx, archive, gitRetirementTrash("unit", attempt), intent.UnitDirectory, intent.UnitFiles) != nil {
		return errGitUnit
	}
	if ctx.Err() != nil || archive.root.Remove(gitRetirementName(attempt)) != nil || syncAuthDirectory(archive.root) != nil {
		return errGitUnit
	}
	if fd, ok := unit.rootPIDFDs[attempt]; ok {
		unix.Close(fd)
		delete(unit.rootPIDFDs, attempt)
	}
	delete(unit.grantEligible, attempt)
	delete(unit.grantFailed, attempt)
	return nil
}

func gitRetirementIntents(archive *gitExportArchive) ([]gitRetirement, error) {
	directory, err := archive.root.Open(".")
	if err != nil {
		return nil, errGitUnit
	}
	defer directory.Close()
	entries, err := directory.ReadDir(gitExportArchiveRecords*4 + 2)
	if err != nil && err != io.EOF || len(entries) > gitExportArchiveRecords*4+1 {
		return nil, errGitUnit
	}
	intents := []gitRetirement{}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "retire-") {
			continue
		}
		attempt := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "retire-"), ".json")
		if entry.Name() != gitRetirementName(attempt) || !gitQuarantineHex(attempt, 32) {
			return nil, errGitUnit
		}
		intent, err := readGitRetirement(archive.root, attempt)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	if len(intents) > gitExportArchiveRecords {
		return nil, errGitUnit
	}
	return intents, nil
}

func resumeGitRetirements(ctx context.Context, unit *gitUnitOwner, archive *gitExportArchive) error {
	if !unit.enter(ctx) {
		return errGitUnit
	}
	defer unit.leave()
	if unit.closed || !archive.enter(ctx) {
		return errGitUnit
	}
	defer archive.leave()
	intents, err := gitRetirementIntents(archive)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if finishGitRetirement(ctx, unit, archive, intent) != nil {
			return errGitUnit
		}
	}
	return nil
}

func (f *gitSourceFactory) PruneSource(ctx context.Context, store *Store, now time.Time) error {
	if !f.enter(ctx) {
		return errGitUnit
	}
	defer f.leave()
	if f.closed || !f.ready || store == nil || f.checkRoot() != nil {
		return errGitUnit
	}
	state, err := store.Snapshot(ctx)
	if err != nil || state.Enrollment != f.credentials.state.options.Journal.enrollment {
		return errGitUnit
	}
	if store.Prune(ctx, now) != nil {
		return errGitUnit
	}
	state, err = store.Snapshot(ctx)
	if err != nil {
		return errGitUnit
	}
	keep := map[string]bool{}
	used := map[string]bool{}
	for _, attempt := range f.pendingCaptures {
		keep[attempt] = true
	}
	for _, attempt := range f.activeJobs {
		keep[attempt] = true
	}
	for attempt := range keep {
		used[attempt] = true
	}
	for _, job := range state.Jobs {
		for attempt, binding := range f.bindings {
			if job.Job.Snapshot == binding.Snapshot && Digest(job.Job.Packet) == f.packets[attempt] {
				keep[attempt] = true
				if job.LeaseToken != "" || job.Outstanding {
					used[attempt] = true
				}
			}
		}
	}
	for attempt, materialized := range f.materialized {
		if !used[attempt] {
			if materialized.Close() != nil {
				return errGitUnit
			}
			delete(f.materialized, attempt)
		}
	}
	if !f.unit.enter(ctx) {
		return errGitUnit
	}
	defer f.unit.leave()
	if f.unit.closed || !f.archive.enter(ctx) {
		return errGitUnit
	}
	defer f.archive.leave()
	records, err := f.unit.records()
	if err != nil {
		return errGitUnit
	}
	for _, record := range records {
		attempt := record.Binding.Attempt
		if keep[attempt] || !record.Stopped || !record.Revoked {
			continue
		}
		if run := f.runs[attempt]; run != nil {
			run.mu.Lock()
			stopped, closed, generation := run.stopped, run.closed, run.generation
			run.mu.Unlock()
			if !stopped || !closed {
				continue
			}
			if generation != nil {
				generation.owner.mu.Lock()
				released := generation.released
				generation.owner.mu.Unlock()
				if !released {
					continue
				}
			}
		}
		intent, err := prepareGitRetirement(f.unit, f.archive, record.Binding)
		if err != nil {
			return errGitUnit
		}
		// Refuse different filesystems before writing an intent or closing cache.
		root, id, err := gitRetirementRootFD(f.archive.root)
		if err != nil {
			return errGitUnit
		}
		root.Close()
		if id.Device != intent.UnitDirectory.Device {
			return errGitUnit
		}
		if _, err := f.archive.root.Lstat(gitRetirementName(attempt)); !os.IsNotExist(err) {
			f.ready = false
			return errGitUnit
		}
		raw, _ := json.Marshal(intent)
		if writeAuth(f.archive.root, gitRetirementName(attempt), raw) != nil {
			return errGitUnit
		}
		f.ready = false
		if materialized := f.materialized[attempt]; materialized != nil {
			if materialized.Close() != nil {
				return errGitUnit
			}
		}
		if finishGitRetirement(ctx, f.unit, f.archive, intent) != nil {
			return errGitUnit
		}
		delete(f.materialized, attempt)
		delete(f.runs, attempt)
		delete(f.bindings, attempt)
		delete(f.packets, attempt)
		f.ready = true
	}
	return nil
}

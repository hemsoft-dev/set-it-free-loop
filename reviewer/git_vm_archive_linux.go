//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

const gitVMArchiveRecords = 64
const gitVMArchivePurpose = "source-vm-export-archive"

type gitVMArchiveRecord struct {
	RequestHash string
	Binding     gitMaterializationBinding
	State       string
}

type gitVMArchiveJournal struct {
	Version int
	Purpose string
	Records []gitVMArchiveRecord
}

func (j gitVMArchiveJournal) valid() bool {
	if j.Version != 1 || j.Purpose != gitVMArchivePurpose || j.Records == nil || len(j.Records) > gitVMArchiveRecords {
		return false
	}
	seen := map[string]bool{}
	for _, r := range j.Records {
		if !validAuthHash(r.RequestHash) || !r.Binding.valid() || seen[r.Binding.Attempt] || (r.State != "receiving" && r.State != "staged" && r.State != "retiring") {
			return false
		}
		seen[r.Binding.Attempt] = true
	}
	return true
}

// This is a source-specific request journal around the existing plain export
// archive. Only framing/blob storage is reused, never host-unit or provider
// lifecycle records. Every return is private staging, including after recovery.
// No method supplies stop proof, a source capability or auth-lease authority.
type gitVMExportArchive struct {
	directory string
	identity  os.FileInfo
	held      *authLock // generic private-directory flock, no provider/auth state
	raw       *gitExportArchive
	operation chan struct{}
	closed    bool
}

func openGitVMExportArchive(ctx context.Context, directory string) (*gitVMExportArchive, error) {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || privateDirectory(directory) != nil {
		return nil, errGitExportArchive
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitExportArchive
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, errGitExportArchive
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errGitExportArchive
	}
	if !gitVMArchiveRootMatches(directory, root, info) || preflightGitVMArchive(root) != nil {
		root.Close()
		return nil, errGitExportArchive
	}
	file, err := lockGitVMArchiveRoot(ctx, root, "refresh.lock")
	if err != nil {
		root.Close()
		return nil, errGitExportArchive
	}
	held := &authLock{root, file}
	a := &gitVMExportArchive{directory: directory, identity: info, held: held, operation: make(chan struct{}, 1)}
	a.operation <- struct{}{}
	good := false
	defer func() {
		if !good {
			if a.raw != nil {
				_ = a.raw.Close(context.Background())
			}
			_ = held.close()
		}
	}()
	if a.verifyDirectory() != nil || clearAuthTemps(root) != nil || a.verifyDirectory() != nil {
		return nil, errGitExportArchive
	}
	data, err := readAuth(held.root, "source-vm.json", true)
	if err != nil {
		return nil, errGitExportArchive
	}
	if data == nil {
		// Never adopt a host archive or another journal's existing payloads.
		entries, err := a.entries()
		if err != nil || len(entries) != 1 || entries[0].Name() != "refresh.lock" {
			return nil, errGitExportArchive
		}
		if a.writeJournal(gitVMArchiveJournal{1, gitVMArchivePurpose, []gitVMArchiveRecord{}}) != nil {
			return nil, errGitExportArchive
		}
	}
	j, err := a.journal()
	if err != nil {
		return nil, errGitExportArchive
	}
	if _, err := held.root.Lstat("materialization"); os.IsNotExist(err) && len(j.Records) == 0 {
		if held.root.Mkdir("materialization", 0700) != nil || syncAuthDirectory(held.root) != nil {
			return nil, errGitExportArchive
		}
	} else if err != nil {
		return nil, errGitExportArchive
	}
	a.raw, err = openGitVMRawArchive(ctx, a)
	if err != nil || a.verify() != nil {
		return nil, errGitExportArchive
	}
	good = true
	return a, nil
}

func (a *gitVMExportArchive) entries() ([]os.DirEntry, error) {
	f, err := a.held.root.Open(".")
	if err != nil {
		return nil, errGitExportArchive
	}
	defer f.Close()
	entries, err := f.ReadDir(5)
	if err != nil && err != io.EOF || len(entries) > 4 {
		return nil, errGitExportArchive
	}
	return entries, nil
}

func (a *gitVMExportArchive) verifyDirectory() error {
	if a.closed || a.held == nil || privateDirectory(a.directory) != nil {
		return errGitExportArchive
	}
	current, err := os.Lstat(a.directory)
	anchored, anchorErr := a.held.root.Stat(".")
	lock, lockErr := a.held.file.Stat()
	atPath, pathErr := a.held.root.Lstat("refresh.lock")
	if err != nil || anchorErr != nil || lockErr != nil || pathErr != nil || current.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(current, a.identity) || !os.SameFile(anchored, a.identity) || !os.SameFile(lock, atPath) || privateAuthFile(a.held.file) != nil {
		return errGitExportArchive
	}
	return nil
}

func (a *gitVMExportArchive) journal() (gitVMArchiveJournal, error) {
	var j gitVMArchiveJournal
	if a.verifyDirectory() != nil {
		return j, errGitExportArchive
	}
	raw, err := readAuth(a.held.root, "source-vm.json", false)
	if err != nil || json.Unmarshal(raw, &j) != nil || !j.valid() {
		return j, errGitExportArchive
	}
	canonical, err := json.Marshal(j)
	if err != nil || !bytes.Equal(raw, canonical) {
		return j, errGitExportArchive
	}
	return j, nil
}

func (a *gitVMExportArchive) writeJournal(j gitVMArchiveJournal) error {
	if !j.valid() || a.verifyDirectory() != nil {
		return errGitExportArchive
	}
	raw, err := json.Marshal(j)
	if err != nil || len(raw) > 65536 || writeAuth(a.held.root, "source-vm.json", raw) != nil || a.verifyDirectory() != nil {
		return errGitExportArchive
	}
	return nil
}

func (a *gitVMExportArchive) verify() error {
	j, err := a.journal()
	if err != nil || a.raw == nil {
		return errGitExportArchive
	}
	entries, err := a.entries()
	if err != nil || len(entries) < 3 {
		return errGitExportArchive
	}
	for _, e := range entries {
		switch e.Name() {
		case "refresh.lock", "source-vm.json":
			if !e.Type().IsRegular() {
				return errGitExportArchive
			}
		case "materialization", "retired":
			if !e.IsDir() {
				return errGitExportArchive
			}
		default:
			return errGitExportArchive
		}
	}
	if a.verifyRetired(j) != nil {
		return errGitExportArchive
	}
	records, err := a.raw.records()
	if err != nil {
		return errGitExportArchive
	}
	known := map[string]gitVMArchiveRecord{}
	for _, r := range j.Records {
		known[r.Binding.Attempt] = r
	}
	// Plain payload storage is shared code, but host retirement/unit records
	// are not part of this source-specific container.
	dir, err := a.raw.root.Open(".")
	if err != nil {
		return errGitExportArchive
	}
	inner, readErr := dir.ReadDir(gitVMArchiveRecords + 2)
	dir.Close()
	if readErr != nil && readErr != io.EOF || len(inner) > gitVMArchiveRecords+1 {
		return errGitExportArchive
	}
	for _, entry := range inner {
		if entry.Name() == "git-export.lock" && entry.Type().IsRegular() {
			continue
		}
		attempt := ""
		for key := range known {
			if entry.Name() == gitExportDirectory(key) {
				attempt = key
				break
			}
		}
		if attempt == "" || !entry.IsDir() {
			return errGitExportArchive
		}
	}
	staged := map[string]bool{}
	for _, r := range records {
		outer, ok := known[r.Binding.Attempt]
		if !ok || outer.Binding != r.Binding || (r.State != "receiving" && r.State != "staged") || outer.State == "staged" && r.State != "staged" {
			return errGitExportArchive
		}
		staged[r.Binding.Attempt] = r.State == "staged"
	}
	for _, r := range j.Records {
		if r.State == "staged" && !staged[r.Binding.Attempt] {
			return errGitExportArchive
		}
	}
	return nil
}

func (a *gitVMExportArchive) enter(ctx context.Context) bool {
	if a == nil || ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-a.operation:
		if ctx.Err() != nil {
			a.operation <- struct{}{}
			return false
		}
		return true
	}
}
func (a *gitVMExportArchive) leave() { a.operation <- struct{}{} }

// Persist request intent before even reading the source header. A failed or
// partial attempt remains receiving and blocks new staging. The caller must
// retain acquisition ownership and obtain independent stop/recovery evidence.
func (a *gitVMExportArchive) stage(ctx context.Context, q gitVMRequest, input io.ReadCloser) (*gitMaterialization, error) {
	if input == nil {
		return nil, errGitExportArchive
	}
	defer input.Close()
	if !a.enter(ctx) {
		return nil, errGitExportArchive
	}
	defer a.leave()
	if !q.valid() || a.verify() != nil {
		return nil, errGitExportArchive
	}
	j, err := a.journal()
	if err != nil || len(j.Records) >= gitVMArchiveRecords {
		return nil, errGitExportArchive
	}
	for _, r := range j.Records {
		if r.State == "receiving" || r.Binding.Attempt == q.Binding.Attempt {
			return nil, errGitExportArchive
		}
	}
	j.Records = append(j.Records, gitVMArchiveRecord{Digest(q), q.Binding, "receiving"})
	if a.writeJournal(j) != nil {
		return nil, errGitExportArchive
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); input.Close() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	header, err := readVMRecord[gitVMExportHeader](input, gitVMRecordBytes)
	if err != nil || !header.valid(q) || ctx.Err() != nil {
		return nil, errGitExportArchive
	}
	m, err := a.raw.stage(ctx, q.Binding, input)
	if err != nil {
		return nil, errGitExportArchive
	}
	j.Records[len(j.Records)-1].State = "staged"
	if a.writeJournal(j) != nil || a.verify() != nil || ctx.Err() != nil {
		m.Close()
		return nil, errGitExportArchive
	}
	return m, nil
}

// Reopen validates retained plain bytes without a fetch or replay. If the raw
// archive committed before its outer acknowledgement, this repairs only that
// staging acknowledgement. It never promotes source or changes a stop journal.
func (a *gitVMExportArchive) reopen(ctx context.Context, q gitVMRequest) (*gitMaterialization, error) {
	if !a.enter(ctx) {
		return nil, errGitExportArchive
	}
	defer a.leave()
	if !q.valid() || a.verify() != nil {
		return nil, errGitExportArchive
	}
	j, err := a.journal()
	if err != nil {
		return nil, errGitExportArchive
	}
	for i, r := range j.Records {
		if r.Binding != q.Binding || r.RequestHash != Digest(q) {
			continue
		}
		if r.State == "retiring" {
			return nil, errGitExportArchive
		}
		m, err := a.raw.reopen(ctx, q.Binding)
		if err != nil {
			return nil, errGitExportArchive
		}
		j.Records[i].State = "staged"
		if a.writeJournal(j) != nil || a.verify() != nil || ctx.Err() != nil {
			m.Close()
			return nil, errGitExportArchive
		}
		return m, nil
	}
	return nil, errGitExportArchive
}

func (a *gitVMExportArchive) Close(ctx context.Context) error {
	if !a.enter(ctx) {
		return errGitExportArchive
	}
	defer a.leave()
	if a.closed {
		return nil
	}
	if a.raw.Close(ctx) != nil {
		return errGitExportArchive
	}
	err := a.held.close()
	a.closed = true
	return err
}

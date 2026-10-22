//go:build linux

package reviewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// gitVMSourceFactory is the PollSource for source VM acquisition. Like the
// host factory it is internal service composition: only Capture/Restore
// callbacks reach the polling controller, never a model. Retained source is
// used only after the owner's durable original-instance stop receipt.
type gitVMSourceFactory struct {
	operation       chan struct{}
	credentials     *GitHubCredentialOwner
	owner           *gitVMUnitOwner
	archive         *gitVMExportArchive
	assets          *gitVMAssets
	manifest        gitVMManifest
	releaseHash     string
	directory       string
	root            *os.Root
	identity        os.FileInfo
	runs            map[string]*gitVMRun
	requests        map[string]gitVMRequest
	packets         map[string]string
	materialized    map[string]*gitMaterialization
	pendingCaptures map[*SnapshotEvidence]string
	activeJobs      map[*JobEvidence]string
	closed, ready   bool
}

// newGitVMSourceFactory performs startup recovery before any capture. Once a
// nonnil factory returns, including with an error, it owns owner, archive and
// assets; retain it for Recover or Close. Credentials remain caller-owned.
// Nothing here refreshes credentials, installs an App or dispatches HTTP.
func newGitVMSourceFactory(ctx context.Context, credentials *GitHubCredentialOwner, owner *gitVMUnitOwner, archive *gitVMExportArchive, assets *gitVMAssets, m gitVMManifest, releaseHash, directory string) (*gitVMSourceFactory, error) {
	if ctx == nil || ctx.Err() != nil || credentials == nil || credentials.state == nil || owner == nil || archive == nil || assets == nil || !m.valid() || !validAuthHash(releaseHash) || !sourcePath(directory) || !gitVMRunPath(filepath.Join(directory, "run-"+strings.Repeat("a", 32))) || privateDirectory(directory) != nil {
		return nil, errGitVMOwner
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory || Digest(assets.manifest) != Digest(m) {
		return nil, errGitVMOwner
	}
	credentials.state.mu.Lock()
	busy := len(credentials.state.acquisitions) != 0
	credentials.state.mu.Unlock()
	if busy {
		return nil, errGitVMOwner
	}
	// Recovery is startup-only: an owner that already started or granted work
	// in this process is not interchangeable with a freshly locked owner.
	if !owner.enter(ctx) {
		return nil, errGitVMOwner
	}
	active := len(owner.rootPIDFDs) != 0 || len(owner.grantEligible) != 0
	owner.leave()
	if active {
		return nil, errGitVMOwner
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errGitVMOwner
	}
	identity, err := root.Stat(".")
	ownerIdentity, ownerErr := owner.root.Stat(".")
	archiveIdentity, archiveErr := archive.held.root.Stat(".")
	if err != nil || ownerErr != nil || archiveErr != nil || os.SameFile(identity, ownerIdentity) || os.SameFile(identity, archiveIdentity) || !gitVMDisjointRoots(directory, owner.root.Name(), archive.directory) {
		root.Close()
		return nil, errGitVMOwner
	}
	f := &gitVMSourceFactory{operation: make(chan struct{}, 1), credentials: credentials, owner: owner, archive: archive, assets: assets, manifest: m, releaseHash: releaseHash, directory: directory, root: root, identity: identity, runs: map[string]*gitVMRun{}, requests: map[string]gitVMRequest{}, packets: map[string]string{}, materialized: map[string]*gitMaterialization{}}
	f.operation <- struct{}{}
	if f.Recover(ctx) != nil {
		return f, errGitVMOwner
	}
	return f, nil
}

// gitVMDisjointRoots rejects any ancestor or descendant relationship between
// canonical roots: run-residue cleanup must never reach a live journal.
func gitVMDisjointRoots(paths ...string) bool {
	for i, a := range paths {
		if !sourcePath(a) {
			return false
		}
		for _, b := range paths[i+1:] {
			if a == b || strings.HasPrefix(a+"/", b+"/") || strings.HasPrefix(b+"/", a+"/") {
				return false
			}
		}
	}
	return true
}

func (f *gitVMSourceFactory) enter(ctx context.Context) bool {
	if f == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-f.operation:
		if ctx.Err() != nil {
			f.leave()
			return false
		}
		return true
	}
}
func (f *gitVMSourceFactory) leave() { f.operation <- struct{}{} }

func (f *gitVMSourceFactory) checkRoot() error {
	anchored, err := f.root.Stat(".")
	path, pathErr := os.Lstat(f.directory)
	if err != nil || pathErr != nil || !path.IsDir() || path.Mode().Perm() != 0700 || !os.SameFile(f.identity, anchored) || !os.SameFile(f.identity, path) {
		return errGitVMOwner
	}
	return nil
}

// stopRun releases a local run through its generation when one was retained,
// so the credential owner sees the same proved stop as the run itself.
func stopGitVMRun(ctx context.Context, run *gitVMRun) error {
	run.mu.Lock()
	generation := run.generation
	run.mu.Unlock()
	if generation != nil {
		if generation.stop(ctx) != nil {
			return errGitVMOwner
		}
	} else if run.stop(ctx) != nil {
		return errGitVMOwner
	}
	return run.close()
}

// Recover is a trusted explicit recovery action. It never refreshes
// credentials or restarts an acquisition. Every unstopped attempt is proved
// gone first; staged source survives only with a stop receipt and a grant.
func (f *gitVMSourceFactory) Recover(ctx context.Context) error {
	if !f.enter(ctx) {
		return errGitVMOwner
	}
	defer f.leave()
	if f.closed {
		return errGitVMOwner
	}
	if f.ready {
		return nil
	}
	if f.checkRoot() != nil {
		return errGitVMOwner
	}
	for attempt, run := range f.runs {
		if stopGitVMRun(ctx, run) != nil {
			return errGitVMOwner
		}
		delete(f.runs, attempt)
	}
	f.credentials.state.mu.Lock()
	busy := len(f.credentials.state.acquisitions) != 0
	f.credentials.state.mu.Unlock()
	if busy || f.owner.recover(ctx) != nil {
		return errGitVMOwner
	}
	owned, err := f.owner.journal.read()
	if err != nil {
		return errGitVMOwner
	}
	units := map[string]gitVMUnitRecord{}
	for _, r := range owned.Records {
		if !r.Stopped {
			return errGitVMOwner
		}
		units[r.Request.Source.Binding.Attempt] = r
	}
	// Validate every export's ownership, including interrupted retirements,
	// before any retirement runs, so a cross-journal inconsistency refuses
	// without changing the archive.
	exports, _, err := f.archive.records(ctx)
	if err != nil {
		return errGitVMOwner
	}
	for _, export := range exports {
		unit, ok := units[export.Binding.Attempt]
		if !ok || unit.Request.Source.Binding != export.Binding || Digest(unit.Request.Source) != export.RequestHash {
			return errGitVMOwner
		}
	}
	if f.archive.resumeRetirements(ctx) != nil {
		return errGitVMOwner
	}
	exports, packets, err := f.archive.records(ctx)
	if err != nil {
		return errGitVMOwner
	}
	requests := map[string]gitVMRequest{}
	retained := map[string]bool{}
	for _, export := range exports {
		attempt := export.Binding.Attempt
		unit := units[attempt]
		if export.State == "staged" && validAuthHash(unit.GrantHash) && validAuthHash(packets[attempt]) {
			requests[attempt] = unit.Request.Source
			retained[attempt] = true
			continue
		}
		// Incomplete intake after a proved stop is discarded, never resumed.
		if f.archive.retire(ctx, export.Binding) != nil {
			return errGitVMOwner
		}
	}
	for attempt := range units {
		if !retained[attempt] && f.owner.forget(ctx, attempt) != nil {
			return errGitVMOwner
		}
	}
	if f.removeRunResidue(retained) != nil {
		return errGitVMOwner
	}
	for _, attempt := range f.pendingCaptures {
		if !retained[attempt] {
			return errGitVMOwner
		}
	}
	for _, attempt := range f.activeJobs {
		if !retained[attempt] {
			return errGitVMOwner
		}
	}
	for attempt, m := range f.materialized {
		if !retained[attempt] {
			if m.Close() != nil {
				return errGitVMOwner
			}
			delete(f.materialized, attempt)
		}
	}
	retainedPackets := map[string]string{}
	for attempt := range retained {
		retainedPackets[attempt] = packets[attempt]
	}
	f.requests, f.packets, f.ready = requests, retainedPackets, true
	return nil
}

// removeRunResidue deletes stopped attempts' run directories. A crashed run
// may leave its read-only QEMU runtime root; write permission is restored only
// inside the anchored factory root, never through a symlink.
func (f *gitVMSourceFactory) removeRunResidue(keep map[string]bool) error {
	dir, err := f.root.Open(".")
	if err != nil {
		return errGitVMOwner
	}
	entries, err := dir.ReadDir(gitVMOwnerRecords*2 + 1)
	dir.Close()
	if err != nil && err != io.EOF || len(entries) > gitVMOwnerRecords*2 {
		return errGitVMOwner
	}
	for _, entry := range entries {
		attempt := strings.TrimPrefix(entry.Name(), "run-")
		if !strings.HasPrefix(entry.Name(), "run-") || !gitQuarantineHex(attempt, 32) || !entry.IsDir() {
			return errGitVMOwner
		}
		if f.runs[attempt] != nil {
			continue
		}
		if gitVMOpenTree(f.root, entry.Name(), 0) != nil || f.root.RemoveAll(entry.Name()) != nil {
			return errGitVMOwner
		}
	}
	return syncAuthDirectory(f.root)
}

// gitVMOpenTree restores owner write permission on directories below one
// anchored root so the whole tree can be removed. Depth is bounded.
func gitVMOpenTree(root *os.Root, name string, depth int) error {
	if depth > 16 {
		return errGitVMOwner
	}
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		return nil
	}
	if root.Chmod(name, 0700) != nil {
		return errGitVMOwner
	}
	dir, err := root.Open(name)
	if err != nil {
		return errGitVMOwner
	}
	entries, err := dir.ReadDir(4097)
	dir.Close()
	if err != nil && err != io.EOF || len(entries) > 4096 {
		return errGitVMOwner
	}
	for _, entry := range entries {
		if entry.IsDir() && gitVMOpenTree(root, filepath.Join(name, entry.Name()), depth+1) != nil {
			return errGitVMOwner
		}
	}
	return nil
}

func (f *gitVMSourceFactory) Capture(ctx context.Context, snapshot Snapshot) (*SnapshotEvidence, error) {
	if !f.enter(ctx) {
		return nil, errGitVMOwner
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil || snapshot.Validate() != nil || snapshot.RepositoryID != f.credentials.state.options.Journal.enrollment.RepositoryID || f.credentials.Ready(ctx) != nil {
		return nil, errGitVMOwner
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errGitVMOwner
	}
	attempt, channel := hex.EncodeToString(nonce[:16]), hex.EncodeToString(nonce[16:])
	binding := gitMaterializationBinding{attempt, f.releaseHash, Digest(f.manifest), snapshot}
	source := gitVMRequest{1, gitVMSourcePurpose, binding, f.manifest}
	name := "run-" + attempt
	if !source.valid() || f.root.Mkdir(name, 0700) != nil || syncAuthDirectory(f.root) != nil {
		return nil, errGitVMOwner
	}
	run, err := newGitVMRun(ctx, f.owner, f.archive, f.assets, source, channel, filepath.Join(f.directory, name))
	if err != nil {
		f.root.Remove(name)
		return nil, errGitVMOwner
	}
	f.runs[attempt] = run
	m, err := runGitFactoryAcquisition(ctx, f.credentials, binding, run)
	if err != nil {
		phase, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if f.retireFailedCapture(phase, run, binding) != nil {
			// Unknown stop keeps the run, its generation and every record owned.
			f.ready = false
		}
		cancel()
		return nil, errGitVMOwner
	}
	delete(f.runs, attempt)
	if f.root.Remove(name) != nil {
		m.Close()
		f.ready = false
		return nil, errGitVMOwner
	}
	evidence, err := captureGitMaterializationEvidence(ctx, m)
	if err != nil {
		m.Close()
		f.abandonCaptured(ctx, binding)
		return nil, errGitVMOwner
	}
	f.requests[attempt] = source
	f.packets[attempt] = Digest(m.packet)
	f.materialized[attempt] = m
	if f.pendingCaptures == nil {
		f.pendingCaptures = map[*SnapshotEvidence]string{}
	}
	f.pendingCaptures[evidence] = attempt
	return evidence, nil
}

// abandonCaptured retires a stopped, granted, staged attempt whose evidence
// could not be built. It uses an uncancelled bound; if that fails, it fences
// the factory so recovery reconciles both journals. Caller holds the token.
func (f *gitVMSourceFactory) abandonCaptured(ctx context.Context, binding gitMaterializationBinding) {
	phase, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if f.archive.retire(phase, binding) != nil || f.owner.forget(phase, binding.Attempt) != nil {
		f.ready = false
	}
}

// retireFailedCapture runs only after a failed acquisition. It requires the
// run's proved stop, then discards any intake and the attempt's records.
func (f *gitVMSourceFactory) retireFailedCapture(ctx context.Context, run *gitVMRun, binding gitMaterializationBinding) error {
	run.mu.Lock()
	stopped, closed := run.stopped, run.closed
	run.mu.Unlock()
	if !stopped || !closed {
		return errGitVMOwner
	}
	r, err := f.owner.read(binding.Attempt)
	prepared := err == nil
	if prepared && !r.Stopped {
		return errGitVMOwner
	}
	exports, _, err := f.archive.records(ctx)
	if err != nil {
		return errGitVMOwner
	}
	for _, export := range exports {
		if export.Binding.Attempt == binding.Attempt && f.archive.retire(ctx, export.Binding) != nil {
			return errGitVMOwner
		}
	}
	if prepared && f.owner.forget(ctx, binding.Attempt) != nil {
		return errGitVMOwner
	}
	delete(f.runs, binding.Attempt)
	if gitVMOpenTree(f.root, "run-"+binding.Attempt, 0) != nil || f.root.RemoveAll("run-"+binding.Attempt) != nil {
		return errGitVMOwner
	}
	return nil
}

func (f *gitVMSourceFactory) Restore(ctx context.Context, job Job) (*JobEvidence, error) {
	if !f.enter(ctx) {
		return nil, errGitVMOwner
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil || job.Validate() != nil || job.Snapshot.RepositoryID != f.credentials.state.options.Journal.enrollment.RepositoryID {
		return nil, ErrFenced
	}
	attempts := []string{}
	for attempt, source := range f.requests {
		if source.Binding.Snapshot == job.Snapshot && f.packets[attempt] == Digest(job.Packet) {
			attempts = append(attempts, attempt)
		}
	}
	sort.Strings(attempts)
	if len(attempts) == 0 {
		return nil, ErrFenced
	}
	attempt := attempts[0]
	// Revalidate the durable original-instance stop before using retained
	// source. There is no network, recapture, Git parser or fallback here.
	if f.owner.confirmStopped(ctx, attempt) != nil {
		return nil, ErrFenced
	}
	m := f.materialized[attempt]
	if m == nil {
		var err error
		m, err = f.archive.reopen(ctx, f.requests[attempt])
		if err != nil {
			return nil, ErrFenced
		}
		f.materialized[attempt] = m
	}
	bound, err := restoreGitMaterialization(ctx, m, job)
	if err != nil {
		return nil, err
	}
	if f.activeJobs == nil {
		f.activeJobs = map[*JobEvidence]string{}
	}
	f.activeJobs[bound] = attempt
	return bound, nil
}

func (f *gitVMSourceFactory) ReleaseCapture(ctx context.Context, evidence *SnapshotEvidence) error {
	if !f.enter(ctx) {
		return errGitVMOwner
	}
	defer f.leave()
	if f.closed || evidence == nil || f.pendingCaptures[evidence] == "" {
		return errGitVMOwner
	}
	delete(f.pendingCaptures, evidence)
	return nil
}

func (f *gitVMSourceFactory) ReleaseJob(ctx context.Context, bound *JobEvidence) error {
	if !f.enter(ctx) {
		return errGitVMOwner
	}
	defer f.leave()
	if f.closed || bound == nil || f.activeJobs[bound] == "" {
		return errGitVMOwner
	}
	delete(f.activeJobs, bound)
	return nil
}

// PruneSource retires attempts no longer referenced by the controller store,
// pending captures or active jobs: archive bytes first, then the owner record.
func (f *gitVMSourceFactory) PruneSource(ctx context.Context, store *Store, now time.Time) error {
	if !f.enter(ctx) {
		return errGitVMOwner
	}
	defer f.leave()
	if f.closed || !f.ready || store == nil || f.checkRoot() != nil {
		return errGitVMOwner
	}
	state, err := store.Snapshot(ctx)
	if err != nil || state.Enrollment != f.credentials.state.options.Journal.enrollment {
		return errGitVMOwner
	}
	if store.Prune(ctx, now) != nil {
		return errGitVMOwner
	}
	state, err = store.Snapshot(ctx)
	if err != nil {
		return errGitVMOwner
	}
	keep, used := map[string]bool{}, map[string]bool{}
	for _, attempt := range f.pendingCaptures {
		keep[attempt], used[attempt] = true, true
	}
	for _, attempt := range f.activeJobs {
		keep[attempt], used[attempt] = true, true
	}
	for _, job := range state.Jobs {
		for attempt, source := range f.requests {
			if job.Job.Snapshot == source.Binding.Snapshot && Digest(job.Job.Packet) == f.packets[attempt] {
				keep[attempt] = true
				if job.LeaseToken != "" || job.Outstanding {
					used[attempt] = true
				}
			}
		}
	}
	for attempt, m := range f.materialized {
		if !used[attempt] {
			if m.Close() != nil {
				return errGitVMOwner
			}
			delete(f.materialized, attempt)
		}
	}
	attempts := make([]string, 0, len(f.requests))
	for attempt := range f.requests {
		attempts = append(attempts, attempt)
	}
	sort.Strings(attempts)
	for _, attempt := range attempts {
		if keep[attempt] {
			continue
		}
		// Archive retirement is idempotent once complete, and any partial
		// step fences the factory until recovery reconciles both journals.
		if f.owner.confirmStopped(ctx, attempt) != nil || f.archive.retire(ctx, f.requests[attempt].Binding) != nil || f.owner.forget(ctx, attempt) != nil {
			f.ready = false
			return errGitVMOwner
		}
		delete(f.requests, attempt)
		delete(f.packets, attempt)
	}
	return nil
}

func (f *gitVMSourceFactory) Close(ctx context.Context) error {
	if !f.enter(ctx) {
		return errGitVMOwner
	}
	defer f.leave()
	if f.closed {
		return nil
	}
	if len(f.pendingCaptures) != 0 || len(f.activeJobs) != 0 {
		return errGitVMOwner
	}
	for attempt, run := range f.runs {
		if stopGitVMRun(ctx, run) != nil {
			return errGitVMOwner
		}
		delete(f.runs, attempt)
	}
	// A failed physical stop returns above with every root and lock retained.
	if f.owner.Close() != nil || f.archive.Close(ctx) != nil {
		return errGitVMOwner
	}
	failed := f.assets.Close() != nil
	for attempt, m := range f.materialized {
		if m.Close() != nil {
			failed = true
		}
		delete(f.materialized, attempt)
	}
	if f.root.Close() != nil {
		failed = true
	}
	f.closed = true
	if failed {
		return errGitVMOwner
	}
	return nil
}

var _ gitFactoryAcquisition = (*gitVMRun)(nil)
var _ PollSource = (*gitVMSourceFactory)(nil)
var _ pollSourceRetention = (*gitVMSourceFactory)(nil)

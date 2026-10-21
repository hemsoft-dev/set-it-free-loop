//go:build linux

package reviewer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Internal service composition, never a model capability or serialized gate.
// Only Capture/Restore callbacks are handed to the polling controller. The
// trusted service retains this owner until controller work has joined.
type gitSourceFactory struct {
	operation                       chan struct{}
	credentials                     *GitHubCredentialOwner
	unit                            *gitUnitOwner
	archive                         *gitExportArchive
	runtime                         *gitRuntimeRoot
	helper                          *os.File
	manifest                        gitGrantManifest
	release, releaseHash, directory string
	root                            *os.Root
	identity                        os.FileInfo
	runs                            map[string]*gitFactoryRun
	bindings                        map[string]gitMaterializationBinding
	packets                         map[string]string
	materialized                    map[string]*gitMaterialization
	pendingCaptures                 map[*SnapshotEvidence]string
	activeJobs                      map[*JobEvidence]string
	closed                          bool
	ready                           bool
}

// Opening performs startup recovery before asking the credential owner for any
// ready generation. Basic validation failures return nil and leave inputs owned
// by the caller. Once a nonnil factory is returned, including with an error, it
// owns unit/archive/runtime: retain it for Recover or Close. Capture/Restore are
// fenced until startup recovery succeeds. Credentials remain caller-owned.
// Call this before credential Refresh. No refresh, App install or HTTP occurs.
func newGitSourceFactory(ctx context.Context, credentials *GitHubCredentialOwner, unit *gitUnitOwner, archive *gitExportArchive, runtime *gitRuntimeRoot, helper *os.File, m gitGrantManifest, release, releaseHash, directory string) (*gitSourceFactory, error) {
	if ctx == nil || ctx.Err() != nil || credentials == nil || credentials.state == nil || unit == nil || archive == nil || runtime == nil || helper == nil || !objectID.MatchString(release) || !validAuthHash(releaseHash) || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !gitFactoryRunPaths(filepath.Join(directory, "run-"+strings.Repeat("a", 32))) || privateDirectory(directory) != nil {
		return nil, errGitUnit
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errGitUnit
	}
	credentials.state.mu.Lock()
	busy := len(credentials.state.acquisitions) != 0
	credentials.state.mu.Unlock()
	if busy {
		return nil, errGitUnit
	}
	if !m.validAssets() || m.Worker.SHA256 != unit.stubHash || verifyNativeBootstrap(ctx, helper, m.NamespaceHelper) != nil {
		return nil, errGitUnit
	}
	runtimeFD, err := runtime.directoryFD()
	if err != nil {
		return nil, errGitUnit
	}
	_, runtimeErr := verifyGitRuntimeDirectory(ctx, runtimeFD, m.Runtime)
	runtimeFD.Close()
	if runtimeErr != nil {
		return nil, errGitUnit
	}
	if !gitRetirementSharedFilesystem(unit.root, archive.root) {
		return nil, errGitUnit
	}
	// Recovery is startup-only. A live owner that has already started work is
	// not interchangeable with a newly opened, cross-process-locked owner.
	if !unit.enter(ctx) {
		return nil, errGitUnit
	}
	active := len(unit.rootPIDFDs) != 0 || len(unit.grantEligible) != 0 || len(unit.grantFailed) != 0
	unit.leave()
	if active {
		return nil, errGitUnit
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errGitUnit
	}
	identity, err := root.Stat(".")
	unitIdentity, unitErr := unit.root.Stat(".")
	archiveIdentity, archiveErr := archive.root.Stat(".")
	if err != nil || unitErr != nil || archiveErr != nil || os.SameFile(identity, unitIdentity) || os.SameFile(identity, archiveIdentity) {
		root.Close()
		return nil, errGitUnit
	}
	retainedHelper, err := duplicateGitControlFile(helper)
	if err != nil {
		root.Close()
		return nil, errGitUnit
	}
	// Ownership transfers only after every required local handle is retained.
	// A returned recovery handle is therefore complete even when recovery fails.
	f := &gitSourceFactory{operation: make(chan struct{}, 1), credentials: credentials, unit: unit, archive: archive, runtime: runtime, manifest: m, release: release, releaseHash: releaseHash, directory: directory, root: root, identity: identity, helper: retainedHelper, runs: map[string]*gitFactoryRun{}, packets: map[string]string{}, materialized: map[string]*gitMaterialization{}}
	f.operation <- struct{}{}
	if f.Recover(ctx) != nil {
		return f, errGitUnit
	}
	return f, nil
}

// Recover is a trusted explicit recovery action. It never refreshes credentials
// or restarts an acquisition. Local unknown runs first drain their actual broker
// and generation; only startup owners can rely on the old process's released
// cross-process lock as proof that its broker cannot still execute.
func (f *gitSourceFactory) Recover(ctx context.Context) error {
	if !f.enter(ctx) {
		return errGitUnit
	}
	defer f.leave()
	if f.closed {
		return errGitUnit
	}
	if f.ready {
		return nil
	}
	if f.helper == nil || f.identity == nil || f.checkRoot() != nil {
		return errGitUnit
	}
	for _, run := range f.runs {
		run.mu.Lock()
		generation := run.generation
		run.mu.Unlock()
		if generation != nil {
			if generation.stop(ctx) != nil {
				return errGitUnit
			}
		} else if run.stop(ctx) != nil {
			return errGitUnit
		}
		if run.close() != nil {
			return errGitUnit
		}
	}
	f.credentials.state.mu.Lock()
	busy := len(f.credentials.state.acquisitions) != 0
	f.credentials.state.mu.Unlock()
	if busy {
		return errGitUnit
	}
	if resumeGitRetirements(ctx, f.unit, f.archive) != nil {
		return errGitUnit
	}
	bindings, err := recoverGitFactory(ctx, f.unit, f.archive)
	if err != nil {
		return errGitUnit
	}
	if !f.archive.enter(ctx) {
		return errGitUnit
	}
	exports, err := f.archive.records()
	f.archive.leave()
	if err != nil {
		return errGitUnit
	}
	packets := map[string]string{}
	for _, record := range exports {
		if record.State == "staged" {
			packets[record.Binding.Attempt] = record.PacketHash
		}
	}
	if !f.unit.enter(ctx) {
		return errGitUnit
	}
	units, err := f.unit.records()
	f.unit.leave()
	if err != nil {
		return errGitUnit
	}
	remaining := map[string]bool{}
	for _, record := range units {
		remaining[record.Binding.Attempt] = true
	}
	for _, attempt := range f.pendingCaptures {
		if !remaining[attempt] {
			return errGitUnit
		}
	}
	for _, attempt := range f.activeJobs {
		if !remaining[attempt] {
			return errGitUnit
		}
	}
	for attempt, materialized := range f.materialized {
		if !remaining[attempt] {
			if materialized.Close() != nil {
				return errGitUnit
			}
			delete(f.materialized, attempt)
		}
	}
	for attempt := range f.runs {
		if !remaining[attempt] {
			delete(f.runs, attempt)
		}
	}
	f.bindings, f.packets, f.ready = bindings, packets, true
	return nil
}

func (f *gitSourceFactory) enter(ctx context.Context) bool {
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
func (f *gitSourceFactory) leave() { f.operation <- struct{}{} }

func (f *gitSourceFactory) checkRoot() error {
	anchored, err := f.root.Stat(".")
	path, pathErr := os.Lstat(f.directory)
	if err != nil || pathErr != nil || !path.IsDir() || path.Mode().Perm() != 0700 || !os.SameFile(f.identity, anchored) || !os.SameFile(f.identity, path) {
		return errGitUnit
	}
	return nil
}

func (f *gitSourceFactory) Capture(ctx context.Context, snapshot Snapshot) (*SnapshotEvidence, error) {
	if !f.enter(ctx) {
		return nil, errGitUnit
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil || snapshot.Validate() != nil || snapshot.RepositoryID != f.credentials.state.options.Journal.enrollment.RepositoryID || f.credentials.Ready(ctx) != nil {
		return nil, errGitUnit
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errGitUnit
	}
	binding := gitMaterializationBinding{hex.EncodeToString(nonce[:]), f.releaseHash, Digest(f.manifest), snapshot}
	name := "run-" + binding.Attempt
	if f.root.Mkdir(name, 0700) != nil || syncAuthDirectory(f.root) != nil {
		return nil, errGitUnit
	}
	runtime, err := f.runtime.directoryFD()
	if err != nil {
		return nil, errGitUnit
	}
	defer runtime.Close()
	run, err := newGitFactoryRun(ctx, f.unit, f.archive, binding, f.manifest, runtime, f.helper, filepath.Join(f.directory, name))
	if err != nil {
		f.ready = false
		return nil, errGitUnit
	}
	f.runs[binding.Attempt] = run
	m, err := runGitFactoryAcquisition(ctx, f.credentials, binding, run)
	if err != nil {
		phase, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		retireErr := f.retireFailedCapture(phase, run)
		cancel()
		if retireErr != nil {
			f.ready = false
		}
		return nil, errGitUnit
	}
	evidence, err := captureGitMaterializationEvidence(ctx, m)
	if err != nil {
		m.Close()
		return nil, errGitUnit
	}
	f.bindings[binding.Attempt] = binding
	f.packets[binding.Attempt] = Digest(m.packet)
	f.materialized[binding.Attempt] = m
	if f.pendingCaptures == nil {
		f.pendingCaptures = map[*SnapshotEvidence]string{}
	}
	f.pendingCaptures[evidence] = binding.Attempt
	return evidence, nil
}

func (f *gitSourceFactory) Restore(ctx context.Context, job Job) (*JobEvidence, error) {
	if !f.enter(ctx) {
		return nil, errGitUnit
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil || job.Validate() != nil || job.Snapshot.RepositoryID != f.credentials.state.options.Journal.enrollment.RepositoryID {
		return nil, ErrFenced
	}
	attempts := []string{}
	for attempt, binding := range f.bindings {
		// Recovery authenticated this retained attempt's original release and
		// runtime binding against both journals. Current capture assets do not
		// replace that provenance when restoring an older immutable packet.
		if binding.Snapshot == job.Snapshot && f.packets[attempt] == Digest(job.Packet) {
			attempts = append(attempts, attempt)
		}
	}
	sort.Strings(attempts)
	if len(attempts) == 0 {
		return nil, ErrFenced
	}
	attempt := attempts[0]
	binding := f.bindings[attempt]
	// Revalidate the durable physical-stop journal before using retained source.
	// There is no network, recapture, Git parser or earlier-label fallback here.
	if f.unit.confirmFactoryStop(ctx, binding) != nil {
		return nil, ErrFenced
	}
	m := f.materialized[attempt]
	if m == nil {
		var err error
		m, err = f.archive.reopen(ctx, binding)
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

func (f *gitSourceFactory) ReleaseCapture(ctx context.Context, evidence *SnapshotEvidence) error {
	if !f.enter(ctx) {
		return errGitUnit
	}
	defer f.leave()
	if f.closed || evidence == nil || f.pendingCaptures[evidence] == "" {
		return errGitUnit
	}
	delete(f.pendingCaptures, evidence)
	return nil
}

func (f *gitSourceFactory) ReleaseJob(ctx context.Context, bound *JobEvidence) error {
	if !f.enter(ctx) {
		return errGitUnit
	}
	defer f.leave()
	if f.closed || bound == nil || f.activeJobs[bound] == "" {
		return errGitUnit
	}
	delete(f.activeJobs, bound)
	return nil
}

func (f *gitSourceFactory) Close(ctx context.Context) error {
	if !f.enter(ctx) {
		return errGitUnit
	}
	defer f.leave()
	if f.closed {
		return nil
	}
	if len(f.pendingCaptures) != 0 || len(f.activeJobs) != 0 {
		return errGitUnit
	}
	for _, run := range f.runs {
		run.mu.Lock()
		generation := run.generation
		run.mu.Unlock()
		if generation != nil {
			if generation.stop(ctx) != nil {
				return errGitUnit
			}
		} else if run.stop(ctx) != nil {
			return errGitUnit
		}
		if run.close() != nil {
			return errGitUnit
		}
	}
	// A failed physical stop returns above with every shared root and service
	// lock retained. Runtime cleanup cannot race an unknown live acquisition.
	if f.unit.Close() != nil {
		return errGitUnit
	}
	if f.archive.Close(ctx) != nil {
		return errGitUnit
	}
	if f.runtime.Close() != nil {
		return errGitUnit
	}
	failed := false
	for attempt, m := range f.materialized {
		if m.Close() != nil {
			failed = true
		}
		delete(f.materialized, attempt)
	}
	if f.helper != nil && f.helper.Close() != nil {
		failed = true
	}
	if f.root.Close() != nil {
		failed = true
	}
	f.closed = true
	if failed {
		return errGitUnit
	}
	return nil
}

// Compile-time guarantee that production uses the concrete fixed driver.
var _ gitFactoryAcquisition = (*gitFactoryRun)(nil)
var _ PollSource = (*gitSourceFactory)(nil)

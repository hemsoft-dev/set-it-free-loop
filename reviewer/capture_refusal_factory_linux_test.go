//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// refusedWorkerStore is the quarantine an acquisition worker would hold for a
// refused snapshot: actual Git, fetched from the synthetic repository.
func refusedWorkerStore(t *testing.T, name string) (snapshotGitFixture, GitStore) {
	t.Helper()
	f := refusedWorkerFixture(t, name)
	store, _ := gitQuarantineFromFixture(t, f, "sha1")
	return f, store
}

// refusingGitFactoryRun replaces only the worker side of a real factory run:
// its export is what writeGitMaterialization produces for the run's own
// binding. Generation retention, stop and close are the run's real methods.
type refusingGitFactoryRun struct {
	*gitFactoryRun
	store   GitStore
	replace func(*gitMaterializationBinding)
}

func (r refusingGitFactoryRun) acquire(ctx context.Context, _ *gitGenerationReader) (*gitMaterialization, error) {
	binding := r.binding
	if r.replace != nil {
		r.replace(&binding)
	}
	var wire bytes.Buffer
	if err := writeGitMaterialization(ctx, &wire, r.store, binding, DefaultGitAcquisitionBounds()); err != nil {
		return nil, err
	}
	return r.archive.stage(ctx, r.binding, io.NopCloser(bytes.NewReader(wire.Bytes())))
}

func refusingHostCapture(store GitStore, replace func(*gitMaterializationBinding)) func(*gitFactoryRun) gitFactoryAcquisition {
	return func(run *gitFactoryRun) gitFactoryAcquisition {
		return refusingGitFactoryRun{run, store, replace}
	}
}

func refusalSourceFactoryFixture(t *testing.T) *gitSourceFactory {
	t.Helper()
	r, f, runtime := gitFactoryAssetsFixture(t)
	owner, _, _, _, signs := credentialOwnerFixture(t)
	factory, err := newGitSourceFactory(context.Background(), owner, f.owner, r.archive, runtime, r.files[2], r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err != nil {
		t.Fatal("offline startup", err)
	}
	t.Cleanup(func() { factory.Close(context.Background()) })
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("prepared fixture cleanup")
	}
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake pair qualification")
	}
	if signs.Load() == 0 {
		t.Fatal("fake pair not minted")
	}
	return factory
}

// hostAttemptRecords returns the unit and export records of attempt.
func hostAttemptRecords(t *testing.T, factory *gitSourceFactory, attempt string) (*gitUnitRecord, *gitExportRecord, bool) {
	t.Helper()
	factory.unit.enter(context.Background())
	units, err := factory.unit.records()
	factory.unit.leave()
	if err != nil {
		t.Fatal(err)
	}
	factory.archive.enter(context.Background())
	exports, err := factory.archive.records()
	factory.archive.leave()
	if err != nil {
		t.Fatal(err)
	}
	var unit *gitUnitRecord
	var export *gitExportRecord
	for i := range units {
		if units[i].Binding.Attempt == attempt {
			unit = &units[i]
		}
	}
	for i := range exports {
		if exports[i].Binding.Attempt == attempt {
			export = &exports[i]
		}
	}
	_, payloadErr := factory.archive.root.Lstat(gitExportDirectory(attempt) + "/payload.bin")
	return unit, export, os.IsNotExist(payloadErr)
}

// The host factory returns the worker's refusal unwrapped only after the
// run's proved stop, generation release and intake discard, exactly like a
// failed capture. Retention then retires the attempt, and the next capture of
// the same snapshot is a new attempt that is not blocked.
func TestGitSourceFactoryCaptureReturnsRetiredWorkerRefusal(t *testing.T) {
	for _, c := range workerRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			git, store := refusedWorkerStore(t, c.name)
			factory := refusalSourceFactoryFixture(t)
			durable, _ := openFixtureStore(t)
			for round := range 2 {
				evidence, err := factory.capture(context.Background(), git.snapshot, refusingHostCapture(store, nil))
				if evidence != nil || err != (CaptureRefusal{c.reason}) {
					t.Fatal("refusal not returned unwrapped", round, err)
				}
				if !factory.ready || len(factory.bindings) != 0 || len(factory.pendingCaptures) != 0 {
					t.Fatal("refusal retained source or fenced the factory", factory.ready)
				}
				factory.credentials.state.mu.Lock()
				owned := len(factory.credentials.state.acquisitions)
				factory.credentials.state.mu.Unlock()
				if owned != 0 {
					t.Fatal("refusal kept its reader generation")
				}
				var attempt string
				for a, run := range factory.runs {
					run.mu.Lock()
					stopped, closed := run.stopped, run.closed
					run.mu.Unlock()
					if !stopped || !closed {
						t.Fatal("refused run not stopped and closed")
					}
					attempt = a
				}
				unit, export, discarded := hostAttemptRecords(t, factory, attempt)
				if unit == nil || !unit.Stopped || !unit.Revoked || export == nil || export.State != "discarded" || !discarded {
					t.Fatal("refused attempt not stopped and discarded", unit, export, discarded)
				}
				if factory.PruneSource(context.Background(), durable, time.Now()) != nil {
					t.Fatal("prune after refusal")
				}
				if unit, export, _ := hostAttemptRecords(t, factory, attempt); unit != nil || export != nil {
					t.Fatal("refused attempt retained a record", unit, export)
				}
				if _, err := factory.unit.root.Lstat(gitUnitDirectory(attempt)); !os.IsNotExist(err) {
					t.Fatal("refused unit receipt not retired", err)
				}
				if _, err := factory.archive.root.Lstat(gitExportDirectory(attempt)); !os.IsNotExist(err) {
					t.Fatal("refused export not retired", err)
				}
				factory.runs = map[string]*gitFactoryRun{}
			}
		})
	}
}

// A refusal for another attempt, or with source data after it, is an opaque
// failure that is still stopped and discarded and never fences the factory.
func TestGitSourceFactoryRejectsForeignWorkerRefusal(t *testing.T) {
	git, store := refusedWorkerStore(t, "inventory over limit")
	factory := refusalSourceFactoryFixture(t)
	foreign := func(b *gitMaterializationBinding) { b.Attempt = strings.Repeat("e", 32) }
	evidence, err := factory.capture(context.Background(), git.snapshot, refusingHostCapture(store, foreign))
	if evidence != nil || errors.As(err, new(CaptureRefusal)) || !errors.Is(err, errGitUnit) || !factory.ready {
		t.Fatal("foreign refusal accepted", err)
	}
	for attempt := range factory.runs {
		if _, export, discarded := hostAttemptRecords(t, factory, attempt); export == nil || export.State != "discarded" || !discarded {
			t.Fatal("foreign refusal intake not discarded", export)
		}
	}
}

// refusingGitVMRun replaces only the source guest: its export is what the
// guest's writer produces for this request, followed by a finished record
// that measures it, through the run's own staging check. Generation
// retention, stop and close are the run's real methods.
type refusingGitVMRun struct {
	*gitVMRun
	store  GitStore
	finish func(*gitVMGuestFinished)
}

func (r refusingGitVMRun) acquire(ctx context.Context, source *gitGenerationReader) (*gitMaterialization, error) {
	q := gitVMGuestRequest{1, gitVMGuestPurpose, r.source, gitVMReadBinding{1, "source-vm-git-read", r.source.Binding, r.channelID, source.generation.id}}
	var export bytes.Buffer
	if err := writeGitVMExport(ctx, &export, r.store, r.source); err != nil {
		return nil, err
	}
	finished := gitVMGuestFinished{1, gitVMGuestPurpose, Digest(q), int64(export.Len()), gitRawHash(export.Bytes()), GitReadBrokerAudit{Version: 1, Connections: 1, Requests: 2, Stopped: true}, true}
	if r.finish != nil {
		r.finish(&finished)
	}
	var control bytes.Buffer
	if err := writeVMRecord(&control, finished, gitVMRecordBytes); err != nil {
		return nil, err
	}
	m, failed, err := stageGitVMExport(ctx, r.archive, q, io.NopCloser(&export), &control)
	if failed != "" {
		return nil, r.fail(failed)
	}
	return m, err
}

func refusingVMCapture(store GitStore, finish func(*gitVMGuestFinished)) func(*gitVMRun) gitFactoryAcquisition {
	return func(run *gitVMRun) gitFactoryAcquisition { return refusingGitVMRun{run, store, finish} }
}

func refusalVMSourceFactoryFixture(t *testing.T) *gitVMSourceFactory {
	t.Helper()
	f := newGitVMFactoryRecoveryFixture(t)
	factory, err := f.open(t)
	if err != nil {
		t.Fatal("offline startup", err)
	}
	if f.credentials.Refresh(context.Background()) != nil {
		t.Fatal("fake pair qualification")
	}
	return factory
}

// The VM factory returns the guest's refusal unwrapped only after the
// guest's finished record measures it, the run is proved stopped and its
// generation released, and the attempt is retired from both journals.
func TestGitVMSourceFactoryCaptureReturnsRetiredGuestRefusal(t *testing.T) {
	for _, c := range workerRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			git, store := refusedWorkerStore(t, c.name)
			factory := refusalVMSourceFactoryFixture(t)
			for round := range 2 {
				evidence, err := factory.capture(context.Background(), git.snapshot, refusingVMCapture(store, nil))
				if evidence != nil || err != (CaptureRefusal{c.reason}) {
					t.Fatal("guest refusal not returned unwrapped", round, err)
				}
				assertGitVMRefusalRetired(t, factory)
			}
		})
	}
}

func assertGitVMRefusalRetired(t *testing.T, factory *gitVMSourceFactory) {
	t.Helper()
	factory.credentials.state.mu.Lock()
	owned := len(factory.credentials.state.acquisitions)
	factory.credentials.state.mu.Unlock()
	journal, err := factory.owner.journal.read()
	if err != nil {
		t.Fatal(err)
	}
	exports, _, err := factory.archive.records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(factory.directory)
	if err != nil {
		t.Fatal(err)
	}
	if !factory.ready || owned != 0 || len(factory.runs) != 0 || len(factory.requests) != 0 || len(journal.Records) != 0 || len(exports) != 0 || len(entries) != 0 {
		t.Fatal("refused attempt not stopped and retired", factory.ready, owned, len(factory.runs), journal.Records, exports, len(entries))
	}
}

// A refusal whose finished record does not measure the staged bytes, or
// whose guest did not report a stopped broker and worker, is opaque. The
// attempt is still retired and the factory stays ready.
func TestGitVMSourceFactoryRefusalRequiresMeasuredFinish(t *testing.T) {
	git, store := refusedWorkerStore(t, "criss-cross merge base")
	for name, finish := range map[string]func(*gitVMGuestFinished){
		"bytes":          func(f *gitVMGuestFinished) { f.ExportBytes-- },
		"hash":           func(f *gitVMGuestFinished) { f.ExportHash = strings.Repeat("0", 64) },
		"worker running": func(f *gitVMGuestFinished) { f.WorkerStopped = false },
		"broker failed":  func(f *gitVMGuestFinished) { f.Broker.Failed = true },
		"other request":  func(f *gitVMGuestFinished) { f.RequestHash = Digest("other request") },
	} {
		t.Run(name, func(t *testing.T) {
			factory := refusalVMSourceFactoryFixture(t)
			evidence, err := factory.capture(context.Background(), git.snapshot, refusingVMCapture(store, finish))
			if evidence != nil || errors.As(err, new(CaptureRefusal)) || !errors.Is(err, errGitVMOwner) {
				t.Fatal("unmeasured refusal accepted", err)
			}
			assertGitVMRefusalRetired(t, factory)
		})
	}
}

// The guest's early refusal writer and its export writer produce the same
// request-bound bytes for a refused quarantine.
func TestGitVMRefusalWritersAgree(t *testing.T) {
	for _, c := range workerRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			git, store := refusedWorkerStore(t, c.name)
			q := gitVMRequestFixture(t, workerRefusalBinding(git.snapshot))
			var exported, refused bytes.Buffer
			if writeGitVMExport(context.Background(), &exported, store, q) != nil || writeGitVMRefusal(context.Background(), &refused, q, c.reason) != nil {
				t.Fatal("refusal writers")
			}
			if !bytes.Equal(exported.Bytes(), refused.Bytes()) {
				t.Fatal("guest refusal writers disagree")
			}
			for _, reason := range []string{"", "synthetic_reason"} {
				var b bytes.Buffer
				if writeGitVMRefusal(context.Background(), &b, q, reason) == nil || b.Len() != 0 {
					t.Fatal("unknown reason written", reason)
				}
			}
		})
	}
}

// End to end in process: the poll worker captures through each production
// factory, whose worker side exports the real refusal. The refusal becomes
// one terminal capture_refused job with one INCOMPLETE status, the request is
// admitted, nothing is retried or deferred, and the attempt is retired.
func TestPollingRecordsProductionFactoryRefusal(t *testing.T) {
	for _, kind := range []string{"host", "source VM"} {
		for _, c := range workerRefusalCases {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				git, store := refusedWorkerStore(t, c.name)
				poll, f, calls := pollControllerFixture(t, nil)
				captures := useCapture(poll, f, git, git.snapshot)
				var capture func(context.Context, Snapshot) (*SnapshotEvidence, error)
				var retired func()
				if kind == "host" {
					factory := refusalSourceFactoryFixture(t)
					capture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
						return factory.capture(ctx, s, refusingHostCapture(store, nil))
					}
					retired = func() {
						for attempt := range factory.runs {
							if _, export, discarded := hostAttemptRecords(t, factory, attempt); export == nil || export.State != "discarded" || !discarded {
								t.Fatal("host refusal intake retained", export)
							}
						}
					}
				} else {
					factory := refusalVMSourceFactoryFixture(t)
					capture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
						return factory.capture(ctx, s, refusingVMCapture(store, nil))
					}
					retired = func() { assertGitVMRefusalRetired(t, factory) }
				}
				poll.options.Capture = func(ctx context.Context, s Snapshot) (*SnapshotEvidence, error) {
					captures.Add(1)
					return capture(ctx, s)
				}
				if _, err := pollOnce(t, poll); err != nil {
					t.Fatal(err)
				}
				settleWorker(t, poll)
				for range 3 {
					f.clock.Advance(2 * time.Second)
					result, err := pollOnce(t, poll)
					if err != nil || result.Active {
						t.Fatal("production refusal retried or deferred", err, result.Active)
					}
				}
				d, err := f.store.Snapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(d.Jobs) != 1 {
					t.Fatal("refusal not one job", len(d.Jobs))
				}
				for _, job := range d.Jobs {
					if job.State != "capture_refused" || job.Job.Snapshot != git.snapshot || refusalPacketReason(t, job.Job) != c.reason {
						t.Fatal("production refusal not recorded with its reason", job.State, job.Job.Packet)
					}
				}
				pr := d.PRs[alphaScope().key()]
				if len(pr.Requests) != 1 || !pr.Requests[0].Admitted || d.Polling == nil || d.Polling.Failures != 0 {
					t.Fatal("production refusal blocked admission or backed off", pr.Requests, d.Polling)
				}
				statuses := statusWrites(t, f)
				if captures.Load() != 1 || calls.Load() != 0 || len(statuses) != 1 || statuses[0].Status != "capture_refused" || !strings.Contains(statuses[0].Document.Summary, captureRefusalCauses[c.reason]) {
					t.Fatal("production refusal not published once", captures.Load(), calls.Load(), len(statuses))
				}
				retired()
			})
		}
	}
}

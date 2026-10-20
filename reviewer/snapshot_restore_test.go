package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Use an actual Git-produced packet captured while one real blob is missing,
// then restore the blob. A fresh capture now differs for the identical snapshot.
func snapshotRestoredBlobFixture(t *testing.T) (snapshotGitFixture, *SnapshotEvidence) {
	t.Helper()
	f := newSnapshotGitFixture(t)
	blob := f.run(t, "rev-parse", "HEAD:safe.go")
	path := filepath.Join(f.dir, ".git", "objects", blob[:2], blob[2:])
	data := mustSnapshotRead(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0444); err != nil {
		t.Fatal(err)
	}
	fresh, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if e.Packet() == fresh.Packet() {
		t.Fatal("fixture did not change omissions")
	}
	return f, e
}

func TestSnapshotRestorePreservesAdmittedOmissionsAfterBlobRecovery(t *testing.T) {
	f, e := snapshotRestoredBlobFixture(t)
	job := testJob()
	job.Snapshot = f.snapshot
	job.Packet = e.Packet()
	bound, err := RestoreSnapshotEvidence(context.Background(), f.store(), job)
	if err != nil {
		t.Fatal(err)
	}
	if bound.evidence.Packet() != job.Packet || !reflect.DeepEqual(bound.evidence.Coverage(), e.Coverage()) || !reflect.DeepEqual(bound.diffLines(), bindSnapshotEvidence(t, e).diffLines()) {
		t.Fatal("restore substituted evidence")
	}
	if len(bound.diffLines()["safe.go"]) != 0 {
		t.Fatal("restored blob gained inline anchors")
	}
	reader, err := bound.Reader(f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, coverage := reader.Evidence()
	if !reflect.DeepEqual(coverage, e.Coverage()) {
		t.Fatal("reader lost original omissions")
	}
}

func TestSnapshotRestoreRejectsAlteredPacketsAndUnavailableIncludedBlob(t *testing.T) {
	f := newSnapshotGitFixture(t)
	e, err := CaptureSnapshotEvidence(context.Background(), f.store(), f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	job.Snapshot = f.snapshot
	job.Packet = e.Packet()
	if _, err := RestoreSnapshotEvidence(context.Background(), f.store(), job); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*snapshotPacket){
		"inventory": func(p *snapshotPacket) { p.Files = p.Files[1:] },
		"identity":  func(p *snapshotPacket) { p.Files[0].NewObject = strings.Repeat("f", 40) },
		"patch": func(p *snapshotPacket) {
			for i := range p.Files {
				if p.Files[i].Path == "safe.go" {
					p.Files[i].Patch = strings.Replace(p.Files[i].Patch, "+FOUR", "+forged", 1)
				}
			}
		},
		"coverage": func(p *snapshotPacket) {
			p.Coverage = append(p.Coverage, Coverage{"outside", "patch unavailable or patch byte limit exhausted"})
		},
		"missing omission":   func(p *snapshotPacket) { p.Coverage = nil },
		"duplicate omission": func(p *snapshotPacket) { p.Coverage = append(p.Coverage, p.Coverage[0]) },
		"version":            func(p *snapshotPacket) { p.Version = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			var p snapshotPacket
			json.Unmarshal([]byte(job.Packet), &p)
			mutate(&p)
			raw, _ := json.Marshal(p)
			bad := job
			bad.Packet = string(raw)
			if _, err := RestoreSnapshotEvidence(context.Background(), f.store(), bad); err == nil {
				t.Fatal("altered evidence accepted")
			}
		})
	}
	bad := job
	bad.Packet += " "
	if _, err := RestoreSnapshotEvidence(context.Background(), f.store(), bad); err == nil {
		t.Fatal("noncanonical packet accepted")
	}
	blob := f.run(t, "rev-parse", "HEAD:safe.go")
	if err := os.Remove(filepath.Join(f.dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if bound, err := RestoreSnapshotEvidence(context.Background(), f.store(), job); err == nil || bound != nil {
		t.Fatal("included unavailable blob silently omitted")
	}
}

func TestPollingRestoresQueuedAndCompletedJobsWithoutRecapture(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "completed"}[completed], func(t *testing.T) {
			c, f, calls := pollControllerFixture(t, nil)
			git, e := snapshotRestoredBlobFixture(t)
			f.remote.Snapshot = git.snapshot
			f.remote.Request.Snapshot = git.snapshot
			f.batch.Requests[0].Snapshot = git.snapshot
			_, batch, _ := f.capture()
			if err := f.store.Discover(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			job, err := f.store.Admit(context.Background(), alphaScope(), 1, c.options.Release, e.Packet(), f.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			c.options.Restore = func(ctx context.Context, j Job) (*JobEvidence, error) {
				return RestoreSnapshotEvidence(ctx, git.store(), j)
			}
			if completed {
				bound, err := e.Bind(job)
				if err != nil {
					t.Fatal(err)
				}
				engine, err := c.options.Runtime(context.Background(), job, bound)
				if err != nil {
					t.Fatal(err)
				}
				engine.Reader = bound.Reader
				if _, err := f.store.RunStopped(context.Background(), job.ID, engine, f.clock.Now); err != nil {
					t.Fatal(err)
				}
			}
			c.options.Capture = func(context.Context, Snapshot) (*SnapshotEvidence, error) {
				t.Error("resumed job recaptured")
				return nil, ErrFenced
			}
			result, err := pollOnce(t, c)
			if err != nil || !result.Active {
				t.Fatal(result, err)
			}
			awaitPollWorker(t, c)
			if err := c.drainWork(); err != nil {
				t.Fatal(err)
			}
			d, err := f.store.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if d.Jobs[job.ID].Job.Packet != job.Packet || d.Jobs[job.ID].State != "dual_complete" || len(d.Writes) != 1 || calls.Load() != 4 {
				t.Fatal("job did not recover original packet", d.Jobs[job.ID].State, len(d.Writes), calls.Load())
			}
		})
	}
}

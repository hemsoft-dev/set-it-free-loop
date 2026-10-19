package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type brokerDriver struct {
	identity Identity
	calls    int
	tools    SourceTools
	work     func(context.Context, SourceTools, Session)
	err      error
}

func (d *brokerDriver) Inspect(context.Context, Invocation) (Identity, error) {
	return d.identity, nil
}

func (d *brokerDriver) Execute(ctx context.Context, inv Invocation) ([]byte, Identity, error) {
	d.calls++
	d.tools = inv.Source
	var s Session
	if err := json.Unmarshal(inv.Input[bytes.IndexByte(inv.Input, '\n')+1:], &s); err != nil {
		return nil, d.identity, err
	}
	d.work(ctx, inv.Source, s)
	if d.err != nil {
		return nil, d.identity, d.err
	}
	raw, err := json.Marshal(protocolReport(s))
	return raw, d.identity, err
}

func brokerReader(s Snapshot) (*Reader, error) {
	return NewReader(&fixtureStore{files: map[string]Object{
		"source.go": {"100644", []byte("original source")},
		"worker.go": {"100644", []byte("worker-only evidence")},
	}}, s, []string{"source.go", "worker.go"}, DefaultLimits())
}

func brokerEngine(t *testing.T, work func(context.Context, SourceTools, Session)) (Engine, []*brokerDriver) {
	t.Helper()
	e := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{}}
	var drivers []*brokerDriver
	for _, p := range []Provider{Codex, Claude} {
		identity, _ := PinnedIdentity(p)
		d := &brokerDriver{identity: identity, work: work}
		r, err := brokerReader(testSnapshot())
		if err != nil {
			t.Fatal(err)
		}
		e.Reviewers[p] = Adapter{Provider: p, Driver: d, AuthDirectory: t.TempDir(), Reader: r}
		drivers = append(drivers, d)
	}
	return e, drivers
}

func TestBrokerReadsAreSeparateFromCitationValidation(t *testing.T) {
	e, drivers := brokerEngine(t, func(ctx context.Context, tools SourceTools, s Session) {
		if tools == nil {
			t.Fatal("worker received no source capability")
		}
		text, err := tools.Read(ctx, s.Snapshot.Head, "worker.go")
		if err != nil || text != "worker-only evidence" {
			t.Fatal(text, err)
		}
	})
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil || out.State != "dual_complete" || len(out.Artifacts) != 4 {
		t.Fatal(out, err)
	}
	for _, a := range out.Artifacts {
		access := a.SourceAccess
		if access == nil || access.SessionHash != a.InputHash || access.ToolCalls != 1 || access.ReaderCalls != 1 || len(access.Reads) != 1 || access.Reads[0].Path != "worker.go" || access.Reads[0].Hash != Digest([]byte("worker-only evidence")) || access.Bytes != len("worker-only evidence") {
			t.Fatal("actual access lost or mixed with validation", access)
		}
		if len(a.ValidationReads) != 1 || a.ValidationReads[0].Path != "source.go" {
			t.Fatal("citation validation mislabeled as worker access", a.ValidationReads)
		}
	}
	for _, d := range drivers {
		if d.calls != 2 {
			t.Fatal("wrong provider session count", d.calls)
		}
		if _, err := d.tools.Read(context.Background(), testSnapshot().Head, "source.go"); err == nil {
			t.Fatal("completed worker retained a live source capability")
		}
	}
	checkpoint := out.Checkpoint()
	checkpoint.Artifacts[0].SourceAccess.Reads[0].Path = "changed.go"
	if out.Artifacts[0].SourceAccess.Reads[0].Path != "worker.go" {
		t.Fatal("checkpoint aliases source receipts")
	}
	checkpoint = out.Checkpoint()
	checkpoint.Artifacts[1].SourceAccess = cloneSourceAccess(checkpoint.Artifacts[0].SourceAccess)
	if bad, err := e.Run(context.Background(), out.Job, checkpoint); err == nil || bad.State != "invalid_output" || len(bad.Ledger) != 0 {
		t.Fatal("cross-phase source receipt accepted", bad, err)
	}
	for _, d := range drivers {
		if d.calls != 2 {
			t.Fatal("invalid checkpoint started another provider session")
		}
	}
}

func TestSourceBrokerBoundsCallsAndSealsSnapshotCapability(t *testing.T) {
	s := testSession(Codex)
	s.Limits.ReaderCalls = 2
	r, _ := brokerReader(s.Snapshot)
	b, err := newSourceBroker(context.Background(), s, r)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = b.Read(ctx, s.Snapshot.Head, "worker.go"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Read(ctx, strings.Repeat("f", 40), "worker.go"); err == nil {
		t.Fatal("cross-job object revision accepted")
	}
	if _, err = b.Search(ctx, s.Snapshot.Head, "evidence", 1); err == nil {
		t.Fatal("tool-call cap bypassed by switching operations")
	}
	a := b.seal()
	if err = a.validate(s); err != nil || !a.Exhausted || a.ToolCalls != 2 || a.ReaderCalls != 2 || len(a.Reads) != 1 || len(a.Coverage) != 2 {
		t.Fatal("denied call evidence missing", a, err)
	}
	a.Reads[0].Path = "changed.go"
	if b.seal().Reads[0].Path != "worker.go" {
		t.Fatal("receipt exposes mutable broker state")
	}
	if _, err = b.Read(ctx, s.Snapshot.Head, "worker.go"); err == nil {
		t.Fatal("sealed capability was reusable")
	}
	other := s
	other.Snapshot.Head = strings.Repeat("f", 40)
	if _, err = newSourceBroker(context.Background(), other, r); err == nil {
		t.Fatal("broker attached to another snapshot")
	}
}

func TestSourceSearchRetainsScannedObjectsAndOmissions(t *testing.T) {
	s := testSession(Claude)
	r, _ := brokerReader(s.Snapshot)
	b, err := newSourceBroker(context.Background(), s, r)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := b.Search(context.Background(), s.Snapshot.Head, "worker-only", 1)
	if err != nil || len(matches) != 1 || matches[0].Path != "worker.go" {
		t.Fatal(matches, err)
	}
	a := b.seal()
	if err = a.validate(s); err != nil || a.ToolCalls != 1 || a.ReaderCalls != 2 || len(a.Reads) != 2 || a.Reads[0].Path != "source.go" || a.Reads[1].Path != "worker.go" {
		t.Fatal("search omitted a scanned nonmatch", a, err)
	}
	e, _ := brokerEngine(t, func(ctx context.Context, tools SourceTools, s Session) {
		if _, err := tools.Search(ctx, s.Snapshot.Head, "invalid\nsearch", 1); err == nil {
			t.Fatal("invalid search accepted")
		}
		if _, err := tools.Read(ctx, s.Snapshot.Head, "../credential-canary"); err == nil {
			t.Fatal("traversal accepted")
		}
	})
	out, err := e.Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range out.Artifacts {
		if len(phase.SourceAccess.Coverage) != 2 || len(phase.Report.Coverage) != 2 || len(phase.SourceAccess.Reads) != 0 {
			t.Fatal("worker omissions erased by a report claiming full coverage", phase)
		}
	}
	doc, err := Render(out, nil)
	if err != nil || !strings.Contains(doc.Summary, "invalid bounded source search") || !strings.Contains(doc.Summary, "path outside job allowlist") || strings.Contains(doc.Summary, "credential-canary") {
		t.Fatal("omissions missing or unsafe path leaked", doc, err)
	}
	checkpoint := out.Checkpoint()
	for i := range checkpoint.Artifacts {
		checkpoint.Artifacts[i].Report.Coverage = []Coverage{}
	}
	resumed, err := e.Run(context.Background(), out.Job, checkpoint)
	if err != nil || resumed.ExecutedSessions != 0 {
		t.Fatal("source coverage recovery repeated inference", err)
	}
	for _, phase := range resumed.Artifacts {
		if len(phase.Report.Coverage) != 2 {
			t.Fatal("saved source omissions disappeared during resume", phase)
		}
	}
}

func TestModelReportCannotSupplySourceReceipt(t *testing.T) {
	s := testSession(Codex)
	raw, _ := json.Marshal(fakeReport(s))
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["source_access"] = map[string]any{"reads": []string{"invented worker read"}}
	raw, _ = json.Marshal(fields)
	r, _ := brokerReader(s.Snapshot)
	d := &fakeDriver{inspect: s.Identity, actual: s.Identity, data: raw}
	_, access, err := (Adapter{Provider: Codex, Driver: d, AuthDirectory: t.TempDir(), Reader: r}).ReviewSource(context.Background(), s)
	if err == nil || phaseState(err) != "invalid_output" || access == nil || len(access.Reads) != 0 || access.ToolCalls != 0 {
		t.Fatal("model forged a controller source receipt", access, err)
	}
}

func TestDurableSourceReceiptsSurviveRestartWithoutProviderReplay(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	e, drivers := brokerEngine(t, func(ctx context.Context, tools SourceTools, session Session) {
		if _, err := tools.Read(ctx, session.Snapshot.Base, "worker.go"); err != nil {
			t.Fatal(err)
		}
	})
	out, err := s.RunStopped(ctx, job.ID, e, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var schema, path string
	if err = s.db.QueryRow("PRAGMA database_list").Scan(&seq, &schema, &path); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path, alphaEnrollment(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.RecoverCompleted(ctx, job.ID, brokerReader)
	if err != nil || recovered.ExecutedSessions != 0 || len(recovered.Artifacts) != 4 {
		t.Fatal(recovered, err)
	}
	for i, a := range recovered.Artifacts {
		if a.SourceAccess == nil || Digest(a.SourceAccess) != Digest(out.Artifacts[i].SourceAccess) || a.SourceAccess.Reads[0].Revision != job.Snapshot.Base {
			t.Fatal("recovery changed actual source access", a)
		}
	}
	durable, err := s.Snapshot(ctx)
	if err != nil || len(durable.Jobs[job.ID].SourceAttempts) != 4 {
		t.Fatal("actual phase attempts not durably retained", err)
	}
	for _, d := range drivers {
		if d.calls != 2 {
			t.Fatal("recovery repeated provider work", d.calls)
		}
	}
}

func TestFailedWorkerStillRetainsActualSourceAccess(t *testing.T) {
	s, job, _ := admittedFixture(t)
	e, drivers := brokerEngine(t, func(ctx context.Context, tools SourceTools, session Session) {
		if _, err := tools.Read(ctx, session.Snapshot.Head, "worker.go"); err != nil {
			t.Fatal(err)
		}
	})
	drivers[0].err = PhaseError{State: "quota_wait"}
	out, err := s.RunStopped(context.Background(), job.ID, e, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if err == nil || out.State != "quota_wait" || len(out.Ledger) != 0 || out.FailedSourceAccess == nil || out.FailedSourceAccess.Reads[0].Path != "worker.go" {
		t.Fatal("failed phase discarded actual source access", out, err)
	}
	d, err := s.Snapshot(context.Background())
	if err != nil || len(d.Jobs[job.ID].SourceAttempts) != 1 || d.Jobs[job.ID].SourceAttempts[0].Access.Reads[0].Path != "worker.go" || d.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 {
		t.Fatal("failed phase audit missing from durable state", err)
	}
}

func TestSourceAuditWriteFailureRetainsLeaseAndRecoversWithoutReplay(t *testing.T) {
	s, job, _ := admittedFixture(t)
	ctx := context.Background()
	e, drivers := brokerEngine(t, func(ctx context.Context, tools SourceTools, session Session) {
		if _, err := tools.Read(ctx, session.Snapshot.Head, "worker.go"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER audit_fault BEFORE UPDATE ON state BEGIN SELECT RAISE(FAIL, 'audit_fault'); END`); err != nil {
			t.Fatal(err)
		}
	})
	out, err := s.RunStopped(ctx, job.ID, e, func() time.Time { return controlTime.Add(4 * time.Minute) })
	if !errors.Is(err, ErrSourceAuditPending) || out.FailedSourceAccess == nil || len(out.Ledger) != 0 {
		t.Fatal("audit write failure was hidden", out, err)
	}
	d, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := d.Jobs[job.ID]
	if j.LeaseToken == "" || !j.Outstanding || j.Checkpoint.ConsumedSessions != 1 || len(j.SourceAttempts) != 0 {
		t.Fatal("audit failure lost lease/attempt recovery state", j)
	}
	if _, err = s.db.Exec("DROP TRIGGER audit_fault"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordStoppedSourceAccess(ctx, job.ID, j.LeaseToken, "wrong-attempt", out.FailedSourceAccess); !errors.Is(err, ErrFenced) {
		t.Fatal("source audit rebound to another attempt", err)
	}
	for i := 0; i < 2; i++ {
		if err = s.RecordStoppedSourceAccess(ctx, job.ID, j.LeaseToken, j.AttemptID, out.FailedSourceAccess); err != nil {
			t.Fatal("exact audit recovery was not idempotent", err)
		}
	}
	changed := cloneSourceAccess(out.FailedSourceAccess)
	changed.Reads[0].Hash = strings.Repeat("f", 64)
	if err = s.RecordStoppedSourceAccess(ctx, job.ID, j.LeaseToken, j.AttemptID, changed); !errors.Is(err, ErrFenced) {
		t.Fatal("accepted replacement source audit", err)
	}
	if err = s.ReleaseStopped(ctx, job.ID, j.LeaseToken, "provider_unavailable"); err != nil {
		t.Fatal(err)
	}
	d, err = s.Snapshot(ctx)
	if err != nil || len(d.Jobs[job.ID].SourceAttempts) != 1 || d.Jobs[job.ID].Checkpoint.ConsumedSessions != 1 || drivers[0].calls != 1 || drivers[1].calls != 0 {
		t.Fatal("audit recovery lost evidence or replayed provider work", err)
	}
}

type cancelSourceStore struct{ entered chan struct{} }

func (s cancelSourceStore) Read(ctx context.Context, _, _ string, _ int) (Object, error) {
	close(s.entered)
	<-ctx.Done()
	return Object{}, ctx.Err()
}

func TestBrokerUsesControllerCancellationInsideStore(t *testing.T) {
	s := testSession(Codex)
	entered := make(chan struct{})
	r, _ := NewReader(cancelSourceStore{entered}, s.Snapshot, []string{"source.go"}, s.Limits)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := newSourceBroker(ctx, s, r)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := b.Read(context.Background(), s.Snapshot.Head, "source.go"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled source call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("worker background context bypassed controller cancellation")
	}
	if _, err = b.Read(context.Background(), s.Snapshot.Head, "source.go"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled broker accepted another call", err)
	}
}

func TestLegacyReviewerHasNoInventedSourceReceipt(t *testing.T) {
	out, err := testEngine(t, func(_ context.Context, s Session) (Report, error) { return protocolReport(s), nil }).Run(context.Background(), testJob(), Checkpoint{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Artifacts {
		if a.SourceAccess != nil {
			t.Fatal("absence of worker access evidence reported as measured zero reads")
		}
	}
}

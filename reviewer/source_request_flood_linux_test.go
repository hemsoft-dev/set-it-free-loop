//go:build linux

package reviewer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The reader service seals a flooded connection's receipt as a valid stopped
// audit instead of failing it, so guest PID1 keeps it in its finished record.
func TestSourceSocketRequestFloodSealsTheAuditInsteadOfFailingIt(t *testing.T) {
	c := sourceServiceConfig(t, Codex)
	c.Session.Limits.ReaderCalls = 2
	reader, _ := brokerReader(c.Session.Snapshot)
	ready := make(chan SourceEndpoint, 1)
	done := make(chan sourceSocketResult, 1)
	go func() {
		a, err := serveSourceSocket(context.Background(), c, reader, func(endpoint SourceEndpoint) error { ready <- endpoint; return nil }, func(*net.UnixConn) (uint32, error) { return uint32(c.WorkerUID), nil })
		done <- sourceSocketResult{a, err}
	}()
	endpoint := sourceSocketReady(t, ready, done)
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: endpoint.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	f := &mcpFixture{client: connection, scan: bufio.NewScanner(connection), session: c.Session}
	f.initialize(t)
	floodSourceMCP(t, f, 3)
	var r sourceSocketResult
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not end on the request flood")
	}
	if r.err != nil || r.access == nil || r.access.validate(c.Session) != nil || !r.access.requestFlooded() || r.access.ToolCalls != 2 {
		t.Fatal("flooded reader audit failed or lost", r.err, r.access)
	}
	if _, err := os.Lstat(filepath.Dir(endpoint.Socket)); !os.IsNotExist(err) {
		t.Fatal("owned source endpoint not removed", err)
	}
}

// nativeHostAuditWith is the guest reader's stopped audit with the given
// broker coverage.
func nativeHostAuditWith(t *testing.T, q nativeVMRequest, coverage []Coverage) []byte {
	t.Helper()
	config := q.readerConfig()
	base := SourceReaderEvent{Version: 1, SessionHash: Digest(q.Session), ManifestHash: Digest(config), LaunchID: config.LaunchID}
	ready := base
	ready.Kind, ready.Socket, ready.ReaderUID = "ready", filepath.Join(config.Root, Digest(q.Session), "source.sock"), config.ReaderUID
	stopped := base
	stopped.Kind = "stopped"
	stopped.Access = &SourceAccess{Version: 1, SessionHash: Digest(q.Session), Reads: []ReadRecord{}, Coverage: coverage}
	var raw []byte
	for _, event := range []SourceReaderEvent{ready, stopped} {
		frame, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(append(raw, frame...), '\n')
	}
	return raw
}

// Guest PID1 keeps a flooded audit for the host and knows the phase cannot be
// complete. A failed audit is still dropped.
func TestNativeGuestKeepsAFloodedAuditAndRefusesCompletion(t *testing.T) {
	q := nativeRequestFixture(t, Codex)
	flooded := nativeHostAuditWith(t, q, []Coverage{{Reason: sourceRequestFloodReason}})
	if raw, flood := nativeGuestKeepAudit(q.readerConfig(), readerAuditResult{raw: flooded}); len(raw) == 0 || !flood {
		t.Fatal("flooded audit dropped or not recognised", len(raw), flood)
	}
	clean := nativeHostAuditFixture(t, q)
	if raw, flood := nativeGuestKeepAudit(q.readerConfig(), readerAuditResult{raw: clean}); len(raw) == 0 || flood {
		t.Fatal("clean audit misclassified", len(raw), flood)
	}
	if raw, _ := nativeGuestKeepAudit(q.readerConfig(), readerAuditResult{raw: clean, err: ErrSourceService}); raw != nil {
		t.Fatal("failed reader audit kept")
	}
}

// A complete guest record whose audit records a request flood is refused: the
// host proves stop, commits the returned auth and keeps the audit, and the
// phase fails typed as invalid output.
func TestNativeVMRequestFloodReturnsAuthAndAuditButNeverCompletes(t *testing.T) {
	p, store, _, info := nativeDrainedPhase(t, false)
	returned, err := NewAuthReturn(info, []byte("synthetic-returned-after-flood"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(fakeReport(p.request.Session))
	code := &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(p.request.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true, RelayComplete: true, Stage: "complete"}
	p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "complete", Report: raw, CodeHost: code, AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditWith(t, p.request, []Coverage{{Reason: sourceRequestFloodReason}})}
	if p.finished.failure(p.request) != nil {
		t.Fatal("fixture record is not a complete guest record")
	}
	report, err := decodeNativeVMTaskReport(p.request, raw)
	if err != nil {
		t.Fatal(err)
	}
	got, access, _, err := p.finishTask(context.Background(), report, nil)
	var pe PhaseError
	if !errors.As(err, &pe) || pe.State != "invalid_output" || pe.BeforeWork || !errors.Is(err, ErrSourceRequestFlood) || got.review.Findings != nil {
		t.Fatal("request flood accepted as a clean phase", err, got)
	}
	if errors.Is(err, ErrAuthRecoveryRequired) || errors.Is(err, ErrSourceAuditPending) || errors.Is(err, ErrWorkerStopPending) || access == nil || !access.requestFlooded() || !p.authReturned {
		t.Fatal("request flood lost its audit or auth return", err, access, p.authReturned)
	}
	state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if err != nil || string(state) != "synthetic-returned-after-flood" {
		t.Fatal("stopped return not committed", err)
	}
	if r := readNativeDrainReceipt(t, p); r.Complete || !r.AuthReturned || r.Failure != "source-flood" {
		t.Fatal("stopped receipt does not record the refused flooded session", r)
	}
}

// A real flood reaches the host as a guest-refused record: guest PID1 already
// downgraded the report, so the host has a typed failure before it judges the
// audit. The flood must still be the recorded cause, with the guest's class.
func TestNativeVMGuestRefusedRequestFloodKeepsItsCause(t *testing.T) {
	p, store, _, info := nativeDrainedPhase(t, false)
	returned, err := NewAuthReturn(info, []byte("synthetic-returned-after-refused-flood"))
	if err != nil {
		t.Fatal(err)
	}
	code := &nativeVMCodeHostAccess{Version: 2, SessionHash: Digest(p.request.Session), UID: nativeVMCodeHostUID, InputHash: Digest("input"), OutputHash: Digest("output"), Stopped: true, RelayComplete: true, Stage: "complete"}
	p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "invalid_output", CodeHost: code, AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditWith(t, p.request, []Coverage{{Reason: sourceRequestFloodReason}})}
	refused := p.finished.failure(p.request)
	var guest PhaseError
	if !errors.As(refused, &guest) || guest.State != "invalid_output" {
		t.Fatal("fixture record is not a guest-refused record", refused)
	}
	_, access, _, err := p.finishTask(context.Background(), nativeVMTaskReport{}, refused)
	var pe PhaseError
	if !errors.As(err, &pe) || pe.State != "invalid_output" || !errors.Is(err, ErrSourceRequestFlood) {
		t.Fatal("guest-refused flood lost its cause or class", err)
	}
	if errors.Is(err, ErrAuthRecoveryRequired) || access == nil || !access.requestFlooded() || !p.authReturned {
		t.Fatal("guest-refused flood lost its audit or auth return", err, access, p.authReturned)
	}
	state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if err != nil || string(state) != "synthetic-returned-after-refused-flood" {
		t.Fatal("stopped return not committed", err)
	}
	if r := readNativeDrainReceipt(t, p); r.Complete || !r.AuthReturned || r.Failure != "source-flood" {
		t.Fatal("stopped receipt does not record the flood", r)
	}
}

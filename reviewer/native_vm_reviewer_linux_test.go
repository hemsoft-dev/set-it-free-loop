//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeHostPhaseFixture(t *testing.T) (*nativeVMHostPhase, *fakeContainmentManager) {
	t.Helper()
	q := nativeRequestFixture(t, Codex)
	q.Binding.LaunchID = strings.Repeat("a", 32)
	s, manager, _ := systemdSupervisorFixture(t)
	s.payloadKind = "native-vm"
	supervisor := &NativeVMSupervisor{s}
	manager.observation.MemoryMax = 3 << 30
	manager.observation.TasksMax = 128
	manager.kernel.group.Path = "/user.slice/user-1000.slice/sfl-alpha-" + q.Binding.LaunchID + ".service"
	manager.observation.ControlGroup = manager.kernel.group.Path
	spec := vmSpec(t)
	spec.Binding = q.Binding
	images, err := openNativeBootTestImages(t, context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { images.Close() })
	manifest, err := images.NativeManifest(q.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Prepare(manifest); err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Start(context.Background(), q.Binding); err != nil {
		t.Fatal(err)
	}
	channels, err := openNativeVMChannels()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channels.Close() })
	reader, err := NewReader(&fixtureStore{files: map[string]Object{}}, q.Session.Snapshot, q.Paths, q.Session.Limits)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeVMHostPhase{request: q, manifest: manifest, supervisor: supervisor, channels: channels, reader: reader, started: true}, manager
}

func nativeHostAuditFixture(t *testing.T, q nativeVMRequest) []byte {
	t.Helper()
	config := q.readerConfig()
	base := SourceReaderEvent{Version: 1, SessionHash: Digest(q.Session), ManifestHash: Digest(config), LaunchID: config.LaunchID}
	ready := base
	ready.Kind = "ready"
	ready.Socket = filepath.Join(config.Root, Digest(q.Session), "source.sock")
	ready.ReaderUID = config.ReaderUID
	stopped := base
	stopped.Kind = "stopped"
	stopped.Access = &SourceAccess{Version: 1, SessionHash: Digest(q.Session), Reads: []ReadRecord{}, Coverage: []Coverage{}}
	var raw []byte
	for _, event := range []SourceReaderEvent{ready, stopped} {
		frame, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, frame...)
		raw = append(raw, '\n')
	}
	return raw
}

func TestNativeVMExportGateReopensExactGrantedPrivateBinding(t *testing.T) {
	p, manager := nativeHostPhaseFixture(t)
	ready := nativeVMReady{1, p.request.Binding, Digest(p.request), p.request.Session.Identity, "native-vm-profile-1"}
	g := nativeVMExportGate{p.supervisor, p.manifest, p.request, ready}
	if g.ConfirmAuthExport(context.Background(), p.request.Binding) != ErrAuthTransfer {
		t.Fatal("prepared gate vouched for private auth")
	}
	s := p.supervisor.supervisor
	j, err := s.snapshotUnit(context.Background(), p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	gate := s.gates[p.request.Binding]
	held, err := gate.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, err := gate.read(held)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = "granted"
	record.Identity = j.Identity
	if err = gate.write(held, record); err != nil {
		t.Fatal(err)
	}
	held.close()
	// Fake manager/journal evidence exercises the gate; it does not qualify an
	// actual VM. The eight-descriptor transport is independently tested on Unix.
	if err = g.ConfirmAuthExport(context.Background(), p.request.Binding); err != nil {
		t.Fatal(err)
	}
	bad := g
	bad.ready.RequestHash = Digest("different-request")
	if bad.ConfirmAuthExport(context.Background(), p.request.Binding) != ErrAuthTransfer {
		t.Fatal("foreign ready admitted auth")
	}
	manager.kernel.changed = true
	if g.ConfirmAuthExport(context.Background(), p.request.Binding) != ErrAuthTransfer {
		t.Fatal("changed containment admitted auth")
	}
	manager.kernel.changed = false
	if err = p.supervisor.AbortAndWait(context.Background(), p.request.Binding); err != nil {
		t.Fatal(err)
	}
	if g.ConfirmAuthExport(context.Background(), p.request.Binding) != ErrAuthTransfer {
		t.Fatal("stopped/aborted binding exported auth")
	}
}

func attachIdleNativeProviderBridge(t *testing.T, p *nativeVMHostPhase) {
	t.Helper()
	guest, host := vmSocketPair(t)
	t.Cleanup(func() { guest.Close(); host.Close() })
	var err error
	p.bridge, err = startNativeProviderBridge(context.Background(), p.request.Session, p.request.Policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("idle provider fixture reached upstream")
		return nil, ErrLiveBlocked
	}), host)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeVMInvalidModelOutputStillCommitsStoppedAuthReturn(t *testing.T) {
	p, manager := nativeHostPhaseFixture(t)
	store := authTestStore(t, Codex)
	profile, err := store.LeaseProfile(context.Background(), p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	p.profile = profile
	seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(context.Context, AuthBinding) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	p.exported = true
	info, _ := seed.Info()
	returned, err := NewAuthReturn(info, []byte("synthetic-refreshed-invalid-output"))
	if err != nil {
		t.Fatal(err)
	}
	wire := authWire(t, returned)
	p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "invalid_output", AuthReturn: wire, SourceAudit: nativeHostAuditFixture(t, p.request)}
	attachIdleNativeProviderBridge(t, p)
	report, _, _, err := p.finish(context.Background(), Report{}, PhaseError{State: "invalid_output"})
	if err == nil || len(report.Findings) != 0 || manager.stops != 1 {
		t.Fatal("invalid result became a review or skipped physical stop", err)
	}
	state, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
	if err != nil || string(state) != "synthetic-refreshed-invalid-output" {
		t.Fatal("stopped returned auth was lost or zeroed before commit", err)
	}
	if profile.held != nil {
		t.Fatal("profile lock not released after auth persistence")
	}
	if !bytes.Equal(wire, make([]byte, len(wire))) {
		t.Fatal("owned auth return wire not cleared after use")
	}
	audit, err := os.ReadFile(filepath.Join(p.supervisor.supervisor.root, p.request.Binding.LaunchID, "native-audit.json"))
	if err != nil || bytes.Contains(audit, []byte("synthetic-refreshed-invalid-output")) {
		t.Fatal("native receipt missing or contains credentials", err)
	}
}

func TestNativeVMUncertainRefreshCannotReleaseReturnedOldAuth(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lost, changed bool
	}{
		{"lost-response-old-file", true, false}, {"lost-response-changed-file", true, true},
		{"complete-response-old-file", false, false}, {"complete-response-changed-file", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lost, changed := tc.lost, tc.changed
			expectedResponses := 1
			if lost {
				expectedResponses = 0
			}

			p, _ := nativeHostPhaseFixture(t)
			store := authTestStore(t, Codex)
			original, err := os.ReadFile(filepath.Join(store.directory, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			profile, err := store.LeaseProfile(context.Background(), p.request.Binding)
			if err != nil {
				t.Fatal(err)
			}
			p.profile = profile
			defer profile.Close()
			seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(context.Context, AuthBinding) error { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			defer seed.Close()
			p.exported = true
			info, _ := seed.Info()
			payload := original
			if changed {
				payload = []byte("synthetic-refreshed-file")
			}
			returned, err := NewAuthReturn(info, payload)
			if err != nil {
				t.Fatal(err)
			}
			p.finished = &nativeVMFinished{Version: 1, Binding: p.request.Binding, RequestHash: Digest(p.request), State: "invalid_output", AuthReturn: authWire(t, returned), SourceAudit: nativeHostAuditFixture(t, p.request)}
			guest, host := vmSocketPair(t)
			policy := egressTestPolicy(Codex)
			policy.ResponsePolicy = CodexSourceResponses
			committed := make(chan struct{}, 1)
			p.bridge, err = startNativeProviderBridge(context.Background(), p.request.Session, policy, egressRoundTrip(func(*http.Request) (*http.Response, error) {
				committed <- struct{}{}
				if lost {
					return nil, errors.New("synthetic response lost after possible rotation")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-rotated"}`))}, nil
			}), host)
			if err != nil {
				t.Fatal(err)
			}
			transport := nativeProviderTransport(guest)
			defer transport.CloseIdleConnections()
			q, _ := http.NewRequest("POST", "http://sfl-private.invalid/refresh", strings.NewReader(`{"refresh_token":"synthetic"}`))
			q.Host = "auth.provider.example"
			q.Header.Set("Content-Type", "application/json")
			response, err := transport.RoundTrip(q)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			select {
			case <-committed:
			default:
				t.Fatal("fault did not reach trusted upstream transport")
			}
			_, _, evidence, err := p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"})
			if evidence == nil || evidence.Access.RefreshRequests != 1 || evidence.Access.Forwarded != 1 || evidence.Access.Responses != expectedResponses {
				t.Fatal("uncertain refresh receipt lost", evidence)
			}
			quarantined := lost || !changed
			if errors.Is(err, ErrAuthRecoveryRequired) != quarantined || p.authReturned == quarantined {
				t.Fatal("refresh return recovery decision incorrect", err)
			}
			profile.Close()
			called := false
			recovery := store.Refresh(context.Background(), func(value []byte) ([]byte, error) {
				called = true
				if !bytes.Equal(value, payload) {
					t.Fatal("returned bytes not persisted")
				}
				return value, nil
			})
			if errors.Is(recovery, ErrAuthRecoveryRequired) != quarantined || called == quarantined {
				t.Fatal("new work reused uncertain seed or lost safe return", recovery)
			}

		})
	}
}

func TestNativeVMMissingReturnNeverRecommitsAnExportedSeed(t *testing.T) {
	p, _ := nativeHostPhaseFixture(t)
	store := authTestStore(t, Codex)
	profile, err := store.LeaseProfile(context.Background(), p.request.Binding)
	if err != nil {
		t.Fatal(err)
	}
	p.profile = profile
	seed, err := profile.ExportAuth(context.Background(), authExportCheck(func(context.Context, AuthBinding) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	seed.Close()
	p.exported = true
	_, _, _, err = p.finish(context.Background(), Report{}, PhaseError{State: "provider_unavailable"})
	if !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("lost returned credentials did not require recovery", err)
	}
	if _, err = os.Stat(filepath.Join(store.directory, "lease.json")); err != nil {
		t.Fatal("exported unknown state lost durable auth journal", err)
	}
	if _, err = store.LeaseProfile(context.Background(), p.request.Binding); !errors.Is(err, ErrAuthRecoveryRequired) {
		t.Fatal("next phase reused possibly refreshed seed", err)
	}
}

func TestNativeVMReviewerChecksAuthorityBeforeAnyFilesystemOrTransport(t *testing.T) {
	s := testSession(Codex)
	reader, err := NewReader(&fixtureStore{files: map[string]Object{}}, s.Snapshot, []string{"source.go"}, s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	policy := egressTestPolicy(Codex)
	policy.ResponsePolicy = CodexSourceResponses
	checks, calls := 0, 0
	o := NativeVMOptions{Provider: Codex, Root: "/unopened/native-root", Stub: "/unopened/native-stub", StubHash: strings.Repeat("a", 64), Auth: &AuthStore{provider: Codex, directory: "/unopened/auth"}, Reader: reader, Policy: policy, Transport: egressRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, ErrLiveBlocked }), Check: func(context.Context, Session) error { checks++; return ErrLiveBlocked }}
	r, err := NewNativeVMReviewer(o)
	if err != nil {
		t.Fatal("constructor performed filesystem access", err)
	}
	if _, _, _, err = r.ReviewAudited(context.Background(), s); !errors.Is(err, ErrLiveBlocked) || checks != 1 || calls != 0 {
		t.Fatal("closed gate admitted runtime work", checks, calls, err)
	}
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{})
	if _, _, _, err = r.ReviewAudited(ctx, s); !errors.Is(err, ErrLiveBlocked) || checks != 1 || calls != 0 {
		t.Fatal("caller trace reached qualification or credentials", checks, calls, err)
	}
	r.options.Check = func(ctx context.Context, _ Session) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > s.Limits.Timeout {
			t.Error("activation gate did not receive the captured phase deadline")
		}
		return ErrLiveBlocked
	}
	_, _, _, _ = r.ReviewAudited(context.Background(), s)
	o.Check = nil
	if _, err = NewNativeVMReviewer(o); err != ErrLiveBlocked {
		t.Fatal("missing gate defaulted to live")
	}
}

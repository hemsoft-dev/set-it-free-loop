//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

func TestCancellationWriterWaitsBeforeResponseHeadersAndDoesNotReplay(t *testing.T) {
	point := newCancellationPoint()
	writerDone := make(chan error, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		writer := &cancellationWriter{ResponseWriter: w, ctx: q.Context(), point: point}
		if q.Header.Get("Authorization") != "Bearer synthetic-cancellation" {
			http.Error(writer, "denied", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if n, err := writer.Write([]byte("synthetic response")); n != 0 || !errors.Is(err, context.Canceled) {
			writerDone <- errors.New("cancelled response was forwarded")
			return
		}
		if n, err := writer.Write([]byte("producer continued after first failed write")); n != 0 || !errors.Is(err, context.Canceled) {
			writerDone <- errors.New("second write lost cancellation")
			return
		}
		writerDone <- nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer func() { cancel(); server.Close() }()
	client := server.Client()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusUnauthorized || string(raw) != "denied\n" || point.calls.Load() != 0 {
		t.Fatal("producer rejection was mistaken for inference", err)
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	request.Header.Set("Authorization", "Bearer synthetic-cancellation")
	done := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-point.entered:
	case <-time.After(time.Second):
		t.Fatal("response fault did not start")
	}
	select {
	case err := <-done:
		t.Fatal("response headers escaped before cancellation", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("client cancellation was lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled client did not return")
	}
	select {
	case err := <-writerDone:
		if err != nil || point.calls.Load() != 1 {
			t.Fatal("fault replayed or forwarded bytes", err, point.calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("response producer did not stop")
	}
}

func TestCancellationStoreOnlyBlocksValidImmutableReadAndReturnsNoBytes(t *testing.T) {
	point := newCancellationPoint()
	store := cancellationStore{point}
	if _, err := store.Read(context.Background(), strings.Repeat("b", 40), "source.go", 4096); err == nil || point.calls.Load() != 0 {
		t.Fatal("foreign revision entered cancellation fixture")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		value, err := store.Read(ctx, strings.Repeat("a", 40), "source.go", 4096)
		if len(value.Data) != 0 || value.Mode != "" {
			done <- errors.New("cancelled immutable object escaped")
			return
		}
		done <- err
	}()
	select {
	case <-point.entered:
	case <-time.After(time.Second):
		t.Fatal("valid read did not enter fixture")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || point.calls.Load() != 1 {
			t.Fatal("read cancellation lost or replayed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled read did not return")
	}
	select {
	case <-point.returned:
	default:
		t.Fatal("read completion not retained")
	}
}

func TestCancellationReceiptsRejectForeignPrivateEvidence(t *testing.T) {
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	session, err := r.NewSession("cancel-binding", snapshot, r.Digest("packet"), r.Codex, 1, "packet", nil, nil, r.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, policy, err := f.New(session, false)
	if err != nil {
		t.Fatal(err)
	}
	launch := strings.Repeat("d", 32)
	hash := r.Digest(session)
	binding := r.AuthBinding{SessionHash: hash, LaunchID: launch, ContainmentID: r.Digest(struct{ Session, Launch string }{hash, launch})}
	evidence := r.ProviderEvidence{Policy: policy, Access: r.ProviderAccess{Version: 1, SessionHash: hash, PolicyHash: r.Digest(policy), Stopped: true}}
	unit := cancellationUnitReceipt{Captured: true, Stopped: true, Binding: binding}
	audit := cancellationAuditReceipt{AuthExported: true, Binding: binding, Provider: &evidence}
	if !cancellationReceiptsBound(session, policy, launch, unit, audit, &evidence) {
		t.Fatal("valid incomplete stopped evidence rejected")
	}
	mutations := map[string]func(*cancellationUnitReceipt, *cancellationAuditReceipt, *r.ProviderEvidence){
		"unit session": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			u.Binding.SessionHash = r.Digest("foreign")
		},
		"launch": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			u.Binding.LaunchID = strings.Repeat("e", 32)
		},
		"containment": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			u.Binding.ContainmentID = r.Digest("foreign")
		},
		"audit launch": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			a.Binding.LaunchID = strings.Repeat("e", 32)
		},
		"provider": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			e.Policy.Provider = r.Claude
		},
		"provider session": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			e.Access.SessionHash = r.Digest("foreign")
		},
		"policy binding": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			e.Access.PolicyHash = r.Digest("foreign")
		},
		"captured policy": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			e.Policy.Headers = append([]string{}, "foreign")
		},
		"unstopped": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			e.Access.Stopped = false
		},
		"missing private provider": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) { a.Provider = nil },
		"completed audit": func(u *cancellationUnitReceipt, a *cancellationAuditReceipt, e *r.ProviderEvidence) {
			a.Complete = true
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			u, a, e := unit, audit, evidence
			mutate(&u, &a, &e)
			if cancellationReceiptsBound(session, policy, launch, u, a, &e) {
				t.Fatal("foreign or incomplete private evidence accepted")
			}
		})
	}
}

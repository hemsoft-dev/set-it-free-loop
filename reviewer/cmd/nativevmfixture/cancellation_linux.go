//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

// The fault is attached only to this fake runner's loopback response or immutable
// store. It changes no native guest protocol and accepts no profile or endpoint.
type cancellationPoint struct {
	entered, returned chan struct{}
	enter, leave      sync.Once
	calls             atomic.Int32
}

func newCancellationPoint() *cancellationPoint {
	return &cancellationPoint{entered: make(chan struct{}), returned: make(chan struct{})}
}

func (p *cancellationPoint) wait(ctx context.Context) error {
	p.calls.Add(1)
	p.enter.Do(func() { close(p.entered) })
	defer p.leave.Do(func() { close(p.returned) })
	<-ctx.Done()
	return ctx.Err()
}

type cancellationWriter struct {
	http.ResponseWriter
	point  *cancellationPoint
	ctx    context.Context
	status int
	once   sync.Once
	err    error
}

func (w *cancellationWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status != http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *cancellationWriter) Write(raw []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status == http.StatusOK && w.Header().Get("Content-Type") == "text/event-stream" {
		// The real fake producer validates auth, model and tool shape before
		// writing SSE. Do not emit headers or body before the cancellation.
		w.once.Do(func() { w.err = w.point.wait(w.ctx) })
		if w.err != nil {
			return 0, w.err
		}
	}
	return w.ResponseWriter.Write(raw)
}

type cancellationStore struct{ point *cancellationPoint }

func (s cancellationStore) Read(ctx context.Context, revision, path string, limit int) (r.Object, error) {
	value, err := (f.Store{}).Read(ctx, revision, path, limit)
	if err != nil {
		return r.Object{}, err
	}
	clear(value.Data)
	return r.Object{}, s.point.wait(ctx)
}

func authDisposition(store *r.AuthStore) (pending bool, hash string, valid bool) {
	err := store.Refresh(context.Background(), func(current []byte) ([]byte, error) {
		if len(current) == 0 {
			return nil, errors.New("missing synthetic auth")
		}
		sum := sha256.Sum256(current)
		hash = hex.EncodeToString(sum[:])
		return current, nil
	})
	return errors.Is(err, r.ErrAuthRecoveryRequired), hash, err == nil || errors.Is(err, r.ErrAuthRecoveryRequired)
}

func runNativeCancellation(c config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, dir := range []string{"auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, dir), 0700) != nil {
			return errors.New("fresh cancellation directories required")
		}
	}
	authRoot := filepath.Join(c.Root, "auth")
	store, err := r.NewAuthStore(c.Provider, authRoot)
	if err != nil {
		return err
	}
	seed := f.Seed(c.Provider)
	if c.Cancellation == "refresh" {
		clear(seed)
		seed = f.ExpiredSeed(c.Provider)
	}
	err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing synthetic seed denied")
		}
		return seed, nil
	})
	clear(seed)
	if err != nil {
		return err
	}
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("native-fake-cancel-"+c.Cancellation, snapshot, r.Digest("synthetic-native-cancellation"), c.Provider, 1, "immutable source evidence", nil, nil, limits)
	if err != nil {
		return err
	}
	backend, policy, err := f.New(s, false)
	if c.Cancellation == "refresh" {
		backend, policy, err = f.NewRefresh(s, false)
	}
	if err != nil {
		return err
	}
	point := newCancellationPoint()
	var objects r.ObjectStore = f.Store{}
	if c.Cancellation == "reader" {
		objects = cancellationStore{point}
	}
	reader, err := r.NewReader(objects, snapshot, []string{"source.go"}, limits)
	if err != nil {
		return err
	}
	var handler http.Handler = backend
	if c.Cancellation != "reader" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			for _, route := range policy.Routes {
				if route.Kind == "inference" && q.Method == route.Method && q.URL.Path == route.Path && q.URL.RawQuery == route.RawQuery {
					backend.ServeHTTP(&cancellationWriter{ResponseWriter: w, point: point, ctx: q.Context()}, q)
					return
				}
			}
			backend.ServeHTTP(w, q)
		})
	}
	transport, closeBackend := protocolUpstream(handler, c.Provider)
	defer closeBackend()
	hints := r.NativeVMProfileHints{}
	if c.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: c.Provider, Root: filepath.Join(c.Root, "launches"), Stub: c.Stub.Path, StubHash: c.Stub.SHA256, QEMU: c.QEMU, Kernel: c.Kernel, Initrd: c.Initrd, Firmware: c.Firmware, Runtime: c.Runtime, Auth: store, Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != r.Digest(s) {
			return errors.New("foreign cancellation session")
		}
		return nil
	}})
	if err != nil {
		return err
	}
	trigger := make(chan time.Time, 1)
	go func() {
		defer close(trigger)
		select {
		case <-point.entered:
			trigger <- time.Now()
			cancel()
		case <-ctx.Done():
		}
	}()
	start := time.Now()
	report, _, evidence, reviewErr := worker.ReviewAudited(ctx, s)
	finish := time.Now()
	cancel()
	cancelledAt, triggered := <-trigger
	hookReturned := false
	select {
	case <-point.returned:
		hookReturned = true
	case <-time.After(2 * time.Second):
	}
	counts := backend.Counts()
	pending, hash, authValid := authDisposition(store)
	reopened, openErr := r.NewAuthStore(c.Provider, authRoot)
	consistent := false
	if openErr == nil {
		otherPending, otherHash, valid := authDisposition(reopened)
		consistent = valid && pending == otherPending && hash == otherHash
	}
	paths, globErr := filepath.Glob(filepath.Join(c.Root, "launches", "*", "unit.json"))
	var unit struct{ Captured, Stopped bool }
	var audit struct{ Complete, AuthExported, AuthReturned bool }
	unitValid := globErr == nil && len(paths) == 1 && readProtocolJSON(paths[0], &unit) == nil && readProtocolJSON(filepath.Join(filepath.Dir(paths[0]), "native-audit.json"), &audit) == nil
	roots, rootErr := filepath.Glob(filepath.Join(c.Root, "launches", "*", "qemu-runtime"))
	cancellationMS := int64(-1)
	if triggered {
		cancellationMS = finish.Sub(cancelledAt).Milliseconds()
	}
	passed := triggered && hookReturned && point.calls.Load() == 1 && errors.Is(reviewErr, context.Canceled) && r.Digest(report) == r.Digest(r.Report{}) && cancellationMS >= 0 && cancellationMS < 20000 && evidence != nil && evidence.Access.Stopped && evidence.Access.InferenceRequests == 1 && counts.Inference == 1 && counts.Failures == 0 && counts.Telemetry == 0 && authValid && consistent && pending == errors.Is(reviewErr, r.ErrAuthRecoveryRequired) && unitValid && unit.Captured && unit.Stopped && audit.AuthExported && !audit.Complete && audit.AuthReturned != pending && rootErr == nil && len(roots) == 0
	if c.Cancellation == "refresh" {
		passed = passed && pending && errors.Is(reviewErr, r.ErrAuthRecoveryRequired) && counts.Refreshes == 1 && counts.RefreshFailures == 0 && evidence != nil && evidence.Access.RefreshRequests == 1
	} else {
		passed = passed && counts.Refreshes == 0
	}
	result := struct {
		Version                                                                               int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead, Cancellation            string
		Provider                                                                              r.Provider
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified, RuntimeRootsRemoved bool
		Triggered, HookReturned, AuthReturned, AuthRecoveryRequired, ReopenedAuthConsistent   bool
		CancellationMS, ElapsedMS                                                             int64
		Counts                                                                                f.Counts
		Evidence                                                                              *r.ProviderEvidence
	}{1, "native-fake-cancellation", sourceHead, sourceTree, c.ImageSourceHead, c.Cancellation, c.Provider, passed, true, false, false, rootErr == nil && len(roots) == 0, triggered, hookReturned, audit.AuthReturned, pending, consistent, cancellationMS, time.Since(start).Milliseconds(), counts, evidence}
	raw, _ := json.MarshalIndent(result, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(raw, '\n'), 0600) != nil {
		return errors.New("cancellation receipt failed")
	}
	if !passed {
		return errors.New("native cancellation qualification incomplete")
	}
	return nil
}

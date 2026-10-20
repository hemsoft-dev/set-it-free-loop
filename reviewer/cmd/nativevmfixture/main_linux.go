//go:build linux

// nativevmfixture opts into an ephemeral fake-only native VM qualification.
// Every auth byte is freshly generated here; no profile path is configurable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

var sourceHead, sourceTree string

type config struct {
	Version                        int
	Purpose, ImageSourceHead, Root string
	Provider                       r.Provider
	Negative                       bool
	ProtocolRounds                 int
	QEMU, Kernel, Initrd, Stub     r.VMBootAsset
}
type localOnly struct {
	target *url.URL
	base   *http.Transport
}

func (t localOnly) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Scheme != "https" || q.URL.Host != "fixture.provider.example" {
		return nil, errors.New("fake-only destination denied")
	}
	copy := q.Clone(q.Context())
	u := *q.URL
	u.Host = t.target.Host
	copy.URL = &u
	return t.base.RoundTrip(copy)
}

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--run-fake-only" || os.Getenv("SFL_NATIVE_VM_FAKE_ONLY") != "1" || len(sourceHead) != 40 || len(sourceTree) != 40 {
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[2])
	if err != nil || len(raw) > 16384 {
		os.Exit(2)
	}
	c, err := r.DecodeStrict[config](raw, 16384)
	if err != nil || c.Version != 1 || c.Purpose != "native-fake-only" || !c.validCase() || !filepath.IsAbs(c.Root) || len(c.ImageSourceHead) != 40 {
		os.Exit(2)
	}
	if err = run(c); err != nil {
		fmt.Fprintln(os.Stderr, "fake-only native qualification failed")
		os.Exit(1)
	}
}

func run(c config) error {
	// Existing directories are never adopted, so stale or interactive auth
	// cannot become a seed. All subsequent state lives in this new private root.
	if os.Mkdir(c.Root, 0700) != nil {
		return errors.New("fresh fake root required")
	}
	if c.ProtocolRounds != 0 {
		return runNativeProtocol(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	s, err := r.NewSession("native-vm-fake-only", snapshot, r.Digest("synthetic-native-only"), c.Provider, 1, "immutable source evidence", nil, nil, limits)
	if err != nil {
		return err
	}
	reader, err := r.NewReader(f.Store{}, snapshot, []string{"source.go"}, limits)
	if err != nil {
		return err
	}
	backend, policy, err := f.New(s, c.Negative)
	if err != nil {
		return err
	}
	upstream := httptest.NewTLSServer(backend)
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	base := upstream.Client().Transport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ForceAttemptHTTP2 = false
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target.Host {
			return nil, errors.New("non-loopback fake dial denied")
		}
		return dialer.DialContext(ctx, network, address)
	}
	defer base.CloseIdleConnections()
	authRoot := filepath.Join(c.Root, "auth")
	if os.Mkdir(authRoot, 0700) != nil {
		return errors.New("fake auth root failed")
	}
	auth, err := r.NewAuthStore(c.Provider, authRoot)
	if err != nil {
		return err
	}
	seed := f.Seed(c.Provider)
	defer clear(seed)
	if auth.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing seed denied")
		}
		return seed, nil
	}) != nil {
		return errors.New("fake seed failed")
	}
	hints := r.NativeVMProfileHints{}
	if c.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	if os.Mkdir(filepath.Join(c.Root, "launches"), 0700) != nil {
		return errors.New("fake launch root failed")
	}
	worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: c.Provider, Root: filepath.Join(c.Root, "launches"), Stub: c.Stub.Path, StubHash: c.Stub.SHA256, QEMU: c.QEMU, Kernel: c.Kernel, Initrd: c.Initrd, Auth: auth, Reader: reader, Policy: policy, Transport: localOnly{target, base}, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != r.Digest(s) {
			return errors.New("foreign fake session")
		}
		return nil
	}})
	if err != nil {
		return err
	}
	start := time.Now()
	report, access, evidence, reviewErr := worker.ReviewAudited(ctx, s)
	counts := backend.Counts()
	passed := reviewErr == nil && access != nil && access.ToolCalls == 2 && len(access.Reads) == 2 && len(report.Findings) == 1 && evidence != nil && counts.Inference == 3 && counts.Failures == 0 && counts.Telemetry == 0
	if c.Provider == r.Codex && (!counts.MetadataCaptured || counts.Negotiation != 0) {
		// Negotiation is handled locally by ProviderEgress and must never reach
		// the fake upstream. Every mandatory metadata route is still captured.
		passed = false
	}
	if c.Negative {
		passed = reviewErr != nil && len(report.Findings) == 0 && evidence != nil && evidence.Access.Failed && counts.Inference == 1 && counts.Failures == 0 && counts.Telemetry == 0 && !errors.Is(reviewErr, r.ErrWorkerStopPending) && !errors.Is(reviewErr, r.ErrAuthRecoveryRequired)
	}
	// AuthStore refuses Refresh while any lease/recovery remains. This local
	// check neither calls a provider nor launches a client, and never logs bytes.
	returned := false
	if err = auth.Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) { returned = len(current) > 0; return current, nil }); err != nil || !returned {
		passed = false
	}
	state := "complete"
	if reviewErr != nil {
		state = "failed"
	}
	receipt := struct {
		Version                                                                                       int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead                                  string
		Provider                                                                                      r.Provider
		Negative, Passed, AuthRecoveryClear, FakeOnly, RuntimeQualified, ProviderEligibilityQualified bool
		State                                                                                         string
		ElapsedMS                                                                                     int64
		Counts                                                                                        f.Counts
		Source                                                                                        *r.SourceAccess
		ProviderEvidence                                                                              *r.ProviderEvidence
	}{1, "native-fake-only", sourceHead, sourceTree, c.ImageSourceHead, c.Provider, c.Negative, passed, returned, true, false, false, state, time.Since(start).Milliseconds(), counts, access, evidence}
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(data, '\n'), 0600) != nil {
		return errors.New("fake receipt failed")
	}
	fmt.Println(string(data))
	if !passed {
		return errors.New("fake acceptance incomplete")
	}
	return nil
}

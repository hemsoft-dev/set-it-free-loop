//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

func (c config) validCase() bool {
	if c.ProtocolRounds == 0 {
		return c.Provider.Valid()
	}
	return c.Provider == "" && !c.Negative && (c.ProtocolRounds == 1 || c.ProtocolRounds == 2)
}

type protocolPhase struct {
	Provider                          r.Provider
	Round                             int
	InputHash, PacketHash, ReportHash string
	PeerReportHashes                  []string
	Counts                            f.Counts
	ToolCalls, ReaderCalls            int
	Passed, AuthRecoveryClear         bool
}

type nativeProtocolFixture struct {
	config config
	job    r.Job
	auth   map[r.Provider]*r.AuthStore
	mu     sync.Mutex
	phases []protocolPhase
}

type protocolReviewer struct {
	fixture  *nativeProtocolFixture
	provider r.Provider
}

func (w protocolReviewer) Review(ctx context.Context, s r.Session) (r.Report, error) {
	report, _, _, err := w.ReviewAudited(ctx, s)
	return report, err
}

func (w protocolReviewer) ReviewAudited(ctx context.Context, s r.Session) (r.Report, *r.SourceAccess, *r.ProviderEvidence, error) {
	p := w.fixture
	if p == nil || !p.mu.TryLock() {
		return r.Report{}, nil, nil, r.PhaseError{State: "blocked", BeforeWork: true}
	}
	defer p.mu.Unlock()
	index := len(p.phases)
	if s.Validate() != nil || s.Identity.Provider != w.provider || index >= p.job.Config.MaxSessions() || w.provider != p.job.Config.Providers()[index%2] || s.Round != index/2+1 || s.JobID != p.job.ID || s.Snapshot != p.job.Snapshot || s.ConfigHash != p.job.Config.Hash() || s.PacketHash != r.Digest(p.job.Packet) {
		return r.Report{}, nil, nil, r.PhaseError{State: "blocked", BeforeWork: true}
	}
	for i, peer := range s.PeerReports {
		if i >= len(p.phases) || r.Digest(peer) != p.phases[i].ReportHash {
			return r.Report{}, nil, nil, r.PhaseError{State: "blocked", BeforeWork: true}
		}
	}
	phase := protocolPhase{Provider: w.provider, Round: s.Round, InputHash: r.Digest(s), PacketHash: s.PacketHash, PeerReportHashes: []string{}}
	for _, peer := range s.PeerReports {
		phase.PeerReportHashes = append(phase.PeerReportHashes, r.Digest(peer))
	}
	defer func() { p.phases = append(p.phases, phase) }()
	backend, policy, err := f.New(s, false)
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	transport, closeUpstream := protocolUpstream(backend)
	defer closeUpstream()
	reader, err := r.NewReader(f.Store{}, s.Snapshot, []string{"source.go"}, s.Limits)
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	hints := r.NativeVMProfileHints{}
	if w.provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: w.provider, Root: filepath.Join(p.config.Root, "launches"), Stub: p.config.Stub.Path, StubHash: p.config.Stub.SHA256, QEMU: p.config.QEMU, Kernel: p.config.Kernel, Initrd: p.config.Initrd, Auth: p.auth[w.provider], Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != phase.InputHash {
			return errors.New("foreign fake protocol session")
		}
		return nil
	}})
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	report, access, evidence, reviewErr := worker.ReviewAudited(ctx, s)
	phase.Counts = backend.Counts()
	phase.ReportHash = r.Digest(report)
	if access != nil {
		phase.ToolCalls, phase.ReaderCalls = access.ToolCalls, access.ReaderCalls
	}
	phase.AuthRecoveryClear = p.auth[w.provider].Refresh(context.WithoutCancel(ctx), func(current []byte) ([]byte, error) {
		if len(current) == 0 {
			return nil, errors.New("missing returned fake auth")
		}
		return current, nil
	}) == nil
	phase.Passed = reviewErr == nil && phase.AuthRecoveryClear && evidence != nil && evidence.Access.Stopped && phase.ToolCalls == 2 && phase.ReaderCalls == 2 && phase.Counts.Inference == 3 && phase.Counts.Failures == 0 && phase.Counts.Telemetry == 0
	if w.provider == r.Codex && (!phase.Counts.MetadataCaptured || phase.Counts.Negotiation != 0) {
		phase.Passed = false
	}
	if reviewErr == nil && !phase.Passed {
		reviewErr = errors.New("native protocol phase acceptance failed")
	}
	return report, access, evidence, reviewErr
}

func protocolUpstream(backend http.Handler) (localOnly, func()) {
	server := httptest.NewTLSServer(backend)
	target, _ := url.Parse(server.URL)
	base := server.Client().Transport.(*http.Transport).Clone()
	base.Proxy, base.ForceAttemptHTTP2 = nil, false
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target.Host {
			return nil, errors.New("non-loopback protocol dial denied")
		}
		return dialer.DialContext(ctx, network, address)
	}
	return localOnly{target, base}, func() { base.CloseIdleConnections(); server.Close() }
}

type protocolUnit struct {
	InputHash              string
	Captured, Stopped      bool
	Complete, AuthReturned bool
}

func readProtocolJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("private protocol receipt exceeds bound")
	}
	return json.Unmarshal(raw, target)
}

func (p *nativeProtocolFixture) units() ([]protocolUnit, bool) {
	paths, err := filepath.Glob(filepath.Join(p.config.Root, "launches", "*", "unit.json"))
	units := []protocolUnit{}
	seen := map[string]bool{}
	for _, path := range paths {
		var unit struct {
			Captured, Stopped bool
			Binding           r.AuthBinding
		}
		var audit struct{ Complete, AuthExported, AuthReturned bool }
		if readProtocolJSON(path, &unit) != nil || readProtocolJSON(filepath.Join(filepath.Dir(path), "native-audit.json"), &audit) != nil || seen[unit.Binding.SessionHash] {
			return units, false
		}
		seen[unit.Binding.SessionHash] = true
		units = append(units, protocolUnit{unit.Binding.SessionHash, unit.Captured, unit.Stopped, audit.Complete, audit.AuthExported && audit.AuthReturned})
	}
	if err != nil || len(units) != len(p.phases) {
		return units, false
	}
	for _, phase := range p.phases {
		if !seen[phase.InputHash] {
			return units, false
		}
	}
	for _, unit := range units {
		if !unit.Captured || !unit.Stopped || !unit.Complete || !unit.AuthReturned {
			return units, false
		}
	}
	return units, true
}

func runNativeProtocol(c config) error {
	configuration := r.DefaultConfig()
	configuration.PeerRounds = c.ProtocolRounds
	configuration.Limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	job := r.Job{ID: "native-vm-fake-protocol", RequesterID: r.OwnerID, Generation: 1, Snapshot: snapshot, Config: configuration, ConfigVersion: 1, Release: sourceHead, Packet: "immutable source evidence"}
	if job.Validate() != nil {
		return errors.New("invalid fake protocol job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(configuration.MaxSessions())*100*time.Second)
	defer cancel()
	p := &nativeProtocolFixture{config: c, job: job, auth: map[r.Provider]*r.AuthStore{}, phases: []protocolPhase{}}
	for _, name := range []string{"auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, name), 0700) != nil {
			return errors.New("fresh protocol directory failed")
		}
	}
	for _, provider := range configuration.Providers() {
		store, err := r.NewAuthStore(provider, filepath.Join(c.Root, "auth"))
		if err != nil {
			return err
		}
		seed := f.Seed(provider)
		err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
			if len(old) != 0 {
				return nil, errors.New("existing protocol seed denied")
			}
			return seed, nil
		})
		clear(seed)
		if err != nil {
			return err
		}
		p.auth[provider] = store
	}
	engine := r.Engine{Reviewers: map[r.Provider]r.Reviewer{r.Codex: protocolReviewer{p, r.Codex}, r.Claude: protocolReviewer{p, r.Claude}}, Reader: func(snap r.Snapshot) (*r.Reader, error) {
		return r.NewReader(f.Store{}, snap, []string{"source.go"}, configuration.Limits)
	}}
	start := time.Now()
	outcome, runErr := engine.Run(ctx, job, r.Checkpoint{})
	passed := runErr == nil && outcome.State == "complete" && outcome.ExecutedSessions == configuration.MaxSessions() && outcome.ConsumedSessions == configuration.MaxSessions() && len(outcome.Artifacts) == configuration.MaxSessions() && len(outcome.Ledger) == 2 && len(p.phases) == configuration.MaxSessions()
	for _, phase := range p.phases {
		passed = passed && phase.Passed
	}
	// A fully retained checkpoint must validate without launching another VM.
	before := len(p.phases)
	resumedWithoutWork := false
	if runErr == nil && outcome.State == "complete" {
		resumed, resumeErr := engine.Run(ctx, job, outcome.Checkpoint())
		resumedWithoutWork = resumeErr == nil && resumed.State == "complete" && resumed.ExecutedSessions == 0 && resumed.ConsumedSessions == configuration.MaxSessions() && len(p.phases) == before && r.Digest(resumed.Ledger) == r.Digest(outcome.Ledger)
	}
	units, stopped := p.units()
	passed = passed && resumedWithoutWork && stopped
	receipt := struct {
		Version                                                          int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead     string
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified bool
		PeerRounds, NativeSessions, ConsumedSessions                     int
		State, LedgerHash                                                string
		ElapsedMS                                                        int64
		ResumedWithoutWork                                               bool
		Phases                                                           []protocolPhase
		Units                                                            []protocolUnit
	}{1, "native-fake-protocol", sourceHead, sourceTree, c.ImageSourceHead, passed, true, false, false, c.ProtocolRounds, len(p.phases), outcome.ConsumedSessions, outcome.State, r.Digest(outcome.Ledger), time.Since(start).Milliseconds(), resumedWithoutWork, p.phases, units}
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(data, '\n'), 0600) != nil {
		return errors.New("fake protocol receipt failed")
	}
	fmt.Println(string(data))
	if !passed {
		return errors.New("native protocol acceptance incomplete")
	}
	return nil
}

//go:build linux

// evaluationfixture is an explicit model-free native execution qualification.
// All source and auth are fresh synthetic data. No real profile is accepted.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

var sourceHead, sourceTree string

type config struct {
	Version                                   int
	Purpose, Root, ImageSourceHead            string
	QEMU, Kernel, Initrd, Stub, Firmware, Git r.VMBootAsset
	Runtime                                   r.NativeVMRuntimeSpec
	StoppedFaultArm                           r.EvaluationArm
}
type fixture struct {
	config  config
	journal *r.EvaluationJournal
	source  r.GitStore
	packets map[string]string
	auth    map[r.Provider]*r.AuthStore
	calls   int
	phases  []phase
}
type phase struct {
	CasePR      int
	Arm         r.EvaluationArm
	Provider    r.Provider
	Step        int
	InputHash   string
	Counts      f.Counts
	SourceCalls int
	Passed      bool
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
	c, err := readConfig(os.Args, os.Getenv("SFL_EVALUATION_NATIVE_FAKE_ONLY"), sourceHead, sourceTree)
	if err != nil {
		os.Exit(2)
	}
	if run(c) != nil {
		fmt.Fprintln(os.Stderr, "fake-only evaluation qualification failed")
		os.Exit(1)
	}
}
func commitID(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 20 && strings.ToLower(value) == value
}
func readConfig(args []string, enabled, head, tree string) (config, error) {
	if len(args) != 3 || args[1] != "--run-fake-only" || enabled != "1" || !commitID(head) || !commitID(tree) {
		return config{}, r.ErrLiveBlocked
	}
	file, err := os.Open(args[2])
	if err != nil {
		return config{}, r.ErrLiveBlocked
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(raw) > 16384 {
		return config{}, r.ErrLiveBlocked
	}
	c, err := r.DecodeStrict[config](raw, 16384)
	if err != nil || c.Version != 1 || c.Purpose != "six-arm-native-fake" || !filepath.IsAbs(c.Root) || !commitID(c.ImageSourceHead) || !validStoppedFaultArm(c.StoppedFaultArm) {
		return config{}, r.ErrLiveBlocked
	}
	return c, nil
}
func gitCommand(c config, directory string, args ...string) (string, error) {
	// Only fixed fixture setup/plumbing calls are supplied by this program.
	command := exec.Command(c.Git.Path, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
	command.Dir = directory
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	data, err := command.Output()
	if err != nil || len(data) > 4096 {
		return "", errors.New("synthetic Git setup failed")
	}
	return strings.TrimSpace(string(data)), nil
}
func syntheticSource(c config) (r.GitStore, r.Snapshot, error) {
	raw, err := os.ReadFile(c.Git.Path)
	if err != nil || int64(len(raw)) != c.Git.Bytes {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != c.Git.SHA256 {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	directory := filepath.Join(c.Root, "source")
	if os.Mkdir(directory, 0700) != nil {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"config", "user.name", "Synthetic evaluation fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if _, err = gitCommand(c, directory, args...); err != nil {
			return r.GitStore{}, r.Snapshot{}, err
		}
	}
	path := filepath.Join(directory, "source.go")
	if os.WriteFile(path, []byte("old source evidence\n"), 0600) != nil {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	if _, err = gitCommand(c, directory, "add", "source.go"); err != nil {
		return r.GitStore{}, r.Snapshot{}, err
	}
	if _, err = gitCommand(c, directory, "commit", "--quiet", "-m", "old synthetic source"); err != nil {
		return r.GitStore{}, r.Snapshot{}, err
	}
	base, err := gitCommand(c, directory, "rev-parse", "HEAD")
	if err != nil {
		return r.GitStore{}, r.Snapshot{}, err
	}
	if os.WriteFile(path, []byte("immutable source evidence\n"), 0600) != nil {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	if _, err = gitCommand(c, directory, "add", "source.go"); err != nil {
		return r.GitStore{}, r.Snapshot{}, err
	}
	if _, err = gitCommand(c, directory, "commit", "--quiet", "-m", "new synthetic source"); err != nil {
		return r.GitStore{}, r.Snapshot{}, err
	}
	head, err := gitCommand(c, directory, "rev-parse", "HEAD")
	if err != nil || f.ConfigureSyntheticRevision(head) != nil {
		return r.GitStore{}, r.Snapshot{}, r.ErrLiveBlocked
	}
	return r.GitStore{Git: c.Git.Path, GitDir: filepath.Join(directory, ".git")}, r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: base, MergeBase: base}, nil
}
func provenance(at time.Time) r.LabelProvenance {
	commit := strings.Repeat("d", 40)
	return r.LabelProvenance{Labeler: r.Labeler{Kind: "agent", Agent: "synthetic fixture", Provider: "test", Model: "test", Session: "synthetic-native-protocol"}, At: at, Evidence: []r.EvaluationEvidence{{Kind: "history", URL: "https://github.com/hemsoft-dev/set-it-free-loop/commit/" + commit, Commit: commit, Description: "synthetic structural fixture, never real history or quality evidence", At: at.Add(-time.Minute), Lines: []r.EvidenceLines{}}}}
}
func buildPlan(ctx context.Context, git r.GitStore, snapshot r.Snapshot) (r.EvaluationCorpus, r.EvaluationPlan, map[string]string, error) {
	now := time.Now().UTC()
	frozen := now.Add(-time.Minute)
	corpus := r.EvaluationCorpus{Version: 2, RepositoryID: r.RepositoryID, FrozenAt: frozen, LabelEvidence: "synthetic structural fixtures only, not evaluation ground truth", Cases: []r.EvaluationCase{}}
	packets := map[string]string{}
	for i := 0; i < 20; i++ {
		snap := snapshot
		snap.PR = i + 1
		evidence, err := r.CaptureSnapshotEvidence(ctx, git, snap)
		if err != nil {
			return corpus, r.EvaluationPlan{}, nil, err
		}
		raw, _ := json.Marshal(evidence.Packet())
		id := fmt.Sprintf("synthetic-native-%d", i)
		merged := now.Add(-8 * 24 * time.Hour)
		at := now.Add(-time.Hour)
		label := r.EvaluationCaseLabel{Clean: i < 10, Rationale: "synthetic protocol label only", Provenance: provenance(at), Survival: []r.SurvivalWindow{}, Defects: []r.DefectLabel{}}
		if label.Clean {
			label.Survival = []r.SurvivalWindow{{Days: 7, From: merged, Through: merged.Add(7 * 24 * time.Hour), HistoryRevision: strings.Repeat("d", 40), Searches: []string{"synthetic fixture"}, Limitations: "no actual history or quality measurement"}}
		} else {
			proof := provenance(at)
			proof.Evidence[0].Kind = "fix"
			proof.Evidence[0].Lines = []r.EvidenceLines{{Revision: snap.Head, Path: "source.go", Start: 1, End: 1}}
			label.Defects = []r.DefectLabel{{ID: "synthetic-defect", Rationale: "synthetic protocol fixture only", Material: true, Provenance: proof}}
		}
		corpus.Cases = append(corpus.Cases, r.EvaluationCase{ID: id, Snapshot: snap, PacketHash: r.Digest(evidence.Packet()), SerializedPacketBytes: len(raw), MergedAt: merged, Label: label, LabelHistory: []r.EvaluationCaseLabel{}})
		packets[id] = evidence.Packet()
	}
	cfg := r.DefaultConfig()
	cfg.Limits.OutputBytes = r.EvaluationOutputBytes
	cfg.Limits.Timeout = 75 * time.Second
	plan := r.EvaluationPlan{Version: 1, CorpusHash: corpus.Hash(), TrustedRelease: sourceHead, Policy: r.PolicyVersion, Config: cfg, CreatedAt: now.Add(-30 * time.Second), Repeats: 1}
	return corpus, plan, packets, plan.Validate(corpus)
}
func upstream(backend http.Handler) (localOnly, func()) {
	server := httptest.NewTLSServer(backend)
	target, _ := url.Parse(server.URL)
	base := server.Client().Transport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ForceAttemptHTTP2 = false
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target.Host {
			return nil, errors.New("non-loopback dial denied")
		}
		return dialer.DialContext(ctx, network, address)
	}
	return localOnly{target, base}, func() { base.CloseIdleConnections(); server.Close() }
}

type nativeReview struct {
	fixture  *fixture
	arm      r.EvaluationArm
	job      r.Job
	bound    *r.JobEvidence
	provider r.Provider
}

func (w nativeReview) ReviewAudited(ctx context.Context, s r.Session) (r.Report, *r.SourceAccess, *r.ProviderEvidence, error) {
	p := w.fixture
	p.calls++
	backend, policy, err := f.New(s, false)
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	transport, close := upstream(backend)
	defer close()
	options, err := p.options(w.arm, w.job, w.bound, w.provider, policy, transport, s)
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	worker, err := r.NewNativeVMReviewer(options)
	if err != nil {
		return r.Report{}, nil, nil, err
	}
	report, source, evidence, err := worker.ReviewAudited(ctx, s)
	p.capture(w.arm, s.Identity.Provider, s.Round, r.Digest(s), backend.Counts(), source, evidence, err)
	return report, source, evidence, err
}

type nativeControl struct{ nativeReview }

func (w nativeControl) Critique(ctx context.Context, s r.ControlSession) (r.ControlReport, error) {
	report, _, _, err := w.CritiqueAudited(ctx, s)
	return report, err
}
func (w nativeControl) CritiqueAudited(ctx context.Context, s r.ControlSession) (r.ControlReport, *r.SourceAccess, *r.ProviderEvidence, error) {
	p := w.fixture
	p.calls++
	capability, err := r.ControlCapabilitySession(s)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	backend, policy, err := f.NewControl(s)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	transport, close := upstream(backend)
	defer close()
	options, err := p.options(w.arm, w.job, w.bound, w.provider, policy, transport, capability)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	worker, err := r.NewNativeVMControlReviewer(options)
	if err != nil {
		return r.ControlReport{}, nil, nil, err
	}
	report, source, evidence, err := worker.CritiqueAudited(ctx, s)
	p.capture(w.arm, s.Provider, s.Step, r.Digest(capability), backend.Counts(), source, evidence, err)
	return report, source, evidence, err
}
func (p *fixture) options(arm r.EvaluationArm, job r.Job, bound *r.JobEvidence, provider r.Provider, policy r.EgressPolicy, transport http.RoundTripper, captured r.Session) (r.NativeVMOptions, error) {
	reader, err := bound.Reader(job.Snapshot)
	if err != nil {
		return r.NativeVMOptions{}, err
	}
	names := map[r.EvaluationArm]string{r.ArmAdversarial: "a", r.ArmIndependent: "i", r.ArmCodex: "x", r.ArmClaude: "c", r.ArmCodexControl: "xc", r.ArmClaudeControl: "cc"}
	name, ok := names[arm]
	if !ok || job.Snapshot.PR != 1 {
		return r.NativeVMOptions{}, r.ErrLiveBlocked
	}
	root := filepath.Join(p.config.Root, "launches", name)
	if err = os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return r.NativeVMOptions{}, err
	}
	hints := r.NativeVMProfileHints{}
	if provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	return r.NativeVMOptions{Provider: provider, Root: root, Stub: p.config.Stub.Path, StubHash: p.config.Stub.SHA256, QEMU: p.config.QEMU, Kernel: p.config.Kernel, Initrd: p.config.Initrd, Firmware: p.config.Firmware, Runtime: p.config.Runtime, Auth: p.auth[provider], Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, s r.Session) error {
		if r.Digest(s) != r.Digest(captured) {
			return r.ErrLiveBlocked
		}
		return nil
	}}, nil
}
func (p *fixture) capture(arm r.EvaluationArm, provider r.Provider, step int, input string, counts f.Counts, source *r.SourceAccess, evidence *r.ProviderEvidence, err error) {
	calls := 0
	if source != nil {
		calls = source.ToolCalls
	}
	passed := err == nil && source != nil && source.ReaderCalls == 2 && calls == 2 && evidence != nil && evidence.Access.Stopped && counts.Inference == 3 && counts.Failures == 0 && counts.Telemetry == 0
	if provider == r.Codex {
		passed = passed && counts.MetadataCaptured
	}
	p.phases = append(p.phases, phase{1, arm, provider, step, input, counts, calls, passed})
}
func run(c config) error {
	if os.Mkdir(c.Root, 0700) != nil {
		return r.ErrLiveBlocked
	}
	for _, name := range []string{"journal", "auth", "launches"} {
		if os.Mkdir(filepath.Join(c.Root, name), 0700) != nil {
			return r.ErrLiveBlocked
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 27*time.Minute)
	defer cancel()
	git, snapshot, err := syntheticSource(c)
	if err != nil {
		return err
	}
	corpus, plan, packets, err := buildPlan(ctx, git, snapshot)
	if err != nil {
		return err
	}
	journal, err := r.OpenEvaluationJournal(filepath.Join(c.Root, "journal", "execution.db"), r.Enrollment{RepositoryID: r.RepositoryID, InstallationID: 9001, AppID: 9002, BotID: 9003, OwnerID: r.OwnerID}, corpus, plan)
	if err != nil {
		return err
	}
	defer journal.Close()
	p := &fixture{config: c, journal: journal, source: git, packets: packets, auth: map[r.Provider]*r.AuthStore{}, phases: []phase{}}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		store, err := r.NewAuthStore(provider, filepath.Join(c.Root, "auth"))
		if err != nil {
			return err
		}
		seed := f.Seed(provider)
		err = store.Refresh(ctx, func(old []byte) ([]byte, error) {
			if len(old) != 0 {
				return nil, r.ErrLiveBlocked
			}
			return seed, nil
		})
		clear(seed)
		if err != nil {
			return err
		}
		p.auth[provider] = store
	}
	options := r.EvaluationRunnerOptions{Journal: journal, Check: func(_ context.Context, got r.EvaluationPlan) error {
		if got.Hash() != plan.Hash() {
			return r.ErrLiveBlocked
		}
		return nil
	}, Source: func(_ context.Context, q r.EvaluationSourceRequest) (r.EvaluationSource, error) {
		// Only one case is authorized by this fixture. Remaining19 are explicitly
		// missing, never padded, scored or silently treated as clean controls.
		if q.Snapshot.PR != 1 {
			return r.EvaluationSource{}, r.ErrLiveBlocked
		}
		packet, ok := packets[q.CaseID]
		if !ok {
			return r.EvaluationSource{}, r.ErrFenced
		}
		return r.EvaluationSource{Packet: packet, Bind: func(ctx context.Context, job r.Job) (*r.JobEvidence, error) {
			return r.RestoreSnapshotEvidence(ctx, git, job)
		}}, nil
	}, Workers: func(_ context.Context, arm r.EvaluationArm, job r.Job, bound *r.JobEvidence) (r.EvaluationWorkers, error) {
		if job.Snapshot.PR != 1 {
			return r.EvaluationWorkers{}, r.ErrLiveBlocked
		}
		if arm == r.ArmCodexControl || arm == r.ArmClaudeControl {
			provider := arm.Schedule()[0]
			return r.EvaluationWorkers{Controls: map[r.Provider]r.AuditedControlReviewer{provider: nativeControl{nativeReview{p, arm, job, bound, provider}}}}, nil
		}
		workers := r.EvaluationWorkers{Reviews: map[r.Provider]r.AuditedReviewer{}}
		for _, provider := range job.Config.Providers() {
			workers.Reviews[provider] = nativeReview{p, arm, job, bound, provider}
		}
		return workers, nil
	}}
	started := time.Now()
	if c.StoppedFaultArm != "" {
		return p.runStoppedJournalFault(ctx, corpus, plan, options)
	}
	out, runErr := r.RunEvaluation(ctx, options)
	passed := errors.Is(runErr, r.ErrLiveBlocked) && len(out.Executions) == 6 && out.ExecutedSessions == 16 && p.calls == 16 && len(p.phases) == 16
	for i, execution := range out.Executions {
		passed = passed && execution.Arm == plan.Order(0, 0)[i] && execution.State == "complete" && execution.Sessions() == execution.Arm.Sessions()
	}
	for _, phase := range p.phases {
		passed = passed && phase.Passed
	}
	before := p.calls
	resumed, resumeErr := r.RunEvaluation(ctx, options)
	resumedWithoutWork := errors.Is(resumeErr, r.ErrLiveBlocked) && resumed.ReusedArms == 6 && resumed.ExecutedSessions == 0 && p.calls == before && r.Digest(resumed.Executions) == r.Digest(out.Executions)
	passed = passed && resumedWithoutWork
	receipt := struct {
		Version                                                                            int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead, CorpusHash, PlanHash string
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified                   bool
		CompletedCases, MissingCases, CompletedArms, PhaseAttempts                         int
		ResumedWithoutWork                                                                 bool
		ElapsedMS                                                                          int64
		Phases                                                                             []phase
	}{1, "six-arm-native-fake", sourceHead, sourceTree, c.ImageSourceHead, corpus.Hash(), plan.Hash(), passed, true, false, false, 1, 19, len(out.Executions), p.calls, resumedWithoutWork, time.Since(started).Milliseconds(), p.phases}
	data, _ := json.MarshalIndent(receipt, "", "  ")
	if os.WriteFile(filepath.Join(c.Root, "result.json"), append(data, '\n'), 0600) != nil {
		return r.ErrFenced
	}
	fmt.Println(string(data))
	if !passed {
		return errors.New("native six-arm acceptance incomplete")
	}
	return nil
}

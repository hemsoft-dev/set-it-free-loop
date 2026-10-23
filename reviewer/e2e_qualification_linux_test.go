//go:build linux

package reviewer_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

// TestE2EQualification is the opt-in aggregate host qualification. It composes
// the public version 2 startup (OpenGitReviewerService with the real user
// systemd source VM owner), polling discovery against a local TLS fake GitHub,
// source acquisition through real KVM source VM units from a local fake Git
// upstream, both native VM providers against fake TLS model backends, and
// COMMENT publication through the outbox. Every credential is synthetic and
// every destination is loopback. It never contacts a real provider or GitHub.
//
// Required: SFL_E2E_SOURCE_IMAGE, SFL_E2E_SOURCE_STUB, SFL_E2E_NATIVE_IMAGE and
// SFL_E2E_CASE (review, isolation, close-capture, close-provider,
// close-provider-reopen, close-publish). close-provider-reopen is a second
// process that attaches to the root retained by close-provider through
// SFL_E2E_ATTACH, as a restarted service would. SFL_E2E_EVIDENCE optionally
// names an existing private directory that receives a nonsecret JSON result.
// Run under the shared VM lock.
func TestE2EQualification(t *testing.T) {
	sourceImage, sourceStub, nativeImage := os.Getenv("SFL_E2E_SOURCE_IMAGE"), os.Getenv("SFL_E2E_SOURCE_STUB"), os.Getenv("SFL_E2E_NATIVE_IMAGE")
	scenario := os.Getenv("SFL_E2E_CASE")
	if sourceImage == "" || sourceStub == "" || nativeImage == "" || scenario == "" {
		t.Skip("explicit source image, exact-head source stub, native image and case required")
	}
	cases := map[string]func(*e2eEnv){"review": e2eReview, "isolation": e2eIsolation, "close-capture": e2eCloseCapture, "close-provider": e2eCloseProvider, "close-provider-reopen": e2eCloseProviderReopen, "close-publish": e2eClosePublish}
	run, ok := cases[scenario]
	if !ok {
		t.Fatal("unknown E2E case", scenario)
	}
	env := newE2EEnv(t, scenario, sourceImage, sourceStub, nativeImage)
	started := time.Now()
	run(env)
	env.result.ElapsedSeconds = time.Since(started).Seconds()
	env.finish()
}

// ---------------------------------------------------------------- harness

var e2eEnrollment = r.Enrollment{RepositoryID: r.RepositoryID, InstallationID: 9001, AppID: 9002, BotID: 9003, OwnerID: r.OwnerID}

const e2eSource = "immutable source evidence\n"

type e2eResult struct {
	Case, Revision, ReleaseHash   string
	Root                          string
	SourceImageCommit             string
	NativeImageCommit             string
	Passed                        bool
	ElapsedSeconds                float64
	Checks                        []string
	Failures                      []string
	Timeline                      []e2eEvent
	Jobs                          []e2eJob
	Writes                        []e2eWrite
	SourceRecords                 [][]r.E2ESourceRecord
	Exports                       [][]r.E2EExportRecord
	Probes                        map[string][]r.E2EProbe
	NativeUnits                   []e2eNativeUnit
	SystemdUnits                  []e2eSystemdUnit
	GitHub                        e2eGitHubSummary
	Models                        map[r.Provider][]e2eModelSession
	AuthRecoveryClear             map[string]bool
	ResidualSystemdUnits          []string
	RunErrors                     []string
	Notes                         []string
	FakeOnly, ProviderEligibility bool
}

type e2eEvent struct {
	At     time.Time
	Kind   string
	Detail string
}

type e2eJob struct {
	ID, State, Scope                    string
	PR                                  int
	Head                                string
	RequestID                           int64
	Consumed, Artifacts                 int
	Lease                               bool
	Outstanding                         bool
	SourceAttempts, ProviderAttempts    int
	LedgerFindings, ProviderAttemptKind []string
}

type e2eWrite struct {
	ID, Kind, State string
	PR              int
	ReceiptID       int64
	Inline          int
	Head            string
}

type e2eEnv struct {
	t                              *testing.T
	scenario                       string
	root                           string
	meta, unit, export, work, auth string
	native                         map[r.Provider]string
	store                          *r.Store
	storePath                      string
	github                         *e2eGitHub
	models                         map[r.Provider]*e2eModel
	options                        r.GitReviewerServiceOptions
	service                        *r.GitReviewerService
	runCancel                      context.CancelFunc
	runDone                        chan error
	config                         r.Config
	result                         *e2eResult
	mu                             sync.Mutex
	units                          *e2eUnitObserver
	finished                       bool
	attached                       bool
	// wedged marks a handle whose Close is expected to keep reporting
	// ErrWorkerStopPending; keepRoot retains the root for an attached phase.
	wedged, keepRoot                  bool
	sourceImageCommit, nativeRevision string
}

func (e *e2eEnv) event(kind, format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.result.Timeline = append(e.result.Timeline, e2eEvent{time.Now().UTC(), kind, fmt.Sprintf(format, args...)})
}
func (e *e2eEnv) check(ok bool, format string, args ...any) {
	e.t.Helper()
	message := fmt.Sprintf(format, args...)
	e.mu.Lock()
	defer e.mu.Unlock()
	if ok {
		e.result.Checks = append(e.result.Checks, message)
		return
	}
	e.result.Failures = append(e.result.Failures, message)
	e.t.Error("E2E check failed: " + message)
}
func (e *e2eEnv) note(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.result.Notes = append(e.result.Notes, fmt.Sprintf(format, args...))
}

func e2eHash(path string) r.VMBootAsset {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return r.VMBootAsset{Path: path, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(raw))}
}

func newE2EEnv(t *testing.T, scenario, sourceImage, sourceStub, nativeImage string) *e2eEnv {
	t.Helper()
	attach := os.Getenv("SFL_E2E_ATTACH")
	root := attach
	if attach == "" {
		var nonce [6]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		// Short private paths preserve sockaddr_un bounds for both VM kinds.
		root = "/tmp/sfl-e2e-" + hex.EncodeToString(nonce[:])
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal("fresh E2E root required", err)
		}
	} else if !regexp.MustCompile(`^/tmp/sfl-e2e-[0-9a-f]{12}$`).MatchString(attach) {
		t.Fatal("attach requires a retained E2E root")
	}
	e := &e2eEnv{t: t, scenario: scenario, root: root, attached: attach != "", native: map[r.Provider]string{}, models: map[r.Provider]*e2eModel{}, result: &e2eResult{Case: scenario, Root: root, Probes: map[string][]r.E2EProbe{}, Models: map[r.Provider][]e2eModelSession{}, AuthRecoveryClear: map[string]bool{}, FakeOnly: true}}
	e.meta, e.unit, e.export, e.work, e.auth = filepath.Join(root, "m"), filepath.Join(root, "u"), filepath.Join(root, "x"), filepath.Join(root, "w"), filepath.Join(root, "a")
	for _, directory := range []string{e.meta, e.unit, e.export, e.work, e.auth, filepath.Join(root, "n")} {
		if attach != "" {
			if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
				t.Fatal("retained E2E directory missing", directory)
			}
		} else if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	e.units = newE2EUnitObserver(t)
	// Release: version 2 source VM package plus both native providers.
	vm, commit := r.E2ESourceVMRelease(t, sourceImage, sourceStub)
	e.sourceImageCommit = commit
	e.result.SourceImageCommit = commit
	config := r.DefaultConfig()
	config.Limits.Timeout = 90 * time.Second
	e.config = config
	release := r.GitReviewerRelease{Version: 2, Revision: commit, SourceVM: &vm, Providers: map[r.Provider]r.GitReviewerProviderRelease{}}
	manifest := e2eNativeManifest(t, nativeImage)
	e.nativeRevision = manifest.Commit
	e.result.NativeImageCommit = manifest.Commit
	hosts := map[r.Provider]r.GitReviewerProviderHost{}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		identity, _ := r.PinnedIdentity(provider)
		probe, err := r.NewSession("e2e-policy-probe", r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}, config.Hash(), provider, 1, "probe", nil, nil, config.Limits)
		if err != nil {
			t.Fatal(err)
		}
		release.Providers[provider] = r.GitReviewerProviderRelease{Identity: identity, Stub: e2eHash(filepath.Join(nativeImage, "nativevmstub")), QEMU: manifest.QEMU, Kernel: e2eHash(filepath.Join(nativeImage, "vmlinuz")), Initrd: e2eHash(filepath.Join(nativeImage, "native.cpio.gz")), Firmware: e2eHash(filepath.Join(nativeImage, "bios.bin")), Runtime: manifest.Runtime, Policy: f.Policy(probe)}
		e.native[provider] = filepath.Join(root, "n", string(provider)[:2])
		if !e.attached {
			if err := os.Mkdir(e.native[provider], 0700); err != nil {
				t.Fatal(err)
			}
		}
		model := newE2EModel(t, e, provider)
		e.models[provider] = model
		store, err := r.NewAuthStore(provider, e.auth)
		if err != nil {
			t.Fatal(err)
		}
		if !e.attached {
			seed := f.Seed(provider)
			if err := store.Refresh(context.Background(), func(old []byte) ([]byte, error) {
				if len(old) != 0 {
					return nil, errors.New("existing synthetic seed refused")
				}
				return seed, nil
			}); err != nil {
				t.Fatal("synthetic seed", err)
			}
			clear(seed)
		}
		hints := r.NativeVMProfileHints{}
		if provider == r.Claude {
			hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
		}
		hosts[provider] = r.GitReviewerProviderHost{Root: e.native[provider], Auth: store, Transport: model.transport, ProfileHints: hints, Check: model.check}
	}
	_, releaseHash, err := r.EncodeGitReviewerRelease(release)
	if err != nil {
		t.Fatal("release package", err)
	}
	e.result.Revision, e.result.ReleaseHash = release.Revision, releaseHash
	e.github = newE2EGitHub(t, e)
	e.storePath = filepath.Join(e.meta, "state.db")
	e.options = r.GitReviewerServiceOptions{
		Current: release.Revision, Releases: map[string]r.GitReviewerRelease{release.Revision: release}, Providers: map[string]map[r.Provider]r.GitReviewerProviderHost{release.Revision: hosts},
		UnitDirectory: e.unit, ExportDirectory: e.export, WorkDirectory: e.work, Triggers: r.TriggerPolicy{EnabledAt: e.github.epoch},
		Check: func(_ context.Context, en r.Enrollment, revision, hash string) error {
			if en != e2eEnrollment || revision != release.Revision || hash != releaseHash {
				return r.ErrLiveBlocked
			}
			return nil
		},
		Interval: time.Second, MaxBackoff: 5 * time.Second, TickTimeout: 2 * time.Minute,
	}
	t.Cleanup(e.cleanup)
	return e
}

type e2eNativeImage struct {
	Commit  string
	QEMU    r.VMBootAsset
	Runtime r.NativeVMRuntimeSpec
}

func e2eNativeManifest(t *testing.T, image string) e2eNativeImage {
	raw, err := os.ReadFile(filepath.Join(image, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Purpose, State string
		Commit         string `json:"source_commit"`
		Dirty          bool   `json:"source_dirty"`
		QEMU           r.VMBootAsset
		Host           struct {
			Artifact     r.VMBootAsset
			Helper       r.VMBootAsset   `json:"namespace_helper"`
			Dependencies []r.VMBootAsset `json:"bootstrap_dependencies"`
		} `json:"host_runtime"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Purpose != "native-review" || m.State != "built-only" || m.Dirty || len(m.Commit) != 40 {
		t.Fatal("explicit clean native build artifact required")
	}
	var probe struct {
		QEMU r.VMBootAsset `json:"qemu"`
	}
	_ = json.Unmarshal(raw, &probe)
	artifact := e2eHash(filepath.Join(image, "host-runtime", "native-qemu-runtime.bin"))
	if artifact.SHA256 != m.Host.Artifact.SHA256 || artifact.Bytes != m.Host.Artifact.Bytes {
		t.Fatal("captured native runtime changed")
	}
	qemu := e2eHash(probe.QEMU.Path)
	if qemu.SHA256 != probe.QEMU.SHA256 {
		t.Fatal("native QEMU changed")
	}
	return e2eNativeImage{Commit: m.Commit, QEMU: qemu, Runtime: r.NativeVMRuntimeSpec{Artifact: artifact, NamespaceHelper: m.Host.Helper, BootstrapLibraries: m.Host.Dependencies}}
}

// open composes the public startup from fresh store/journal handles, as a
// restarted process would, then explicitly refreshes and starts Run.
func (e *e2eEnv) open(label string) {
	e.t.Helper()
	store, err := r.OpenStore(e.storePath, e2eEnrollment, e.config)
	if err != nil {
		e.t.Fatal("store", err)
	}
	e.store = store
	journal, err := r.NewGitHubInstallationJournal(context.Background(), store, time.Now)
	if err != nil {
		e.t.Fatal("installation journal", err)
	}
	e.options.Credentials = r.GitHubCredentialOwnerOptions{Journal: journal, JWT: func(context.Context) ([]byte, error) { return []byte("synthetic-app-jwt"), nil }, ReaderTransport: e.github.route("reader"), PublisherTransport: e.github.route("publisher"), Check: func(_ context.Context, en r.Enrollment) error {
		if en != e2eEnrollment {
			return r.ErrLiveBlocked
		}
		return nil
	}}
	// Each reopen gets fresh provider auth handles over the same directory.
	for provider, host := range e.options.Providers[e.options.Current] {
		auth, err := r.NewAuthStore(provider, e.auth)
		if err != nil {
			e.t.Fatal(err)
		}
		host.Auth = auth
		e.options.Providers[e.options.Current][provider] = host
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	started := time.Now()
	s, err := r.OpenGitReviewerService(ctx, e.options)
	if err != nil {
		e.event("startup", "%s OpenGitReviewerService failed: %v", label, err)
		if s != nil && e.service == nil {
			e.service = s
		}
		e.t.Fatal("public startup", label, err)
	}
	e.service = s
	e.event("startup", "%s OpenGitReviewerService in %s", label, time.Since(started).Round(time.Millisecond))
	if err := s.Refresh(ctx); err != nil {
		e.t.Fatal("explicit refresh", err)
	}
	e.event("refresh", "%s explicit credential refresh", label)
	run, stop := context.WithCancel(context.Background())
	e.runCancel = stop
	done := make(chan error, 1)
	e.runDone = done
	go func() { done <- s.Run(run) }()
}

// close is the controlled shutdown/crash point: it closes the whole public
// service (controller, factory, credential owner, startup lock) and the store.
func (e *e2eEnv) close(label string) {
	e.t.Helper()
	if e.service == nil {
		return
	}
	e.snapshotSource(label + " before close")
	// Close has a 30-second ceiling and a timeout is not a stop receipt.
	// Retrying operates on the same retained owners, as documented.
	var err error
	attempts := 0
	for attempts < 4 {
		attempts++
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		started := time.Now()
		err = e.service.Close(ctx)
		cancel()
		e.event("close", "%s Close attempt %d returned %v in %s", label, attempts, err, time.Since(started).Round(time.Millisecond))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if e.wedged {
		e.check(errors.Is(err, r.ErrWorkerStopPending), "%s: Close keeps reporting ErrWorkerStopPending while the durable lease is outstanding (attempts %d)", label, attempts)
	} else {
		e.check(err == nil, "%s: service Close proved completion (attempts %d)", label, attempts)
	}
	if attempts > 1 && err == nil {
		e.note("%s: first Close did not prove completion; retry %d on the same owners did", label, attempts)
	}
	select {
	case runErr := <-e.runDone:
		e.result.RunErrors = append(e.result.RunErrors, fmt.Sprintf("%s: %v", label, runErr))
	case <-time.After(10 * time.Second):
		e.check(false, "%s: Run did not return after Close", label)
	}
	e.runCancel()
	if err == nil {
		e.service = nil
	}
	e.snapshotJobs(label + " after close")
	if e.store != nil {
		e.store.Close()
		e.store = nil
	}
}

func (e *e2eEnv) state() r.DurableState {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := e.store.Snapshot(ctx)
	if err != nil {
		e.t.Fatal("store snapshot", err)
	}
	return d
}

// wait polls durable state until cond holds. A failed run ends the wait.
func (e *e2eEnv) wait(label string, limit time.Duration, cond func(r.DurableState) bool) bool {
	return e.waitFor(label, limit, true, cond)
}

func (e *e2eEnv) waitFor(label string, limit time.Duration, required bool, cond func(r.DurableState) bool) bool {
	e.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond(e.state()) {
			e.event("reached", "%s", label)
			return true
		}
		select {
		case err := <-e.runDone:
			e.result.RunErrors = append(e.result.RunErrors, fmt.Sprintf("while waiting for %s: %v", label, err))
			e.runDone = make(chan error, 1)
			e.check(false, "Run returned while waiting for %s: %v", label, err)
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	if required {
		e.check(false, "timed out waiting for %s", label)
	} else {
		e.event("timeout", "%s not reached within %s", label, limit)
	}
	return false
}

func e2eJobFor(d r.DurableState, pr int) *r.DurableJob {
	var found *r.DurableJob
	for _, j := range d.Jobs {
		if j.Scope.PR == pr && (found == nil || j.CreatedAt.After(found.CreatedAt)) {
			found = j
		}
	}
	return found
}
func e2eWritesFor(d r.DurableState, pr int, kind string) []*r.WriteIntent {
	writes := []*r.WriteIntent{}
	for _, w := range d.Writes {
		if w.Scope.PR == pr && w.Kind == kind {
			writes = append(writes, w)
		}
	}
	return writes
}
func e2eConfirmed(d r.DurableState, pr int) bool {
	for _, w := range e2eWritesFor(d, pr, "review") {
		if w.State == "confirmed" {
			return true
		}
	}
	return false
}

func (e *e2eEnv) snapshotJobs(label string) {
	if e.store == nil {
		return
	}
	d := e.state()
	ids := slices.Sorted(maps.Keys(d.Jobs))
	for _, id := range ids {
		j := d.Jobs[id]
		e.event("job", "%s: job %s PR %d state %s consumed %d artifacts %d lease %v outstanding %v source attempts %d provider attempts %d", label, j.Job.ID, j.Scope.PR, j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), j.LeaseToken != "", j.Outstanding, len(j.SourceAttempts), len(j.ProviderAttempts))
	}
	for _, id := range slices.Sorted(maps.Keys(d.Writes)) {
		w := d.Writes[id]
		e.event("write", "%s: %s %s PR %d state %s receipt %d", label, w.Kind, w.ID, w.Scope.PR, w.State, w.ReceiptID)
	}
}

func (e *e2eEnv) snapshotSource(label string) {
	if e.service == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, exports, err := e.service.E2ESourceRecords(ctx)
	if err != nil {
		e.event("source", "%s: records unavailable: %v", label, err)
		return
	}
	e.mu.Lock()
	e.result.SourceRecords = append(e.result.SourceRecords, records)
	e.result.Exports = append(e.result.Exports, exports)
	e.mu.Unlock()
	for _, rec := range records {
		e.event("source", "%s: attempt %s unit %s invocation %s PR %d head %s settled %v captured %v revoked %v stopped %v grant %v", label, rec.Attempt, rec.Unit, rec.InvocationID, rec.Snapshot.PR, rec.Snapshot.Head[:12], rec.StartSettled, rec.Captured, rec.Revoked, rec.Stopped, rec.GrantHash != "")
	}
	for _, x := range exports {
		e.event("export", "%s: attempt %s state %s PR %d", label, x.Attempt, x.State, x.Snapshot.PR)
	}
}

// finish proves no unit of this run remains, auth disposition, native unit
// receipts and writes the nonsecret evidence record.
func (e *e2eEnv) finish() {
	e.finished = true
	t := e.t
	if e.service != nil && !e.wedged {
		e.close("final")
	}
	e.result.SystemdUnits, e.result.ResidualSystemdUnits = e.units.stop(30 * time.Second)
	e.check(len(e.result.ResidualSystemdUnits) == 0, "no sfl-alpha unit started by this run remains (%d observed)", len(e.result.SystemdUnits))
	sourceUnits, nativeUnits := 0, 0
	for _, u := range e.result.SystemdUnits {
		if u.Source {
			sourceUnits++
		} else {
			nativeUnits++
		}
	}
	e.event("systemd", "observed %d distinct sfl-alpha units (source+native)", len(e.result.SystemdUnits))
	_ = sourceUnits
	_ = nativeUnits
	e.result.NativeUnits = e2eReadNativeUnits(e.native)
	for _, u := range e.result.NativeUnits {
		e.check(u.Captured && u.Stopped, "native unit %s captured and stopped", u.Launch)
	}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		e.result.AuthRecoveryClear[string(provider)] = e2eAuthUsable(e.auth, provider)
	}
	e.result.GitHub = e.github.summary()
	e.check(len(e.result.GitHub.Violations) == 0, "fake GitHub saw no role, destination or credential violation: %v", e.result.GitHub.Violations)
	for provider, model := range e.models {
		e.result.Models[provider] = model.sessionsCopy()
	}
	e.result.Passed = !t.Failed() && len(e.result.Failures) == 0
	data, _ := json.MarshalIndent(e.result, "", "  ")
	if directory := os.Getenv("SFL_E2E_EVIDENCE"); directory != "" {
		if err := os.WriteFile(filepath.Join(directory, e.scenario+".json"), append(data, '\n'), 0600); err != nil {
			t.Error("evidence", err)
		}
	}
	e.report()
}

func (e *e2eEnv) report() {
	t := e.t
	t.Logf("E2E %s passed=%v elapsed=%.1fs checks=%d failures=%d", e.scenario, e.result.Passed, e.result.ElapsedSeconds, len(e.result.Checks), len(e.result.Failures))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.result.Timeline {
		t.Logf("%s %-8s %s", ev.At.Format("15:04:05.000"), ev.Kind, ev.Detail)
	}
	for _, n := range e.result.Notes {
		t.Logf("note: %s", n)
	}
}

func (e *e2eEnv) cleanup() {
	if !e.finished {
		// A fatal path skipped finish: keep its timeline and partial evidence.
		e.result.Passed = false
		e.report()
		if directory := os.Getenv("SFL_E2E_EVIDENCE"); directory != "" {
			data, _ := json.MarshalIndent(e.result, "", "  ")
			_ = os.WriteFile(filepath.Join(directory, e.scenario+".json"), append(data, '\n'), 0600)
		}
		e.units.stop(30 * time.Second)
	}
	// A wedged handle already reported ErrWorkerStopPending and its store
	// handle is closed. It keeps its startup and credential locks until this
	// process exits, exactly like a crashed service process.
	if e.service != nil && !e.wedged {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err := e.service.Close(ctx)
		cancel()
		if err != nil {
			e.t.Error("cleanup could not prove service stop; retaining", e.root, err)
			return
		}
	}
	if e.keepRoot && !e.t.Failed() {
		e.t.Log("retaining E2E root for the attached phase", e.root)
		return
	}
	if e.store != nil {
		e.store.Close()
	}
	if e.t.Failed() && os.Getenv("SFL_E2E_KEEP_FAILED") != "" {
		e.t.Log("retaining failed E2E state", e.root)
		return
	}
	// Only this run's fresh root; units are proved gone before this point.
	_ = filepath.WalkDir(e.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0700)
		}
		return nil
	})
	if err := os.RemoveAll(e.root); err != nil {
		e.t.Error("remove E2E root", err)
	}
}

// ---------------------------------------------------------------- systemd

type e2eSystemdUnit struct {
	Name           string
	Source         bool
	FirstSeen      time.Time
	LastSeen       time.Time
	States         []string
	DescriptionTag string
}

type e2eUnitObserver struct {
	mu       sync.Mutex
	before   map[string]bool
	seen     map[string]*e2eSystemdUnit
	stopOnce sync.Once
	done     chan struct{}
	joined   chan struct{}
}

func e2eListUnits() (map[string]string, error) {
	out, err := exec.Command("systemctl", "--user", "list-units", "sfl-alpha-*", "--all", "--plain", "--no-legend", "--no-pager").Output()
	if err != nil {
		return nil, err
	}
	units := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			units[fields[0]] = fields[1] + "/" + fields[2] + "/" + fields[3] + " " + strings.Join(fields[4:], " ")
		}
	}
	return units, nil
}

func newE2EUnitObserver(t *testing.T) *e2eUnitObserver {
	before, err := e2eListUnits()
	if err != nil {
		t.Fatal("user systemd unavailable", err)
	}
	o := &e2eUnitObserver{before: map[string]bool{}, seen: map[string]*e2eSystemdUnit{}, done: make(chan struct{}), joined: make(chan struct{})}
	for name := range before {
		o.before[name] = true
	}
	go func() {
		defer close(o.joined)
		for {
			o.sample()
			select {
			case <-o.done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return o
}
func (o *e2eUnitObserver) sample() map[string]string {
	units, err := e2eListUnits()
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	o.mu.Lock()
	defer o.mu.Unlock()
	for name, state := range units {
		if o.before[name] {
			continue
		}
		u := o.seen[name]
		if u == nil {
			u = &e2eSystemdUnit{Name: name, FirstSeen: now, Source: strings.Contains(state, "source VM")}
			o.seen[name] = u
		}
		u.LastSeen = now
		short := strings.SplitN(state, " ", 2)[0]
		if len(u.States) == 0 || u.States[len(u.States)-1] != short {
			u.States = append(u.States, short)
		}
		if len(state) > len(short) {
			u.DescriptionTag = strings.TrimSpace(state[len(short):])
			if len(u.DescriptionTag) > 48 {
				u.DescriptionTag = u.DescriptionTag[:48]
			}
		}
	}
	return units
}

// stop waits until no unit first seen by this run remains, then reports.
func (o *e2eUnitObserver) stop(grace time.Duration) ([]e2eSystemdUnit, []string) {
	deadline := time.Now().Add(grace)
	residual := []string{}
	for {
		units := o.sample()
		residual = residual[:0]
		o.mu.Lock()
		for name := range units {
			if !o.before[name] {
				residual = append(residual, name+" "+units[name])
			}
		}
		o.mu.Unlock()
		if units != nil && len(residual) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	o.stopOnce.Do(func() { close(o.done) })
	<-o.joined
	o.mu.Lock()
	defer o.mu.Unlock()
	all := []e2eSystemdUnit{}
	for _, u := range o.seen {
		all = append(all, *u)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].FirstSeen.Before(all[j].FirstSeen) })
	return all, append([]string(nil), residual...)
}

// ---------------------------------------------------------------- native units

type e2eNativeUnit struct {
	Provider, Launch, SessionHash        string
	Captured, Stopped                    bool
	Complete, AuthExported, AuthReturned bool
	AuditPresent                         bool
}

func e2eReadNativeUnits(roots map[r.Provider]string) []e2eNativeUnit {
	units := []e2eNativeUnit{}
	for provider, root := range roots {
		paths, _ := filepath.Glob(filepath.Join(root, "*", "unit.json"))
		for _, path := range paths {
			var unit struct {
				Captured, Stopped bool
				Binding           r.AuthBinding
			}
			raw, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(raw, &unit) != nil {
				units = append(units, e2eNativeUnit{Provider: string(provider), Launch: filepath.Base(filepath.Dir(path))})
				continue
			}
			u := e2eNativeUnit{Provider: string(provider), Launch: filepath.Base(filepath.Dir(path)), SessionHash: unit.Binding.SessionHash, Captured: unit.Captured, Stopped: unit.Stopped}
			var audit struct{ Complete, AuthExported, AuthReturned bool }
			if raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "native-audit.json")); err == nil && json.Unmarshal(raw, &audit) == nil {
				u.AuditPresent, u.Complete, u.AuthExported, u.AuthReturned = true, audit.Complete, audit.AuthExported, audit.AuthReturned
			}
			units = append(units, u)
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Launch < units[j].Launch })
	return units
}

// ---------------------------------------------------------------- fake models

type e2eModelSession struct {
	JobID         string
	Provider      r.Provider
	Round         int
	PR            int
	Head          string
	SessionHash   string
	CheckedAt     time.Time
	Counts        f.Counts
	HeldInCheck   bool
	HeldInference bool
}

// e2eModel serves one provider's fake TLS model backend. The host Check, which
// the native reviewer calls exactly once before launch, binds a fresh canned
// backend to that session's captured revision.
type e2eModel struct {
	t         *testing.T
	env       *e2eEnv
	provider  r.Provider
	server    *httptest.Server
	transport http.RoundTripper
	mu        sync.Mutex
	backend   *f.Backend
	sessions  []e2eModelSession
	// Fault hooks: holdCheck blocks the first Check; holdInference withholds
	// the first inference response. Both release on cancellation or timeout.
	holdCheck, holdInference bool
	held                     chan string
}

func newE2EModel(t *testing.T, env *e2eEnv, provider r.Provider) *e2eModel {
	m := &e2eModel{t: t, env: env, provider: provider, held: make(chan string, 4)}
	m.server = httptest.NewTLSServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	target, _ := url.Parse(m.server.URL)
	base := m.server.Client().Transport.(*http.Transport).Clone()
	base.Proxy, base.ForceAttemptHTTP2 = nil, false
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target.Host {
			return nil, errors.New("non-loopback fake model dial denied")
		}
		return dialer.DialContext(ctx, network, address)
	}
	m.transport = e2eModelRoute{target: target, base: base}
	return m
}

type e2eModelRoute struct {
	target *url.URL
	base   *http.Transport
}

func (t e2eModelRoute) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Scheme != "https" || q.URL.Host != "fixture.provider.example" {
		return nil, errors.New("fake-only model destination denied")
	}
	clone := q.Clone(q.Context())
	u := *q.URL
	u.Host = t.target.Host
	clone.URL = &u
	return t.base.RoundTrip(clone)
}

func (m *e2eModel) check(ctx context.Context, s r.Session) error {
	backend, _, err := f.NewForSnapshot(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.backend = backend
	hold := m.holdCheck
	m.holdCheck = false
	m.sessions = append(m.sessions, e2eModelSession{JobID: s.JobID, Provider: s.Identity.Provider, Round: s.Round, PR: s.Snapshot.PR, Head: s.Snapshot.Head, SessionHash: r.Digest(s), CheckedAt: time.Now().UTC(), HeldInCheck: hold})
	m.mu.Unlock()
	m.env.event("session", "%s R%d PR %d head %s job %s check", s.Identity.Provider, s.Round, s.Snapshot.PR, s.Snapshot.Head[:12], s.JobID)
	if hold {
		m.held <- "check"
		select {
		case <-ctx.Done():
			m.env.event("fault", "%s R%d check held until cancellation: %v", s.Identity.Provider, s.Round, ctx.Err())
			return ctx.Err()
		case <-time.After(3 * time.Minute):
			return errors.New("held check timed out")
		}
	}
	return nil
}

func (m *e2eModel) serve(w http.ResponseWriter, q *http.Request) {
	m.mu.Lock()
	backend := m.backend
	inference := q.Method == "POST" && (q.URL.Path == "/v1/responses" || q.URL.Path == "/v1/messages")
	hold := inference && m.holdInference
	if hold {
		m.holdInference = false
		if len(m.sessions) > 0 {
			m.sessions[len(m.sessions)-1].HeldInference = true
		}
	}
	m.mu.Unlock()
	if backend == nil {
		http.Error(w, "no bound session", http.StatusServiceUnavailable)
		return
	}
	if hold {
		// Validate nothing and return no bytes: the client is mid-session.
		_, _ = io.Copy(io.Discard, io.LimitReader(q.Body, 4<<20))
		m.env.event("fault", "%s inference request withheld", m.provider)
		m.held <- "inference"
		select {
		case <-q.Context().Done():
		case <-time.After(3 * time.Minute):
		}
		m.env.event("fault", "%s withheld inference released: %v", m.provider, q.Context().Err())
		return
	}
	backend.ServeHTTP(w, q)
	if inference {
		m.mu.Lock()
		if len(m.sessions) > 0 {
			m.sessions[len(m.sessions)-1].Counts = backend.Counts()
		}
		m.mu.Unlock()
	}
}

func (m *e2eModel) sessionsCopy() []e2eModelSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]e2eModelSession(nil), m.sessions...)
}

// ---------------------------------------------------------------- fake GitHub

type e2ePR struct {
	Number                int
	ID, Author            int64
	Created               time.Time
	Head, Base, MergeBase string
	Comments              []map[string]any
	Reviews               []map[string]any
	ReviewComments        map[int64][]map[string]any
	ReviewPosts           int
}

type e2eGitHubSummary struct {
	Requests     map[string]int
	ReviewPosts  map[int]int
	Reviews      map[int][]map[string]any
	Mints        map[string]int
	GitExchanges []e2eGitExchange
	Violations   []string
}

type e2eGitExchange struct {
	At        time.Time
	Operation string
	Wants     []string
	Status    int
}

type e2eGitHub struct {
	t              *testing.T
	env            *e2eEnv
	server         *httptest.Server
	base           *url.URL
	mu             sync.Mutex
	epoch          time.Time
	prs            map[int]*e2ePR
	baseSHA        string
	gitDir, git    string
	tokens         map[string]string
	mints          map[string]int
	requests       map[string]int
	violations     []string
	exchanges      []e2eGitExchange
	nextID         atomic.Int64
	holdReviewPost bool
	heldPost       chan int
	commits        map[string]string
}

func e2eGit(t *testing.T, git string, dir string, args ...string) string {
	t.Helper()
	command := exec.Command(git, args...)
	command.Dir = dir
	command.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "LC_ALL=C", "GIT_AUTHOR_NAME=Synthetic", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Synthetic", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=2026-10-19T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-19T00:00:00Z"}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func newE2EGitHub(t *testing.T, env *e2eEnv) *e2eGitHub {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	g := &e2eGitHub{t: t, env: env, prs: map[int]*e2ePR{}, git: git, tokens: map[string]string{}, mints: map[string]int{}, requests: map[string]int{}, heldPost: make(chan int, 4), commits: map[string]string{}}
	g.nextID.Store(5000)
	// Synthetic upstream: base B, PR1 = B + source.go + pr1-only.go,
	// PR2 = B + source.go + pr2-only.go. Only source.go contains "immutable".
	// An attached second process builds its own identical deterministic
	// upstream (fixed contents and commit dates) beside the retained one.
	suffix := ""
	if env.attached {
		suffix = "2"
	}
	work := filepath.Join(env.root, "g"+suffix)
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	e2eGit(t, git, work, "init", "--quiet", "--initial-branch=main")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "synthetic base\n")
	e2eGit(t, git, work, "add", "README.md")
	e2eGit(t, git, work, "commit", "--quiet", "-m", "synthetic base")
	g.baseSHA = e2eGit(t, git, work, "rev-parse", "HEAD")
	for _, n := range []int{1, 2} {
		e2eGit(t, git, work, "checkout", "--quiet", "-b", "pr"+strconv.Itoa(n), g.baseSHA)
		write("source.go", e2eSource)
		write(fmt.Sprintf("pr%d-only.go", n), fmt.Sprintf("pull request %d only\n", n))
		e2eGit(t, git, work, "add", "source.go", fmt.Sprintf("pr%d-only.go", n))
		e2eGit(t, git, work, "commit", "--quiet", "-m", fmt.Sprintf("synthetic PR %d", n))
		g.commits[strconv.Itoa(n)] = e2eGit(t, git, work, "rev-parse", "HEAD")
	}
	g.gitDir = filepath.Join(env.root, "upstream"+suffix+".git")
	e2eGit(t, git, env.root, "init", "--quiet", "--bare", g.gitDir)
	e2eGit(t, git, work, "push", "--quiet", g.gitDir, "main:refs/heads/main", "pr1:refs/pull/1/head", "pr2:refs/pull/2/head")
	e2eGit(t, git, env.root, "--git-dir="+g.gitDir, "config", "uploadpack.allowReachableSHA1InWant", "true")
	now := time.Now().UTC().Truncate(time.Second)
	g.epoch = now.Add(-2 * time.Minute)
	// The trigger epoch is durable policy; a restarted process must reuse it.
	epochFile := filepath.Join(env.root, "epoch")
	if env.attached {
		raw, err := os.ReadFile(epochFile)
		seconds, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || parseErr != nil {
			t.Fatal("retained epoch", err, parseErr)
		}
		g.epoch = time.Unix(seconds, 0).UTC()
	} else if err := os.WriteFile(epochFile, []byte(strconv.FormatInt(g.epoch.Unix(), 10)), 0600); err != nil {
		t.Fatal(err)
	}
	g.server = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	g.base, _ = url.Parse(g.server.URL)
	return g
}

// addPR opens PR n. An automatic request needs creation at/after the epoch;
// a PR created before the epoch is reviewed only after an owner comment.
func (g *e2eGitHub) addPR(n int, author int64, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[n] = &e2ePR{Number: n, ID: int64(7000 + n), Author: author, Created: created.UTC().Truncate(time.Second), Head: g.commits[strconv.Itoa(n)], Base: g.baseSHA, MergeBase: g.baseSHA, Comments: []map[string]any{}, Reviews: []map[string]any{}, ReviewComments: map[int64][]map[string]any{}}
}
func (g *e2eGitHub) addComment(n int, author int64, body string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	at = at.UTC().Truncate(time.Second)
	issue := "https://api.github.com/repos/hemsoft-dev/set-it-free-loop/issues/" + strconv.Itoa(n)
	g.prs[n].Comments = append(g.prs[n].Comments, map[string]any{"id": g.nextID.Add(1), "user": map[string]any{"id": author}, "body": body, "issue_url": issue, "created_at": at, "updated_at": at})
}

type e2eGitHubRoute struct {
	g    *e2eGitHub
	role string
}

func (rt e2eGitHubRoute) RoundTrip(q *http.Request) (*http.Response, error) {
	g := rt.g
	git := q.URL.Host == "github.com" && strings.HasPrefix(q.URL.Path, "/hemsoft-dev/set-it-free-loop.git/")
	if q.URL.Scheme != "https" || (q.URL.Host != "api.github.com" && !git) {
		g.violation("transport %s reached destination %s", rt.role, q.URL.Host)
		return nil, errors.New("fake GitHub destination denied")
	}
	clone := q.Clone(q.Context())
	u := *q.URL
	u.Scheme, u.Host = g.base.Scheme, g.base.Host
	clone.URL, clone.Host = &u, g.base.Host
	clone.Header.Set("X-E2E-Transport", rt.role)
	return g.server.Client().Transport.RoundTrip(clone)
}
func (g *e2eGitHub) route(role string) http.RoundTripper { return e2eGitHubRoute{g, role} }

func (g *e2eGitHub) violation(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.violations = append(g.violations, fmt.Sprintf(format, args...))
}

var e2eWant = regexp.MustCompile(`want ([0-9a-f]{40})`)

func (g *e2eGitHub) serve(w http.ResponseWriter, q *http.Request) {
	transport := q.Header.Get("X-E2E-Transport")
	if strings.HasPrefix(q.URL.Path, "/hemsoft-dev/set-it-free-loop.git/") {
		g.serveGit(w, q, transport)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status, doc := g.api(q, transport)
	if status == 0 {
		return
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}

func (g *e2eGitHub) role(q *http.Request) string {
	auth := q.Header.Get("Authorization")
	if auth == "Bearer synthetic-app-jwt" {
		return "app"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokens[strings.TrimPrefix(auth, "Bearer ")]
}

func e2eRepo() map[string]any {
	return map[string]any{"id": r.RepositoryID, "full_name": "hemsoft-dev/set-it-free-loop", "owner": map[string]any{"id": int64(71), "login": "hemsoft-dev", "type": "Organization"}}
}
func e2eInstallation() map[string]any {
	return map[string]any{"id": e2eEnrollment.InstallationID, "app_id": e2eEnrollment.AppID, "target_id": int64(71), "target_type": "Organization", "account": map[string]any{"id": int64(71), "login": "hemsoft-dev", "type": "Organization"}, "repository_selection": "selected", "suspended_at": nil, "permissions": map[string]string{"metadata": "read", "contents": "read", "pull_requests": "write"}}
}

const e2eRepoPath = "/repos/hemsoft-dev/set-it-free-loop"

var e2ePRPath = regexp.MustCompile(`^/repos/hemsoft-dev/set-it-free-loop/(pulls|issues)/([0-9]+)(/.*)?$`)

func (g *e2eGitHub) api(q *http.Request, transport string) (int, any) {
	p := q.URL.Path
	role := g.role(q)
	g.mu.Lock()
	g.requests[role+" "+q.Method+" "+e2eRouteName(p)]++
	g.mu.Unlock()
	if transport != "reader" && transport != "publisher" {
		g.violation("request without a role transport %s %s", q.Method, p)
	}
	appRequest := p == "/app" || p == e2eRepoPath+"/installation" || strings.HasPrefix(p, "/app/installations/")
	if appRequest != (role == "app") {
		g.violation("credential crossed app/installation boundary on %s %s", q.Method, p)
		return 401, map[string]any{"message": "bad credential"}
	}
	if role == "" {
		g.violation("unknown credential on %s %s", q.Method, p)
		return 401, map[string]any{"message": "bad credential"}
	}
	if role != "app" && role != transport {
		g.violation("%s token used on %s transport for %s %s", role, transport, q.Method, p)
	}
	if q.Method != "GET" && role != "publisher" && !strings.HasSuffix(p, "/access_tokens") {
		g.violation("%s attempted a write %s %s", role, q.Method, p)
		return 403, map[string]any{"message": "write refused"}
	}
	switch {
	case p == "/app":
		return 200, map[string]any{"id": e2eEnrollment.AppID, "slug": "sfl-test"}
	case p == e2eRepoPath+"/installation", p == "/app/installations/9001" && q.Method == "GET":
		return 200, e2eInstallation()
	case p == "/app/installations/9001/access_tokens" && q.Method == "POST":
		var request struct {
			RepositoryIDs []int64 `json:"repository_ids"`
			Permissions   map[string]string
		}
		if json.NewDecoder(q.Body).Decode(&request) != nil || len(request.RepositoryIDs) != 1 || request.RepositoryIDs[0] != r.RepositoryID {
			g.violation("token request omitted the exact repository")
			return 422, map[string]any{}
		}
		minted := ""
		switch {
		case maps.Equal(request.Permissions, map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read"}):
			minted = "reader"
		case maps.Equal(request.Permissions, map[string]string{"metadata": "read", "pull_requests": "write"}):
			minted = "publisher"
		default:
			g.violation("token request widened permissions %v", request.Permissions)
			return 422, map[string]any{}
		}
		if minted != transport {
			g.violation("%s token minted through %s transport", minted, transport)
		}
		var nonce [8]byte
		_, _ = rand.Read(nonce[:])
		token := "synthetic-installation-" + minted + "-" + hex.EncodeToString(nonce[:])
		g.mu.Lock()
		g.tokens[token] = minted
		g.mints[minted]++
		g.mu.Unlock()
		return 201, map[string]any{"token": token, "expires_at": time.Now().UTC().Add(59 * time.Minute), "permissions": request.Permissions, "repository_selection": "selected", "repositories": []any{e2eRepo()}}
	case p == "/installation/repositories":
		return 200, map[string]any{"total_count": 1, "repositories": []any{e2eRepo()}}
	case p == "/users/sfl-test[bot]":
		return 200, map[string]any{"id": e2eEnrollment.BotID, "login": "sfl-test[bot]", "type": "Bot"}
	case p == e2eRepoPath:
		return 200, e2eRepo()
	case p == e2eRepoPath+"/pulls" && q.Method == "GET":
		if q.URL.RawQuery != "per_page=100&page=1" {
			return 400, map[string]any{}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		list := []any{}
		for _, n := range slicesSortedInts(g.prs) {
			list = append(list, g.pullDoc(g.prs[n]))
		}
		return 200, list
	case p == e2eRepoPath+"/git/ref/heads/main":
		return 200, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"type": "commit", "sha": g.baseSHA}}
	case strings.HasPrefix(p, e2eRepoPath+"/compare/"):
		g.mu.Lock()
		defer g.mu.Unlock()
		for _, pr := range g.prs {
			if p == e2eRepoPath+"/compare/"+pr.Base+"..."+pr.Head && q.URL.RawQuery == "per_page=1&page=2" {
				return 200, map[string]any{"base_commit": map[string]any{"sha": pr.Base}, "merge_base_commit": map[string]any{"sha": pr.MergeBase}}
			}
		}
		return 404, map[string]any{"message": "not found"}
	}
	m := e2ePRPath.FindStringSubmatch(p)
	if m == nil {
		g.violation("unexpected API destination %s %s", q.Method, p)
		return 404, map[string]any{"message": "not found"}
	}
	n, _ := strconv.Atoi(m[2])
	g.mu.Lock()
	pr := g.prs[n]
	g.mu.Unlock()
	if pr == nil {
		return 404, map[string]any{"message": "not found"}
	}
	rest := m[3]
	switch {
	case m[1] == "pulls" && rest == "" && q.Method == "GET":
		g.mu.Lock()
		defer g.mu.Unlock()
		return 200, g.pullDoc(pr)
	case m[1] == "issues" && rest == "/comments" && q.Method == "GET":
		g.mu.Lock()
		defer g.mu.Unlock()
		return 200, append([]map[string]any{}, pr.Comments...)
	case m[1] == "issues" && rest == "/comments" && q.Method == "POST":
		var body struct{ Body string }
		_ = json.NewDecoder(q.Body).Decode(&body)
		at := time.Now().UTC().Truncate(time.Second)
		c := map[string]any{"id": g.nextID.Add(1), "user": map[string]any{"id": e2eEnrollment.BotID}, "body": body.Body, "issue_url": "https://api.github.com" + e2eRepoPath + "/issues/" + strconv.Itoa(n), "created_at": at, "updated_at": at}
		g.mu.Lock()
		pr.Comments = append(pr.Comments, c)
		g.mu.Unlock()
		return 201, c
	case m[1] == "pulls" && rest == "/reviews" && q.Method == "GET":
		g.mu.Lock()
		defer g.mu.Unlock()
		return 200, append([]map[string]any{}, pr.Reviews...)
	case m[1] == "pulls" && rest == "/reviews" && q.Method == "POST":
		return g.postReview(q, pr)
	case m[1] == "pulls" && strings.HasPrefix(rest, "/reviews/"):
		parts := strings.Split(strings.TrimPrefix(rest, "/reviews/"), "/")
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		g.mu.Lock()
		defer g.mu.Unlock()
		var review map[string]any
		for _, v := range pr.Reviews {
			if v["id"].(int64) == id {
				review = v
			}
		}
		if review == nil {
			return 404, map[string]any{"message": "not found"}
		}
		if len(parts) == 2 && parts[1] == "comments" && q.Method == "GET" {
			return 200, append([]map[string]any{}, pr.ReviewComments[id]...)
		}
		if len(parts) == 1 && q.Method == "GET" {
			return 200, review
		}
		if len(parts) == 1 && q.Method == "PUT" {
			var body struct{ Body string }
			_ = json.NewDecoder(q.Body).Decode(&body)
			review["body"] = body.Body
			return 200, review
		}
	}
	g.violation("unexpected PR operation %s %s", q.Method, p)
	return 404, map[string]any{"message": "not found"}
}

func (g *e2eGitHub) postReview(q *http.Request, pr *e2ePR) (int, any) {
	var v struct {
		Event, Body string
		CommitID    string `json:"commit_id"`
		Comments    []struct {
			Path, Side, Body string
			Line             int
		}
	}
	raw, err := io.ReadAll(io.LimitReader(q.Body, 16<<20))
	if err != nil || json.Unmarshal(raw, &v) != nil || v.Event != "COMMENT" || v.CommitID != pr.Head {
		g.violation("review POST was not a COMMENT on the captured head")
		return 422, map[string]any{"message": "invalid"}
	}
	id := g.nextID.Add(1)
	submitted := time.Now().UTC().Truncate(time.Second)
	review := map[string]any{"id": id, "user": map[string]any{"id": e2eEnrollment.BotID}, "body": v.Body, "state": "COMMENTED", "commit_id": v.CommitID, "pull_request_url": "https://api.github.com" + e2eRepoPath + "/pulls/" + strconv.Itoa(pr.Number), "submitted_at": submitted}
	comments := []map[string]any{}
	for _, c := range v.Comments {
		comments = append(comments, map[string]any{"id": g.nextID.Add(1), "user": map[string]any{"id": e2eEnrollment.BotID}, "body": c.Body, "path": c.Path, "side": c.Side, "original_line": c.Line, "line": c.Line, "original_commit_id": v.CommitID, "pull_request_review_id": id})
	}
	g.mu.Lock()
	pr.ReviewPosts++
	pr.Reviews = append(pr.Reviews, review)
	pr.ReviewComments[id] = comments
	hold := g.holdReviewPost
	g.holdReviewPost = false
	g.mu.Unlock()
	g.env.event("github", "review POST %d to PR %d (%d inline) committed remotely", id, pr.Number, len(comments))
	if hold {
		// The write is visible remotely, but its acknowledgement is withheld
		// until the client gives up: an intent with an unknown outcome.
		g.heldPost <- pr.Number
		select {
		case <-q.Context().Done():
		case <-time.After(3 * time.Minute):
		}
		g.env.event("fault", "withheld review POST acknowledgement released: %v", q.Context().Err())
		return 0, nil
	}
	return 200, review
}

func (g *e2eGitHub) pullDoc(pr *e2ePR) map[string]any {
	return map[string]any{"id": pr.ID, "number": pr.Number, "state": "open", "user": map[string]any{"id": pr.Author}, "created_at": pr.Created, "updated_at": pr.Created, "head": map[string]any{"sha": pr.Head, "repo": map[string]any{"id": r.RepositoryID}}, "base": map[string]any{"ref": "main", "sha": pr.Base, "repo": map[string]any{"id": r.RepositoryID}}}
}

func slicesSortedInts[V any](m map[int]V) []int {
	out := []int{}
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

var e2eNumbers = regexp.MustCompile(`/[0-9]+`)

func e2eRouteName(p string) string {
	p = strings.TrimPrefix(p, e2eRepoPath)
	p = e2eNumbers.ReplaceAllString(p, "/N")
	if strings.HasPrefix(p, "/compare/") {
		return "/compare"
	}
	if p == "" {
		return "/repo"
	}
	return p
}

func (g *e2eGitHub) serveGit(w http.ResponseWriter, q *http.Request, transport string) {
	user, password, ok := q.BasicAuth()
	g.mu.Lock()
	role := g.tokens[password]
	g.mu.Unlock()
	if !ok || user != "x-access-token" || role != "reader" || transport != "reader" {
		g.violation("Git read without the reader credential (%s via %s)", role, transport)
		http.Error(w, "denied", http.StatusUnauthorized)
		return
	}
	advertisement := q.Method == http.MethodGet && q.URL.Path == "/hemsoft-dev/set-it-free-loop.git/info/refs" && q.URL.RawQuery == "service=git-upload-pack"
	upload := q.Method == http.MethodPost && q.URL.Path == "/hemsoft-dev/set-it-free-loop.git/git-upload-pack"
	if !advertisement && !upload {
		g.violation("unexpected Git operation %s %s", q.Method, q.URL.Path)
		http.Error(w, "denied", http.StatusNotFound)
		return
	}
	args := []string{"upload-pack", "--stateless-rpc"}
	if advertisement {
		args = append(args, "--advertise-refs")
	}
	command := exec.CommandContext(q.Context(), g.git, append(args, g.gitDir)...)
	command.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	protocol := q.Header.Get("Git-Protocol")
	if protocol != "" {
		command.Env = append(command.Env, "GIT_PROTOCOL="+protocol)
	}
	exchange := e2eGitExchange{At: time.Now().UTC(), Operation: "advertisement"}
	if upload {
		body, err := io.ReadAll(io.LimitReader(q.Body, 4<<20))
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		exchange.Operation = "upload-pack"
		for _, m := range e2eWant.FindAllStringSubmatch(string(body), -1) {
			exchange.Wants = append(exchange.Wants, m[1])
		}
		command.Stdin = bytes.NewReader(body)
	}
	data, err := command.Output()
	exchange.Status = 200
	if err != nil {
		exchange.Status = 500
	}
	g.mu.Lock()
	g.exchanges = append(g.exchanges, exchange)
	g.mu.Unlock()
	if err != nil {
		http.Error(w, "fixture Git failed", http.StatusInternalServerError)
		return
	}
	media := "application/x-git-upload-pack-result"
	if advertisement {
		media = "application/x-git-upload-pack-advertisement"
		if protocol != "version=2" {
			data = append([]byte("001e# service=git-upload-pack\n0000"), data...)
		}
	}
	w.Header().Set("Content-Type", media)
	_, _ = w.Write(data)
}

func (g *e2eGitHub) gitExchanges() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.exchanges)
}
func (g *e2eGitHub) reviewPosts(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.prs[n] == nil {
		return 0
	}
	return g.prs[n].ReviewPosts
}

func (g *e2eGitHub) summary() e2eGitHubSummary {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := e2eGitHubSummary{Requests: maps.Clone(g.requests), ReviewPosts: map[int]int{}, Reviews: map[int][]map[string]any{}, Mints: maps.Clone(g.mints), GitExchanges: append([]e2eGitExchange(nil), g.exchanges...), Violations: append([]string(nil), g.violations...)}
	for n, pr := range g.prs {
		s.ReviewPosts[n] = pr.ReviewPosts
		for _, review := range pr.Reviews {
			body, _ := review["body"].(string)
			// Keep the evidence bounded and nonsecret: marker line and size only.
			marker := regexp.MustCompile(`<!-- sfl-alpha:[0-9a-f]{32} -->`).FindString(body)
			inline := []string{}
			for _, c := range pr.ReviewComments[review["id"].(int64)] {
				inline = append(inline, fmt.Sprintf("%v:%v", c["path"], c["original_line"]))
			}
			s.Reviews[n] = append(s.Reviews[n], map[string]any{"id": review["id"], "state": review["state"], "commit_id": review["commit_id"], "marker": marker, "body_bytes": len(body), "inline": inline})
		}
	}
	return s
}

// ---------------------------------------------------------------- assertions

// assertReviewed checks the durable outcome, remote COMMENT and outbox state
// for one PR, and returns its job.
func (e *e2eEnv) assertReviewed(pr int) *r.DurableJob {
	d := e.state()
	j := e2eJobFor(d, pr)
	if j == nil {
		e.check(false, "PR %d has a durable job", pr)
		return nil
	}
	e.check(j.State == "dual_complete", "PR %d job %s reached dual_complete (got %s)", pr, j.Job.ID, j.State)
	e.check(j.Job.Config.Mode == r.Adversarial && len(j.Checkpoint.Artifacts) == 4 && j.Checkpoint.ConsumedSessions >= 4, "PR %d adversarial engine stored 4 artifacts (consumed %d)", pr, j.Checkpoint.ConsumedSessions)
	e.check(j.Job.Snapshot.Head == e.github.commits[strconv.Itoa(pr)] && j.Job.Release == e.options.Current, "PR %d job binds its own head and the current release", pr)
	providers := map[r.Provider]int{}
	for _, a := range j.Checkpoint.Artifacts {
		providers[a.Report.Provider]++
	}
	e.check(providers[r.Codex] == 2 && providers[r.Claude] == 2, "PR %d artifacts from both providers %v", pr, providers)
	writes := e2eWritesFor(d, pr, "review")
	confirmed := 0
	for _, w := range writes {
		if w.State == "confirmed" {
			confirmed++
			e.check(w.Snapshot.Head == j.Job.Snapshot.Head && w.ReceiptID > 0, "PR %d confirmed write binds the reviewed head and a receipt", pr)
			e.check(len(w.Document.Inline) >= 1 && w.Document.Inline[0].Path == "source.go", "PR %d document carries the source-backed inline finding", pr)
		}
	}
	e.check(confirmed == 1, "PR %d exactly one confirmed review write (%d writes)", pr, len(writes))
	e.check(e.github.reviewPosts(pr) == 1, "PR %d received exactly one review POST (got %d)", pr, e.github.reviewPosts(pr))
	summary := e.github.summary()
	reviews := summary.Reviews[pr]
	e.check(len(reviews) == 1 && reviews[0]["state"] == "COMMENTED" && reviews[0]["commit_id"] == j.Job.Snapshot.Head && reviews[0]["marker"] != "", "PR %d remote review is one marked COMMENT on its head", pr)
	return j
}

func (e *e2eEnv) assertSourceStopped(label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e.service == nil {
		return
	}
	records, exports, err := e.service.E2ESourceRecords(ctx)
	if err != nil {
		e.check(false, "%s: source records readable: %v", label, err)
		return
	}
	for _, rec := range records {
		e.check(rec.Stopped && rec.Revoked && rec.GrantHash != "", "%s: source attempt %s (unit %s) has a durable granted stop receipt", label, rec.Attempt, rec.Unit)
	}
	for _, x := range exports {
		e.check(x.State == "staged", "%s: retained export %s is staged (%s)", label, x.Attempt, x.State)
	}
	e.mu.Lock()
	e.result.SourceRecords = append(e.result.SourceRecords, records)
	e.result.Exports = append(e.result.Exports, exports)
	e.mu.Unlock()
}

func (e *e2eEnv) assertNativeSessions(job *r.DurableJob, expectComplete int) {
	units := e2eReadNativeUnits(e.native)
	complete := 0
	for _, u := range units {
		if u.Captured && u.Stopped && u.Complete && u.AuthReturned {
			complete++
		}
	}
	e.check(complete >= expectComplete, "at least %d native units completed with auth returned and whole-unit stop (got %d of %d)", expectComplete, complete, len(units))
}

// ---------------------------------------------------------------- scenarios

// review: automatic PR-open request, source VM capture, four native sessions,
// one COMMENT publication confirmed through the outbox.
func e2eReview(e *e2eEnv) {
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.open("initial")
	if !e.wait("PR 1 review confirmed", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	job := e.assertReviewed(1)
	e.assertSourceStopped("after review")
	e.assertNativeSessions(job, 4)
	d := e.state()
	request := d.PRs[e2eScopeKey(1)]
	e.check(request != nil && len(request.Requests) == 1 && request.Requests[0].Origin != nil && request.Requests[0].Origin.Kind == "opened", "PR 1 request came from an automatic PR-open observation")
	e.check(len(job.SourceAttempts) > 0, "job retained %d trusted source receipts", len(job.SourceAttempts))
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(job))
	e.collectWrites()
}

// isolation: PR 1 automatic and PR 2 owner-comment requests, both pending at
// once. The controller admits one invocation at a time; each job must see only
// its own snapshot objects and source attempt.
func e2eIsolation(e *e2eEnv) {
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.github.addPR(2, 4242, e.github.epoch.Add(-time.Minute))
	e.github.addComment(2, r.OwnerID, "@sfl review", e.github.epoch.Add(30*time.Second))
	e.open("initial")
	if !e.wait("both reviews confirmed", 9*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) && e2eConfirmed(d, 2) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	j1, j2 := e.assertReviewed(1), e.assertReviewed(2)
	if j1 == nil || j2 == nil {
		return
	}
	d := e.state()
	e.check(d.PRs[e2eScopeKey(2)].Requests[0].Origin.Kind == "comment", "PR 2 request came from the owner @sfl review comment")
	e.assertSourceStopped("after both reviews")
	e.assertNativeSessions(j1, 8)
	// Admission: sessions of the two jobs never interleave.
	sessions := append(e.models[r.Codex].sessionsCopy(), e.models[r.Claude].sessionsCopy()...)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CheckedAt.Before(sessions[j].CheckedAt) })
	switches := 0
	for i := 1; i < len(sessions); i++ {
		if sessions[i].JobID != sessions[i-1].JobID {
			switches++
		}
		e.check(sessions[i].Head == e.github.commits[strconv.Itoa(sessions[i].PR)], "session %d binds its own PR head", i)
	}
	e.check(len(sessions) == 8 && switches == 1, "eight sessions ran as two non-interleaved jobs (%d job switches)", switches)
	// Source attempts are distinct per job, and each upload-pack wanted only
	// that job's commits.
	records := e.result.SourceRecords[len(e.result.SourceRecords)-1]
	attempts := map[int]r.E2ESourceRecord{}
	for _, rec := range records {
		attempts[rec.Snapshot.PR] = rec
	}
	a1, a2 := attempts[1], attempts[2]
	e.check(a1.Attempt != "" && a2.Attempt != "" && a1.Attempt != a2.Attempt && a1.Unit != a2.Unit && a1.ChannelID != a2.ChannelID && a1.InvocationID != a2.InvocationID, "distinct source attempts, units, invocations and read channels per job")
	e.check(a1.Snapshot.Head == j1.Job.Snapshot.Head && a2.Snapshot.Head == j2.Job.Snapshot.Head, "each source attempt binds its own job snapshot")
	h1, h2 := e.github.commits["1"], e.github.commits["2"]
	mixed := 0
	for _, x := range e.github.summary().GitExchanges {
		if slices.Contains(x.Wants, h1) && slices.Contains(x.Wants, h2) {
			mixed++
		}
	}
	e.check(mixed == 0, "no upload-pack exchange wanted both PR heads")
	// Restore each job from retained bytes and probe the other job's objects.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	before := e.github.gitExchanges()
	for _, pair := range []struct {
		job          *r.DurableJob
		own, foreign string
		ownFile      string
		foreignFile  string
	}{{j1, h1, h2, "pr1-only.go", "pr2-only.go"}, {j2, h2, h1, "pr2-only.go", "pr1-only.go"}} {
		probes, err := e.service.E2ERestoreProbe(ctx, pair.job.Job, [][2]string{{pair.own, "source.go"}, {pair.own, pair.ownFile}, {pair.own, pair.foreignFile}, {pair.foreign, pair.foreignFile}, {pair.foreign, "source.go"}})
		if err != nil {
			e.check(false, "restore probe for PR %d: %v", pair.job.Scope.PR, err)
			continue
		}
		e.result.Probes[fmt.Sprintf("PR%d", pair.job.Scope.PR)] = probes
		e.check(probes[0].StoreOK && probes[0].ReaderOK && probes[1].StoreOK && probes[1].ReaderOK, "PR %d reads its own head objects", pair.job.Scope.PR)
		for _, p := range probes[2:] {
			e.check(!p.StoreOK && !p.ReaderOK, "PR %d cannot read foreign object %s:%s from its retained source", pair.job.Scope.PR, p.Revision[:12], p.Path)
		}
	}
	e.check(e.github.gitExchanges() == before, "restore probes used retained bytes only (no upstream exchange)")
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(j1), e2eJobSummary(j2))
	e.collectWrites()
}

// close-capture: the whole service closes after the source capture and job
// admission but before the first provider session can launch.
func e2eCloseCapture(e *e2eEnv) {
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.models[r.Codex].mu.Lock()
	e.models[r.Codex].holdCheck = true
	e.models[r.Codex].mu.Unlock()
	e.open("initial")
	select {
	case <-e.models[r.Codex].held:
	case <-time.After(5 * time.Minute):
		e.check(false, "first session check was never reached")
		return
	}
	e.snapshotJobs("held before first session")
	exchanges := e.github.gitExchanges()
	e.check(exchanges > 0, "source VM fetched from the fake upstream before review (%d exchanges)", exchanges)
	e.close("close-capture")
	d := e.reopenState()
	j := e2eJobFor(d, 1)
	e.check(j != nil && j.LeaseToken == "" && !j.Outstanding && j.Checkpoint.ConsumedSessions == 0 && len(j.Checkpoint.Artifacts) == 0, "closed-before-work job has no lease, no outstanding attempt and no consumed session")
	if j != nil {
		e.check(j.State == "queued", "closed-before-work job returned to queued (got %s)", j.State)
	}
	e.check(e.github.reviewPosts(1) == 0, "nothing was published before review")
	e.open("reopen")
	e.assertSourceStopped("after reopen recovery")
	if !e.wait("PR 1 review confirmed after reopen", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	job := e.assertReviewed(1)
	e.check(e.github.gitExchanges() == exchanges, "reopened service restored retained source without refetching (%d exchanges)", e.github.gitExchanges())
	e.assertNativeSessions(job, 4)
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(job))
	e.collectWrites()
}

// close-provider: the service closes while the first native session's pinned
// client waits for a withheld fake inference response. The unit must be
// proved stopped and the exported auth returned or fenced. If the attempt
// stays uncertain, its durable lease is retained and Close keeps reporting
// ErrWorkerStopPending; the root is then retained for close-provider-reopen,
// which a separate process runs as a restarted service.
func e2eCloseProvider(e *e2eEnv) {
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.models[r.Codex].mu.Lock()
	e.models[r.Codex].holdInference = true
	e.models[r.Codex].mu.Unlock()
	e.open("initial")
	select {
	case <-e.models[r.Codex].held:
	case <-time.After(5 * time.Minute):
		e.check(false, "first inference request was never reached")
		return
	}
	e.snapshotJobs("held during provider session")
	e.snapshotSource("close-provider before close")
	var closeErr error
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		started := time.Now()
		closeErr = e.service.Close(ctx)
		cancel()
		e.event("close", "close-provider Close attempt %d returned %v in %s", attempt, closeErr, time.Since(started).Round(time.Millisecond))
		if closeErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	e.runCancel()
	select {
	case runErr := <-e.runDone:
		e.result.RunErrors = append(e.result.RunErrors, fmt.Sprintf("close-provider: %v", runErr))
	case <-time.After(10 * time.Second):
		e.check(false, "Run did not return after Close")
	}
	e.wedged = closeErr != nil
	if !e.wedged {
		e.service = nil
	}
	e.check(closeErr == nil || errors.Is(closeErr, r.ErrWorkerStopPending), "Close either proved completion or reported ErrWorkerStopPending (%v)", closeErr)
	e.store.Close()
	e.store = nil
	d := e.reopenState()
	j := e2eJobFor(d, 1)
	if j == nil {
		e.check(false, "job exists after provider close")
		return
	}
	e.note("after provider-session close: job state %s, consumed %d, artifacts %d, lease %v, outstanding %v, provider attempts %d", j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), j.LeaseToken != "", j.Outstanding, len(j.ProviderAttempts))
	e.check(j.Checkpoint.ConsumedSessions == 1 && len(j.Checkpoint.Artifacts) == 0, "the interrupted session stays charged and produced no artifact")
	units := e2eReadNativeUnits(e.native)
	e.check(len(units) == 1 && units[0].Captured && units[0].Stopped && !units[0].Complete, "the interrupted native unit was captured and whole-unit stopped without completion")
	returned := len(units) == 1 && units[0].AuthReturned
	for _, u := range units {
		e.note("interrupted native unit %s: audit %v complete %v exported %v returned %v", u.Launch, u.AuditPresent, u.Complete, u.AuthExported, u.AuthReturned)
	}
	usable := map[r.Provider]bool{r.Codex: e2eAuthUsable(e.auth, r.Codex), r.Claude: e2eAuthUsable(e.auth, r.Claude)}
	e.note("auth disposition after close: codex usable=%v claude usable=%v", usable[r.Codex], usable[r.Claude])
	e.check(usable[r.Claude], "the untouched provider's auth remains usable")
	e.check(usable[r.Codex] == returned, "codex auth is usable exactly when its stopped return was committed (returned %v)", returned)
	e.check(e.github.reviewPosts(1) == 0, "nothing was published for the interrupted job")
	if !e.wedged {
		e.check(j.LeaseToken == "" && j.State == "queued" && returned, "a completed Close leaves a queued job with returned auth (state %s)", j.State)
		e.open("reopen")
		e.wait("PR 1 review after reopen", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) })
		e.snapshotJobs("after reopen")
		e.collectWrites()
		return
	}
	e.check(j.LeaseToken != "" && j.Outstanding && j.State == "running", "the uncertain provider attempt keeps its durable lease and outstanding reservation")
	e.check(!returned && !usable[r.Codex], "the unreturned exported auth is fenced for recovery")
	// ReleaseStopped is the explicit trusted-supervisor path. Probe it on a
	// private copy: it is accepted only with durably recorded stopped
	// provider evidence, and the service itself never calls it.
	err := e2eProbeRelease(e, j)
	e.note("ReleaseStopped(auth_failed) on a private copy of the retained state: %v", err)
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(j))
	e.keepRoot = true
	e.note("retained root %s for close-provider-reopen; this process exits holding the wedged handle", e.root)
}

// close-provider-reopen is a fresh process attached to the retained root. The
// restarted service must prove the source unit stopped, discover new requests
// and admit nothing while the uncertain lease remains, including a new PR.
func e2eCloseProviderReopen(e *e2eEnv) {
	if !e.attached {
		e.t.Fatal("close-provider-reopen requires SFL_E2E_ATTACH")
	}
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.github.addPR(2, 4242, e.github.epoch.Add(-time.Minute))
	e.github.addComment(2, r.OwnerID, "@sfl review", time.Now().Add(-time.Second))
	before := e2eJobFor(e.reopenState(), 1)
	if before == nil || before.LeaseToken == "" {
		e.t.Fatal("attached root has no retained lease")
	}
	codexFenced := !e2eAuthUsable(e.auth, r.Codex)
	e.check(codexFenced, "codex auth is still fenced in the restarted process")
	e.open("restart")
	e.assertSourceStopped("after restart recovery")
	e.waitFor("PR 2 comment request discovered", time.Minute, true, func(d r.DurableState) bool {
		pr := d.PRs[e2eScopeKey(2)]
		return pr != nil && len(pr.Requests) == 1
	})
	e.waitFor("any admission while leased (expected none)", 30*time.Second, false, func(d r.DurableState) bool {
		return len(d.Jobs) > 1 || e2eJobFor(d, 1).LeaseToken != before.LeaseToken
	})
	d := e.state()
	final := e2eJobFor(d, 1)
	sessions := len(e.models[r.Codex].sessionsCopy()) + len(e.models[r.Claude].sessionsCopy())
	e.check(len(d.Jobs) == 1 && final.LeaseToken == before.LeaseToken && final.Outstanding && final.Checkpoint.ConsumedSessions == before.Checkpoint.ConsumedSessions, "the retained lease fenced every admission after restart, including PR 2")
	e.check(sessions == 0, "no provider session started after restart (%d)", sessions)
	e.check(e.github.reviewPosts(1) == 0 && e.github.reviewPosts(2) == 0, "nothing was published while fenced")
	e.check(e.github.gitExchanges() == 0, "no source capture ran while fenced")
	e.check(!e2eAuthUsable(e.auth, r.Codex) && e2eAuthUsable(e.auth, r.Claude), "codex auth stays fenced; claude auth stays usable")
	e.snapshotJobs("fenced after restart")
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(final))
	e.wedged = true
	e.close("final")
}

func e2eAuthUsable(directory string, provider r.Provider) bool {
	auth, err := r.NewAuthStore(provider, directory)
	return err == nil && auth.Refresh(context.Background(), func(current []byte) ([]byte, error) {
		if len(current) == 0 {
			return nil, errors.New("missing returned synthetic auth")
		}
		return current, nil
	}) == nil
}

// e2eProbeRelease attempts ReleaseStopped on a private copy of the store.
func e2eProbeRelease(e *e2eEnv, j *r.DurableJob) error {
	directory := filepath.Join(e.root, "probe")
	if err := os.Mkdir(directory, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	raw, err := os.ReadFile(e.storePath)
	if err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(e.storePath + suffix); err == nil {
			return errors.New("store not checkpointed")
		}
	}
	copied := filepath.Join(directory, "state.db")
	if err := os.WriteFile(copied, raw, 0600); err != nil {
		return err
	}
	store, err := r.OpenStore(copied, e2eEnrollment, e.config)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.ReleaseStopped(context.Background(), j.Job.ID, j.LeaseToken, "auth_failed")
}

// close-publish: the review POST is committed remotely but its response is
// withheld; the service closes, reopens and must reconcile without a second POST.
func e2eClosePublish(e *e2eEnv) {
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.github.mu.Lock()
	e.github.holdReviewPost = true
	e.github.mu.Unlock()
	e.open("initial")
	select {
	case <-e.github.heldPost:
	case <-time.After(6 * time.Minute):
		e.check(false, "review POST was never reached")
		return
	}
	e.snapshotJobs("held review POST")
	e.close("close-publish")
	d := e.reopenState()
	writes := e2eWritesFor(d, 1, "review")
	unknown := 0
	for _, w := range writes {
		if w.State == "unknown" || w.State == "dispatched" {
			unknown++
		}
	}
	e.check(len(writes) == 1 && unknown == 1, "the closed POST left exactly one unresolved intent (%d writes)", len(writes))
	e.check(e.github.reviewPosts(1) == 1, "exactly one POST reached GitHub before close")
	e.open("reopen")
	if !e.wait("PR 1 write reconciled", 3*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("reconciled")
	job := e.assertReviewed(1)
	e.check(e.github.reviewPosts(1) == 1, "reconciliation after reopen never POSTed again")
	e.assertSourceStopped("after reconciliation")
	e.assertNativeSessions(job, 4)
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(job))
	e.collectWrites()
}

// reopenState reads the closed service's store through a fresh handle.
func (e *e2eEnv) reopenState() r.DurableState {
	store, err := r.OpenStore(e.storePath, e2eEnrollment, e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	defer store.Close()
	d, err := store.Snapshot(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *e2eEnv) collectWrites() {
	d := e.state()
	for _, id := range slices.Sorted(maps.Keys(d.Writes)) {
		w := d.Writes[id]
		e.result.Writes = append(e.result.Writes, e2eWrite{ID: w.ID, Kind: w.Kind, State: w.State, PR: w.Scope.PR, ReceiptID: w.ReceiptID, Inline: len(w.Document.Inline), Head: w.Snapshot.Head})
	}
}

func e2eScopeKey(pr int) string {
	return fmt.Sprintf("%d/%d/%d", r.RepositoryID, e2eEnrollment.InstallationID, pr)
}

func e2eJobSummary(j *r.DurableJob) e2eJob {
	s := e2eJob{ID: j.Job.ID, State: j.State, PR: j.Scope.PR, Head: j.Job.Snapshot.Head, RequestID: j.RequestID, Consumed: j.Checkpoint.ConsumedSessions, Artifacts: len(j.Checkpoint.Artifacts), Lease: j.LeaseToken != "", Outstanding: j.Outstanding, SourceAttempts: len(j.SourceAttempts), ProviderAttempts: len(j.ProviderAttempts)}
	for _, a := range j.Checkpoint.Artifacts {
		for _, finding := range a.Report.Findings {
			s.LedgerFindings = append(s.LedgerFindings, fmt.Sprintf("%s R%d %s %s:%d", a.Report.Provider, a.Report.Round, finding.ID, finding.Anchor.Path, finding.Anchor.Line))
		}
	}
	return s
}

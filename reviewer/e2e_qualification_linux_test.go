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
	"runtime"
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
	TestBinary                    r.E2EBuildStamp
	NativeImageCommit             string
	Passed                        bool
	ElapsedSeconds                float64
	Checks                        []string
	Failures                      []string
	Timeline                      []e2eEvent
	Jobs                          []e2eJob
	Requests                      []e2eRequest
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
	UnitListingError              string
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
	t                              testing.TB
	scenario                       string
	root                           string
	meta, unit, export, work, auth string
	native                         map[r.Provider]string
	policies                       map[r.Provider]r.EgressPolicy
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
	// servers are the fake GitHub and model backends. cleanup closes them
	// only after the service, which may still be using them.
	servers  []*httptest.Server
	finished bool
	attached bool
	// wedged marks a handle whose Close is expected to keep reporting
	// ErrWorkerStopPending; keepRoot retains the root for an attached phase.
	wedged, keepRoot                  bool
	sourceImageCommit, nativeRevision string
	// expect is the case's declared end state; nativeBefore holds native
	// launches already present in an attached root.
	expect       e2eExpect
	nativeBefore map[string]bool
}

// e2eExpect is a case's declared end state: the transient units this process
// must start (source acquisitions and native launches) and whether both
// provider auth stores must end usable. Fence cases check their own auth.
type e2eExpect struct {
	SourceUnits, NativeUnits int
	AuthUsable               bool
}

var e2eExpectations = map[string]e2eExpect{
	"review": {1, 4, true}, "isolation": {2, 8, true}, "close-capture": {1, 4, true}, "close-publish": {1, 4, true},
	"close-provider": {1, 1, false}, "close-provider-reopen": {0, 0, false},
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
	e := e2eAllocate(t, scenario, os.Getenv("SFL_E2E_ATTACH"), e2eListUnits)
	root := e.root
	// Release: version 2 source VM package plus both native providers.
	vm, commit := r.E2ESourceVMRelease(t, sourceImage, sourceStub)
	e.sourceImageCommit = commit
	e.result.SourceImageCommit = commit
	// The running test binary is the harness that judges the run; it must be
	// an unmodified build of the release commit, not merely the same name.
	stamp, err := r.E2ETestBinaryStamp(commit)
	e.result.TestBinary = stamp
	if err != nil {
		t.Fatal(err)
	}
	config := r.DefaultConfig()
	config.Limits.Timeout = 90 * time.Second
	e.config = config
	release := r.GitReviewerRelease{Version: 2, Revision: commit, SourceVM: &vm, Providers: map[r.Provider]r.GitReviewerProviderRelease{}}
	manifest := e2eNativeManifest(t, nativeImage)
	e.nativeRevision = manifest.Commit
	e.result.NativeImageCommit = manifest.Commit
	if manifest.Commit != commit {
		t.Fatalf("native image commit %s differs from the source image and release commit %s", manifest.Commit, commit)
	}
	hosts := map[r.Provider]r.GitReviewerProviderHost{}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		identity, _ := r.PinnedIdentity(provider)
		probe, err := r.NewSession("e2e-policy-probe", r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}, config.Hash(), provider, 1, "probe", nil, nil, config.Limits)
		if err != nil {
			t.Fatal(err)
		}
		release.Providers[provider] = r.GitReviewerProviderRelease{Identity: identity, Stub: manifest.Stub, QEMU: manifest.QEMU, Kernel: manifest.Kernel, Initrd: manifest.Initrd, Firmware: manifest.Firmware, Runtime: manifest.Runtime, Policy: f.Policy(probe)}
		e.policies[provider] = release.Providers[provider].Policy
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
	// An attached root keeps the earlier process's native launches.
	// An attached root's retained launch is proved, and only then treated as
	// pre-existing, once the retained job is read (e2eCloseProviderReopen).
	e.expect, e.nativeBefore = e2eExpectations[scenario], map[string]bool{}
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
	return e
}

// e2eAllocate creates a fresh run root, or adopts the retained root named by
// attach, and starts the unit observer. Cleanup is registered as soon as the
// root is owned, before the observer starts, and tolerates a partially
// initialized environment: a later t.Fatal in setup still stops the observer
// and removes a fresh root.
func e2eAllocate(t testing.TB, scenario, attach string, list func() (map[string]string, error)) *e2eEnv {
	t.Helper()
	// An earlier result for this case must not outlive a run that later
	// fails to write its own; a run that cannot remove it does not start.
	if err := e2eClearEvidence(os.Getenv("SFL_E2E_EVIDENCE"), scenario); err != nil {
		t.Fatal("earlier E2E result could not be removed", err)
	}
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
	e := &e2eEnv{t: t, scenario: scenario, root: root, attached: attach != "", native: map[r.Provider]string{}, policies: map[r.Provider]r.EgressPolicy{}, models: map[r.Provider]*e2eModel{}, result: &e2eResult{Case: scenario, Root: root, Probes: map[string][]r.E2EProbe{}, Models: map[r.Provider][]e2eModelSession{}, AuthRecoveryClear: map[string]bool{}, FakeOnly: true}}
	if attach == "" {
		// The fresh root is owned from here: any later setup failure removes
		// it (unless SFL_E2E_KEEP_FAILED) and seals partial evidence.
		t.Cleanup(e.cleanup)
	}
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
	if attach != "" {
		// A retained root is adopted only once its layout is proved.
		t.Cleanup(e.cleanup)
	}
	// cleanup stops the observer, so it never outlives a failed setup.
	e.units = newE2EUnitObserver(t, list)
	return e
}

type e2eNativeImage struct {
	Commit                               string
	QEMU, Stub, Kernel, Initrd, Firmware r.VMBootAsset
	Runtime                              r.NativeVMRuntimeSpec
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
		KernelSHA256   string            `json:"kernel_sha256"`
		InitrdSHA256   string            `json:"initrd_sha256"`
		InitrdBytes    int64             `json:"initrd_bytes"`
		BIOSSHA256     string            `json:"bios_sha256"`
		Programs       map[string]string `json:"programs"`
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
	// Every release asset must match the pin the image build recorded. The
	// release is built from these verified hashes, never from whatever bytes
	// happen to sit in the directory now.
	pinned := func(name, sha string, size int64) r.VMBootAsset {
		asset := e2eHash(filepath.Join(image, name))
		if len(sha) != 64 || asset.SHA256 != sha || (size != 0 && asset.Bytes != size) {
			t.Fatalf("native image asset %s does not match its recorded pin", name)
		}
		return asset
	}
	stub := pinned("nativevmstub", m.Programs["nativevmstub"], 0)
	kernel := pinned("vmlinuz", m.KernelSHA256, 0)
	initrd := pinned("native.cpio.gz", m.InitrdSHA256, m.InitrdBytes)
	firmware := pinned("bios.bin", m.BIOSSHA256, 0)
	return e2eNativeImage{Commit: m.Commit, QEMU: qemu, Stub: stub, Kernel: kernel, Initrd: initrd, Firmware: firmware, Runtime: r.NativeVMRuntimeSpec{Artifact: artifact, NamespaceHelper: m.Host.Helper, BootstrapLibraries: m.Host.Dependencies}}
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
		e.check(e2eRunErrorExpected(runErr, e.wedged), "%s: Run returned only the expected shutdown result (wedged %v): %v", label, e.wedged, runErr)
	case <-time.After(10 * time.Second):
		e.check(false, "%s: Run did not return after Close", label)
	}
	e.runCancel()
	if err == nil {
		e.service = nil
	}
	e.snapshotJobs(label + " after close")
	if e.store != nil {
		e.closeStore(label, e.store)
		e.store = nil
	}
}

// closeStore closes a store handle and records a failed close or checkpoint
// as a failed check, which forbids a persisted pass. database/sql marks the
// handle closed even when Close fails and a repeated Close reports nil, so the
// failure is recorded here, before the caller drops the handle, rather than
// left to a retry that could not see it.
func (e *e2eEnv) closeStore(label string, store io.Closer) {
	e.t.Helper()
	if err := store.Close(); err != nil {
		e.check(false, "%s: store Close completed the SQLite close and checkpoint: %v", label, err)
	}
}

// e2eRunErrorExpected accepts only the cancellation that Close causes, plus
// ErrWorkerStopPending when the case expects a retained uncertain lease.
// Any other joined cause is an unexpected Run failure.
func e2eRunErrorExpected(err error, wedged bool) bool {
	if !errors.Is(err, context.Canceled) || errors.Is(err, r.ErrWorkerStopPending) != wedged {
		return false
	}
	var leaves func(error) bool
	leaves = func(err error) bool {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if !leaves(cause) {
					return false
				}
			}
			return true
		}
		return err == context.Canceled || (wedged && err == r.ErrWorkerStopPending)
	}
	return leaves(err)
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

// finish proves no unit of this run remains, correlates the observed units
// with recorded launches, and checks auth disposition and native unit
// receipts. cleanup writes the evidence record after releasing the run.
func (e *e2eEnv) finish() {
	e.finished = true
	if e.service != nil && !e.wedged {
		e.close("final")
	}
	var listing error
	e.result.SystemdUnits, e.result.ResidualSystemdUnits, listing = e.units.stop(30 * time.Second)
	if listing != nil {
		e.result.UnitListingError = listing.Error()
	}
	e.event("systemd", "observed %d distinct sfl-alpha units (source+native)", len(e.result.SystemdUnits))
	e.result.NativeUnits = e2eReadNativeUnits(e.native, e.policies)
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		e.result.AuthRecoveryClear[string(provider)] = e2eAuthUsable(e.auth, provider)
	}
	e.result.GitHub = e.github.summary()
	for provider, model := range e.models {
		e.result.Models[provider] = model.sessionsCopy()
	}
	for _, v := range e2eFinalVerdicts(e.result, e.expect, e.nativeBefore, e.attached) {
		e.check(v.OK, "%s", v.Message)
	}
}

type e2eVerdict struct {
	OK      bool
	Message string
}

// e2eFinalVerdicts evaluates the end-of-run evidence against the case's
// declared expectation. before holds native launches retained from an
// earlier process; attached marks such a retained root.
func e2eFinalVerdicts(res *e2eResult, want e2eExpect, before map[string]bool, attached bool) []e2eVerdict {
	verdicts := []e2eVerdict{
		{res.UnitListingError == "", fmt.Sprintf("the final sfl-alpha unit listing succeeded (%q)", res.UnitListingError)},
		{len(res.ResidualSystemdUnits) == 0, fmt.Sprintf("no sfl-alpha unit started by this run remains (%d observed, residual %v)", len(res.SystemdUnits), res.ResidualSystemdUnits)},
	}
	for _, u := range res.NativeUnits {
		verdicts = append(verdicts, e2eVerdict{u.Captured && u.Stopped, fmt.Sprintf("native unit %s captured and stopped", u.Launch)})
	}
	verdicts = append(verdicts, e2eVerdict{len(res.GitHub.Violations) == 0, fmt.Sprintf("fake GitHub saw no role, destination or credential violation: %v", res.GitHub.Violations)})
	verdicts = append(verdicts, e2eCorrelateUnits(res, want, before, attached)...)
	if want.AuthUsable {
		for _, provider := range []r.Provider{r.Codex, r.Claude} {
			verdicts = append(verdicts, e2eVerdict{res.AuthRecoveryClear[string(provider)], fmt.Sprintf("%s auth store is usable after the run", provider)})
		}
	}
	return verdicts
}

// e2eCorrelateUnits matches every transient unit the observer saw against
// the launches this process recorded: source units by the owner journal's
// unit name, native units as sfl-alpha-<launch>.service for each launch
// directory created by this process. Counts must equal the case's
// expectation. On a fresh root every recorded source attempt is this
// process's, so recorded and observed source units must coincide; an attached
// root may also hold the earlier process's attempts.
func e2eCorrelateUnits(res *e2eResult, want e2eExpect, before map[string]bool, attached bool) []e2eVerdict {
	sources, launches := map[string]bool{}, map[string]bool{}
	for _, snapshot := range res.SourceRecords {
		for _, rec := range snapshot {
			sources[rec.Unit] = true
		}
	}
	present := map[string]bool{}
	for _, u := range res.NativeUnits {
		present[u.Launch] = true
		if !before[u.Launch] {
			launches["sfl-alpha-"+u.Launch+".service"] = true
		}
	}
	vanished := []string{}
	for launch := range before {
		if !present[launch] {
			vanished = append(vanished, launch)
		}
	}
	sort.Strings(vanished)
	observedSource, observedNative, uncorrelated := map[string]bool{}, map[string]bool{}, []string{}
	for _, u := range res.SystemdUnits {
		switch {
		case u.Source && sources[u.Name]:
			observedSource[u.Name] = true
		case !u.Source && launches[u.Name]:
			observedNative[u.Name] = true
		default:
			uncorrelated = append(uncorrelated, u.Name)
		}
	}
	unobserved := []string{}
	for name := range launches {
		if !observedNative[name] {
			unobserved = append(unobserved, name)
		}
	}
	if !attached {
		for name := range sources {
			if !observedSource[name] {
				unobserved = append(unobserved, name)
			}
		}
	}
	sort.Strings(unobserved)
	return []e2eVerdict{
		{len(uncorrelated) == 0, fmt.Sprintf("every observed sfl-alpha unit is a recorded source attempt or native launch of this run (uncorrelated %v)", uncorrelated)},
		{len(unobserved) == 0, fmt.Sprintf("every source attempt and native launch this run recorded was observed as a transient unit (unobserved %v)", unobserved)},
		{len(observedSource) == want.SourceUnits && len(observedNative) == want.NativeUnits, fmt.Sprintf("observed %d source and %d native units, case expects %d and %d", len(observedSource), len(observedNative), want.SourceUnits, want.NativeUnits)},
		{len(vanished) == 0, fmt.Sprintf("every native launch retained from the earlier process is still present (vanished %v)", vanished)},
	}
}

// seal records the pass flag and writes the nonsecret evidence record.
func (e *e2eEnv) seal() {
	e.mu.Lock()
	e.result.Passed = e.finished && !e.t.Failed() && len(e.result.Failures) == 0
	data, _ := json.MarshalIndent(e.result, "", "  ")
	e.mu.Unlock()
	if directory := os.Getenv("SFL_E2E_EVIDENCE"); directory != "" {
		if err := e2eWriteEvidence(directory, e.scenario, append(data, '\n')); err != nil {
			e.t.Error("evidence", err)
		}
	}
	e.report()
}

// e2eClearEvidence removes an earlier result for scenario before a run, so a
// run that later cannot write its own result leaves no result at all.
func e2eClearEvidence(directory, scenario string) error {
	if directory == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(directory, scenario+".json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return e2eSyncDirectory(directory)
}

// e2eWriteEvidence replaces <scenario>.json whole: a private temporary in the
// same directory is written, synced and renamed over the result, and the
// directory is synced. A failure leaves neither a partial result nor the
// temporary, and a result whose rename is not durable is removed again.
func e2eWriteEvidence(directory, scenario string, data []byte) (err error) {
	file, err := os.CreateTemp(directory, "."+scenario+".json.*")
	if err != nil {
		return err
	}
	temp, result := file.Name(), filepath.Join(directory, scenario+".json")
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(temp))
		}
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, result); err != nil {
		return err
	}
	if err = e2eSyncDirectory(directory); err != nil {
		return errors.Join(err, os.Remove(result))
	}
	return nil
}

func e2eSyncDirectory(directory string) error {
	d, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
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

// cleanup releases the run and only then seals the evidence, so a failed
// close or root removal can never leave a persisted pass.
func (e *e2eEnv) cleanup() {
	defer e.seal()
	defer func() {
		for _, server := range slices.Backward(e.servers) {
			server.Close()
		}
	}()
	if !e.finished && e.units != nil {
		// A fatal path skipped finish: keep its timeline and partial evidence.
		if _, residual, err := e.units.stop(30 * time.Second); len(residual) != 0 || err != nil {
			e.t.Error("sfl-alpha units may remain after an unfinished run", residual, err)
		}
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
		if err := e.store.Close(); err != nil {
			e.t.Error("close retained store", err)
		}
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
	list     func() (map[string]string, error)
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

// newE2EUnitObserver records the units list reports before the run, then
// samples it every 200 ms until stop.
func newE2EUnitObserver(t testing.TB, list func() (map[string]string, error)) *e2eUnitObserver {
	before, err := list()
	if err != nil {
		t.Fatal("user systemd unavailable", err)
	}
	o := &e2eUnitObserver{list: list, before: map[string]bool{}, seen: map[string]*e2eSystemdUnit{}, done: make(chan struct{}), joined: make(chan struct{})}
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
func (o *e2eUnitObserver) sample() (map[string]string, error) {
	units, err := o.list()
	if err != nil {
		return nil, err
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
	return units, nil
}

// stop waits until a successful listing shows no unit first seen by this run,
// then reports. A failed listing is never read as an empty one: it keeps the
// last successful residual (or, before any, every unit this run saw) and,
// when the grace ends without a later successful listing, returns its error.
func (o *e2eUnitObserver) stop(grace time.Duration) ([]e2eSystemdUnit, []string, error) {
	deadline := time.Now().Add(grace)
	residual, listed := []string{}, false
	var listing error
	for {
		units, err := o.sample()
		o.mu.Lock()
		switch {
		case err == nil:
			listing, listed, residual = nil, true, residual[:0]
			for name := range units {
				if !o.before[name] {
					residual = append(residual, name+" "+units[name])
				}
			}
		case !listed:
			listing, residual = err, residual[:0]
			for name, u := range o.seen {
				residual = append(residual, name+" last seen "+strings.Join(u.States, ","))
			}
		default:
			listing = err
		}
		o.mu.Unlock()
		if listing == nil && len(residual) == 0 || time.Now().After(deadline) {
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
	sort.Strings(residual)
	if listing != nil {
		listing = fmt.Errorf("final sfl-alpha unit listing failed: %w", listing)
	}
	return all, append([]string(nil), residual...), listing
}

// ---------------------------------------------------------------- native units

type e2eNativeUnit struct {
	Provider, Launch, SessionHash        string
	Captured, Stopped                    bool
	Complete, AuthExported, AuthReturned bool
	AuditPresent                         bool
}

// e2eReadNativeUnits projects every native launch directory under roots,
// judging each provider's audits against that provider's release policy.
func e2eReadNativeUnits(roots map[r.Provider]string, policies map[r.Provider]r.EgressPolicy) []e2eNativeUnit {
	units := []e2eNativeUnit{}
	for provider, root := range roots {
		paths, _ := filepath.Glob(filepath.Join(root, "*", "unit.json"))
		for _, path := range paths {
			unit, _ := os.ReadFile(path)
			audit, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "native-audit.json"))
			units = append(units, e2eNativeUnitFrom(provider, policies[provider], filepath.Base(filepath.Dir(path)), unit, audit))
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Launch < units[j].Launch })
	return units
}

// e2eNativeUnitFrom projects one native launch directory's unit journal and
// stopped audit receipt. The journal counts only as version 1 for this
// directory's launch: it names sfl-alpha-<launch>.service and its binding
// derives the containment ID from its session and launch. The audit counts
// (AuditPresent) only as a version 1 receipt repeating that exact binding.
// Its provider evidence, required once auth was exported or the session
// completed, must be the stopped receipt of the same session under the
// release's policy. A copied or foreign audit therefore proves nothing.
func e2eNativeUnitFrom(provider r.Provider, policy r.EgressPolicy, launch string, unitRaw, auditRaw []byte) e2eNativeUnit {
	u := e2eNativeUnit{Provider: string(provider), Launch: launch}
	var unit struct {
		Version           int
		Unit              string
		Captured, Stopped bool
		Binding           r.AuthBinding
	}
	if unitRaw == nil || json.Unmarshal(unitRaw, &unit) != nil {
		return u
	}
	b := unit.Binding
	if unit.Version != 1 || b.LaunchID != launch || unit.Unit != "sfl-alpha-"+launch+".service" || len(b.SessionHash) != 64 || b.ContainmentID != r.Digest(struct{ Session, Launch string }{b.SessionHash, launch}) {
		return u
	}
	u.SessionHash, u.Captured, u.Stopped = b.SessionHash, unit.Captured, unit.Stopped
	var audit struct {
		Version                              int
		Binding                              r.AuthBinding
		Provider                             *r.ProviderEvidence
		Complete, AuthExported, AuthReturned bool
	}
	if auditRaw == nil || json.Unmarshal(auditRaw, &audit) != nil || audit.Version != 1 || audit.Binding != b {
		return u
	}
	if evidence := audit.Provider; evidence != nil {
		if evidence.Policy.Provider != provider || r.Digest(evidence.Policy) != r.Digest(policy) || evidence.Access.Version != 1 || evidence.Access.SessionHash != b.SessionHash || evidence.Access.PolicyHash != r.Digest(policy) || !evidence.Access.Stopped {
			return u
		}
	} else if audit.Complete || audit.AuthExported {
		return u
	}
	u.AuditPresent, u.Complete, u.AuthExported, u.AuthReturned = true, audit.Complete, audit.AuthExported, audit.AuthReturned
	return u
}

// e2eRetainedLaunches proves the native launch a root attached after
// close-provider must hold, and returns it as the only pre-existing launch:
// exactly one launch, captured and whole-unit stopped, with a bound stopped
// audit recording the auth as exported, not returned and the session not
// complete. Its provider and session must be the retained job's outstanding
// attempt, and the job must hold that attempt's durably recorded stopped
// provider evidence. A missing or unreadable launch directory yields no
// launch and is refused, so the fence cannot pass without its interrupted
// launch.
func e2eRetainedLaunches(units []e2eNativeUnit, j *r.DurableJob) (map[string]bool, error) {
	if j == nil {
		return nil, errors.New("no retained job")
	}
	if len(units) != 1 {
		return nil, fmt.Errorf("%d native launches retained, want the one interrupted launch", len(units))
	}
	u := units[0]
	if !u.Captured || !u.Stopped || !u.AuditPresent || !u.AuthExported || u.AuthReturned || u.Complete {
		return nil, fmt.Errorf("retained launch %s lacks a captured whole-unit stop with a stopped audit of exported, unreturned auth: %+v", u.Launch, u)
	}
	if !j.Outstanding || u.Provider != string(j.AttemptProvider) || u.SessionHash != j.AttemptInputHash {
		return nil, fmt.Errorf("retained launch %s is not job %s's outstanding attempt", u.Launch, j.Job.ID)
	}
	for _, a := range j.ProviderAttempts {
		if a.ID == j.AttemptID && a.Evidence != nil && a.Evidence.Access.Stopped && a.Evidence.Access.SessionHash == u.SessionHash {
			return map[string]bool{u.Launch: true}, nil
		}
	}
	return nil, fmt.Errorf("job %s holds no stopped provider evidence for retained launch %s", j.Job.ID, u.Launch)
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
	env.servers = append(env.servers, m.server)
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
	env.servers = append(env.servers, g.server)
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

// e2eCompletion is the evidence a completed review is judged on: durable
// state, the fake upstream heads, the fake GitHub's POSTs and reviews, the
// source owner journal and archive, and the native launch receipts.
type e2eCompletion struct {
	State     r.DurableState
	Heads     map[int]string
	Release   string
	Posts     map[int]int
	Reviews   map[int][]map[string]any
	Records   []r.E2ESourceRecord
	Exports   []r.E2EExportRecord
	SourceErr error
	Units     []e2eNativeUnit
}

// completion gathers the current evidence from the open service.
func (e *e2eEnv) completion() e2eCompletion {
	summary := e.github.summary()
	c := e2eCompletion{State: e.state(), Heads: map[int]string{}, Release: e.options.Current, Posts: summary.ReviewPosts, Reviews: summary.Reviews, Units: e2eReadNativeUnits(e.native, e.policies)}
	for n := range c.Posts {
		c.Heads[n] = e.github.commits[strconv.Itoa(n)]
	}
	if e.service == nil {
		c.SourceErr = errors.New("source records need an open service")
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Records, c.Exports, c.SourceErr = e.service.E2ESourceRecords(ctx)
	return c
}

func (e *e2eEnv) checkAll(verdicts []e2eVerdict) {
	e.t.Helper()
	for _, v := range verdicts {
		e.check(v.OK, "%s", v.Message)
	}
}

// assertReviewed checks the durable outcome, remote COMMENT and outbox state
// for one PR, and returns its job.
func (e *e2eEnv) assertReviewed(pr int) *r.DurableJob {
	e.t.Helper()
	verdicts, j := e2eReviewedVerdicts(e.completion(), pr)
	e.checkAll(verdicts)
	return j
}

// e2eReviewedVerdicts judges one PR's completed review. It returns the PR's
// job, or nil when none exists.
func e2eReviewedVerdicts(c e2eCompletion, pr int) ([]e2eVerdict, *r.DurableJob) {
	j := e2eJobFor(c.State, pr)
	if j == nil {
		return []e2eVerdict{{false, fmt.Sprintf("PR %d has a durable job", pr)}}, nil
	}
	v := []e2eVerdict{
		{j.State == "dual_complete", fmt.Sprintf("PR %d job %s reached dual_complete (got %s)", pr, j.Job.ID, j.State)},
		{j.Job.Config.Mode == r.Adversarial && len(j.Checkpoint.Artifacts) == 4 && j.Checkpoint.ConsumedSessions == 4, fmt.Sprintf("PR %d adversarial engine stored 4 artifacts and consumed exactly 4 sessions (consumed %d)", pr, j.Checkpoint.ConsumedSessions)},
		{c.Heads[pr] != "" && j.Job.Snapshot.Head == c.Heads[pr] && j.Job.Release == c.Release, fmt.Sprintf("PR %d job binds its own head and the current release", pr)},
	}
	providers := map[r.Provider]int{}
	for _, a := range j.Checkpoint.Artifacts {
		providers[a.Report.Provider]++
	}
	v = append(v, e2eVerdict{providers[r.Codex] == 2 && providers[r.Claude] == 2, fmt.Sprintf("PR %d artifacts from both providers %v", pr, providers)})
	writes := e2eWritesFor(c.State, pr, "review")
	w := e2eSoleConfirmed(writes)
	v = append(v, e2eVerdict{w != nil, fmt.Sprintf("PR %d has exactly one review intent and it is confirmed (%d writes)", pr, len(writes))})
	if w != nil {
		v = append(v,
			e2eVerdict{w.Snapshot.Head == j.Job.Snapshot.Head && w.ReceiptID > 0, fmt.Sprintf("PR %d confirmed write binds the reviewed head and a receipt", pr)},
			e2eVerdict{len(w.Document.Inline) >= 1 && w.Document.Inline[0].Path == "source.go", fmt.Sprintf("PR %d document carries the source-backed inline finding", pr)})
	}
	v = append(v, e2eVerdict{c.Posts[pr] == 1, fmt.Sprintf("PR %d received exactly one review POST (got %d)", pr, c.Posts[pr])})
	reviews := c.Reviews[pr]
	v = append(v, e2eVerdict{len(reviews) == 1 && reviews[0]["state"] == "COMMENTED" && reviews[0]["commit_id"] == j.Job.Snapshot.Head && reviews[0]["marker"] != "", fmt.Sprintf("PR %d remote review is one marked COMMENT on its head", pr)})
	var inline []string
	if len(reviews) == 1 {
		inline, _ = reviews[0]["inline"].([]string)
	}
	v = append(v, e2eVerdict{slices.Contains(inline, "source.go:1"), fmt.Sprintf("PR %d remote review carries the inline comment at source.go:1 (%v)", pr, inline)})
	return v, j
}

// e2eSoleConfirmed returns a PR's review intent when it is the only one and
// confirmed. Any other prepared, dispatched, unknown or failed intent could
// still publish a duplicate in a later outbox cycle.
func e2eSoleConfirmed(writes []*r.WriteIntent) *r.WriteIntent {
	if len(writes) != 1 || writes[0].State != "confirmed" {
		return nil
	}
	return writes[0]
}

// e2eSourceEvidence requires exactly want source records, each with one
// archive export of the same attempt and snapshot, before any entry is
// validated, so a missing journal or archive cannot pass vacuously.
func e2eSourceEvidence(records []r.E2ESourceRecord, exports []r.E2EExportRecord, want int) error {
	if want <= 0 || len(records) != want || len(exports) != want {
		return fmt.Errorf("%d records and %d exports, want %d of each", len(records), len(exports), want)
	}
	staged := map[string]r.E2EExportRecord{}
	for _, x := range exports {
		if _, duplicate := staged[x.Attempt]; duplicate {
			return fmt.Errorf("export attempt %s repeated", x.Attempt)
		}
		staged[x.Attempt] = x
	}
	seen := map[string]bool{}
	for _, rec := range records {
		x, ok := staged[rec.Attempt]
		if seen[rec.Attempt] || !ok || x.Snapshot != rec.Snapshot {
			return fmt.Errorf("source attempt %s lacks its own export", rec.Attempt)
		}
		seen[rec.Attempt] = true
	}
	return nil
}

func (e *e2eEnv) assertSourceStopped(label string, want int) {
	e.t.Helper()
	c := e.completion()
	e.checkAll(e2eSourceVerdicts(label, c, want))
	if c.SourceErr == nil {
		e.mu.Lock()
		e.result.SourceRecords = append(e.result.SourceRecords, c.Records)
		e.result.Exports = append(e.result.Exports, c.Exports)
		e.mu.Unlock()
	}
}

// e2eSourceVerdicts requires want source attempts, each with a durable
// granted stop receipt and a staged export of its own.
func e2eSourceVerdicts(label string, c e2eCompletion, want int) []e2eVerdict {
	if c.SourceErr != nil {
		return []e2eVerdict{{false, fmt.Sprintf("%s: source records readable: %v", label, c.SourceErr)}}
	}
	err := e2eSourceEvidence(c.Records, c.Exports, want)
	v := []e2eVerdict{{err == nil, fmt.Sprintf("%s: %d source records with matching staged-export entries: %v", label, want, err)}}
	for _, rec := range c.Records {
		v = append(v, e2eVerdict{rec.Stopped && rec.Revoked && rec.GrantHash != "", fmt.Sprintf("%s: source attempt %s (unit %s) has a durable granted stop receipt", label, rec.Attempt, rec.Unit)})
	}
	for _, x := range c.Exports {
		v = append(v, e2eVerdict{x.State == "staged", fmt.Sprintf("%s: retained export %s is staged (%s)", label, x.Attempt, x.State)})
	}
	return v
}

// assertNativeSessions requires exactly one completed native launch per
// artifact session of the given jobs, with auth returned and whole-unit
// stop, and no other launch except the listed extra session hashes.
func (e *e2eEnv) assertNativeSessions(jobs []*r.DurableJob, extra ...string) {
	e.t.Helper()
	e.checkAll([]e2eVerdict{e2eNativeSessionVerdict(e2eReadNativeUnits(e.native, e.policies), jobs, extra...)})
}

func e2eNativeSessionVerdict(units []e2eNativeUnit, jobs []*r.DurableJob, extra ...string) e2eVerdict {
	expected := map[string]bool{}
	for _, j := range jobs {
		for _, a := range j.Checkpoint.Artifacts {
			expected[a.InputHash] = true
		}
	}
	allowed := map[string]int{}
	for _, hash := range extra {
		allowed[hash]++
	}
	seen := map[string]int{}
	unexpected := 0
	for _, u := range units {
		if expected[u.SessionHash] && u.Captured && u.Stopped && u.AuditPresent && u.Complete && u.AuthReturned {
			seen[u.SessionHash]++
		} else if allowed[u.SessionHash] > 0 {
			allowed[u.SessionHash]--
		} else {
			unexpected++
		}
	}
	once := len(seen) == len(expected)
	for _, n := range seen {
		once = once && n == 1
	}
	return e2eVerdict{once && unexpected == 0, fmt.Sprintf("each of %d job-bound sessions ran in exactly one completed native launch with auth returned, and no other launch exists (%d matched, %d unexpected)", len(expected), len(seen), unexpected)}
}

// e2eProviderReopenVerdicts judges close-provider after a Close that proved
// completion and a reopen with the same assertions as every other completed
// review: one confirmed intent, one POST and the expected remote COMMENT,
// stopped source receipts, and one completed native launch per artifact. The
// interrupted launch is the only other launch allowed.
func e2eProviderReopenVerdicts(c e2eCompletion, interrupted string) ([]e2eVerdict, *r.DurableJob) {
	verdicts, job := e2eReviewedVerdicts(c, 1)
	verdicts = append(verdicts, e2eSourceVerdicts("after reopen", c, 1)...)
	if job != nil {
		verdicts = append(verdicts, e2eNativeSessionVerdict(c.Units, []*r.DurableJob{job}, interrupted))
	}
	return verdicts, job
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
	e.assertSourceStopped("after review", 1)
	e.assertNativeSessions([]*r.DurableJob{job})
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
	e.assertSourceStopped("after both reviews", 2)
	e.assertNativeSessions([]*r.DurableJob{j1, j2})
	// Admission: sessions of the two jobs never interleave.
	sessions := append(e.models[r.Codex].sessionsCopy(), e.models[r.Claude].sessionsCopy()...)
	switches, unbound := e2eSessionsBound(sessions, e2eJobSummary(j1), e2eJobSummary(j2))
	e.check(len(unbound) == 0, "every session binds its own job's PR and head (unbound %v)", unbound)
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
	// Each acquisition's upload-pack wants exactly its own head and the base.
	uploads := map[string]int{}
	other := 0
	for _, x := range e.github.summary().GitExchanges {
		if x.Operation != "upload-pack" {
			continue
		}
		wants := slices.Sorted(slices.Values(x.Wants))
		switch {
		case slices.Equal(wants, slices.Sorted(slices.Values([]string{h1, e.github.baseSHA}))):
			uploads[h1]++
		case slices.Equal(wants, slices.Sorted(slices.Values([]string{h2, e.github.baseSHA}))):
			uploads[h2]++
		default:
			other++
		}
	}
	e.check(other == 0 && uploads[h1] == 1 && uploads[h2] == 1, "each job's single upload-pack wanted exactly its own head and the base (PR1 %d, PR2 %d, other %d)", uploads[h1], uploads[h2], other)
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

// e2eSessionsBound orders sessions by host check and counts job switches. It
// reports each session, the first included, whose job ID is not one of jobs
// or whose PR and head differ from that job's snapshot.
func e2eSessionsBound(sessions []e2eModelSession, jobs ...e2eJob) (int, []string) {
	sessions = slices.Clone(sessions)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CheckedAt.Before(sessions[j].CheckedAt) })
	byID := map[string]e2eJob{}
	for _, j := range jobs {
		byID[j.ID] = j
	}
	switches, unbound := 0, []string{}
	for i, s := range sessions {
		if i > 0 && s.JobID != sessions[i-1].JobID {
			switches++
		}
		if j, ok := byID[s.JobID]; !ok || s.PR != j.PR || s.Head != j.Head {
			unbound = append(unbound, fmt.Sprintf("session %d (job %s PR %d)", i, s.JobID, s.PR))
		}
	}
	return switches, unbound
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
	e.assertSourceStopped("after reopen recovery", 1)
	if !e.wait("PR 1 review confirmed after reopen", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	job := e.assertReviewed(1)
	e.check(e.github.gitExchanges() == exchanges, "reopened service restored retained source without refetching (%d exchanges)", e.github.gitExchanges())
	e.assertNativeSessions([]*r.DurableJob{job})
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
	e.wedged = closeErr != nil
	select {
	case runErr := <-e.runDone:
		e.result.RunErrors = append(e.result.RunErrors, fmt.Sprintf("close-provider: %v", runErr))
		e.check(e2eRunErrorExpected(runErr, e.wedged), "close-provider: Run returned only the expected shutdown result (wedged %v): %v", e.wedged, runErr)
	case <-time.After(10 * time.Second):
		e.check(false, "Run did not return after Close")
	}
	if !e.wedged {
		e.service = nil
	}
	e.check(closeErr == nil || errors.Is(closeErr, r.ErrWorkerStopPending), "Close either proved completion or reported ErrWorkerStopPending (%v)", closeErr)
	e.closeStore("close-provider", e.store)
	e.store = nil
	d := e.reopenState()
	j := e2eJobFor(d, 1)
	if j == nil {
		e.check(false, "job exists after provider close")
		return
	}
	e.note("after provider-session close: job state %s, consumed %d, artifacts %d, lease %v, outstanding %v, provider attempts %d", j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), j.LeaseToken != "", j.Outstanding, len(j.ProviderAttempts))
	e.result.Requests = e2eRequests(d)
	e.checkAll(e2eRequestVerdicts("after provider close", e.result.Requests, map[int]e2eRequest{1: e2eAdmittedOpened(1, j.RequestID)}))
	e.check(j.Checkpoint.ConsumedSessions == 1 && len(j.Checkpoint.Artifacts) == 0, "the interrupted session stays charged and produced no artifact")
	units := e2eReadNativeUnits(e.native, e.policies)
	e.check(len(units) == 1 && units[0].Captured && units[0].Stopped && !units[0].Complete, "the interrupted native unit was captured and whole-unit stopped without completion")
	returned := len(units) == 1 && units[0].AuthReturned
	e.check(len(units) == 1 && units[0].AuditPresent, "the interrupted native unit has a well-formed stopped audit receipt")
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
		interrupted := ""
		if len(units) == 1 {
			interrupted = units[0].SessionHash
		}
		verdicts, reopened := e2eProviderReopenVerdicts(e.completion(), interrupted)
		e.checkAll(verdicts)
		// Not yet observed on a host: the interrupted launch plus one completed
		// launch per artifact of the reopened job, still one acquisition, and
		// both auth stores usable.
		if reopened != nil {
			e.expect = e2eExpect{1, 1 + len(reopened.Checkpoint.Artifacts), true}
			e.result.Jobs = append(e.result.Jobs, e2eJobSummary(reopened))
		}
		e.collectWrites()
		return
	}
	e.check(j.LeaseToken != "" && j.Outstanding && j.State == "running", "the uncertain provider attempt keeps its durable lease and outstanding reservation")
	e.check(!returned && !usable[r.Codex], "the unreturned exported auth is fenced for recovery")
	e.check(len(units) == 1 && units[0].AuditPresent && units[0].AuthExported && !units[0].AuthReturned, "the stopped audit records the auth as exported and not returned")
	// ReleaseStopped is the explicit trusted-supervisor path. Probe it on a
	// private copy: it is accepted only with durably recorded stopped
	// provider evidence, and the service itself never calls it.
	err := e2eProbeRelease(e, j)
	e.check(err == nil, "the trusted ReleaseStopped(auth_failed) succeeds on a private copy of the retained state: %v", err)
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
	retained := e.reopenState()
	before := e2eJobFor(retained, 1)
	if before == nil || before.LeaseToken == "" {
		e.t.Fatal("attached root has no retained lease")
	}
	launches, err := e2eRetainedLaunches(e2eReadNativeUnits(e.native, e.policies), before)
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted launch and its stopped audit:", err)
	}
	e.nativeBefore = launches
	e.check(true, "the attached root holds the interrupted launch, captured, stopped and audited with exported unreturned auth, bound to the retained attempt's stopped provider evidence")
	codexFenced := !e2eAuthUsable(e.auth, r.Codex)
	e.check(codexFenced, "codex auth is still fenced in the restarted process")
	e.open("restart")
	e.assertSourceStopped("after restart recovery", 1)
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
	// The fence holds every request back; none may have been duplicated
	// behind it, or releasing the lease could admit a second review.
	e.result.Requests = e2eRequests(d)
	e.checkAll(e2eRequestVerdicts("fenced after restart", e.result.Requests, map[int]e2eRequest{1: e2eAdmittedOpened(1, before.RequestID), 2: e2ePendingComment(2)}))
	e.snapshotJobs("fenced after restart")
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(final))
	e.wedged = true
	e.close("final")
}

// e2eRequest is a nonsecret projection of one retained native request.
type e2eRequest struct {
	PR                int
	ID, ActorID       int64
	Kind, ActorRole   string
	Admitted, Removed bool
}

// e2eRequests projects every native request every PR retains.
func e2eRequests(d r.DurableState) []e2eRequest {
	out := []e2eRequest{}
	for _, key := range slices.Sorted(maps.Keys(d.PRs)) {
		p := d.PRs[key]
		for _, q := range p.Requests {
			s := e2eRequest{PR: p.Scope.PR, ID: q.ID, Admitted: q.Admitted, Removed: q.Removed}
			if q.Origin != nil {
				s.ActorID, s.Kind, s.ActorRole = q.Origin.ActorID, q.Origin.Kind, q.Origin.ActorRole
			}
			out = append(out, s)
		}
	}
	return out
}

// e2eAdmittedOpened is the automatic PR-open request of an owner-authored PR,
// admitted as the request of job request id.
func e2eAdmittedOpened(pr int, id int64) e2eRequest {
	return e2eRequest{PR: pr, ID: id, ActorID: r.OwnerID, Kind: "opened", ActorRole: "pr_author", Admitted: true}
}

// e2ePendingComment is an owner @sfl review request not yet admitted.
func e2ePendingComment(pr int) e2eRequest {
	return e2eRequest{PR: pr, ActorID: r.OwnerID, Kind: "comment", ActorRole: "commenter"}
}

// e2eRequestVerdicts requires each PR in want to retain exactly one request
// equal to its want entry (an ID of 0 matches any ID), and every other PR to
// retain none. While a lease fences admission, a duplicate request cannot
// become a job, but it would once the fence is released.
func e2eRequestVerdicts(label string, requests []e2eRequest, want map[int]e2eRequest) []e2eVerdict {
	byPR := map[int][]e2eRequest{}
	for _, q := range requests {
		byPR[q.PR] = append(byPR[q.PR], q)
	}
	v := []e2eVerdict{}
	for _, pr := range slicesSortedInts(want) {
		w, got := want[pr], byPR[pr]
		ok := len(got) == 1
		if ok {
			g := got[0]
			if w.ID == 0 {
				g.ID = 0
			}
			ok = g == w
		}
		v = append(v, e2eVerdict{ok, fmt.Sprintf("%s: PR %d retains exactly its one %s request (%+v)", label, pr, w.Kind, got)})
	}
	others := []e2eRequest{}
	for _, pr := range slicesSortedInts(byPR) {
		if _, ok := want[pr]; !ok {
			others = append(others, byPR[pr]...)
		}
	}
	return append(v, e2eVerdict{len(others) == 0, fmt.Sprintf("%s: no other PR retains a request (%+v)", label, others)})
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
func e2eProbeRelease(e *e2eEnv, j *r.DurableJob) (err error) {
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
	defer func() { err = errors.Join(err, store.Close()) }()
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
	e.assertSourceStopped("after reconciliation", 1)
	e.assertNativeSessions([]*r.DurableJob{job})
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(job))
	e.collectWrites()
}

// reopenState reads the closed service's store through a fresh handle.
func (e *e2eEnv) reopenState() r.DurableState {
	store, err := r.OpenStore(e.storePath, e2eEnrollment, e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	d, err := store.Snapshot(context.Background())
	e.closeStore("reopened state read", store)
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

// ---------------------------------------------------------------- harness unit tests

func TestE2ESoleConfirmed(t *testing.T) {
	intent := func(state string) *r.WriteIntent { return &r.WriteIntent{Kind: "review", State: state} }
	if w := e2eSoleConfirmed([]*r.WriteIntent{intent("confirmed")}); w == nil || w.State != "confirmed" {
		t.Fatal("sole confirmed intent refused")
	}
	for _, other := range []string{"prepared", "dispatched", "unknown", "failed", "confirmed"} {
		if e2eSoleConfirmed([]*r.WriteIntent{intent("confirmed"), intent(other)}) != nil {
			t.Error("confirmed intent beside a", other, "intent accepted as exact-once")
		}
	}
	for _, writes := range [][]*r.WriteIntent{nil, {intent("unknown")}, {intent("dispatched")}} {
		if e2eSoleConfirmed(writes) != nil {
			t.Error("unconfirmed writes accepted", len(writes))
		}
	}
}

func TestE2ESourceEvidence(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	snapshot := func(pr int) r.Snapshot { return r.Snapshot{PR: pr, Head: strings.Repeat(strconv.Itoa(pr), 40)} }
	rec := func(attempt string, pr int) r.E2ESourceRecord {
		return r.E2ESourceRecord{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", Snapshot: snapshot(pr), Stopped: true, Revoked: true, GrantHash: "grant"}
	}
	export := func(attempt string, pr int) r.E2EExportRecord {
		return r.E2EExportRecord{Attempt: attempt, State: "staged", Snapshot: snapshot(pr)}
	}
	type input struct {
		records []r.E2ESourceRecord
		exports []r.E2EExportRecord
		want    int
	}
	for name, in := range map[string]input{
		"one":            {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, 1},
		"two, unordered": {[]r.E2ESourceRecord{rec(a, 1), rec(b, 2)}, []r.E2EExportRecord{export(b, 2), export(a, 1)}, 2},
	} {
		if err := e2eSourceEvidence(in.records, in.exports, in.want); err != nil {
			t.Error(name, "refused:", err)
		}
	}
	for name, in := range map[string]input{
		"journal and archive empty": {nil, nil, 1},
		"nothing expected":          {nil, nil, 0},
		"records without exports":   {[]r.E2ESourceRecord{rec(a, 1)}, nil, 1},
		"exports without records":   {nil, []r.E2EExportRecord{export(a, 1)}, 1},
		"fewer than expected":       {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, 2},
		"more than expected":        {[]r.E2ESourceRecord{rec(a, 1), rec(b, 2)}, []r.E2EExportRecord{export(a, 1), export(b, 2)}, 1},
		"export of another attempt": {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(b, 1)}, 1},
		"export of another PR":      {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 2)}, 1},
		"duplicate attempt":         {[]r.E2ESourceRecord{rec(a, 1), rec(a, 1)}, []r.E2EExportRecord{export(a, 1), export(a, 1)}, 2},
		"extra export":              {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1), export(b, 2)}, 1},
	} {
		if e2eSourceEvidence(in.records, in.exports, in.want) == nil {
			t.Error(name, "accepted")
		}
	}
}

// e2eUnitRun is a synthetic end-of-run result: one source attempt and the
// given native launches, every one of them observed by the unit observer.
func e2eUnitRun(attempt string, launches ...string) *e2eResult {
	res := &e2eResult{AuthRecoveryClear: map[string]bool{"codex": true, "claude": true}}
	if attempt != "" {
		unit := "sfl-alpha-" + attempt + ".service"
		res.SourceRecords = [][]r.E2ESourceRecord{{{Attempt: attempt, Unit: unit}}, {{Attempt: attempt, Unit: unit}}}
		res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: unit, Source: true})
	}
	for _, launch := range launches {
		res.NativeUnits = append(res.NativeUnits, e2eNativeUnit{Launch: launch, Captured: true, Stopped: true})
		res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: "sfl-alpha-" + launch + ".service"})
	}
	return res
}

func TestE2EFinalVerdicts(t *testing.T) {
	id := func(c string) string { return strings.Repeat(c, 32) }
	source, n1, n2, n3, n4 := id("a"), id("1"), id("2"), id("3"), id("4")
	review := e2eExpectations["review"]
	failures := func(res *e2eResult, want e2eExpect, before map[string]bool, attached bool) []string {
		out := []string{}
		for _, v := range e2eFinalVerdicts(res, want, before, attached) {
			if !v.OK {
				out = append(out, v.Message)
			}
		}
		return out
	}
	if f := failures(e2eUnitRun(source, n1, n2, n3, n4), review, nil, false); len(f) != 0 {
		t.Fatal("correlated review run refused:", f)
	}
	reopen := e2eUnitRun("", n1)
	reopen.SystemdUnits = nil
	reopen.SourceRecords = [][]r.E2ESourceRecord{{{Attempt: source, Unit: "sfl-alpha-" + source + ".service"}}}
	reopen.AuthRecoveryClear["codex"] = false
	if f := failures(reopen, e2eExpectations["close-provider-reopen"], map[string]bool{n1: true}, true); len(f) != 0 {
		t.Fatal("attached fenced restart refused:", f)
	}
	fenced := e2eUnitRun(source, n1)
	fenced.AuthRecoveryClear["codex"] = false
	if f := failures(fenced, e2eExpectations["close-provider"], nil, false); len(f) != 0 {
		t.Fatal("fenced provider close refused:", f)
	}
	type input struct {
		res      *e2eResult
		want     e2eExpect
		before   map[string]bool
		attached bool
	}
	mutate := func(res *e2eResult, change func(*e2eResult)) *e2eResult { change(res); return res }
	for name, in := range map[string]input{
		"nothing observed or recorded":          {&e2eResult{AuthRecoveryClear: map[string]bool{"codex": true, "claude": true}}, review, nil, false},
		"recorded but never observed":           {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.SystemdUnits = nil }), review, nil, false},
		"one native launch unobserved":          {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.SystemdUnits = res.SystemdUnits[:4] }), review, nil, false},
		"source attempt unobserved":             {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.SystemdUnits = res.SystemdUnits[1:] }), review, nil, false},
		"observed unit with no recorded launch": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.SystemdUnits[4].Name = "sfl-alpha-" + id("9") + ".service" }), review, nil, false},
		"source unit observed as native":        {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.SystemdUnits[0].Source = false }), review, nil, false},
		"fewer launches than the case":          {e2eUnitRun(source, n1, n2, n3), review, nil, false},
		"more launches than the case":           {e2eUnitRun(source, n1, n2, n3, n4, id("5")), review, nil, false},
		"no source acquisition":                 {e2eUnitRun("", n1, n2, n3, n4), review, nil, false},
		"new launch after restart": {mutate(e2eUnitRun("", n1, n2), func(res *e2eResult) {
			res.SystemdUnits = res.SystemdUnits[1:]
			res.AuthRecoveryClear["codex"] = false
		}), e2eExpectations["close-provider-reopen"], map[string]bool{n1: true}, true},
		"retained launch vanished": {mutate(e2eUnitRun("", n1), func(res *e2eResult) {
			res.SystemdUnits, res.NativeUnits = nil, nil
			res.AuthRecoveryClear["codex"] = false
		}), e2eExpectations["close-provider-reopen"], map[string]bool{n1: true}, true},
		"codex auth fenced after success": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.AuthRecoveryClear["codex"] = false }), review, nil, false},
		"claude auth unrecorded":          {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { delete(res.AuthRecoveryClear, "claude") }), review, nil, false},
		"final unit listing failed":       {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.UnitListingError = "systemctl failed" }), review, nil, false},
	} {
		if f := failures(in.res, in.want, in.before, in.attached); len(f) == 0 {
			t.Error(name, "accepted")
		}
	}
}

// e2eCleanupTB records cleanup errors instead of failing the enclosing test.
type e2eCleanupTB struct {
	*testing.T
	errors []string
}

func (tb *e2eCleanupTB) Error(args ...any) { tb.errors = append(tb.errors, fmt.Sprint(args...)) }
func (tb *e2eCleanupTB) Failed() bool      { return len(tb.errors) != 0 }

func TestE2ESealAfterCleanup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only parent that makes removal fail")
	}
	for _, removable := range []bool{true, false} {
		t.Run(fmt.Sprint("removable=", removable), func(t *testing.T) {
			evidence, parent := t.TempDir(), t.TempDir()
			t.Setenv("SFL_E2E_EVIDENCE", evidence)
			root := filepath.Join(parent, "root")
			if os.Mkdir(root, 0700) != nil || os.WriteFile(filepath.Join(root, "state"), []byte("x"), 0600) != nil {
				t.Fatal("fixture root")
			}
			if !removable {
				if err := os.Chmod(parent, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
			}
			tb := &e2eCleanupTB{T: t}
			e := &e2eEnv{t: tb, scenario: "review", root: root, finished: true, result: &e2eResult{Case: "review"}}
			e.cleanup()
			raw, err := os.ReadFile(filepath.Join(evidence, "review.json"))
			var sealed e2eResult
			if err != nil || json.Unmarshal(raw, &sealed) != nil {
				t.Fatal("cleanup left no sealed evidence", err)
			}
			if sealed.Passed != removable || tb.Failed() == removable {
				t.Fatalf("sealed Passed %v after cleanup errors %v", sealed.Passed, tb.errors)
			}
			if _, err := os.Stat(root); removable != errors.Is(err, os.ErrNotExist) {
				t.Fatal("root disposition", err)
			}
		})
	}
}

func TestE2EUnitObserverListing(t *testing.T) {
	unit := map[string]string{"sfl-alpha-" + strings.Repeat("a", 32) + ".service": "loaded/active/running source VM"}
	scripted := func(failAfter bool) func() (map[string]string, error) {
		var calls atomic.Int32
		return func() (map[string]string, error) {
			switch n := calls.Add(1); {
			case n == 1:
				return map[string]string{}, nil
			case n == 2:
				return unit, nil
			case failAfter:
				return nil, errors.New("synthetic systemctl failure")
			default:
				return map[string]string{}, nil
			}
		}
	}
	units, residual, err := newE2EUnitObserver(t, scripted(false)).stop(5 * time.Second)
	if err != nil || len(residual) != 0 || len(units) != 1 {
		t.Fatal("observed unit that ended was not reported clean", units, residual, err)
	}
	units, residual, err = newE2EUnitObserver(t, scripted(true)).stop(time.Second)
	if err == nil || len(residual) != 1 || len(units) != 1 {
		t.Error("unit of unknown disposition after a failed final listing was reported clean", units, residual, err)
	}
}

func TestE2ENativeUnitFrom(t *testing.T) {
	session, launch := strings.Repeat("5", 64), strings.Repeat("6", 32)
	binding := func(session, launch string) r.AuthBinding {
		return r.AuthBinding{SessionHash: session, LaunchID: launch, ContainmentID: r.Digest(struct{ Session, Launch string }{session, launch})}
	}
	policy, otherPolicy := r.EgressPolicy{Provider: r.Codex, Headers: []string{"authorization"}}, r.EgressPolicy{Provider: r.Codex, Headers: []string{"cookie"}}
	evidence := func(session string, policy r.EgressPolicy, stopped bool) *r.ProviderEvidence {
		return &r.ProviderEvidence{Policy: policy, Access: r.ProviderAccess{Version: 1, SessionHash: session, PolicyHash: r.Digest(policy), Stopped: stopped}}
	}
	type receipts struct{ unit, audit map[string]any }
	valid := func(complete bool) receipts {
		return receipts{
			map[string]any{"Version": 1, "Unit": "sfl-alpha-" + launch + ".service", "Binding": binding(session, launch), "Captured": true, "Stopped": true},
			map[string]any{"Version": 1, "Binding": binding(session, launch), "Provider": evidence(session, policy, true), "Complete": complete, "AuthExported": true, "AuthReturned": complete},
		}
	}
	project := func(in receipts) e2eNativeUnit {
		var unit, audit []byte
		if in.unit != nil {
			unit, _ = json.Marshal(in.unit)
		}
		if in.audit != nil {
			audit, _ = json.Marshal(in.audit)
		}
		return e2eNativeUnitFrom(r.Codex, policy, launch, unit, audit)
	}
	if u := project(valid(true)); !u.AuditPresent || !u.Captured || !u.Stopped || !u.Complete || !u.AuthReturned || u.SessionHash != session {
		t.Fatal("bound completed receipts refused", u)
	}
	if u := project(valid(false)); !u.AuditPresent || u.Complete || !u.AuthExported || u.AuthReturned {
		t.Fatal("bound interrupted receipts refused", u)
	}
	if u := project(receipts{valid(true).unit, nil}); u.AuditPresent || !u.Captured || u.SessionHash != session {
		t.Fatal("unit without an audit misreported", u)
	}
	change := func(complete bool, edit func(receipts)) receipts { in := valid(complete); edit(in); return in }
	for name, in := range map[string]receipts{
		"audit of another session":         change(true, func(in receipts) { in.audit["Binding"] = binding(strings.Repeat("7", 64), launch) }),
		"audit of another launch":          change(true, func(in receipts) { in.audit["Binding"] = binding(session, strings.Repeat("8", 32)) }),
		"audit without a binding":          change(true, func(in receipts) { delete(in.audit, "Binding") }),
		"audit of another version":         change(true, func(in receipts) { in.audit["Version"] = 2 }),
		"audit without a version":          change(true, func(in receipts) { delete(in.audit, "Version") }),
		"completed audit without evidence": change(true, func(in receipts) { delete(in.audit, "Provider") }),
		"exported audit without evidence":  change(false, func(in receipts) { delete(in.audit, "Provider") }),
		"evidence of another session":      change(true, func(in receipts) { in.audit["Provider"] = evidence(strings.Repeat("7", 64), policy, true) }),
		"evidence under another policy":    change(true, func(in receipts) { in.audit["Provider"] = evidence(session, otherPolicy, true) }),
		"evidence of another provider":     change(true, func(in receipts) { in.audit["Provider"] = evidence(session, r.EgressPolicy{Provider: r.Claude}, true) }),
		"evidence not stopped":             change(false, func(in receipts) { in.audit["Provider"] = evidence(session, policy, false) }),
		"unit journal of another launch": change(true, func(in receipts) {
			in.unit["Binding"], in.audit["Binding"] = binding(session, strings.Repeat("8", 32)), binding(session, strings.Repeat("8", 32))
		}),
		"unit journal naming another unit": change(true, func(in receipts) { in.unit["Unit"] = "sfl-alpha-" + strings.Repeat("8", 32) + ".service" }),
		"unit journal of another version":  change(true, func(in receipts) { in.unit["Version"] = 2 }),
		"containment not derived from launch": change(true, func(in receipts) {
			b := binding(session, launch)
			b.ContainmentID = strings.Repeat("9", 64)
			in.unit["Binding"], in.audit["Binding"] = b, b
		}),
	} {
		if u := project(in); u.AuditPresent {
			t.Error(name, "accepted as this launch's audit", u)
		}
	}
}

// e2eSetupTB runs registered cleanups itself and ends the calling goroutine
// on Fatal, as testing does, without failing the enclosing test.
type e2eSetupTB struct {
	e2eCleanupTB
	cleanups []func()
}

func (tb *e2eSetupTB) Cleanup(f func())  { tb.cleanups = append(tb.cleanups, f) }
func (tb *e2eSetupTB) Fatal(args ...any) { tb.Error(args...); runtime.Goexit() }
func (tb *e2eSetupTB) Fatalf(format string, args ...any) {
	tb.Error(fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (tb *e2eSetupTB) run(f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			for i := len(tb.cleanups) - 1; i >= 0; i-- {
				tb.cleanups[i]()
			}
		}()
		f()
	}()
	<-done
}

func TestE2EAllocateCleanupAfterSetupFailure(t *testing.T) {
	t.Setenv("SFL_E2E_EVIDENCE", "")
	t.Setenv("SFL_E2E_KEEP_FAILED", "")
	var samples atomic.Int32
	list := func() (map[string]string, error) { samples.Add(1); return map[string]string{}, nil }
	tb := &e2eSetupTB{e2eCleanupTB: e2eCleanupTB{T: t}}
	var e *e2eEnv
	tb.run(func() {
		e = e2eAllocate(tb, "review", "", list)
		tb.Fatal("synthetic setup failure after the observer started")
	})
	if e == nil {
		t.Fatal("allocation failed", tb.errors)
	}
	t.Cleanup(func() {
		e.units.stop(0)
		_ = os.RemoveAll(e.root)
	})
	if _, err := os.Stat(e.root); !errors.Is(err, os.ErrNotExist) {
		t.Error("fresh root left behind by a setup failure", e.root, err)
	}
	select {
	case <-e.units.joined:
	default:
		t.Error("unit observer still sampling after a setup failure", samples.Load())
	}
	// A malformed attach path is refused before cleanup can own it.
	foreign := t.TempDir()
	tb = &e2eSetupTB{e2eCleanupTB: e2eCleanupTB{T: t}}
	tb.run(func() { e2eAllocate(tb, "close-provider-reopen", foreign, list) })
	if _, err := os.Stat(foreign); err != nil || !tb.Failed() {
		t.Error("malformed attach root removed or accepted", err, tb.errors)
	}
}

// e2eReopenFixture is a valid completion after close-provider's reopen: the
// reopened job's four completed launches plus the interrupted launch.
func e2eReopenFixture() (e2eCompletion, string) {
	head, attempt := strings.Repeat("1", 40), strings.Repeat("a", 32)
	hashes := []string{strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64)}
	job := &r.DurableJob{Job: r.Job{ID: "request-1-1", Snapshot: r.Snapshot{PR: 1, Head: head}, Config: r.DefaultConfig(), Release: "release"}, Scope: r.Scope{PR: 1}, State: "dual_complete", Checkpoint: r.Checkpoint{ConsumedSessions: 4}}
	units := []e2eNativeUnit{{Launch: "interrupted", SessionHash: hashes[0], Captured: true, Stopped: true, AuditPresent: true, AuthExported: true, AuthReturned: true}}
	for i, hash := range hashes {
		provider := []r.Provider{r.Codex, r.Claude}[i%2]
		job.Checkpoint.Artifacts = append(job.Checkpoint.Artifacts, r.Artifact{InputHash: hash, Report: r.Report{Provider: provider}})
		units = append(units, e2eNativeUnit{Launch: strconv.Itoa(i), SessionHash: hash, Captured: true, Stopped: true, AuditPresent: true, Complete: true, AuthExported: true, AuthReturned: true})
	}
	write := &r.WriteIntent{ID: "w1", Kind: "review", Scope: r.Scope{PR: 1}, State: "confirmed", Snapshot: r.Snapshot{PR: 1, Head: head}, ReceiptID: 5001, Document: r.Document{Inline: []r.InlineComment{{Path: "source.go", Line: 1}}}}
	snapshot := r.Snapshot{PR: 1, Head: head}
	return e2eCompletion{
		State:   r.DurableState{Jobs: map[string]*r.DurableJob{job.Job.ID: job}, Writes: map[string]*r.WriteIntent{write.ID: write}},
		Heads:   map[int]string{1: head},
		Release: "release",
		Posts:   map[int]int{1: 1},
		Reviews: map[int][]map[string]any{1: {{"state": "COMMENTED", "commit_id": head, "marker": "<!-- sfl-alpha:" + attempt + " -->", "inline": []string{"source.go:1"}}}},
		Records: []r.E2ESourceRecord{{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", Snapshot: snapshot, Stopped: true, Revoked: true, GrantHash: "grant"}},
		Exports: []r.E2EExportRecord{{Attempt: attempt, State: "staged", Snapshot: snapshot}},
		Units:   units,
	}, hashes[0]
}

func TestE2EProviderReopenVerdicts(t *testing.T) {
	failures := func(c e2eCompletion, interrupted string) []string {
		verdicts, _ := e2eProviderReopenVerdicts(c, interrupted)
		out := []string{}
		for _, v := range verdicts {
			if !v.OK {
				out = append(out, v.Message)
			}
		}
		return out
	}
	if f := failures(e2eReopenFixture()); len(f) != 0 {
		t.Fatal("completed reopen refused:", f)
	}
	for name, edit := range map[string]func(*e2eCompletion){
		"duplicate unknown intent": func(c *e2eCompletion) {
			c.State.Writes["w2"] = &r.WriteIntent{ID: "w2", Kind: "review", Scope: r.Scope{PR: 1}, State: "unknown"}
		},
		"second review POST":             func(c *e2eCompletion) { c.Posts[1] = 2 },
		"remote review without inline":   func(c *e2eCompletion) { c.Reviews[1][0]["inline"] = []string{} },
		"remote review on another head":  func(c *e2eCompletion) { c.Reviews[1][0]["commit_id"] = strings.Repeat("9", 40) },
		"completed launch kept its auth": func(c *e2eCompletion) { c.Units[1].AuthReturned = false },
		"completed launch audit unbound": func(c *e2eCompletion) { c.Units[2].AuditPresent = false },
		"second interrupted launch":      func(c *e2eCompletion) { c.Units = append(c.Units, c.Units[0]) },
		"source attempt not stopped":     func(c *e2eCompletion) { c.Records[0].Stopped = false },
		"source records unreadable":      func(c *e2eCompletion) { c.SourceErr = r.ErrLiveBlocked },
		"interrupted session recharged":  func(c *e2eCompletion) { e2eJobFor(c.State, 1).Checkpoint.ConsumedSessions = 5 },
	} {
		c, interrupted := e2eReopenFixture()
		edit(&c)
		if len(failures(c, interrupted)) == 0 {
			t.Error(name, "accepted")
		}
	}
}

func TestE2ESessionsBound(t *testing.T) {
	h1, h2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	j1, j2 := e2eJob{ID: "request-1-1", PR: 1, Head: h1}, e2eJob{ID: "request-2-1", PR: 2, Head: h2}
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	run := func() []e2eModelSession {
		sessions := []e2eModelSession{}
		for i, j := range []e2eJob{j2, j2, j2, j2, j1, j1, j1, j1} {
			sessions = append(sessions, e2eModelSession{JobID: j.ID, PR: j.PR, Head: j.Head, CheckedAt: at.Add(time.Duration(i) * time.Second)})
		}
		// Provider order is not check order.
		slices.Reverse(sessions)
		return sessions
	}
	if switches, unbound := e2eSessionsBound(run(), j1, j2); switches != 1 || len(unbound) != 0 {
		t.Fatal("two bound sequential jobs refused", switches, unbound)
	}
	for name, edit := range map[string]func([]e2eModelSession){
		"first session on the other head":   func(s []e2eModelSession) { s[7].Head = h1 },
		"first session of an unknown job":   func(s []e2eModelSession) { s[7].JobID = "request-3-1" },
		"session of job 2 bound to PR 1":    func(s []e2eModelSession) { s[5].PR, s[5].Head = 1, h1 },
		"session of job 1 on the PR 2 head": func(s []e2eModelSession) { s[0].Head = h2 },
	} {
		sessions := run()
		edit(sessions)
		if _, unbound := e2eSessionsBound(sessions, j1, j2); len(unbound) == 0 {
			t.Error(name, "accepted")
		}
	}
}

// e2eCloser is a store handle whose Close result is scripted.
type e2eCloser struct {
	err   error
	calls int
}

func (c *e2eCloser) Close() error { c.calls++; return c.err }

func TestE2ECloseStoreReported(t *testing.T) {
	t.Setenv("SFL_E2E_EVIDENCE", "")
	for _, failing := range []bool{false, true} {
		tb := &e2eCleanupTB{T: t}
		e := &e2eEnv{t: tb, scenario: "review", finished: true, result: &e2eResult{Case: "review"}}
		closer := &e2eCloser{}
		if failing {
			closer.err = errors.New("synthetic SQLite checkpoint failure")
		}
		e.closeStore("final", closer)
		e.seal()
		if closer.calls != 1 || e.result.Passed == failing || tb.Failed() != failing {
			t.Errorf("store Close failure %v: calls %d, sealed Passed %v, errors %v", failing, closer.calls, e.result.Passed, tb.errors)
		}
	}
}

func TestE2EStaleEvidence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only evidence directory")
	}
	t.Setenv("SFL_E2E_KEEP_FAILED", "")
	list := func() (map[string]string, error) { return map[string]string{}, nil }
	stalePass := func(t *testing.T) (string, string) {
		evidence := t.TempDir()
		stale := filepath.Join(evidence, "review.json")
		if err := os.WriteFile(stale, []byte("{\"Case\": \"review\", \"Passed\": true}\n"), 0400); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SFL_E2E_EVIDENCE", evidence)
		t.Cleanup(func() { _ = os.Chmod(evidence, 0700) })
		return evidence, stale
	}
	allocated := func(t *testing.T, e *e2eEnv) {
		t.Cleanup(func() {
			if e != nil {
				e.units.stop(0)
				_ = os.RemoveAll(e.root)
			}
		})
	}
	t.Run("unwritable after setup", func(t *testing.T) {
		evidence, stale := stalePass(t)
		tb := &e2eSetupTB{e2eCleanupTB: e2eCleanupTB{T: t}}
		var e *e2eEnv
		tb.run(func() {
			e = e2eAllocate(tb, "review", "", list)
			if err := os.Chmod(evidence, 0500); err != nil {
				t.Error(err)
			}
			tb.Fatal("synthetic run failure after the evidence directory became unwritable")
		})
		allocated(t, e)
		var sealed e2eResult
		if raw, err := os.ReadFile(stale); err == nil && json.Unmarshal(raw, &sealed) == nil && sealed.Passed {
			t.Error("an earlier passing result survived a failed run that could not write its own")
		}
		if entries, _ := os.ReadDir(evidence); len(entries) != 0 {
			t.Error("evidence directory not empty after a failed write", len(entries))
		}
	})
	t.Run("irremovable before setup", func(t *testing.T) {
		evidence, _ := stalePass(t)
		if err := os.Chmod(evidence, 0500); err != nil {
			t.Fatal(err)
		}
		tb := &e2eSetupTB{e2eCleanupTB: e2eCleanupTB{T: t}}
		var e *e2eEnv
		tb.run(func() { e = e2eAllocate(tb, "review", "", list) })
		allocated(t, e)
		if e != nil || !tb.Failed() {
			t.Error("run started beside an earlier passing result it could not remove", tb.errors)
		}
	})
}

func TestE2EWriteEvidence(t *testing.T) {
	directory := t.TempDir()
	names := func() []string {
		entries, _ := os.ReadDir(directory)
		out := []string{}
		for _, entry := range entries {
			out = append(out, entry.Name())
		}
		return out
	}
	for _, body := range []string{"{\"Passed\": true}\n", "{\"Passed\": false}\n"} {
		if err := e2eWriteEvidence(directory, "review", []byte(body)); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(directory, "review.json"))
		info, _ := os.Stat(filepath.Join(directory, "review.json"))
		if err != nil || string(raw) != body || info.Mode().Perm() != 0600 || !slices.Equal(names(), []string{"review.json"}) {
			t.Fatal("evidence not replaced whole", string(raw), err, names())
		}
	}
	// A destination that cannot be replaced fails without leaving a temporary.
	occupied := filepath.Join(directory, "close-publish.json")
	if os.Mkdir(occupied, 0700) != nil || os.WriteFile(filepath.Join(occupied, "x"), nil, 0600) != nil {
		t.Fatal("fixture")
	}
	if err := e2eWriteEvidence(directory, "close-publish", []byte("{}\n")); err == nil {
		t.Error("write over an occupied destination reported success")
	}
	if got := names(); !slices.Equal(got, []string{"close-publish.json", "review.json"}) {
		t.Error("failed write left", got)
	}
}

func TestE2ERetainedLaunches(t *testing.T) {
	session, launch := strings.Repeat("5", 64), strings.Repeat("6", 32)
	unit := func() e2eNativeUnit {
		return e2eNativeUnit{Provider: "codex", Launch: launch, SessionHash: session, Captured: true, Stopped: true, AuditPresent: true, AuthExported: true}
	}
	job := func() *r.DurableJob {
		return &r.DurableJob{LeaseToken: "lease", Outstanding: true, AttemptProvider: r.Codex, AttemptID: "attempt", AttemptInputHash: session, ProviderAttempts: []r.ProviderAttempt{{ID: "attempt", Evidence: &r.ProviderEvidence{Access: r.ProviderAccess{Version: 1, SessionHash: session, Stopped: true}}}}}
	}
	if before, err := e2eRetainedLaunches([]e2eNativeUnit{unit()}, job()); err != nil || !maps.Equal(before, map[string]bool{launch: true}) {
		t.Fatal("retained interrupted launch refused", before, err)
	}
	type input struct {
		units []e2eNativeUnit
		job   *r.DurableJob
	}
	edit := func(u func(*e2eNativeUnit), j func(*r.DurableJob)) input {
		in := input{[]e2eNativeUnit{unit()}, job()}
		if u != nil {
			u(&in.units[0])
		}
		if j != nil {
			j(in.job)
		}
		return in
	}
	for name, in := range map[string]input{
		"launch directory missing":     {nil, job()},
		"launch directory unreadable":  {[]e2eNativeUnit{{Provider: "codex", Launch: launch}}, job()},
		"second retained launch":       {[]e2eNativeUnit{unit(), unit()}, job()},
		"no retained job":              {[]e2eNativeUnit{unit()}, nil},
		"unit not captured":            edit(func(u *e2eNativeUnit) { u.Captured = false }, nil),
		"unit not stopped":             edit(func(u *e2eNativeUnit) { u.Stopped = false }, nil),
		"stopped audit missing":        edit(func(u *e2eNativeUnit) { u.AuditPresent = false }, nil),
		"auth never exported":          edit(func(u *e2eNativeUnit) { u.AuthExported = false }, nil),
		"auth returned":                edit(func(u *e2eNativeUnit) { u.AuthReturned = true }, nil),
		"session completed":            edit(func(u *e2eNativeUnit) { u.Complete = true }, nil),
		"launch of another session":    edit(func(u *e2eNativeUnit) { u.SessionHash = strings.Repeat("7", 64) }, nil),
		"launch of another provider":   edit(func(u *e2eNativeUnit) { u.Provider = "claude" }, nil),
		"attempt not outstanding":      edit(nil, func(j *r.DurableJob) { j.Outstanding = false }),
		"no provider evidence":         edit(nil, func(j *r.DurableJob) { j.ProviderAttempts = nil }),
		"evidence of another attempt":  edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].ID = "other" }),
		"evidence of another session":  edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence.Access.SessionHash = strings.Repeat("7", 64) }),
		"evidence not stopped":         edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence.Access.Stopped = false }),
		"evidence record without body": edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence = nil }),
	} {
		if _, err := e2eRetainedLaunches(in.units, in.job); err == nil {
			t.Error(name, "accepted as the retained interrupted launch")
		}
	}
}

func TestE2ERequestVerdicts(t *testing.T) {
	origin := func(kind, role string, actor int64) *r.RequestOrigin {
		return &r.RequestOrigin{Kind: kind, ActorID: actor, ActorRole: role}
	}
	state := func() r.DurableState {
		return r.DurableState{PRs: map[string]*r.PRRecord{
			e2eScopeKey(1): {Scope: r.Scope{PR: 1}, Requests: []r.NativeRequest{{ID: 1, Admitted: true, Origin: origin("opened", "pr_author", r.OwnerID)}}},
			e2eScopeKey(2): {Scope: r.Scope{PR: 2}, Requests: []r.NativeRequest{{ID: 1, Origin: origin("comment", "commenter", r.OwnerID)}}},
		}}
	}
	want := map[int]e2eRequest{1: e2eAdmittedOpened(1, 1), 2: e2ePendingComment(2)}
	failures := func(d r.DurableState) []string {
		out := []string{}
		for _, v := range e2eRequestVerdicts("fenced", e2eRequests(d), want) {
			if !v.OK {
				out = append(out, v.Message)
			}
		}
		return out
	}
	if f := failures(state()); len(f) != 0 {
		t.Fatal("one request per PR refused:", f)
	}
	pr := func(d r.DurableState, n int) *r.PRRecord { return d.PRs[e2eScopeKey(n)] }
	for name, edit := range map[string]func(r.DurableState){
		"second automatic request on PR 1": func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("synchronize", "pr_author", r.OwnerID)})
		},
		"repeated open request on PR 1": func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("opened", "pr_author", r.OwnerID)})
		},
		"second owner comment on PR 2": func(d r.DurableState) {
			pr(d, 2).Requests = append(pr(d, 2).Requests, r.NativeRequest{ID: 2, Origin: origin("comment", "commenter", r.OwnerID)})
		},
		"PR 1 request not the leased job's": func(d r.DurableState) { pr(d, 1).Requests[0].ID = 3 },
		"PR 1 request without origin":       func(d r.DurableState) { pr(d, 1).Requests[0].Origin = nil },
		"PR 1 request removed":              func(d r.DurableState) { pr(d, 1).Requests[0].Removed = true },
		"PR 2 request admitted":             func(d r.DurableState) { pr(d, 2).Requests[0].Admitted = true },
		"PR 2 request automatic":            func(d r.DurableState) { pr(d, 2).Requests[0].Origin = origin("opened", "pr_author", 4242) },
		"PR 2 comment by another actor":     func(d r.DurableState) { pr(d, 2).Requests[0].Origin.ActorID = 4242 },
		"PR 2 request lost":                 func(d r.DurableState) { pr(d, 2).Requests = nil },
		"PR 2 record lost":                  func(d r.DurableState) { delete(d.PRs, e2eScopeKey(2)) },
		"request on another PR": func(d r.DurableState) {
			d.PRs[e2eScopeKey(3)] = &r.PRRecord{Scope: r.Scope{PR: 3}, Requests: []r.NativeRequest{{ID: 1, Origin: origin("comment", "commenter", r.OwnerID)}}}
		},
	} {
		d := state()
		edit(d)
		if len(failures(d)) == 0 {
			t.Error(name, "accepted")
		}
	}
}

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

// e2eFixtureFiles is every file the deterministic fake upstream holds at PR
// pr's head, with its exact content.
func e2eFixtureFiles(pr int) map[string]string {
	return map[string]string{"README.md": "synthetic base\n", "source.go": e2eSource, fmt.Sprintf("pr%d-only.go", pr): fmt.Sprintf("pull request %d only\n", pr)}
}

type e2eResult struct {
	Case, Revision, ReleaseHash   string
	SourceRuntimeHash             string
	SourceStubHash                string
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
	ID, State, Scope, Release           string
	PR                                  int
	Head                                string
	RequestID                           int64
	Consumed, Artifacts                 int
	Lease                               bool
	Outstanding                         bool
	SourceAttempts, ProviderAttempts    int
	LedgerFindings, ProviderAttemptKind []string
	// ArtifactSessions names each artifact's provider, round and session;
	// ArtifactSourceCalls each artifact's source tool and reader calls.
	ArtifactSessions, ArtifactSourceCalls []string
}

type e2eWrite struct {
	ID, Kind, State  string
	PR               int
	ReceiptID        int64
	Inline           int
	Head             string
	Marker, BodyHash string
}

type e2eEnv struct {
	t                              testing.TB
	scenario                       string
	root                           string
	meta, unit, export, work, auth string
	native                         map[r.Provider]string
	releases                       map[r.Provider]r.GitReviewerProviderRelease
	store                          *r.Store
	storePath                      string
	// live is the open service the fake upstream samples the source owner
	// journal through while it serves a Git exchange.
	live      atomic.Pointer[r.GitReviewerService]
	github    *e2eGitHub
	models    map[r.Provider]*e2eModel
	options   r.GitReviewerServiceOptions
	service   *r.GitReviewerService
	runCancel context.CancelFunc
	runDone   chan error
	config    r.Config
	result    *e2eResult
	mu        sync.Mutex
	units     *e2eUnitObserver
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
	runtimeHash, err := r.E2ESourceRuntimeHash(vm)
	if err != nil {
		t.Fatal("source VM runtime hash", err)
	}
	e.result.SourceRuntimeHash, e.result.SourceStubHash = runtimeHash, vm.Stub.SHA256
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
		e.releases[provider] = release.Providers[provider]
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
	e := &e2eEnv{t: t, scenario: scenario, root: root, attached: attach != "", native: map[r.Provider]string{}, releases: map[r.Provider]r.GitReviewerProviderRelease{}, models: map[r.Provider]*e2eModel{}, result: &e2eResult{Case: scenario, Root: root, Probes: map[string][]r.E2EProbe{}, Models: map[r.Provider][]e2eModelSession{}, AuthRecoveryClear: map[string]bool{}, FakeOnly: true}}
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
	e.live.Store(s)
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

// e2eJobFor returns PR pr's durable job when it is the only one. A second job
// for the PR could still run after every assertion judged the first, so any
// other count yields nil, which every caller treats as a failure.
func e2eJobFor(d r.DurableState, pr int) *r.DurableJob {
	var found *r.DurableJob
	for _, j := range d.Jobs {
		if j.Scope.PR == pr {
			if found != nil {
				return nil
			}
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
	e.result.NativeUnits = e2eReadNativeUnits(e.native, e.releases, e.config.Limits)
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
		verdicts = append(verdicts, e2eVerdict{u.Captured && u.Stopped && u.AuditPresent, fmt.Sprintf("native unit %s captured and stopped with its bound stopped audit", u.Launch)})
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
	return e2eParseUnits(string(out))
}

// e2eParseUnits parses a plain list-units listing into unit name and
// load/active/sub state plus description. A nonblank line that is not one
// sfl-alpha unit with its three states, or that repeats a unit, fails the
// listing rather than dropping a unit.
func e2eParseUnits(out string) (map[string]string, error) {
	units := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "sfl-alpha-") {
			return nil, fmt.Errorf("malformed unit listing line %q", line)
		}
		if _, repeated := units[fields[0]]; repeated {
			return nil, fmt.Errorf("unit listing repeats %s", fields[0])
		}
		units[fields[0]] = fields[1] + "/" + fields[2] + "/" + fields[3] + " " + strings.Join(fields[4:], " ")
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
	Provider, Launch, SessionHash string
	// ManifestHash is the pinned native release manifest both the unit
	// journal and the audit record for this launch and session.
	ManifestHash                         string
	Captured, Stopped                    bool
	Complete, AuthExported, AuthReturned bool
	AuditPresent                         bool
	// Access is the nonsecret provider access receipt of a present audit,
	// and EvidenceHash the digest of its complete provider evidence.
	Access       r.ProviderAccess
	EvidenceHash string
}

// e2eReadNativeUnits projects every entry of each provider's native root
// except the supervisor's own executables directory, judging each launch
// against that provider's release. An entry is never skipped: a directory
// without a readable unit journal, any other entry, and an unreadable root
// each project as a failed launch, which no launch verdict accepts.
func e2eReadNativeUnits(roots map[r.Provider]string, releases map[r.Provider]r.GitReviewerProviderRelease, limits r.Limits) []e2eNativeUnit {
	units := []e2eNativeUnit{}
	for provider, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			units = append(units, e2eNativeUnit{Provider: string(provider), Launch: "unreadable native root " + root})
			continue
		}
		for _, entry := range entries {
			if entry.Name() == "executables" && entry.IsDir() {
				continue
			}
			directory := filepath.Join(root, entry.Name())
			unit, err := os.ReadFile(filepath.Join(directory, "unit.json"))
			if err != nil {
				unit = nil
			}
			audit, err := os.ReadFile(filepath.Join(directory, "native-audit.json"))
			if err != nil {
				audit = nil
			}
			units = append(units, e2eNativeUnitFrom(provider, releases[provider], limits, entry.Name(), unit, audit))
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Launch < units[j].Launch })
	return units
}

// e2eNativeUnitFrom projects one native launch directory's unit journal and
// stopped audit receipt. The journal counts only as version 1 for this
// directory's launch: it names sfl-alpha-<launch>.service, its binding
// derives the containment ID from its session and launch, and it is the
// pinned native payload of release: payload kind native-vm, the release's
// stub image hash, and the manifest hash production derives from the
// release's pins for this launch and session. The audit counts
// (AuditPresent) only as a version 1 receipt repeating that exact binding
// and manifest hash.
// Its provider evidence, required once auth was exported or the session
// completed, must be the stopped receipt of the same session under the
// release's policy and pass production's provider receipt validation under
// the session limits: the stopped-receipt invariants always, and the
// completed-receipt validation when the audit records a completed session. A
// copied, foreign, failed or impossible receipt therefore proves nothing. The
// audit's auth lifecycle must be possible: production returns only exported
// auth, and a completed session has both exported and returned it.
func e2eNativeUnitFrom(provider r.Provider, release r.GitReviewerProviderRelease, limits r.Limits, launch string, unitRaw, auditRaw []byte) e2eNativeUnit {
	u := e2eNativeUnit{Provider: string(provider), Launch: launch}
	policy := release.Policy
	var unit struct {
		Version                int
		Unit, PayloadKind      string
		StubHash, ManifestHash string
		Captured, Stopped      bool
		Binding                r.AuthBinding
	}
	if unitRaw == nil || json.Unmarshal(unitRaw, &unit) != nil {
		return u
	}
	b := unit.Binding
	if unit.Version != 1 || b.LaunchID != launch || unit.Unit != "sfl-alpha-"+launch+".service" || len(b.SessionHash) != 64 || b.ContainmentID != r.Digest(struct{ Session, Launch string }{b.SessionHash, launch}) {
		return u
	}
	manifest, err := r.E2ENativeManifestHash(release, provider, b)
	if err != nil || unit.PayloadKind != "native-vm" || len(release.Stub.SHA256) != 64 || unit.StubHash != release.Stub.SHA256 || unit.ManifestHash != manifest {
		return u
	}
	u.SessionHash, u.Captured, u.Stopped, u.ManifestHash = b.SessionHash, unit.Captured, unit.Stopped, unit.ManifestHash
	var audit struct {
		Version                              int
		Binding                              r.AuthBinding
		ManifestHash                         string
		Provider                             *r.ProviderEvidence
		Complete, AuthExported, AuthReturned bool
	}
	if auditRaw == nil || json.Unmarshal(auditRaw, &audit) != nil || audit.Version != 1 || audit.Binding != b || audit.ManifestHash != unit.ManifestHash {
		return u
	}
	if (audit.AuthReturned && !audit.AuthExported) || (audit.Complete && !audit.AuthReturned) {
		return u
	}
	if evidence := audit.Provider; evidence != nil {
		if evidence.Policy.Provider != provider || r.Digest(evidence.Policy) != r.Digest(policy) || evidence.Access.Version != 1 || evidence.Access.SessionHash != b.SessionHash || evidence.Access.PolicyHash != r.Digest(policy) || !evidence.Access.Stopped {
			return u
		}
		if r.E2EValidateProviderReceipt(evidence, provider, limits, b.SessionHash, audit.Complete) != nil {
			return u
		}
	} else if audit.Complete || audit.AuthExported {
		return u
	}
	u.AuditPresent, u.Complete, u.AuthExported, u.AuthReturned = true, audit.Complete, audit.AuthExported, audit.AuthReturned
	if audit.Provider != nil {
		u.Access, u.EvidenceHash = audit.Provider.Access, r.Digest(audit.Provider)
	}
	return u
}

// e2eRetainedLaunches proves the native launch a root attached after
// close-provider must hold, and returns it as the only pre-existing launch:
// exactly one launch, captured and whole-unit stopped, with a bound stopped
// audit recording the auth as exported, not returned and the session not
// complete. Its provider and session must be the retained job's outstanding
// attempt, and the job must hold exactly one record of that attempt, whose
// durably recorded stopped provider evidence is the audit's complete evidence
// (by digest), not merely another stopped receipt of the same session. A
// missing launch yields no launch, and an unreadable launch directory or any
// other root entry a failed launch beside it, so either is refused and the
// fence cannot pass without exactly its interrupted launch.
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
	records := 0
	for _, a := range j.ProviderAttempts {
		if a.ID == j.AttemptID {
			records++
		}
	}
	if j.AttemptID == "" || records != 1 {
		return nil, fmt.Errorf("job %s holds %d records of its outstanding attempt %q, want one", j.Job.ID, records, j.AttemptID)
	}
	recorded, err := e2eRecordedEvidence(j, u)
	if err != nil || recorded.ID != j.AttemptID || !recorded.Evidence.Access.Stopped {
		return nil, fmt.Errorf("job %s holds no stopped provider evidence of its outstanding attempt equal to retained launch %s's audit: %v", j.Job.ID, u.Launch, err)
	}
	return map[string]bool{u.Launch: true}, nil
}

// e2eArtifactAttempt requires job j to record artifact a's provider evidence
// and source access under one attempt, as the durable reviewer records both
// before storing the artifact: exactly one provider attempt holds a's
// evidence (of a's session and provider), and exactly one source attempt of
// that attempt ID holds a's source access, which is of a's session.
func e2eArtifactAttempt(j *r.DurableJob, a r.Artifact) error {
	if a.ProviderEvidence == nil || a.SourceAccess == nil || a.SourceAccess.SessionHash != a.InputHash {
		return fmt.Errorf("artifact %s R%d lacks provider evidence or source access of its session", a.Report.Provider, a.Report.Round)
	}
	attempt, err := e2eRecordedEvidence(j, e2eNativeUnit{Provider: string(a.Report.Provider), Launch: "of artifact " + a.InputHash, SessionHash: a.InputHash, EvidenceHash: r.Digest(a.ProviderEvidence)})
	if err != nil {
		return err
	}
	records := 0
	for _, s := range j.SourceAttempts {
		if s.ID == attempt.ID {
			records++
			if s.Access == nil || r.Digest(s.Access) != r.Digest(a.SourceAccess) {
				return fmt.Errorf("attempt %s records other source access than artifact %s R%d", attempt.ID, a.Report.Provider, a.Report.Round)
			}
		}
	}
	if records != 1 {
		return fmt.Errorf("attempt %s has %d source access records, want one", attempt.ID, records)
	}
	return nil
}

// e2eRecordedEvidence returns the one provider attempt job j durably records
// with launch u's complete audit evidence: equal by digest, of u's session
// and provider. A launch without projected evidence matches nothing.
func e2eRecordedEvidence(j *r.DurableJob, u e2eNativeUnit) (r.ProviderAttempt, error) {
	if j == nil || u.EvidenceHash == "" {
		return r.ProviderAttempt{}, errors.New("no job or no audit evidence to compare")
	}
	found := []r.ProviderAttempt{}
	for _, a := range j.ProviderAttempts {
		if a.Evidence != nil && r.Digest(a.Evidence) == u.EvidenceHash {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return r.ProviderAttempt{}, fmt.Errorf("job %s records launch %s's evidence %d times, want once", j.Job.ID, u.Launch, len(found))
	}
	a := found[0]
	if a.ID == "" || a.Evidence.Access.SessionHash != u.SessionHash || string(a.Evidence.Policy.Provider) != u.Provider {
		return r.ProviderAttempt{}, fmt.Errorf("job %s records launch %s's evidence under another attempt, session or provider", j.Job.ID, u.Launch)
	}
	return a, nil
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
	// backends[i] is the canned backend bound to sessions[i]; its counts are
	// read whenever the sessions are copied, so they include every request
	// the backend served, not only those up to the last inference.
	backends []*f.Backend
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
	m.backends = append(m.backends, backend)
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
}

// sessionsCopy returns every checked session with the counts its own
// backend has observed so far.
func (m *e2eModel) sessionsCopy() []e2eModelSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]e2eModelSession(nil), m.sessions...)
	for i := range out {
		out[i].Counts = m.backends[i].Counts()
	}
	return out
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
	// Journal is the source owner journal sampled while the fake upstream
	// served this exchange, before it answered; JournalErr a failed sample.
	Journal    []r.E2ESourceRecord
	JournalErr string
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
	write("README.md", e2eFixtureFiles(0)["README.md"])
	e2eGit(t, git, work, "add", "README.md")
	e2eGit(t, git, work, "commit", "--quiet", "-m", "synthetic base")
	g.baseSHA = e2eGit(t, git, work, "rev-parse", "HEAD")
	for _, n := range []int{1, 2} {
		e2eGit(t, git, work, "checkout", "--quiet", "-b", "pr"+strconv.Itoa(n), g.baseSHA)
		files := e2eFixtureFiles(n)
		write("source.go", files["source.go"])
		write(fmt.Sprintf("pr%d-only.go", n), files[fmt.Sprintf("pr%d-only.go", n)])
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

var (
	e2eWant   = regexp.MustCompile(`[0-9a-f]{4}want(\S*)(?: (\S*))?`)
	e2eObject = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// e2eWants returns every want line of an upload-pack request body: the
// object ID of a well-formed want, and an unrecognized entry for any other
// (a want-ref, or a missing or malformed object ID), which no exact set of
// wanted heads matches.
func e2eWants(body []byte) []string {
	wants := []string{}
	for _, m := range e2eWant.FindAllStringSubmatch(string(body), -1) {
		if m[1] == "" && e2eObject.MatchString(m[2]) {
			wants = append(wants, m[2])
		} else {
			wants = append(wants, fmt.Sprintf("unrecognized want%s %.48s", m[1], m[2]))
		}
	}
	return wants
}

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
	// Production rechecks the live granted instance before every upstream
	// read, so the journal sampled here, before this exchange is answered,
	// shows the acquiring attempt's lifecycle while its unit reads.
	exchange.Journal, exchange.JournalErr = g.env.sampleJournal()
	if upload {
		body, err := io.ReadAll(io.LimitReader(q.Body, 4<<20))
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		exchange.Operation = "upload-pack"
		exchange.Wants = e2eWants(body)
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

// sampleJournal reads the open service's source owner journal. The read
// waits only for the journal's operation token, which every journal write
// holds just for that write; a sample that does not return within its bound
// is recorded as failed and the exchange proceeds.
func (e *e2eEnv) sampleJournal() ([]r.E2ESourceRecord, string) {
	service := e.live.Load()
	if service == nil {
		return nil, "no open service to sample"
	}
	type sample struct {
		records []r.E2ESourceRecord
		err     error
	}
	done := make(chan sample, 1)
	go func() {
		records, err := service.E2ESourceJournal()
		done <- sample{records, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return nil, got.err.Error()
		}
		return got.records, ""
	case <-time.After(10 * time.Second):
		return nil, "journal sample timed out"
	}
}

// snapshot is the snapshot PR n binds on the deterministic fake upstream.
func (g *e2eGitHub) snapshot(n int) r.Snapshot {
	return r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: n, Head: g.commits[strconv.Itoa(n)], Base: g.baseSHA, MergeBase: g.baseSHA}
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
			// Keep the evidence bounded and nonsecret: the marker, size, anchors
			// and the canonical document hash production derives from the
			// remote review in comment order, which the intent's must equal.
			inline, document := []string{}, []r.InlineComment{}
			for _, c := range pr.ReviewComments[review["id"].(int64)] {
				inline = append(inline, fmt.Sprintf("%v:%v", c["path"], c["original_line"]))
				path, _ := c["path"].(string)
				line, _ := c["original_line"].(int)
				text, _ := c["body"].(string)
				document = append(document, r.InlineComment{Path: path, Line: line, Body: text})
			}
			marker, hash := r.E2ERemoteReview(body, document)
			s.Reviews[n] = append(s.Reviews[n], map[string]any{"id": review["id"], "state": review["state"], "commit_id": review["commit_id"], "marker": marker, "document_hash": hash, "body_bytes": len(body), "inline": inline})
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
	Base      string
	Release   string
	Source    e2eSourceBinding
	Posts     map[int]int
	Reviews   map[int][]map[string]any
	Records   []r.E2ESourceRecord
	Exports   []r.E2EExportRecord
	SourceErr error
	Units     []e2eNativeUnit
	Sessions  []e2eModelSession
	// Exchanges are this process's fake upstream exchanges with their
	// journal samples. Attached marks a process whose source attempts were
	// acquired, and their order judged, by the earlier process.
	Exchanges []e2eGitExchange
	Attached  bool
}

// e2eSourceBinding is the release binding every source record of a run must
// carry: the current release package hash, the runtime hash of the source VM
// manifest that release derives, and the release's source stub image hash.
type e2eSourceBinding struct{ Release, Runtime, Stub string }

// completion gathers the current evidence from the open service.
func (e *e2eEnv) completion() e2eCompletion {
	summary := e.github.summary()
	c := e2eCompletion{State: e.state(), Heads: map[int]string{}, Base: e.github.baseSHA, Release: e.options.Current, Source: e2eSourceBinding{e.result.ReleaseHash, e.result.SourceRuntimeHash, e.result.SourceStubHash}, Posts: summary.ReviewPosts, Reviews: summary.Reviews, Units: e2eReadNativeUnits(e.native, e.releases, e.config.Limits), Exchanges: summary.GitExchanges, Attached: e.attached}
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		c.Sessions = append(c.Sessions, e.models[provider].sessionsCopy()...)
	}
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

// assertCompleted judges the open service's completed evidence for prs with
// e2eCompletedVerdicts, records the source records it read, and returns the
// PRs' jobs in order, nil for a PR without exactly one.
func (e *e2eEnv) assertCompleted(label string, prs ...int) []*r.DurableJob {
	e.t.Helper()
	c := e.completion()
	verdicts, jobs := e2eCompletedVerdicts(label, c, prs)
	e.checkAll(verdicts)
	if c.SourceErr == nil {
		e.mu.Lock()
		e.result.SourceRecords = append(e.result.SourceRecords, c.Records)
		e.result.Exports = append(e.result.Exports, c.Exports)
		e.mu.Unlock()
	}
	return jobs
}

// e2eReviewedVerdicts judges one PR's completed review. The PR must have
// exactly one durable job, bound to its scope, the fake upstream's snapshot
// and the current release, holding exactly the adversarial round set of
// artifacts produced for that job, each session checked once by its own
// provider's backend. Its sole confirmed intent must be bound to that job and
// to the one remote review, whose canonical document is the intent's. It
// returns the PR's job, or nil when there is not exactly one.
func e2eReviewedVerdicts(c e2eCompletion, pr int) ([]e2eVerdict, *r.DurableJob) {
	j := e2eJobFor(c.State, pr)
	if j == nil {
		return []e2eVerdict{{false, fmt.Sprintf("PR %d has exactly one durable job", pr)}}, nil
	}
	want := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: pr, Head: c.Heads[pr], Base: c.Base, MergeBase: c.Base}
	rounds := e2eArtifactRounds(j)
	checked := e2eCheckedSessions(c.Sessions, j)
	v := []e2eVerdict{
		{j.State == "dual_complete", fmt.Sprintf("PR %d job %s reached dual_complete (got %s)", pr, j.Job.ID, j.State)},
		{j.Job.Config.Mode == r.Adversarial && j.Checkpoint.ConsumedSessions == 4 && j.Checkpoint.JobHash == r.Digest(j.Job), fmt.Sprintf("PR %d adversarial engine checkpoint of its own job consumed exactly 4 sessions (consumed %d)", pr, j.Checkpoint.ConsumedSessions)},
		{c.Heads[pr] != "" && c.Base != "" && j.Job.Snapshot == want && j.Scope == e2eScope(pr) && j.Job.Release == c.Release, fmt.Sprintf("PR %d job binds its own scope, head, base and the current release", pr)},
		{rounds == nil, fmt.Sprintf("PR %d stored exactly Codex R1, Claude R1, Codex R2 and Claude R2, each produced for its job: %v", pr, rounds)},
		{checked == nil, fmt.Sprintf("PR %d each artifact session was checked once by its own provider's backend for its job and round, and made exactly the fixture's exchanges: %v", pr, checked)},
	}
	writes := e2eWritesFor(c.State, pr, "review")
	w := e2eSoleConfirmed(writes)
	v = append(v, e2eVerdict{w != nil, fmt.Sprintf("PR %d has exactly one review intent and it is confirmed (%d writes)", pr, len(writes))})
	if w != nil {
		v = append(v,
			e2eVerdict{w.Scope == j.Scope && w.RequestID == j.RequestID && w.Generation == j.Job.Generation && w.Snapshot == j.Job.Snapshot && w.JobHash == r.Digest(j.Job) && w.CheckpointHash == r.Digest(j.Checkpoint) && w.ReceiptID > 0, fmt.Sprintf("PR %d confirmed intent binds its job's request, generation, snapshot, job and checkpoint, and a receipt", pr)},
			e2eVerdict{w.Marker == "<!-- sfl-alpha:"+w.ID+" -->" && w.BodyHash == r.Digest(w.Document), fmt.Sprintf("PR %d confirmed intent's marker names its own ID and its body hash is its document's", pr)},
			e2eVerdict{len(w.Document.Inline) == 1 && w.Document.Inline[0].Path == "source.go" && w.Document.Inline[0].Line == 1, fmt.Sprintf("PR %d document carries exactly the source-backed inline finding at source.go:1", pr)})
	}
	v = append(v, e2eVerdict{c.Posts[pr] == 1, fmt.Sprintf("PR %d received exactly one review POST (got %d)", pr, c.Posts[pr])})
	reviews := c.Reviews[pr]
	v = append(v, e2eVerdict{len(reviews) == 1 && reviews[0]["state"] == "COMMENTED" && reviews[0]["commit_id"] == j.Job.Snapshot.Head && reviews[0]["marker"] != "", fmt.Sprintf("PR %d remote review is one marked COMMENT on its head", pr)})
	if w != nil {
		v = append(v, e2eVerdict{len(reviews) == 1 && reviews[0]["id"] == any(w.ReceiptID) && reviews[0]["marker"] == any(w.Marker) && reviews[0]["document_hash"] == any(w.BodyHash), fmt.Sprintf("PR %d remote review is the confirmed intent's: its receipt ID, exact marker and canonical document hash", pr)})
	}
	var inline []string
	if len(reviews) == 1 {
		inline, _ = reviews[0]["inline"].([]string)
	}
	v = append(v, e2eVerdict{slices.Equal(inline, []string{"source.go:1"}), fmt.Sprintf("PR %d remote review carries exactly the inline comment at source.go:1 (%v)", pr, inline)})
	return v, j
}

// e2eAdversarialRounds is the exact artifact set of an adversarial review.
var e2eAdversarialRounds = map[string]int{"codex R1": 1, "claude R1": 1, "codex R2": 1, "claude R2": 1}

// e2eArtifactRounds requires j's checkpoint to hold exactly one artifact of
// each adversarial provider round, every one produced for j: its job hash,
// report job ID, snapshot hash and config hash are j's, and no two artifacts
// share a session.
func e2eArtifactRounds(j *r.DurableJob) error {
	rounds, sessions := map[string]int{}, map[string]bool{}
	for _, a := range j.Checkpoint.Artifacts {
		rounds[fmt.Sprintf("%s R%d", a.Report.Provider, a.Report.Round)]++
		if a.JobHash != r.Digest(j.Job) || a.Report.JobID != j.Job.ID || a.Report.SnapshotHash != r.Digest(j.Job.Snapshot) || a.Report.ConfigHash != j.Job.Config.Hash() {
			return fmt.Errorf("artifact %s R%d was not produced for job %s", a.Report.Provider, a.Report.Round, j.Job.ID)
		}
		if a.InputHash == "" || sessions[a.InputHash] {
			return fmt.Errorf("artifact %s R%d repeats or lacks a session", a.Report.Provider, a.Report.Round)
		}
		sessions[a.InputHash] = true
	}
	if !maps.Equal(rounds, e2eAdversarialRounds) {
		return fmt.Errorf("artifact rounds %v", rounds)
	}
	return nil
}

// e2eCheckedSessions requires each artifact session of j to have been checked
// by the fake backends exactly once outside a fault hold, and every check of
// that session to name the artifact's provider and round, j and j's PR and
// head. The unheld check must have made exactly the canned exchanges
// (e2eFixtureExchanges). Every other check of j must have been held by a
// fault hook: an unheld check that produced no artifact is unexplained
// provider work.
func e2eCheckedSessions(sessions []e2eModelSession, j *r.DurableJob) error {
	artifacts := map[string]r.Artifact{}
	for _, a := range j.Checkpoint.Artifacts {
		artifacts[a.InputHash] = a
	}
	unheld := map[string]int{}
	for _, s := range sessions {
		held := s.HeldInCheck || s.HeldInference
		a, ok := artifacts[s.SessionHash]
		if !ok {
			if s.JobID == j.Job.ID && !held {
				return fmt.Errorf("unheld %s R%d check of job %s produced no artifact", s.Provider, s.Round, j.Job.ID)
			}
			continue
		}
		if s.Provider != a.Report.Provider || s.Round != a.Report.Round || s.JobID != j.Job.ID || s.PR != j.Job.Snapshot.PR || s.Head != j.Job.Snapshot.Head {
			return fmt.Errorf("check of %s R%d session is %s R%d of job %s PR %d", a.Report.Provider, a.Report.Round, s.Provider, s.Round, s.JobID, s.PR)
		}
		if !held {
			unheld[s.SessionHash]++
			if err := e2eFixtureExchanges(s.Counts, a, j.Job.Snapshot.Head, e2eFixtureFiles(j.Job.Snapshot.PR)); err != nil {
				return fmt.Errorf("%s R%d session: %v", a.Report.Provider, a.Report.Round, err)
			}
		}
	}
	for hash, a := range artifacts {
		if unheld[hash] != 1 {
			return fmt.Errorf("%s R%d session checked %d times outside a fault hold", a.Report.Provider, a.Report.Round, unheld[hash])
		}
	}
	return nil
}

// e2eFixtureExchanges is the exchange contract the native fixtures require of
// every completed canned phase (cmd/nativevmfixture protocol and control
// phases): the backend answered exactly three inferences with no failure and
// no telemetry; Codex captured every mandatory metadata route and no
// negotiation reached the backend; and the phase made the canned source read
// and search (two tool calls). The reader calls are not fixed here: search
// reads every allowlisted path of the job, which the single-file fixtures do
// not have. Instead every object the source service served the phase must be
// the fake upstream's exact bytes of that path at the job's head, and
// source.go must be among them. The artifact's stopped provider receipt must
// also count exactly the inference and metadata requests the backend
// observed.
func e2eFixtureExchanges(counts f.Counts, a r.Artifact, head string, files map[string]string) error {
	if counts.Inference != 3 || counts.Failures != 0 || counts.Telemetry != 0 {
		return fmt.Errorf("backend counted %d inferences, %d failures and %d telemetry requests, want 3, 0 and 0", counts.Inference, counts.Failures, counts.Telemetry)
	}
	if a.Report.Provider == r.Codex && (!counts.MetadataCaptured || counts.Negotiation != 0) {
		return fmt.Errorf("codex metadata captured %v with %d negotiations reaching the backend", counts.MetadataCaptured, counts.Negotiation)
	}
	if a.SourceAccess == nil || a.SourceAccess.ToolCalls != 2 {
		return errors.New("phase did not make exactly the canned source read and search")
	}
	source := false
	for _, read := range a.SourceAccess.Reads {
		want, known := files[read.Path]
		if !known || read.Revision != head || read.Hash != r.Digest([]byte(want)) || read.Bytes != len(want) {
			return fmt.Errorf("phase was served %s at %.12s, not the fake upstream's bytes at its head", read.Path, read.Revision)
		}
		source = source || read.Path == "source.go"
	}
	if !source {
		return errors.New("phase was never served source.go")
	}
	if a.ProviderEvidence == nil || a.ProviderEvidence.Access.InferenceRequests != counts.Inference || a.ProviderEvidence.Access.MetadataRequests != counts.Metadata {
		return fmt.Errorf("receipt counts differ from the backend's %d inferences and %d metadata requests", counts.Inference, counts.Metadata)
	}
	return nil
}

// e2eDurableSetVerdicts requires the durable state to hold exactly one job
// and exactly one review intent for each of prs, and no other job or write
// of any kind. A further job or intent could still run or publish later.
func e2eDurableSetVerdicts(label string, d r.DurableState, prs ...int) []e2eVerdict {
	want, jobs, writes := map[int]int{}, map[int]int{}, map[int]int{}
	for _, pr := range prs {
		want[pr] = 1
	}
	for _, j := range d.Jobs {
		jobs[j.Scope.PR]++
	}
	other := 0
	for _, w := range d.Writes {
		if w.Kind == "review" {
			writes[w.Scope.PR]++
		} else {
			other++
		}
	}
	return []e2eVerdict{
		{maps.Equal(jobs, want), fmt.Sprintf("%s: durable state holds exactly one job for each of PRs %v and no other job (%v)", label, prs, jobs)},
		{maps.Equal(writes, want) && other == 0, fmt.Sprintf("%s: durable state holds exactly one review intent for each of PRs %v and no other write (%v, %d other)", label, prs, writes, other)},
	}
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

// e2eSourceEvidence requires exactly one source record per job, each with one
// archive export of the same attempt, before any entry is validated, so a
// missing journal or archive cannot pass vacuously. As production recovery
// does, an export counts only when its request hash is the digest of its
// record's source request and its binding (attempt, release, runtime and
// snapshot) is the record's complete materialization binding. Record and
// export agreeing is not enough: each record must name its attempt's unit,
// carry the current release package hash and the runtime hash that release
// derives, record the release's source stub image, and bind the snapshot of
// exactly one of jobs, each job's once.
func e2eSourceEvidence(records []r.E2ESourceRecord, exports []r.E2EExportRecord, jobs []*r.DurableJob, bind e2eSourceBinding) error {
	want := len(jobs)
	if want <= 0 || len(records) != want || len(exports) != want {
		return fmt.Errorf("%d records and %d exports, want %d of each", len(records), len(exports), want)
	}
	if bind.Release == "" || bind.Runtime == "" || bind.Stub == "" {
		return errors.New("no current release binding to compare")
	}
	owners := map[r.Snapshot]int{}
	for _, j := range jobs {
		if j == nil {
			return errors.New("a source record has no job to bind")
		}
		owners[j.Job.Snapshot]++
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
		if seen[rec.Attempt] || !ok || x.Snapshot != rec.Snapshot || rec.SourceRequestHash == "" || x.RequestHash != rec.SourceRequestHash || x.ReleaseHash != rec.ReleaseHash || x.RuntimeHash != rec.RuntimeHash {
			return fmt.Errorf("source attempt %s lacks its own export", rec.Attempt)
		}
		if rec.Unit != "sfl-alpha-"+rec.Attempt+".service" || rec.ReleaseHash != bind.Release || rec.RuntimeHash != bind.Runtime || rec.StubHash != bind.Stub {
			return fmt.Errorf("source attempt %s is not this release's unit", rec.Attempt)
		}
		if owners[rec.Snapshot] != 1 {
			return fmt.Errorf("source attempt %s binds no job snapshot of its own (PR %d)", rec.Attempt, rec.Snapshot.PR)
		}
		owners[rec.Snapshot]--
		seen[rec.Attempt] = true
	}
	return nil
}

func (e *e2eEnv) assertSourceStopped(label string, jobs ...*r.DurableJob) {
	e.t.Helper()
	c := e.completion()
	e.checkAll(e2eSourceVerdicts(label, c, jobs))
	if c.SourceErr == nil {
		e.mu.Lock()
		e.result.SourceRecords = append(e.result.SourceRecords, c.Records)
		e.result.Exports = append(e.result.Exports, c.Exports)
		e.mu.Unlock()
	}
}

// e2eSourceVerdicts requires one source attempt per job, bound to it and the
// current release, each with a staged export of its own and a final journal
// record holding the start intent, settled start, capture, grant, revocation
// and durable stop. A process that acquired the attempts also requires the
// order (e2eSourceOrderVerdicts); an attached restart cannot observe the
// earlier process's acquisition, whose order that process judged.
func e2eSourceVerdicts(label string, c e2eCompletion, jobs []*r.DurableJob) []e2eVerdict {
	if c.SourceErr != nil {
		return []e2eVerdict{{false, fmt.Sprintf("%s: source records readable: %v", label, c.SourceErr)}}
	}
	err := e2eSourceEvidence(c.Records, c.Exports, jobs, c.Source)
	v := []e2eVerdict{{err == nil, fmt.Sprintf("%s: %d source records with matching staged-export entries: %v", label, len(jobs), err)}}
	for _, rec := range c.Records {
		v = append(v, e2eVerdict{rec.StartIntent && rec.StartSettled && rec.Captured && rec.Stopped && rec.Revoked && rec.GrantHash != "", fmt.Sprintf("%s: source attempt %s (unit %s) final record holds its start intent, settled start, capture, grant, revocation and durable stop", label, rec.Attempt, rec.Unit)})
	}
	for _, x := range c.Exports {
		v = append(v, e2eVerdict{x.State == "staged", fmt.Sprintf("%s: retained export %s is staged (%s)", label, x.Attempt, x.State)})
	}
	if !c.Attached {
		v = append(v, e2eSourceOrderVerdicts(label, c.Records, c.Exchanges)...)
	}
	return v
}

// e2eSourceOrderVerdicts proves each source attempt's lifecycle order from
// two observations: the journal the fake upstream sampled while serving the
// attempt's one successful upload-pack (wanting its head), and the final
// record. The sample must hold exactly one record of the attempt, with its
// start recorded and settled, its unit captured and granted under the final
// record's unit, invocation and grant, and not yet revoked or stopped; the
// final record must be revoked and stopped. Start, settle, capture and grant
// therefore precede the upstream read, and revocation and stop follow it.
// The order among start, settle and capture is the journal's own invariant
// (each later state requires the earlier ones in every recorded write).
func e2eSourceOrderVerdicts(label string, records []r.E2ESourceRecord, exchanges []e2eGitExchange) []e2eVerdict {
	v := []e2eVerdict{{len(records) > 0, fmt.Sprintf("%s: %d source records to order", label, len(records))}}
	for _, rec := range records {
		uploads := []e2eGitExchange{}
		for _, x := range exchanges {
			if x.Operation == "upload-pack" && x.Status == 200 && slices.Contains(x.Wants, rec.Snapshot.Head) {
				uploads = append(uploads, x)
			}
		}
		ok := len(uploads) == 1 && uploads[0].JournalErr == "" && rec.Revoked && rec.Stopped
		if ok {
			sampled := []r.E2ESourceRecord{}
			for _, s := range uploads[0].Journal {
				if s.Attempt == rec.Attempt {
					sampled = append(sampled, s)
				}
			}
			ok = len(sampled) == 1
			if ok {
				s := sampled[0]
				ok = s.StartIntent && s.StartSettled && s.Captured && s.GrantHash != "" && s.GrantHash == rec.GrantHash && s.Unit == rec.Unit && s.InvocationID == rec.InvocationID && !s.Revoked && !s.Stopped
			}
		}
		v = append(v, e2eVerdict{ok, fmt.Sprintf("%s: source attempt %s was started, settled, captured and granted, and not yet revoked or stopped, when the fake upstream served its one upload-pack (%d), and was revoked and stopped afterwards", label, rec.Attempt, len(uploads))})
	}
	return v
}

// e2eNativeSessionVerdict requires exactly one completed native launch per
// artifact session of the given jobs, of the artifact's provider and carrying
// the artifact's provider evidence, with auth exported and returned and a
// whole-unit stop, and no other launch except one interrupted launch per
// listed extra session hash. It keys each artifact session to the artifact's
// provider and evidence digest. An artifact counts only when its job records
// its provider evidence and source access under one attempt
// (e2eArtifactAttempt); a launch counts only as its artifact's provider with
// the same evidence. An allowed extra launch must be stopped,
// audited and not complete. No job or artifact passes vacuously.
func e2eNativeSessionVerdict(units []e2eNativeUnit, jobs []*r.DurableJob, extra ...string) e2eVerdict {
	type session struct{ provider, evidence string }
	expected := map[string]session{}
	artifacts, unbound := 0, 0
	for _, j := range jobs {
		if j == nil {
			unbound++
			continue
		}
		for _, a := range j.Checkpoint.Artifacts {
			artifacts++
			want := session{string(a.Report.Provider), ""}
			if a.ProviderEvidence != nil {
				want.evidence = r.Digest(a.ProviderEvidence)
			}
			if e2eArtifactAttempt(j, a) != nil {
				unbound++
			}
			expected[a.InputHash] = want
		}
	}
	allowed := map[string]int{}
	for _, hash := range extra {
		allowed[hash]++
	}
	seen := map[string]int{}
	unexpected := 0
	for _, u := range units {
		want, ok := expected[u.SessionHash]
		switch {
		case ok && want.evidence != "" && u.Provider == want.provider && u.EvidenceHash == want.evidence && u.Captured && u.Stopped && u.AuditPresent && u.Complete && u.AuthExported && u.AuthReturned:
			seen[u.SessionHash]++
		case allowed[u.SessionHash] > 0 && u.Captured && u.Stopped && u.AuditPresent && !u.Complete:
			allowed[u.SessionHash]--
		default:
			unexpected++
		}
	}
	once := len(expected) > 0 && artifacts == len(expected) && unbound == 0 && len(seen) == len(expected)
	for _, n := range seen {
		once = once && n == 1
	}
	return e2eVerdict{once && unexpected == 0, fmt.Sprintf("each of %d job-bound sessions ran in exactly one completed native launch of its artifact's provider and evidence with auth exported and returned, and no other launch exists (%d artifacts, %d unbound, %d matched, %d unexpected)", len(expected), artifacts, unbound, len(seen), unexpected)}
}

// e2eProviderReopenVerdicts judges close-provider after a Close that proved
// completion and a reopen with the same assertions as every other completed
// review: one confirmed intent, one POST and the expected remote COMMENT,
// stopped source receipts, and one completed native launch per artifact. The
// interrupted launch is the only other launch allowed.
func e2eProviderReopenVerdicts(c e2eCompletion, interrupted string) ([]e2eVerdict, *r.DurableJob) {
	verdicts, jobs := e2eCompletedVerdicts("after reopen", c, []int{1}, interrupted)
	return verdicts, jobs[0]
}

// e2eCompletedVerdicts judges a completed case's evidence for the reviewed
// prs: exactly their jobs and review intents, each PR's review, the source
// attempts of their jobs, and the native launches of their artifact sessions,
// where extra lists the session hashes of the only other launches allowed. It
// returns the PRs' jobs in order, nil for a PR without exactly one.
func e2eCompletedVerdicts(label string, c e2eCompletion, prs []int, extra ...string) ([]e2eVerdict, []*r.DurableJob) {
	verdicts, jobs := e2eDurableSetVerdicts(label, c.State, prs...), []*r.DurableJob{}
	for _, pr := range prs {
		v, j := e2eReviewedVerdicts(c, pr)
		verdicts, jobs = append(verdicts, v...), append(jobs, j)
	}
	if slices.Contains(jobs, nil) {
		return verdicts, jobs
	}
	verdicts = append(verdicts, e2eSourceVerdicts(label, c, jobs)...)
	return append(verdicts, e2eNativeSessionVerdict(c.Units, jobs, extra...)), jobs
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
	job := e.assertCompleted("after review", 1)[0]
	if job == nil {
		return
	}
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eOpenedRequestVerdicts("after review", d, job) })
	e.checkUploads(e.github.commits["1"])
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
	jobs := e.assertCompleted("after both reviews", 1, 2)
	j1, j2 := jobs[0], jobs[1]
	if j1 == nil || j2 == nil || len(e.result.SourceRecords) == 0 {
		return
	}
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eIsolationRequestVerdicts(d, j1, j2) })
	// Admission: the host checks (session starts) of the two jobs never
	// interleave. That whole sessions do not overlap is the serial admission
	// rule (no job is selected while another holds its lease), not observed.
	sessions := append(e.models[r.Codex].sessionsCopy(), e.models[r.Claude].sessionsCopy()...)
	switches, unbound := e2eSessionsBound(sessions, e2eJobSummary(j1), e2eJobSummary(j2))
	e.check(len(unbound) == 0, "every session binds its own job's PR and head (unbound %v)", unbound)
	e.check(len(sessions) == 8 && switches == 1, "eight session checks ran as two non-interleaved blocks, one per job (%d job switches)", switches)
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
	e.checkUploads(h1, h2)
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
		e.checkAll(e2eProbeVerdicts(pair.job.Scope.PR, probes, pair.own, pair.foreign, pair.ownFile, pair.foreignFile))
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

// checkUploads judges the fake upstream's Git exchanges against the heads
// this run must have acquired, one acquisition each.
func (e *e2eEnv) checkUploads(heads ...string) {
	e.t.Helper()
	e.checkAll([]e2eVerdict{e2eUploadVerdict(e.github.summary().GitExchanges, e.github.baseSHA, heads...)})
}

// e2eUploadVerdict requires exactly one successful advertisement and one
// successful upload-pack per head, each upload-pack wanting exactly its head
// and the base, and no other Git exchange.
func e2eUploadVerdict(exchanges []e2eGitExchange, base string, heads ...string) e2eVerdict {
	wanted, uploads, advertisements, other := map[string]string{}, map[string]int{}, 0, 0
	for _, head := range heads {
		wanted[strings.Join(slices.Sorted(slices.Values([]string{head, base})), " ")] = head
	}
	for _, x := range exchanges {
		head, ok := wanted[strings.Join(slices.Sorted(slices.Values(x.Wants)), " ")]
		switch {
		case x.Status != 200:
			other++
		case x.Operation == "advertisement" && len(x.Wants) == 0:
			advertisements++
		case x.Operation == "upload-pack" && ok:
			uploads[head]++
		default:
			other++
		}
	}
	once := len(heads) > 0 && base != "" && len(uploads) == len(heads) && advertisements == len(heads) && other == 0
	for _, n := range uploads {
		once = once && n == 1
	}
	return e2eVerdict{once, fmt.Sprintf("each of %d acquisitions made one advertisement and one upload-pack wanting exactly its head and the base, and nothing else reached the fake upstream (%d advertisements, uploads %v, %d other)", len(heads), advertisements, uploads, other)}
}

// e2eProbeVerdicts judges one job's restore probes, issued as: own head
// source.go, own head ownFile, own head foreignFile, foreign head
// foreignFile, foreign head source.go. Exactly those reads must be reported;
// both own reads must return the fake upstream's exact bytes through both the
// store and the model-facing reader, each judged by the hash of what it
// returned, and every other read must be refused by both and return nothing.
func e2eProbeVerdicts(pr int, probes []r.E2EProbe, own, foreign, ownFile, foreignFile string) []e2eVerdict {
	digest := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	reads := [][2]string{{own, "source.go"}, {own, ownFile}, {own, foreignFile}, {foreign, foreignFile}, {foreign, "source.go"}}
	ok := len(probes) == len(reads)
	for i := 0; ok && i < len(reads); i++ {
		ok = probes[i].Revision == reads[i][0] && probes[i].Path == reads[i][1]
	}
	if !ok {
		return []e2eVerdict{{false, fmt.Sprintf("PR %d restore probe reported exactly its %d reads (%d)", pr, len(reads), len(probes))}}
	}
	ownBytes := fmt.Sprintf("pull request %d only\n", pr)
	exact := func(p r.E2EProbe, want string) bool {
		return p.StoreOK && p.ReaderOK && p.StoreHash == digest(want) && p.ReaderHash == digest(want)
	}
	v := []e2eVerdict{{exact(probes[0], e2eSource) && exact(probes[1], ownBytes), fmt.Sprintf("PR %d reads its own head objects with the fake upstream's exact bytes through both the store and the reader", pr)}}
	for _, p := range probes[2:] {
		v = append(v, e2eVerdict{!p.StoreOK && !p.ReaderOK && p.StoreHash == "" && p.ReaderHash == "", fmt.Sprintf("PR %d cannot read foreign object %s:%s from its retained source", pr, p.Revision[:min(12, len(p.Revision))], p.Path)})
	}
	return v
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
	e.check(len(d.Jobs) == 1 && j != nil && j.LeaseToken == "" && !j.Outstanding && j.Checkpoint.ConsumedSessions == 0 && len(j.Checkpoint.Artifacts) == 0, "closed-before-work state holds only PR 1's job, with no lease, no outstanding attempt and no consumed session (%d jobs)", len(d.Jobs))
	e.check(len(d.Writes) == 0, "closed-before-work state holds no write intent (%d)", len(d.Writes))
	if j != nil {
		e.check(j.State == "queued", "closed-before-work job returned to queued (got %s)", j.State)
	}
	e.check(e.github.reviewPosts(1) == 0, "nothing was published before review")
	e.open("reopen")
	e.assertSourceStopped("after reopen recovery", j)
	if !e.wait("PR 1 review confirmed after reopen", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	job := e.assertCompleted("after reopen", 1)[0]
	e.check(e.github.gitExchanges() == exchanges, "reopened service restored retained source without refetching (%d exchanges)", e.github.gitExchanges())
	if job == nil {
		return
	}
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eOpenedRequestVerdicts("after reopen", d, job) })
	e.checkUploads(e.github.commits["1"])
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
	// The source unit is stopped before any provider session starts. Only
	// this process sampled its acquisition, so it judges the order now; the
	// attached restart judges only the final records.
	if c := e.completion(); c.SourceErr != nil {
		e.check(false, "close-provider: source records readable: %v", c.SourceErr)
	} else {
		e.checkAll(e2eSourceOrderVerdicts("close-provider", c.Records, c.Exchanges))
	}
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
	if len(d.Jobs) != 1 || j == nil {
		e.check(false, "PR 1's job is the only job after provider close (%d jobs)", len(d.Jobs))
		return
	}
	e.check(len(d.Writes) == 0, "the interrupted job left no write intent (%d)", len(d.Writes))
	e.checkUploads(e.github.commits["1"])
	e.note("after provider-session close: job state %s, consumed %d, artifacts %d, lease %v, outstanding %v, provider attempts %d", j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), j.LeaseToken != "", j.Outstanding, len(j.ProviderAttempts))
	e.result.Requests = e2eRequests(d)
	e.checkAll(e2eRequestVerdicts("after provider close", e.result.Requests, map[int]e2eRequest{1: e2eAdmittedOpened(1, j.RequestID)}))
	e.check(j.Checkpoint.ConsumedSessions == 1 && len(j.Checkpoint.Artifacts) == 0, "the interrupted session stays charged and produced no artifact")
	units := e2eReadNativeUnits(e.native, e.releases, e.config.Limits)
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
		// Not yet observed on a host: the stopped receipt the job records for
		// the interrupted attempt is the launch's audit evidence.
		if len(units) == 1 {
			_, err := e2eRecordedEvidence(j, units[0])
			e.check(err == nil, "the job durably records the interrupted launch's complete stopped audit evidence: %v", err)
		}
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
	_, err := e2eRetainedLaunches(units, j)
	e.check(err == nil, "the interrupted launch is the job's outstanding attempt and the job records its complete stopped audit evidence: %v", err)
	// ReleaseStopped is the explicit trusted-supervisor path. Probe it on a
	// private copy: it is accepted only with durably recorded stopped
	// provider evidence, and the service itself never calls it.
	err = e2eProbeRelease(e, j)
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
	before, err := e2eAttachedJob(e.reopenState(), e.options.Current, e.github.snapshot(1))
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted job:", err)
	}
	launches, err := e2eRetainedLaunches(e2eReadNativeUnits(e.native, e.releases, e.config.Limits), before)
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted launch and its stopped audit:", err)
	}
	e.nativeBefore = launches
	e.check(true, "the attached root holds the interrupted launch, captured, stopped and audited with exported unreturned auth, bound to the retained attempt's stopped provider evidence")
	codexFenced := !e2eAuthUsable(e.auth, r.Codex)
	e.check(codexFenced, "codex auth is still fenced in the restarted process")
	e.open("restart")
	e.assertSourceStopped("after restart recovery", before)
	e.waitFor("PR 2 comment request discovered", time.Minute, true, func(d r.DurableState) bool {
		pr := d.PRs[e2eScopeKey(2)]
		return pr != nil && len(pr.Requests) == 1
	})
	e.waitFor("any admission while leased (expected none)", 30*time.Second, false, func(d r.DurableState) bool {
		j := e2eJobFor(d, 1)
		return len(d.Jobs) != 1 || j == nil || j.LeaseToken != before.LeaseToken
	})
	d := e.state()
	final := e2eJobFor(d, 1)
	sessions := len(e.models[r.Codex].sessionsCopy()) + len(e.models[r.Claude].sessionsCopy())
	if len(d.Jobs) != 1 || final == nil {
		e.check(false, "PR 1's retained job is the only job after restart (%d jobs)", len(d.Jobs))
		e.wedged = true
		e.close("final")
		return
	}
	e.check(final.LeaseToken == before.LeaseToken && final.Outstanding && final.Checkpoint.ConsumedSessions == before.Checkpoint.ConsumedSessions, "the retained lease fenced every admission after restart, including PR 2")
	e.check(len(d.Writes) == 0, "the fenced restart holds no write intent (%d)", len(d.Writes))
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

// e2eAttachedJob returns the PR 1 job an attached root retains, which must
// be its only job, still hold its durable lease, be bound to PR 1's scope and
// the deterministic fake upstream's snapshot, and have been admitted under
// the current release. A root retained by another revision's close-provider would
// otherwise be judged, and its result recorded, as this revision's restart.
func e2eAttachedJob(d r.DurableState, current string, snapshot r.Snapshot) (*r.DurableJob, error) {
	j := e2eJobFor(d, 1)
	if len(d.Jobs) != 1 || j == nil || j.LeaseToken == "" {
		return nil, fmt.Errorf("%d retained jobs, want PR 1's one leased job", len(d.Jobs))
	}
	if j.Scope != e2eScope(1) || j.Job.Snapshot != snapshot {
		return nil, fmt.Errorf("retained job %s is not bound to PR 1's scope and fake upstream snapshot", j.Job.ID)
	}
	if current == "" || j.Job.Release != current {
		return nil, fmt.Errorf("retained job %s was admitted under release %q, not the current release %q", j.Job.ID, j.Job.Release, current)
	}
	return j, nil
}

// e2eOpenedRequestVerdicts judges the requests a confirmed single-PR case
// (review, close-capture, close-publish) retains: PR 1 holds exactly the
// admitted automatic PR-open request its job j came from, and no PR holds any
// other request. A further request could still be admitted after the review
// is confirmed and produce a duplicate review.
func e2eOpenedRequestVerdicts(label string, d r.DurableState, j *r.DurableJob) []e2eVerdict {
	return e2eRequestVerdicts(label, e2eRequests(d), map[int]e2eRequest{1: e2eAdmittedOpened(1, e2eRequestOf(j))})
}

// e2eIsolationRequestVerdicts judges the requests the isolation case retains:
// PR 1 holds exactly the admitted PR-open request of j1, PR 2 exactly the
// admitted owner comment request of j2, and no PR holds any other request.
func e2eIsolationRequestVerdicts(d r.DurableState, j1, j2 *r.DurableJob) []e2eVerdict {
	return e2eRequestVerdicts("after both reviews", e2eRequests(d), map[int]e2eRequest{1: e2eAdmittedOpened(1, e2eRequestOf(j1)), 2: e2eAdmittedComment(2, e2eRequestOf(j2))})
}

// e2eRequestOf is the request ID job j was admitted from. A missing job, or
// one without a request ID, yields -1, which no retained request matches.
func e2eRequestOf(j *r.DurableJob) int64 {
	if j == nil || j.RequestID <= 0 {
		return -1
	}
	return j.RequestID
}

// checkRequests records the requests the open service retains and judges them.
func (e *e2eEnv) checkRequests(judge func(r.DurableState) []e2eVerdict) {
	e.t.Helper()
	d := e.state()
	e.result.Requests = e2eRequests(d)
	e.checkAll(judge(d))
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

// e2eAdmittedComment is an owner @sfl review request admitted as the request
// of job request id.
func e2eAdmittedComment(pr int, id int64) e2eRequest {
	return e2eRequest{PR: pr, ID: id, ActorID: r.OwnerID, Kind: "comment", ActorRole: "commenter", Admitted: true}
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
// A nil error is not taken as the release: the copy is closed, reopened
// through a fresh handle and must hold the state e2eReleasedVerdict requires
// of job j, before the copy is removed.
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
	released := store.ReleaseStopped(context.Background(), j.Job.ID, j.LeaseToken, "auth_failed")
	if err := errors.Join(released, store.Close()); err != nil {
		return err
	}
	reread, err := r.OpenStore(copied, e2eEnrollment, e.config)
	if err != nil {
		return err
	}
	after, err := reread.Snapshot(context.Background())
	if err = errors.Join(err, reread.Close()); err != nil {
		return err
	}
	return e2eReleasedVerdict(j, after)
}

// e2eReleasedVerdict judges a private copy's state, reread after
// ReleaseStopped(auth_failed) returned nil, against the retained job before:
// the copy still holds exactly that job and no write; the job is auth_failed,
// unleased and holds no outstanding reservation; and its job, charged
// checkpoint, attempt ID, charge days and recorded provider attempts,
// including the outstanding attempt's stopped evidence, are unchanged.
func e2eReleasedVerdict(before *r.DurableJob, after r.DurableState) error {
	if before == nil {
		return errors.New("no retained job to compare")
	}
	a := after.Jobs[before.Job.ID]
	if len(after.Jobs) != 1 || a == nil || len(after.Writes) != 0 {
		return fmt.Errorf("released copy holds %d jobs and %d writes, want only job %s", len(after.Jobs), len(after.Writes), before.Job.ID)
	}
	if a.State != "auth_failed" || a.LeaseToken != "" || !a.LeaseUntil.IsZero() || a.Outstanding {
		return fmt.Errorf("released job %s is %s with lease %v and outstanding %v, want auth_failed, unleased and not outstanding", a.Job.ID, a.State, a.LeaseToken != "", a.Outstanding)
	}
	if r.Digest(a.Job) != r.Digest(before.Job) || r.Digest(a.Checkpoint) != r.Digest(before.Checkpoint) || a.Checkpoint.ConsumedSessions < 1 || a.AttemptID == "" || a.AttemptID != before.AttemptID || !slices.Equal(a.ChargeDays, before.ChargeDays) || r.Digest(a.ProviderAttempts) != r.Digest(before.ProviderAttempts) {
		return fmt.Errorf("released job %s lost or changed its job, charged checkpoint or attempt evidence", a.Job.ID)
	}
	if _, err := e2eRecordedAttempt(a); err != nil {
		return err
	}
	return nil
}

// e2eRecordedAttempt returns job j's one record of its attempt ID, which must
// carry stopped provider evidence.
func e2eRecordedAttempt(j *r.DurableJob) (r.ProviderAttempt, error) {
	found := []r.ProviderAttempt{}
	for _, a := range j.ProviderAttempts {
		if a.ID == j.AttemptID {
			found = append(found, a)
		}
	}
	if len(found) != 1 || found[0].Evidence == nil || !found[0].Evidence.Access.Stopped {
		return r.ProviderAttempt{}, fmt.Errorf("job %s holds %d records of attempt %q, want one with stopped evidence", j.Job.ID, len(found), j.AttemptID)
	}
	return found[0], nil
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
	closed := e.reopenState()
	e.checkAll(e2eDurableSetVerdicts("after publish close", closed, 1))
	writes := e2eWritesFor(closed, 1, "review")
	e.check(len(writes) == 1 && (writes[0].State == "unknown" || writes[0].State == "dispatched"), "the closed POST left exactly one unresolved intent (%d writes)", len(writes))
	e.check(e.github.reviewPosts(1) == 1, "exactly one POST reached GitHub before close")
	e.open("reopen")
	if !e.wait("PR 1 write reconciled", 3*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("reconciled")
	job := e.assertCompleted("after reconciliation", 1)[0]
	e.check(e.github.reviewPosts(1) == 1, "reconciliation after reopen never POSTed again")
	e.checkAll([]e2eVerdict{e2eReconciledVerdict(closed, e.state())})
	if job == nil {
		return
	}
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eOpenedRequestVerdicts("after reconciliation", d, job) })
	e.checkUploads(e.github.commits["1"])
	e.result.Jobs = append(e.result.Jobs, e2eJobSummary(job))
	e.collectWrites()
}

// e2eReconciledVerdict requires reconciliation to have confirmed the very
// intent the closed POST left unresolved: the reopened state's sole confirmed
// PR 1 intent has the closed state's sole unresolved intent's ID, marker,
// document hash, job, checkpoint and snapshot. A new intent confirmed in its
// place, or a changed document, is not the reconciled write.
func e2eReconciledVerdict(closed, reopened r.DurableState) e2eVerdict {
	before, after := e2eWritesFor(closed, 1, "review"), e2eSoleConfirmed(e2eWritesFor(reopened, 1, "review"))
	ok := len(before) == 1 && (before[0].State == "unknown" || before[0].State == "dispatched") && after != nil
	if ok {
		b := before[0]
		ok = after.ID == b.ID && after.Marker == b.Marker && after.BodyHash == b.BodyHash && after.JobHash == b.JobHash && after.CheckpointHash == b.CheckpointHash && after.Snapshot == b.Snapshot && after.RequestID == b.RequestID
	}
	return e2eVerdict{ok, "reconciliation confirmed exactly the intent the closed POST left unresolved, with its document unchanged"}
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
		e.result.Writes = append(e.result.Writes, e2eWrite{ID: w.ID, Kind: w.Kind, State: w.State, PR: w.Scope.PR, ReceiptID: w.ReceiptID, Inline: len(w.Document.Inline), Head: w.Snapshot.Head, Marker: w.Marker, BodyHash: w.BodyHash})
	}
}

// e2eScope is PR pr's durable scope under the synthetic enrollment.
func e2eScope(pr int) r.Scope {
	return r.Scope{RepositoryID: r.RepositoryID, InstallationID: e2eEnrollment.InstallationID, PR: pr}
}

func e2eScopeKey(pr int) string {
	return fmt.Sprintf("%d/%d/%d", r.RepositoryID, e2eEnrollment.InstallationID, pr)
}

func e2eJobSummary(j *r.DurableJob) e2eJob {
	s := e2eJob{ID: j.Job.ID, State: j.State, Release: j.Job.Release, PR: j.Scope.PR, Head: j.Job.Snapshot.Head, RequestID: j.RequestID, Consumed: j.Checkpoint.ConsumedSessions, Artifacts: len(j.Checkpoint.Artifacts), Lease: j.LeaseToken != "", Outstanding: j.Outstanding, SourceAttempts: len(j.SourceAttempts), ProviderAttempts: len(j.ProviderAttempts)}
	for _, a := range j.Checkpoint.Artifacts {
		s.ArtifactSessions = append(s.ArtifactSessions, fmt.Sprintf("%s R%d %s", a.Report.Provider, a.Report.Round, a.InputHash))
		if a.SourceAccess != nil {
			s.ArtifactSourceCalls = append(s.ArtifactSourceCalls, fmt.Sprintf("%s R%d tools %d reader %d", a.Report.Provider, a.Report.Round, a.SourceAccess.ToolCalls, a.SourceAccess.ReaderCalls))
		}
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
	job := func(pr int) *r.DurableJob {
		return &r.DurableJob{Job: r.Job{Snapshot: snapshot(pr)}, Scope: r.Scope{PR: pr}}
	}
	bind := e2eSourceBinding{r.Digest("release package"), r.Digest("source VM manifest"), r.Digest("source stub")}
	// Each attempt's source request hash derives from it, so distinct
	// attempts never share one; release and runtime hashes are the release's.
	hash := func(kind, attempt string) string { return r.Digest(kind + attempt) }
	rec := func(attempt string, pr int) r.E2ESourceRecord {
		return r.E2ESourceRecord{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", RequestHash: hash("unit", attempt), SourceRequestHash: hash("source", attempt), StubHash: bind.Stub, ReleaseHash: bind.Release, RuntimeHash: bind.Runtime, Snapshot: snapshot(pr), Stopped: true, Revoked: true, GrantHash: "grant"}
	}
	export := func(attempt string, pr int) r.E2EExportRecord {
		return r.E2EExportRecord{Attempt: attempt, State: "staged", RequestHash: hash("source", attempt), ReleaseHash: bind.Release, RuntimeHash: bind.Runtime, Snapshot: snapshot(pr)}
	}
	exportWith := func(edit func(*r.E2EExportRecord)) []r.E2EExportRecord {
		x := export(a, 1)
		edit(&x)
		return []r.E2EExportRecord{x}
	}
	// pairWith edits a record and its export alike, so they still agree
	// with each other and only their binding to the job or release differs.
	pairWith := func(edit func(*r.Snapshot, *string, *string)) ([]r.E2ESourceRecord, []r.E2EExportRecord) {
		x, y := rec(a, 1), export(a, 1)
		edit(&x.Snapshot, &x.ReleaseHash, &x.RuntimeHash)
		edit(&y.Snapshot, &y.ReleaseHash, &y.RuntimeHash)
		return []r.E2ESourceRecord{x}, []r.E2EExportRecord{y}
	}
	type input struct {
		records []r.E2ESourceRecord
		exports []r.E2EExportRecord
		jobs    []*r.DurableJob
	}
	pair := func(records []r.E2ESourceRecord, exports []r.E2EExportRecord) input {
		return input{records, exports, []*r.DurableJob{job(1)}}
	}
	one := []*r.DurableJob{job(1)}
	for name, in := range map[string]input{
		"one":            {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, one},
		"two, unordered": {[]r.E2ESourceRecord{rec(a, 1), rec(b, 2)}, []r.E2EExportRecord{export(b, 2), export(a, 1)}, []*r.DurableJob{job(2), job(1)}},
	} {
		if err := e2eSourceEvidence(in.records, in.exports, in.jobs, bind); err != nil {
			t.Error(name, "refused:", err)
		}
	}
	for name, in := range map[string]input{
		"journal and archive empty":        {nil, nil, one},
		"nothing expected":                 {nil, nil, nil},
		"records without exports":          {[]r.E2ESourceRecord{rec(a, 1)}, nil, one},
		"exports without records":          {nil, []r.E2EExportRecord{export(a, 1)}, one},
		"fewer than expected":              {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, []*r.DurableJob{job(1), job(2)}},
		"more than expected":               {[]r.E2ESourceRecord{rec(a, 1), rec(b, 2)}, []r.E2EExportRecord{export(a, 1), export(b, 2)}, one},
		"export of another attempt":        {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(b, 1)}, one},
		"export of another PR":             {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 2)}, one},
		"duplicate attempt":                {[]r.E2ESourceRecord{rec(a, 1), rec(a, 1)}, []r.E2EExportRecord{export(a, 1), export(a, 1)}, []*r.DurableJob{job(1), job(2)}},
		"extra export":                     {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1), export(b, 2)}, one},
		"export of another source request": pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.RequestHash = hash("source", b) })),
		"export bound to the unit request": pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.RequestHash = hash("unit", a) })),
		"export without a request hash":    pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.RequestHash = "" })),
		"export of another release":        pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.ReleaseHash = hash("release", b) })),
		"export of another runtime":        pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.RuntimeHash = hash("runtime", b) })),
		"export of another base":           pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.Snapshot.Base = strings.Repeat("e", 40) })),
		// Finding 4: a record and export that agree with each other but not
		// with the durable job or the current release.
		"pair of another PR":             pair(pairWith(func(s *r.Snapshot, _, _ *string) { *s = snapshot(2) })),
		"pair of another head":           pair(pairWith(func(s *r.Snapshot, _, _ *string) { s.Head = strings.Repeat("9", 40) })),
		"pair of another release":        pair(pairWith(func(_ *r.Snapshot, release, _ *string) { *release = r.Digest("earlier release package") })),
		"pair of another runtime":        pair(pairWith(func(_ *r.Snapshot, _, runtime *string) { *runtime = r.Digest("earlier source VM manifest") })),
		"records swapped between jobs":   {[]r.E2ESourceRecord{rec(a, 1), rec(b, 1)}, []r.E2EExportRecord{export(a, 1), export(b, 1)}, []*r.DurableJob{job(1), job(2)}},
		"unit not named for its attempt": {[]r.E2ESourceRecord{func() r.E2ESourceRecord { x := rec(a, 1); x.Unit = "sfl-alpha-" + b + ".service"; return x }()}, []r.E2EExportRecord{export(a, 1)}, one},
		"no job to bind":                 {[]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, []*r.DurableJob{nil}},
		// Self-audit (f): the owner recorded the release's source stub.
		"unit of another source stub": {[]r.E2ESourceRecord{func() r.E2ESourceRecord { x := rec(a, 1); x.StubHash = hash("stub", b); return x }()}, []r.E2EExportRecord{export(a, 1)}, one},
		"unit without a stub hash":    {[]r.E2ESourceRecord{func() r.E2ESourceRecord { x := rec(a, 1); x.StubHash = ""; return x }()}, []r.E2EExportRecord{export(a, 1)}, one},
	} {
		if e2eSourceEvidence(in.records, in.exports, in.jobs, bind) == nil {
			t.Error(name, "accepted")
		}
	}
	if e2eSourceEvidence([]r.E2ESourceRecord{rec(a, 1)}, []r.E2EExportRecord{export(a, 1)}, one, e2eSourceBinding{}) == nil {
		t.Error("records accepted without a release binding to compare")
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
		res.NativeUnits = append(res.NativeUnits, e2eNativeUnit{Launch: launch, Captured: true, Stopped: true, AuditPresent: true})
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
		"native launch without its audit": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.NativeUnits[2].AuditPresent = false }), review, nil, false},
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

// e2eProviderReleaseFixture is a well-formed native provider release with
// synthetic pins and the given egress policy.
func e2eProviderReleaseFixture(policy r.EgressPolicy) r.GitReviewerProviderRelease {
	asset := func(path string) r.VMBootAsset { return r.VMBootAsset{Path: path, SHA256: r.Digest(path), Bytes: 4096} }
	return r.GitReviewerProviderRelease{Stub: asset("/fixture/nativevmstub"), QEMU: asset("/fixture/qemu"), Kernel: asset("/fixture/vmlinuz"), Initrd: asset("/fixture/native.cpio.gz"), Firmware: asset("/fixture/bios.bin"), Runtime: r.NativeVMRuntimeSpec{Artifact: asset("/fixture/native-qemu-runtime.bin"), NamespaceHelper: asset("/fixture/namespace-helper"), BootstrapLibraries: []r.VMBootAsset{asset("/lib/x86_64-linux-gnu/libc.so.6"), asset("/lib64/ld-linux-x86-64.so.2")}}, Policy: policy}
}

func TestE2ENativeUnitFrom(t *testing.T) {
	session, launch := strings.Repeat("5", 64), strings.Repeat("6", 32)
	binding := func(session, launch string) r.AuthBinding {
		return r.AuthBinding{SessionHash: session, LaunchID: launch, ContainmentID: r.Digest(struct{ Session, Launch string }{session, launch})}
	}
	config := r.DefaultConfig()
	policyOf := func(provider r.Provider) r.EgressPolicy {
		probe, err := r.NewSession("e2e-policy-probe", r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}, config.Hash(), provider, 1, "probe", nil, nil, config.Limits)
		if err != nil {
			t.Fatal(err)
		}
		return f.Policy(probe)
	}
	policy, claudePolicy := policyOf(r.Codex), policyOf(r.Claude)
	otherPolicy := policy
	otherPolicy.Headers = append(slices.Clone(policy.Headers), "X-Other")
	release := e2eProviderReleaseFixture(policy)
	// Another native release differs only in its pinned kernel.
	otherRelease := release
	otherRelease.Kernel.SHA256 = r.Digest("another kernel")
	manifest := func(rel r.GitReviewerProviderRelease, b r.AuthBinding) string {
		hash, err := r.E2ENativeManifestHash(rel, r.Codex, b)
		if err != nil {
			t.Fatal("manifest of the fixture release", err)
		}
		return hash
	}
	pinned := manifest(release, binding(session, launch))
	// A completed session's receipt and an interrupted one's: the held
	// inference was forwarded but never answered.
	access := func(complete bool) r.ProviderAccess {
		if complete {
			return r.ProviderAccess{Version: 1, Requests: 9, InferenceRequests: 3, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 8, Responses: 8, RequestBytes: 4096, ResponseBytes: 8192, Stopped: true}
		}
		return r.ProviderAccess{Version: 1, Requests: 7, InferenceRequests: 1, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 6, Responses: 5, RequestBytes: 2048, ResponseBytes: 4096, Failed: true, Stopped: true}
	}
	evidence := func(session string, policy r.EgressPolicy, a r.ProviderAccess) *r.ProviderEvidence {
		a.SessionHash, a.PolicyHash = session, r.Digest(policy)
		return &r.ProviderEvidence{Policy: policy, Access: a}
	}
	type receipts struct{ unit, audit map[string]any }
	valid := func(complete bool) receipts {
		return receipts{
			map[string]any{"Version": 1, "Unit": "sfl-alpha-" + launch + ".service", "Binding": binding(session, launch), "PayloadKind": "native-vm", "StubHash": release.Stub.SHA256, "ManifestHash": pinned, "Captured": true, "Stopped": true},
			map[string]any{"Version": 1, "Binding": binding(session, launch), "ManifestHash": pinned, "Provider": evidence(session, policy, access(complete)), "Complete": complete, "AuthExported": true, "AuthReturned": complete},
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
		return e2eNativeUnitFrom(r.Codex, release, config.Limits, launch, unit, audit)
	}
	if u := project(valid(true)); !u.AuditPresent || !u.Captured || !u.Stopped || !u.Complete || !u.AuthReturned || u.SessionHash != session || u.ManifestHash != pinned || u.Access.InferenceRequests != 3 || u.Access.SessionHash != session || u.EvidenceHash != r.Digest(evidence(session, policy, access(true))) {
		t.Fatal("bound completed receipts refused", u)
	}
	if u := project(valid(false)); !u.AuditPresent || u.Complete || !u.AuthExported || u.AuthReturned || !u.Access.Failed {
		t.Fatal("bound interrupted receipts refused", u)
	}
	if u := project(receipts{valid(true).unit, nil}); u.AuditPresent || !u.Captured || u.SessionHash != session {
		t.Fatal("unit without an audit misreported", u)
	}
	change := func(complete bool, edit func(receipts)) receipts { in := valid(complete); edit(in); return in }
	// receipt edits only the provider access counters of a bound audit.
	receipt := func(complete bool, edit func(*r.ProviderAccess)) receipts {
		a := access(complete)
		edit(&a)
		return change(complete, func(in receipts) { in.audit["Provider"] = evidence(session, policy, a) })
	}
	l := policy.Limits
	for name, in := range map[string]receipts{
		"audit of another session":                  change(true, func(in receipts) { in.audit["Binding"] = binding(strings.Repeat("7", 64), launch) }),
		"audit of another launch":                   change(true, func(in receipts) { in.audit["Binding"] = binding(session, strings.Repeat("8", 32)) }),
		"audit without a binding":                   change(true, func(in receipts) { delete(in.audit, "Binding") }),
		"audit of another version":                  change(true, func(in receipts) { in.audit["Version"] = 2 }),
		"audit without a version":                   change(true, func(in receipts) { delete(in.audit, "Version") }),
		"completed audit without evidence":          change(true, func(in receipts) { delete(in.audit, "Provider") }),
		"exported audit without evidence":           change(false, func(in receipts) { delete(in.audit, "Provider") }),
		"evidence of another session":               change(true, func(in receipts) { in.audit["Provider"] = evidence(strings.Repeat("7", 64), policy, access(true)) }),
		"evidence under another policy":             change(true, func(in receipts) { in.audit["Provider"] = evidence(session, otherPolicy, access(true)) }),
		"evidence of another provider":              change(true, func(in receipts) { in.audit["Provider"] = evidence(session, claudePolicy, access(true)) }),
		"completed audit with unreturned auth":      change(true, func(in receipts) { in.audit["AuthReturned"] = false }),
		"completed audit without exported auth":     change(true, func(in receipts) { in.audit["AuthExported"] = false }),
		"auth returned but never exported":          change(false, func(in receipts) { in.audit["AuthExported"], in.audit["AuthReturned"] = false, true }),
		"evidence not stopped":                      receipt(false, func(a *r.ProviderAccess) { a.Stopped = false }),
		"completed audit with a failed receipt":     receipt(true, func(a *r.ProviderAccess) { a.Failed = true }),
		"completed audit with an exhausted receipt": receipt(true, func(a *r.ProviderAccess) { a.Failed, a.Exhausted = true, true }),
		"completed audit without inference": receipt(true, func(a *r.ProviderAccess) {
			a.InferenceRequests, a.Requests, a.Forwarded, a.Responses = 0, 6, 5, 5
		}),
		"completed audit with an unanswered request": receipt(true, func(a *r.ProviderAccess) { a.Responses-- }),
		"completed audit with negative bytes":        receipt(true, func(a *r.ProviderAccess) { a.RequestBytes = -1 }),
		"completed audit over the inference budget": receipt(true, func(a *r.ProviderAccess) {
			extra := l.InferenceRequests + 1 - a.InferenceRequests
			a.InferenceRequests, a.Requests, a.Forwarded, a.Responses = a.InferenceRequests+extra, a.Requests+extra, a.Forwarded+extra, a.Responses+extra
		}),
		"completed audit with an impossible request total": receipt(true, func(a *r.ProviderAccess) { a.Requests++ }),
		"completed audit with a route denial":              receipt(true, func(a *r.ProviderAccess) { a.RouteDenials = 1 }),
		"interrupted audit with negative responses":        receipt(false, func(a *r.ProviderAccess) { a.Responses = -1 }),
		"interrupted audit over the request budget": receipt(false, func(a *r.ProviderAccess) {
			extra := l.InferenceRequests + 1 - a.InferenceRequests
			a.InferenceRequests, a.Requests, a.Forwarded = a.InferenceRequests+extra, a.Requests+extra, a.Forwarded+extra
		}),
		"interrupted audit exhausted without failure":        receipt(false, func(a *r.ProviderAccess) { a.Failed, a.Exhausted = false, true }),
		"interrupted audit answering more than it forwarded": receipt(false, func(a *r.ProviderAccess) { a.Responses = a.Forwarded + 1 }),
		"interrupted audit with an impossible request total": receipt(false, func(a *r.ProviderAccess) { a.Requests++ }),
		"interrupted audit with an unfailed route denial":    receipt(false, func(a *r.ProviderAccess) { a.Failed, a.RouteDenials = false, 1 }),
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
		// Round 8 finding 1: the journal and audit must be of the pinned
		// native payload and this launch's release manifest.
		"unit journal of the fixture payload":     change(true, func(in receipts) { in.unit["PayloadKind"] = "vm-fixture" }),
		"unit journal without a payload kind":     change(true, func(in receipts) { delete(in.unit, "PayloadKind") }),
		"unit journal of another stub":            change(true, func(in receipts) { in.unit["StubHash"] = r.Digest("another stub") }),
		"unit journal without a stub hash":        change(true, func(in receipts) { delete(in.unit, "StubHash") }),
		"unit journal without a manifest hash":    change(true, func(in receipts) { delete(in.unit, "ManifestHash") }),
		"audit without a manifest hash":           change(true, func(in receipts) { delete(in.audit, "ManifestHash") }),
		"audit of another manifest than its unit": change(true, func(in receipts) { in.audit["ManifestHash"] = manifest(otherRelease, binding(session, launch)) }),
		"unit journal of another native release": change(true, func(in receipts) {
			in.unit["ManifestHash"], in.audit["ManifestHash"] = manifest(otherRelease, binding(session, launch)), manifest(otherRelease, binding(session, launch))
		}),
		"manifest of another launch's binding": change(true, func(in receipts) {
			other := manifest(release, binding(session, strings.Repeat("8", 32)))
			in.unit["ManifestHash"], in.audit["ManifestHash"] = other, other
		}),
	} {
		if u := project(in); u.AuditPresent {
			t.Error(name, "accepted as this launch's audit", u)
		}
	}
}

// TestE2EReadNativeUnits enumerates every entry of each native root except
// the supervisor's executables directory: a launch directory without a
// readable journal, or any other entry, is a failed launch, never skipped.
func TestE2EReadNativeUnits(t *testing.T) {
	root := t.TempDir()
	corrupt, missing := strings.Repeat("6", 32), strings.Repeat("7", 32)
	for _, directory := range []string{"executables", corrupt, missing} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if os.WriteFile(filepath.Join(root, corrupt, "unit.json"), []byte("{"), 0600) != nil || os.WriteFile(filepath.Join(root, "stray"), nil, 0600) != nil {
		t.Fatal("fixture")
	}
	releases := map[r.Provider]r.GitReviewerProviderRelease{r.Codex: e2eProviderReleaseFixture(r.EgressPolicy{Provider: r.Codex}), r.Claude: e2eProviderReleaseFixture(r.EgressPolicy{Provider: r.Claude})}
	units := e2eReadNativeUnits(map[r.Provider]string{r.Codex: root, r.Claude: filepath.Join(root, "absent")}, releases, r.DefaultConfig().Limits)
	launches := map[string]bool{}
	for _, u := range units {
		launches[u.Launch] = true
		if u.AuditPresent || u.Captured || u.Stopped || u.SessionHash != "" {
			t.Error("failed launch entry projected as a launch", u)
		}
	}
	if len(units) != 4 || !launches[corrupt] || !launches[missing] || !launches["stray"] || launches["executables"] {
		t.Error("native roots not enumerated whole", units)
	}
	// The failed entries fail every launch verdict that reads them.
	if v := e2eNativeSessionVerdict(units, []*r.DurableJob{e2eJobFor(e2eCompletedFixture().State, 1)}); v.OK {
		t.Error("failed launch entries accepted beside a completed review")
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

// e2eCompletedFixture is a valid completed PR 1 review as the open service
// reports it: the sole job with the four adversarial artifacts, each bound to
// the job and carrying provider evidence the job records; one completed
// native launch with that evidence and one unheld backend check per artifact
// session; the sole confirmed intent bound to the job; the one remote review
// projected from that intent's document; and one stopped source attempt of
// the job's snapshot under the current release.
func e2eCompletedFixture() e2eCompletion {
	head, base, attempt, id := strings.Repeat("1", 40), strings.Repeat("e", 40), strings.Repeat("a", 32), strings.Repeat("c", 32)
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: head, Base: base, MergeBase: base}
	config := r.DefaultConfig()
	job := &r.DurableJob{Job: r.Job{ID: "request-1-1", RequesterID: r.OwnerID, Generation: 1, Snapshot: snapshot, Config: config, ConfigVersion: 1, Release: "release"}, Scope: e2eScope(1), RequestID: 1, State: "dual_complete", Checkpoint: r.Checkpoint{ConsumedSessions: 4}}
	job.Checkpoint.JobHash = r.Digest(job.Job)
	c := e2eCompletion{Heads: map[int]string{1: head}, Base: base, Release: "release", Source: e2eSourceBinding{r.Digest("release package"), r.Digest("source VM manifest"), r.Digest("source stub")}, Posts: map[int]int{1: 1}}
	for i, run := range []struct {
		provider r.Provider
		round    int
	}{{r.Codex, 1}, {r.Claude, 1}, {r.Codex, 2}, {r.Claude, 2}} {
		hash := r.Digest(fmt.Sprintf("session %d", i))
		// The canned backends' exchange contract: three inferences, and for
		// Codex its five captured metadata requests.
		counts := f.Counts{Inference: 3}
		if run.provider == r.Codex {
			counts.Metadata, counts.MetadataCaptured = 5, true
		}
		evidence := &r.ProviderEvidence{Policy: r.EgressPolicy{Provider: run.provider}, Access: r.ProviderAccess{Version: 1, SessionHash: hash, InferenceRequests: 3, MetadataRequests: counts.Metadata, Stopped: true}}
		recorded := *evidence
		access := &r.SourceAccess{Version: 1, SessionHash: hash, ToolCalls: 2, ReaderCalls: 3, Bytes: 64, Reads: []r.ReadRecord{{Revision: head, Path: "source.go", Hash: r.Digest([]byte(e2eSource)), Bytes: len(e2eSource)}, {Revision: head, Path: "pr1-only.go", Hash: r.Digest([]byte("pull request 1 only\n")), Bytes: 20}}}
		read := *access
		job.Checkpoint.Artifacts = append(job.Checkpoint.Artifacts, r.Artifact{JobHash: r.Digest(job.Job), InputHash: hash, Report: r.Report{JobID: job.Job.ID, Provider: run.provider, Round: run.round, SnapshotHash: r.Digest(snapshot), ConfigHash: config.Hash()}, SourceAccess: access, ProviderEvidence: evidence})
		job.ProviderAttempts = append(job.ProviderAttempts, r.ProviderAttempt{ID: fmt.Sprintf("attempt-%d", i), Evidence: &recorded})
		job.SourceAttempts = append(job.SourceAttempts, r.SourceAttempt{ID: fmt.Sprintf("attempt-%d", i), Access: &read})
		c.Units = append(c.Units, e2eNativeUnit{Provider: string(run.provider), Launch: strconv.Itoa(i), SessionHash: hash, Captured: true, Stopped: true, AuditPresent: true, Complete: true, AuthExported: true, AuthReturned: true, Access: evidence.Access, EvidenceHash: r.Digest(evidence)})
		c.Sessions = append(c.Sessions, e2eModelSession{JobID: job.Job.ID, Provider: run.provider, Round: run.round, PR: 1, Head: head, SessionHash: hash, Counts: counts})
	}
	marker := "<!-- sfl-alpha:" + id + " -->"
	document := r.Document{Summary: "review\n\n" + marker, Inline: []r.InlineComment{{Path: "source.go", Line: 1, Body: "finding"}}}
	write := &r.WriteIntent{ID: id, Kind: "review", Scope: job.Scope, RequestID: job.RequestID, Generation: job.Job.Generation, JobHash: r.Digest(job.Job), CheckpointHash: r.Digest(job.Checkpoint), Snapshot: snapshot, Document: document, BodyHash: r.Digest(document), Marker: marker, State: "confirmed", ReceiptID: 5001}
	remoteMarker, remoteHash := r.E2ERemoteReview(document.Summary, document.Inline)
	c.Reviews = map[int][]map[string]any{1: {{"id": int64(5001), "state": "COMMENTED", "commit_id": head, "marker": remoteMarker, "document_hash": remoteHash, "inline": []string{"source.go:1"}}}}
	c.State = r.DurableState{Jobs: map[string]*r.DurableJob{job.Job.ID: job}, Writes: map[string]*r.WriteIntent{write.ID: write}}
	c.Records = []r.E2ESourceRecord{{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", InvocationID: strings.Repeat("9", 32), RequestHash: strings.Repeat("6", 64), SourceRequestHash: strings.Repeat("7", 64), StubHash: c.Source.Stub, ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot, StartIntent: true, StartSettled: true, Captured: true, Stopped: true, Revoked: true, GrantHash: "grant"}}
	c.Exchanges = e2eAcquisition(c.Records[0])
	c.Exports = []r.E2EExportRecord{{Attempt: attempt, State: "staged", RequestHash: strings.Repeat("7", 64), ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot}}
	return c
}

// e2eAcquisition is the fake upstream's exchanges for one acquisition of
// final record rec, each sampling the journal while rec's unit was captured
// and granted but not yet revoked or stopped.
func e2eAcquisition(rec r.E2ESourceRecord) []e2eGitExchange {
	live := rec
	live.Revoked, live.Stopped = false, false
	return []e2eGitExchange{
		{Operation: "advertisement", Status: 200, Journal: []r.E2ESourceRecord{live}},
		{Operation: "upload-pack", Wants: []string{rec.Snapshot.Head, rec.Snapshot.Base}, Status: 200, Journal: []r.E2ESourceRecord{live}},
	}
}

// e2eReopenFixture is a valid completion after close-provider's reopen: the
// completed review plus the interrupted launch of the first session, whose
// stopped receipt the job also records, and its held backend check.
func e2eReopenFixture() (e2eCompletion, string) {
	c := e2eCompletedFixture()
	job := e2eJobFor(c.State, 1)
	first := job.Checkpoint.Artifacts[0].InputHash
	evidence := &r.ProviderEvidence{Policy: r.EgressPolicy{Provider: r.Codex}, Access: r.ProviderAccess{Version: 1, SessionHash: first, InferenceRequests: 1, Failed: true, Stopped: true}}
	job.ProviderAttempts = append([]r.ProviderAttempt{{ID: "interrupted", Evidence: evidence}}, job.ProviderAttempts...)
	c.Units = append(c.Units, e2eNativeUnit{Provider: "codex", Launch: "interrupted", SessionHash: first, Captured: true, Stopped: true, AuditPresent: true, AuthExported: true, AuthReturned: true, Access: evidence.Access, EvidenceHash: r.Digest(evidence)})
	c.Sessions = append([]e2eModelSession{{JobID: job.Job.ID, Provider: r.Codex, Round: 1, PR: 1, Head: job.Job.Snapshot.Head, SessionHash: first, HeldInference: true}}, c.Sessions...)
	return c, first
}

// e2eFailures returns the messages of the verdicts that fail.
func e2eFailures(verdicts []e2eVerdict) []string {
	out := []string{}
	for _, v := range verdicts {
		if !v.OK {
			out = append(out, v.Message)
		}
	}
	return out
}

func TestE2EProviderReopenVerdicts(t *testing.T) {
	failures := func(c e2eCompletion, interrupted string) []string {
		verdicts, _ := e2eProviderReopenVerdicts(c, interrupted)
		return e2eFailures(verdicts)
	}
	if f := failures(e2eReopenFixture()); len(f) != 0 {
		t.Fatal("completed reopen refused:", f)
	}
	for name, edit := range map[string]func(*e2eCompletion){
		"duplicate unknown intent": func(c *e2eCompletion) {
			c.State.Writes["w2"] = &r.WriteIntent{ID: "w2", Kind: "review", Scope: e2eScope(1), State: "unknown"}
		},
		"second review POST":             func(c *e2eCompletion) { c.Posts[1] = 2 },
		"remote review without inline":   func(c *e2eCompletion) { c.Reviews[1][0]["inline"] = []string{} },
		"remote review on another head":  func(c *e2eCompletion) { c.Reviews[1][0]["commit_id"] = strings.Repeat("9", 40) },
		"completed launch kept its auth": func(c *e2eCompletion) { c.Units[1].AuthReturned = false },
		"completed launch audit unbound": func(c *e2eCompletion) { c.Units[2].AuditPresent = false },
		"second interrupted launch":      func(c *e2eCompletion) { c.Units = append(c.Units, c.Units[len(c.Units)-1]) },
		"source attempt not stopped":     func(c *e2eCompletion) { c.Records[0].Stopped = false },
		"source records unreadable":      func(c *e2eCompletion) { c.SourceErr = r.ErrLiveBlocked },
		"interrupted session recharged":  func(c *e2eCompletion) { e2eJobFor(c.State, 1).Checkpoint.ConsumedSessions = 5 },
		"interrupted launch completed": func(c *e2eCompletion) {
			// A completed launch with a foreign receipt must not pass as the
			// interrupted one.
			c.Units[len(c.Units)-1].Complete = true
		},
		"interrupted launch without its audit": func(c *e2eCompletion) { c.Units[len(c.Units)-1].AuditPresent = false },
	} {
		c, interrupted := e2eReopenFixture()
		edit(&c)
		if len(failures(c, interrupted)) == 0 {
			t.Error(name, "accepted")
		}
	}
}

// TestE2ECompletedVerdicts binds every piece of a completed review's evidence
// to its owner: job to PR, scope, snapshot and release; artifacts to the job
// and the exact adversarial round set; launches to their artifact's provider
// and evidence; backend checks to their artifact's provider and round; the
// intent to the job; and the remote review to the intent's exact document.
func TestE2ECompletedVerdicts(t *testing.T) {
	failures := func(c e2eCompletion) []string {
		verdicts, _ := e2eCompletedVerdicts("completed", c, []int{1})
		return e2eFailures(verdicts)
	}
	if f := failures(e2eCompletedFixture()); len(f) != 0 {
		t.Fatal("completed review refused:", f)
	}
	job := func(c *e2eCompletion) *r.DurableJob { return c.State.Jobs["request-1-1"] }
	artifact := func(c *e2eCompletion, i int) *r.Artifact { return &job(c).Checkpoint.Artifacts[i] }
	write := func(c *e2eCompletion) *r.WriteIntent { return c.State.Writes[strings.Repeat("c", 32)] }
	remote := func(c *e2eCompletion) map[string]any { return c.Reviews[1][0] }
	// rehash re-derives an edited intent's own document hash, so only its
	// binding to the remote review or the job can refuse it.
	rehash := func(c *e2eCompletion) { write(c).BodyHash = r.Digest(write(c).Document) }
	otherSnapshot := func(c *e2eCompletion) r.Snapshot {
		s := job(c).Job.Snapshot
		s.Head = strings.Repeat("9", 40)
		return s
	}
	// refusal names the verdict each edit must fail, so an edit refused only
	// by an unrelated check (such as the checkpoint hash every artifact edit
	// also changes) does not prove the check it targets.
	refusal := map[string]string{
		"completed launch never exported its auth":     "job-bound sessions",
		"launch of another provider":                   "job-bound sessions",
		"launch with another receipt":                  "job-bound sessions",
		"artifact with another receipt":                "job-bound sessions",
		"artifact without provider evidence":           "job-bound sessions",
		"artifact evidence the job never recorded":     "job-bound sessions",
		"artifact evidence recorded twice":             "job-bound sessions",
		"artifact without source access":               "job-bound sessions",
		"source access the job never recorded":         "job-bound sessions",
		"source access recorded under another attempt": "job-bound sessions",
		"artifact with other source access":            "job-bound sessions",
		"source access of another session":             "job-bound sessions",
		"two artifacts of one session":                 "exactly Codex R1",
		"source record and export of another head":     "source records with matching",
		"source record and export of another release":  "source records with matching",
		"source record and export of another runtime":  "source records with matching",
		"source unit not the attempt's":                "source records with matching",
		"source stopped without capture":               "final record holds",
		"source stopped without settled start":         "final record holds",
		"source revoked before its upload-pack":        "when the fake upstream served",
		"source captured only after its upload-pack":   "when the fake upstream served",
		"source record of another stub":                "source records with matching",
		"duplicated round, omitted round":              "exactly Codex R1",
		"artifact of another job":                      "exactly Codex R1",
		"report of another job ID":                     "exactly Codex R1",
		"report of another snapshot":                   "exactly Codex R1",
		"report of another config":                     "exactly Codex R1",
		"checkpoint of another job":                    "checkpoint of its own job",
		"second job for PR 1":                          "exactly one job for each",
		"job for another PR":                           "exactly one job for each",
		"write for another PR":                         "exactly one review intent for each",
		"acknowledgement write":                        "exactly one review intent for each",
		"job of another scope":                         "binds its own scope",
		"job of another base":                          "binds its own scope",
		"job of another release":                       "binds its own scope",
		"remote review with another marker":            "remote review is the confirmed intent's",
		"remote review with another document":          "remote review is the confirmed intent's",
		"remote review with another inline body":       "remote review is the confirmed intent's",
		"remote review with another ID":                "remote review is the confirmed intent's",
		"intent with a marker of another ID":           "marker names its own ID",
		"intent body hash not its document":            "marker names its own ID",
		"intent of another job":                        "intent binds its job's",
		"intent of another checkpoint":                 "intent binds its job's",
		"intent of another request":                    "intent binds its job's",
		"intent of another generation":                 "intent binds its job's",
		"intent of another base":                       "intent binds its job's",
		"intent with a second inline finding":          "exactly the source-backed inline finding",
		"artifact session never checked":               "checked once by its own provider's backend",
		"artifact session checked twice":               "checked once by its own provider's backend",
		"check of another round":                       "checked once by its own provider's backend",
		"check of another provider":                    "checked once by its own provider's backend",
		"check of another job":                         "checked once by its own provider's backend",
		"unheld check without an artifact":             "checked once by its own provider's backend",
		"artifact session made two inferences":         "fixture's exchanges",
		"artifact session made four inferences":        "fixture's exchanges",
		"artifact session with a backend failure":      "fixture's exchanges",
		"artifact session with telemetry":              "fixture's exchanges",
		"codex session without its metadata":           "fixture's exchanges",
		"codex session reached negotiation":            "fixture's exchanges",
		"receipt counted other metadata than served":   "fixture's exchanges",
		"receipt counted other inferences than served": "fixture's exchanges",
		"artifact phase without its search call":       "fixture's exchanges",
		"artifact phase served substituted source":     "fixture's exchanges",
		"artifact phase served the other PR's file":    "fixture's exchanges",
		"artifact phase served another revision":       "fixture's exchanges",
		"artifact phase never served source.go":        "fixture's exchanges",
	}
	for name, edit := range map[string]func(*e2eCompletion){
		// Finding 2: a completed launch must have exported the auth it returned.
		"completed launch never exported its auth": func(c *e2eCompletion) { c.Units[0].AuthExported = false },
		// Finding 3: a launch must be the artifact's provider and carry the
		// artifact's evidence, not merely its session hash.
		"launch of another provider": func(c *e2eCompletion) { c.Units[0].Provider = "claude" },
		"launch with another receipt": func(c *e2eCompletion) {
			c.Units[0].EvidenceHash = r.Digest("another receipt")
		},
		"artifact with another receipt": func(c *e2eCompletion) {
			e := *artifact(c, 0).ProviderEvidence
			e.Access.InferenceRequests++
			artifact(c, 0).ProviderEvidence = &e
		},
		"artifact without provider evidence": func(c *e2eCompletion) { artifact(c, 0).ProviderEvidence = nil },
		"artifact evidence the job never recorded": func(c *e2eCompletion) {
			job(c).ProviderAttempts = job(c).ProviderAttempts[1:]
		},
		"artifact evidence recorded twice": func(c *e2eCompletion) {
			job(c).ProviderAttempts = append(job(c).ProviderAttempts, r.ProviderAttempt{ID: "again", Evidence: job(c).ProviderAttempts[0].Evidence})
		},
		"artifact without source access": func(c *e2eCompletion) { artifact(c, 1).SourceAccess = nil },
		"source access the job never recorded": func(c *e2eCompletion) {
			job(c).SourceAttempts = job(c).SourceAttempts[1:]
		},
		"source access recorded under another attempt": func(c *e2eCompletion) {
			job(c).SourceAttempts[0].ID, job(c).SourceAttempts[1].ID = job(c).SourceAttempts[1].ID, job(c).SourceAttempts[0].ID
		},
		"artifact with other source access": func(c *e2eCompletion) {
			access := *artifact(c, 0).SourceAccess
			access.Bytes++
			artifact(c, 0).SourceAccess = &access
		},
		"source access of another session": func(c *e2eCompletion) {
			access := *artifact(c, 0).SourceAccess
			access.SessionHash = artifact(c, 1).InputHash
			artifact(c, 0).SourceAccess = &access
			job(c).SourceAttempts[0].Access = &access
		},
		"two artifacts of one session": func(c *e2eCompletion) {
			artifact(c, 3).InputHash = artifact(c, 2).InputHash
		},
		// Finding 4: the source record must bind the job's snapshot and the
		// current release, not merely agree with its export.
		"source record and export of another head": func(c *e2eCompletion) {
			c.Records[0].Snapshot, c.Exports[0].Snapshot = otherSnapshot(c), otherSnapshot(c)
		},
		"source record and export of another release": func(c *e2eCompletion) {
			c.Records[0].ReleaseHash, c.Exports[0].ReleaseHash = r.Digest("earlier release"), r.Digest("earlier release")
		},
		"source record and export of another runtime": func(c *e2eCompletion) {
			c.Records[0].RuntimeHash, c.Exports[0].RuntimeHash = r.Digest("earlier runtime"), r.Digest("earlier runtime")
		},
		"source unit not the attempt's":        func(c *e2eCompletion) { c.Records[0].Unit = "sfl-alpha-" + strings.Repeat("d", 32) + ".service" },
		"source stopped without capture":       func(c *e2eCompletion) { c.Records[0].Captured = false },
		"source stopped without settled start": func(c *e2eCompletion) { c.Records[0].StartSettled = false },
		// Round 8 finding 6: the upload-pack the fake upstream served must
		// have found the attempt captured and granted, not yet revoked.
		"source revoked before its upload-pack":      func(c *e2eCompletion) { c.Exchanges[1].Journal[0].Revoked = true },
		"source captured only after its upload-pack": func(c *e2eCompletion) { c.Exchanges[1].Journal[0].Captured = false },
		// Self-audit (f): the record's stub is the release's.
		"source record of another stub": func(c *e2eCompletion) { c.Records[0].StubHash = r.Digest("another stub") },
		// Finding 5: exactly Codex R1, Claude R1, Codex R2 and Claude R2.
		"duplicated round, omitted round": func(c *e2eCompletion) { artifact(c, 2).Report.Round = 1; artifact(c, 3).Report.Round = 1 },
		"artifact of another job":         func(c *e2eCompletion) { artifact(c, 0).JobHash = r.Digest("another job") },
		"report of another job ID":        func(c *e2eCompletion) { artifact(c, 0).Report.JobID = "request-2-1" },
		"report of another snapshot":      func(c *e2eCompletion) { artifact(c, 0).Report.SnapshotHash = r.Digest(otherSnapshot(c)) },
		"report of another config":        func(c *e2eCompletion) { artifact(c, 0).Report.ConfigHash = r.Digest("another config") },
		"checkpoint of another job":       func(c *e2eCompletion) { job(c).Checkpoint.JobHash = r.Digest("another job") },
		// Finding 6: one job per expected PR and no other durable job.
		"second job for PR 1": func(c *e2eCompletion) {
			// An older queued duplicate beside the judged newest job.
			job(c).CreatedAt = time.Unix(100, 0)
			second := *job(c)
			second.Job.ID, second.State, second.CreatedAt = "request-1-2", "queued", time.Unix(1, 0)
			c.State.Jobs[second.Job.ID] = &second
		},
		"job for another PR": func(c *e2eCompletion) {
			c.State.Jobs["request-2-1"] = &r.DurableJob{Job: r.Job{ID: "request-2-1"}, Scope: e2eScope(2), State: "queued"}
		},
		"write for another PR": func(c *e2eCompletion) {
			c.State.Writes["w2"] = &r.WriteIntent{ID: "w2", Kind: "review", Scope: e2eScope(2), State: "prepared"}
		},
		"acknowledgement write": func(c *e2eCompletion) {
			c.State.Writes["w2"] = &r.WriteIntent{ID: "w2", Kind: "ack", Scope: e2eScope(1), State: "confirmed"}
		},
		"job of another scope":   func(c *e2eCompletion) { job(c).Scope.InstallationID++ },
		"job of another base":    func(c *e2eCompletion) { job(c).Job.Snapshot.Base = strings.Repeat("9", 40) },
		"job of another release": func(c *e2eCompletion) { job(c).Job.Release = "earlier" },
		// Finding 7: the remote review must be the intent's exact document.
		"remote review with another marker": func(c *e2eCompletion) {
			remote(c)["marker"] = "<!-- sfl-alpha:" + strings.Repeat("d", 32) + " -->"
		},
		"remote review with another document": func(c *e2eCompletion) {
			_, remote(c)["document_hash"] = r.E2ERemoteReview(write(c).Document.Summary+" edited", write(c).Document.Inline)
		},
		"remote review with another inline body": func(c *e2eCompletion) {
			_, remote(c)["document_hash"] = r.E2ERemoteReview(write(c).Document.Summary, []r.InlineComment{{Path: "source.go", Line: 1, Body: "other"}})
		},
		"remote review with another ID": func(c *e2eCompletion) { remote(c)["id"] = int64(5002) },
		"intent with a marker of another ID": func(c *e2eCompletion) {
			marker := "<!-- sfl-alpha:" + strings.Repeat("d", 32) + " -->"
			write(c).Marker, write(c).Document.Summary = marker, "review\n\n"+marker
			rehash(c)
			remote(c)["marker"], remote(c)["document_hash"] = r.E2ERemoteReview(write(c).Document.Summary, write(c).Document.Inline)
		},
		"intent body hash not its document": func(c *e2eCompletion) { write(c).BodyHash = r.Digest("other") },
		"intent of another job":             func(c *e2eCompletion) { write(c).JobHash = r.Digest("another job") },
		"intent of another checkpoint":      func(c *e2eCompletion) { write(c).CheckpointHash = r.Digest("another checkpoint") },
		"intent of another request":         func(c *e2eCompletion) { write(c).RequestID = 2 },
		"intent of another generation":      func(c *e2eCompletion) { write(c).Generation = 2 },
		"intent of another base":            func(c *e2eCompletion) { write(c).Snapshot.Base = strings.Repeat("9", 40) },
		"intent with a second inline finding": func(c *e2eCompletion) {
			write(c).Document.Inline = append(write(c).Document.Inline, r.InlineComment{Path: "pr1-only.go", Line: 1, Body: "extra"})
			rehash(c)
			_, remote(c)["document_hash"] = r.E2ERemoteReview(write(c).Document.Summary, write(c).Document.Inline)
			remote(c)["inline"] = []string{"source.go:1", "pr1-only.go:1"}
		},
		// Backend checks must be the artifact's provider, round and job.
		"artifact session never checked": func(c *e2eCompletion) { c.Sessions = c.Sessions[1:] },
		"artifact session checked twice": func(c *e2eCompletion) { c.Sessions = append(c.Sessions, c.Sessions[0]) },
		"check of another round":         func(c *e2eCompletion) { c.Sessions[0].Round = 2 },
		"check of another provider":      func(c *e2eCompletion) { c.Sessions[0].Provider = r.Claude },
		"check of another job":           func(c *e2eCompletion) { c.Sessions[0].JobID = "request-2-1" },
		"unheld check without an artifact": func(c *e2eCompletion) {
			c.Sessions = append(c.Sessions, e2eModelSession{JobID: "request-1-1", Provider: r.Codex, Round: 1, PR: 1, Head: c.Heads[1], SessionHash: r.Digest("unrecorded")})
		},
		// Round 8 finding 2: every artifact session made exactly the canned
		// backend's exchanges, as the native fixtures require.
		"artifact session made two inferences":    func(c *e2eCompletion) { c.Sessions[1].Counts.Inference = 2 },
		"artifact session made four inferences":   func(c *e2eCompletion) { c.Sessions[3].Counts.Inference = 4 },
		"artifact session with a backend failure": func(c *e2eCompletion) { c.Sessions[0].Counts.Failures = 1 },
		"artifact session with telemetry":         func(c *e2eCompletion) { c.Sessions[2].Counts.Telemetry = 1 },
		"codex session without its metadata":      func(c *e2eCompletion) { c.Sessions[0].Counts.MetadataCaptured = false },
		"codex session reached negotiation":       func(c *e2eCompletion) { c.Sessions[2].Counts.Negotiation = 1 },
		// The launch's receipt must count what the backend served.
		"receipt counted other metadata than served": func(c *e2eCompletion) { c.Sessions[0].Counts.Metadata = 4 },
		"receipt counted other inferences than served": func(c *e2eCompletion) {
			// A receipt consistently recorded by the job and the launch.
			e := *artifact(c, 1).ProviderEvidence
			e.Access.InferenceRequests = 4
			recorded := e
			artifact(c, 1).ProviderEvidence, job(c).ProviderAttempts[1].Evidence = &e, &recorded
			c.Units[1].Access, c.Units[1].EvidenceHash = e.Access, r.Digest(&e)
		},
		// Each phase made the canned read and search calls.
		"artifact phase without its search call": func(c *e2eCompletion) {
			artifact(c, 1).SourceAccess.ToolCalls--
			job(c).SourceAttempts[1].Access.ToolCalls--
		},
		// Self-audit (i): what the source service served each phase is the
		// fake upstream's exact bytes at the job head. Artifact and job
		// records share the read records, so both change together.
		"artifact phase served substituted source": func(c *e2eCompletion) {
			artifact(c, 2).SourceAccess.Reads[0].Hash = r.Digest([]byte("substituted\n"))
		},
		"artifact phase served the other PR's file": func(c *e2eCompletion) {
			artifact(c, 0).SourceAccess.Reads[1].Path = "pr2-only.go"
		},
		"artifact phase served another revision": func(c *e2eCompletion) {
			artifact(c, 3).SourceAccess.Reads[0].Revision = strings.Repeat("e", 40)
		},
		"artifact phase never served source.go": func(c *e2eCompletion) {
			artifact(c, 1).SourceAccess.Reads = artifact(c, 1).SourceAccess.Reads[1:]
			job(c).SourceAttempts[1].Access.Reads = job(c).SourceAttempts[1].Access.Reads[1:]
		},
	} {
		c := e2eCompletedFixture()
		edit(&c)
		f, target := failures(c), refusal[name]
		if target == "" || !slices.ContainsFunc(f, func(m string) bool { return strings.Contains(m, target) }) {
			t.Error(name, "not refused by its target check", target, f)
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
	evidence := func() *r.ProviderEvidence {
		return &r.ProviderEvidence{Policy: r.EgressPolicy{Provider: r.Codex}, Access: r.ProviderAccess{Version: 1, SessionHash: session, InferenceRequests: 1, Failed: true, Stopped: true}}
	}
	unit := func() e2eNativeUnit {
		return e2eNativeUnit{Provider: "codex", Launch: launch, SessionHash: session, Captured: true, Stopped: true, AuditPresent: true, AuthExported: true, Access: evidence().Access, EvidenceHash: r.Digest(evidence())}
	}
	job := func() *r.DurableJob {
		return &r.DurableJob{LeaseToken: "lease", Outstanding: true, AttemptProvider: r.Codex, AttemptID: "attempt", AttemptInputHash: session, ProviderAttempts: []r.ProviderAttempt{{ID: "attempt", Evidence: evidence()}}}
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
		// Finding 1: the recorded receipt must be the audit's complete
		// evidence, not another valid stopped receipt of the same session.
		"recorded receipt differs from the audit": edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence.Access.Responses = 1 }),
		"recorded under another policy":           edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence.Policy.Headers = []string{"X-Other"} }),
		"audit evidence unprojected":              edit(func(u *e2eNativeUnit) { u.EvidenceHash = "" }, nil),
		"attempt recorded twice": edit(nil, func(j *r.DurableJob) {
			other := evidence()
			other.Access.Responses = 1
			j.ProviderAttempts = append(j.ProviderAttempts, r.ProviderAttempt{ID: "attempt", Evidence: other})
		}),
		"outstanding attempt without an ID": edit(nil, func(j *r.DurableJob) { j.AttemptID, j.ProviderAttempts[0].ID = "", "" }),
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

func TestE2EAttachedJob(t *testing.T) {
	current, earlier := strings.Repeat("a", 40), strings.Repeat("b", 40)
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("1", 40), Base: strings.Repeat("e", 40), MergeBase: strings.Repeat("e", 40)}
	state := func(edit func(*r.DurableJob)) r.DurableState {
		j := &r.DurableJob{Job: r.Job{ID: "request-1-1", Release: current, Snapshot: snapshot}, Scope: e2eScope(1), LeaseToken: "lease", Outstanding: true}
		if edit != nil {
			edit(j)
		}
		return r.DurableState{Jobs: map[string]*r.DurableJob{j.Job.ID: j}}
	}
	if j, err := e2eAttachedJob(state(nil), current, snapshot); err != nil || j == nil || j.Job.ID != "request-1-1" {
		t.Fatal("retained job of the current release refused", j, err)
	}
	second := state(nil)
	second.Jobs["request-1-2"] = &r.DurableJob{Job: r.Job{ID: "request-1-2", Release: current, Snapshot: snapshot}, Scope: e2eScope(1), CreatedAt: time.Unix(-1, 0)}
	another := state(nil)
	another.Jobs["request-2-1"] = &r.DurableJob{Job: r.Job{ID: "request-2-1", Release: current}, Scope: e2eScope(2)}
	for name, d := range map[string]r.DurableState{
		"no retained job":           {},
		"job without a lease":       state(func(j *r.DurableJob) { j.LeaseToken = "" }),
		"job of an earlier release": state(func(j *r.DurableJob) { j.Job.Release = earlier }),
		"job without a release":     state(func(j *r.DurableJob) { j.Job.Release = "" }),
		"job of another PR":         state(func(j *r.DurableJob) { j.Scope.PR = 2 }),
		"second job for PR 1":       second,
		"job for another PR beside": another,
		"job of another head":       state(func(j *r.DurableJob) { j.Job.Snapshot.Head = strings.Repeat("9", 40) }),
		"job of another base":       state(func(j *r.DurableJob) { j.Job.Snapshot.Base = strings.Repeat("9", 40) }),
		"job of another scope":      state(func(j *r.DurableJob) { j.Scope.InstallationID++ }),
	} {
		if j, err := e2eAttachedJob(d, current, snapshot); err == nil {
			t.Error(name, "accepted as the attached root's interrupted job", j)
		}
	}
}

// TestE2EConfirmedRequestVerdicts covers the request set a confirmed case
// leaves: each reviewed PR retains exactly the admitted request its job came
// from, of the expected origin, and no PR retains any other request. A
// further request could otherwise be admitted later as a duplicate review.
func TestE2EConfirmedRequestVerdicts(t *testing.T) {
	origin := func(kind, role string, actor int64) *r.RequestOrigin {
		return &r.RequestOrigin{Kind: kind, ActorID: actor, ActorRole: role}
	}
	opened := func(id int64) r.NativeRequest {
		return r.NativeRequest{ID: id, Admitted: true, Origin: origin("opened", "pr_author", r.OwnerID)}
	}
	comment := func(id int64, admitted bool) r.NativeRequest {
		return r.NativeRequest{ID: id, Admitted: admitted, Origin: origin("comment", "commenter", r.OwnerID)}
	}
	job := func(pr int, id int64) *r.DurableJob {
		return &r.DurableJob{Job: r.Job{ID: fmt.Sprintf("request-%d-%d", pr, id)}, Scope: r.Scope{PR: pr}, RequestID: id}
	}
	j1, j2 := job(1, 1), job(2, 1)
	state := func(prs int) r.DurableState {
		d := r.DurableState{PRs: map[string]*r.PRRecord{e2eScopeKey(1): {Scope: r.Scope{PR: 1}, Requests: []r.NativeRequest{opened(1)}}}}
		if prs == 2 {
			d.PRs[e2eScopeKey(2)] = &r.PRRecord{Scope: r.Scope{PR: 2}, Requests: []r.NativeRequest{comment(1, true)}}
		}
		return d
	}
	failures := func(verdicts []e2eVerdict) []string {
		out := []string{}
		for _, v := range verdicts {
			if !v.OK {
				out = append(out, v.Message)
			}
		}
		return out
	}
	single := func(d r.DurableState) []string { return failures(e2eOpenedRequestVerdicts("confirmed", d, j1)) }
	isolation := func(d r.DurableState) []string { return failures(e2eIsolationRequestVerdicts(d, j1, j2)) }
	if f := single(state(1)); len(f) != 0 {
		t.Fatal("sole admitted open request refused:", f)
	}
	if f := isolation(state(2)); len(f) != 0 {
		t.Fatal("admitted open and comment requests refused:", f)
	}
	pr := func(d r.DurableState, n int) *r.PRRecord { return d.PRs[e2eScopeKey(n)] }
	type mutation struct {
		prs   int
		judge func(r.DurableState) []string
		edit  func(r.DurableState)
	}
	for name, m := range map[string]mutation{
		"single: open request not admitted": {1, single, func(d r.DurableState) { pr(d, 1).Requests[0].Admitted = false }},
		"single: request not the job's":     {1, single, func(d r.DurableState) { pr(d, 1).Requests[0].ID = 2 }},
		"single: request removed":           {1, single, func(d r.DurableState) { pr(d, 1).Requests[0].Removed = true }},
		"single: request by another actor":  {1, single, func(d r.DurableState) { pr(d, 1).Requests[0].Origin.ActorID = 4242 }},
		"single: comment request instead":   {1, single, func(d r.DurableState) { pr(d, 1).Requests[0] = comment(1, true) }},
		"single: further unadmitted request": {1, single, func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("synchronize", "pr_author", r.OwnerID)})
		}},
		"single: request on another PR": {1, single, func(d r.DurableState) {
			d.PRs[e2eScopeKey(2)] = &r.PRRecord{Scope: r.Scope{PR: 2}, Requests: []r.NativeRequest{comment(1, false)}}
		}},
		"isolation: further comment on PR 2": {2, isolation, func(d r.DurableState) {
			pr(d, 2).Requests = append(pr(d, 2).Requests, comment(2, false))
		}},
		"isolation: further automatic request on PR 1": {2, isolation, func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("synchronize", "pr_author", r.OwnerID)})
		}},
		"isolation: PR 1 request lost":             {2, isolation, func(d r.DurableState) { pr(d, 1).Requests = nil }},
		"isolation: PR 1 request not admitted":     {2, isolation, func(d r.DurableState) { pr(d, 1).Requests[0].Admitted = false }},
		"isolation: PR 1 comment request":          {2, isolation, func(d r.DurableState) { pr(d, 1).Requests[0] = comment(1, true) }},
		"isolation: PR 2 comment not admitted":     {2, isolation, func(d r.DurableState) { pr(d, 2).Requests[0].Admitted = false }},
		"isolation: PR 2 request not the job's":    {2, isolation, func(d r.DurableState) { pr(d, 2).Requests[0].ID = 2 }},
		"isolation: PR 2 comment by another actor": {2, isolation, func(d r.DurableState) { pr(d, 2).Requests[0].Origin.ActorID = 4242 }},
		"isolation: PR 2 record lost":              {2, isolation, func(d r.DurableState) { delete(d.PRs, e2eScopeKey(2)) }},
		"isolation: request on a third PR": {2, isolation, func(d r.DurableState) {
			d.PRs[e2eScopeKey(3)] = &r.PRRecord{Scope: r.Scope{PR: 3}, Requests: []r.NativeRequest{comment(1, false)}}
		}},
	} {
		d := state(m.prs)
		m.edit(d)
		if len(m.judge(d)) == 0 {
			t.Error(name, "accepted")
		}
	}
}

func TestE2EUploadVerdict(t *testing.T) {
	h1, h2, base := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("e", 40)
	acquire := func(head string) []e2eGitExchange {
		return []e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Wants: []string{head, base}, Status: 200}}
	}
	if v := e2eUploadVerdict(acquire(h1), base, h1); !v.OK {
		t.Fatal("one acquisition refused:", v.Message)
	}
	if v := e2eUploadVerdict(append(acquire(h2), acquire(h1)...), base, h1, h2); !v.OK {
		t.Fatal("two acquisitions refused:", v.Message)
	}
	type input struct {
		exchanges []e2eGitExchange
		heads     []string
	}
	for name, in := range map[string]input{
		"nothing expected":             {nil, nil},
		"no acquisition":               {nil, []string{h1}},
		"refetched":                    {append(acquire(h1), acquire(h1)...), []string{h1}},
		"other head wanted":            {acquire(h2), []string{h1}},
		"foreign head wanted as well":  {[]e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Wants: []string{h1, h2, base}, Status: 200}}, []string{h1}},
		"base not wanted":              {[]e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Wants: []string{h1}, Status: 200}}, []string{h1}},
		"failed upload-pack":           {[]e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Wants: []string{h1, base}, Status: 500}}, []string{h1}},
		"failed advertisement":         {[]e2eGitExchange{{Operation: "advertisement", Status: 500}, {Operation: "upload-pack", Wants: []string{h1, base}, Status: 200}}, []string{h1}},
		"extra advertisement":          {append(acquire(h1), e2eGitExchange{Operation: "advertisement", Status: 200}), []string{h1}},
		"extra exchange without wants": {append(acquire(h1), e2eGitExchange{Operation: "upload-pack", Status: 200}), []string{h1}},
		"one of two acquisitions":      {acquire(h1), []string{h1, h2}},
	} {
		if v := e2eUploadVerdict(in.exchanges, base, in.heads...); v.OK {
			t.Error(name, "accepted:", v.Message)
		}
	}
}

func TestE2EProbeVerdicts(t *testing.T) {
	own, foreign := strings.Repeat("1", 40), strings.Repeat("2", 40)
	digest := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	probes := func() []r.E2EProbe {
		return []r.E2EProbe{
			{Revision: own, Path: "source.go", StoreOK: true, ReaderOK: true, StoreHash: digest(e2eSource), ReaderHash: digest(e2eSource)},
			{Revision: own, Path: "pr1-only.go", StoreOK: true, ReaderOK: true, StoreHash: digest("pull request 1 only\n"), ReaderHash: digest("pull request 1 only\n")},
			{Revision: own, Path: "pr2-only.go", ReaderErr: "path outside job allowlist"},
			{Revision: foreign, Path: "pr2-only.go", ReaderErr: "path outside job allowlist"},
			{Revision: foreign, Path: "source.go", ReaderErr: "revision outside job snapshot"},
		}
	}
	judge := func(p []r.E2EProbe) []string {
		return e2eFailures(e2eProbeVerdicts(1, p, own, foreign, "pr1-only.go", "pr2-only.go"))
	}
	if f := judge(probes()); len(f) != 0 {
		t.Fatal("isolated restore refused:", f)
	}
	for name, edit := range map[string]func([]r.E2EProbe) []r.E2EProbe{
		"no probes":                    func(p []r.E2EProbe) []r.E2EProbe { return nil },
		"probe missing":                func(p []r.E2EProbe) []r.E2EProbe { return p[:4] },
		"probes reordered":             func(p []r.E2EProbe) []r.E2EProbe { p[0], p[1] = p[1], p[0]; return p },
		"own source with other bytes":  func(p []r.E2EProbe) []r.E2EProbe { p[0].StoreHash = digest("other"); return p },
		"own file with the other PR's": func(p []r.E2EProbe) []r.E2EProbe { p[1].StoreHash = digest("pull request 2 only\n"); return p },
		"own file unreadable":          func(p []r.E2EProbe) []r.E2EProbe { p[1].ReaderOK = false; return p },
		"foreign object in the store":  func(p []r.E2EProbe) []r.E2EProbe { p[4].StoreOK, p[4].StoreHash = true, digest(e2eSource); return p },
		"foreign file through reader":  func(p []r.E2EProbe) []r.E2EProbe { p[2].ReaderOK = true; return p },
		// Round 8 finding 4: the model-facing reader must return the exact
		// bytes, not merely succeed.
		"own source substituted by the reader": func(p []r.E2EProbe) []r.E2EProbe { p[0].ReaderHash = digest("substituted"); return p },
		"own file read as the other PR's":      func(p []r.E2EProbe) []r.E2EProbe { p[1].ReaderHash = digest("pull request 2 only\n"); return p },
		"own read without reader bytes":        func(p []r.E2EProbe) []r.E2EProbe { p[1].ReaderHash = ""; return p },
		"foreign read with reader bytes":       func(p []r.E2EProbe) []r.E2EProbe { p[3].ReaderHash = digest("pull request 2 only\n"); return p },
	} {
		if len(judge(edit(probes()))) == 0 {
			t.Error(name, "accepted")
		}
	}
}

func TestE2EReconciledVerdict(t *testing.T) {
	states := func() (r.DurableState, r.DurableState) {
		c := e2eCompletedFixture()
		confirmed := c.State.Writes[strings.Repeat("c", 32)]
		unresolved := *confirmed
		unresolved.State, unresolved.ReceiptID = "unknown", 0
		closed := r.DurableState{Writes: map[string]*r.WriteIntent{unresolved.ID: &unresolved}}
		return closed, c.State
	}
	if closed, reopened := states(); !e2eReconciledVerdict(closed, reopened).OK {
		t.Fatal("reconciled unresolved intent refused")
	}
	for name, edit := range map[string]func(closed, reopened r.DurableState){
		"another intent confirmed in its place": func(_, reopened r.DurableState) {
			w := reopened.Writes[strings.Repeat("c", 32)]
			delete(reopened.Writes, w.ID)
			w.ID = strings.Repeat("d", 32)
			reopened.Writes[w.ID] = w
		},
		"document changed": func(_, reopened r.DurableState) {
			reopened.Writes[strings.Repeat("c", 32)].BodyHash = r.Digest("other")
		},
		"marker changed": func(_, reopened r.DurableState) { reopened.Writes[strings.Repeat("c", 32)].Marker = "other" },
		"checkpoint changed": func(_, reopened r.DurableState) {
			reopened.Writes[strings.Repeat("c", 32)].CheckpointHash = r.Digest("other")
		},
		"closed intent confirmed": func(closed, _ r.DurableState) { closed.Writes[strings.Repeat("c", 32)].State = "confirmed" },
		"nothing left unresolved": func(closed, _ r.DurableState) { clear(closed.Writes) },
		"not confirmed":           func(_, reopened r.DurableState) { reopened.Writes[strings.Repeat("c", 32)].State = "unknown" },
	} {
		closed, reopened := states()
		edit(closed, reopened)
		if e2eReconciledVerdict(closed, reopened).OK {
			t.Error(name, "accepted")
		}
	}
}

// TestE2EReleasedVerdict requires the copied store, reread after
// ReleaseStopped returned nil, to hold the retained job as auth_failed,
// unleased and not outstanding, with its job, charged checkpoint and recorded
// attempt evidence unchanged, and nothing else changed.
func TestE2EReleasedVerdict(t *testing.T) {
	before := func() *r.DurableJob {
		evidence := &r.ProviderEvidence{Policy: r.EgressPolicy{Provider: r.Codex}, Access: r.ProviderAccess{Version: 1, SessionHash: strings.Repeat("5", 64), InferenceRequests: 1, Failed: true, Stopped: true}}
		return &r.DurableJob{Job: r.Job{ID: "request-1-1", Release: "release"}, Scope: e2eScope(1), RequestID: 1, State: "running", Checkpoint: r.Checkpoint{ConsumedSessions: 1}, LeaseToken: "lease", LeaseUntil: time.Unix(1000, 0).UTC(), Outstanding: true, AttemptProvider: r.Codex, AttemptRound: 1, AttemptChargeDay: "2026-10-23", ChargeDays: []string{"2026-10-23"}, AttemptID: "attempt", AttemptInputHash: strings.Repeat("5", 64), ProviderAttempts: []r.ProviderAttempt{{ID: "attempt", Evidence: evidence}}, AttemptProviderEvidenceRequired: true}
	}
	released := func() r.DurableState {
		j := before()
		j.State, j.LeaseToken, j.LeaseUntil, j.Outstanding, j.AttemptChargeDay = "auth_failed", "", time.Time{}, false, ""
		return r.DurableState{Jobs: map[string]*r.DurableJob{j.Job.ID: j}, Writes: map[string]*r.WriteIntent{}}
	}
	if err := e2eReleasedVerdict(before(), released()); err != nil {
		t.Fatal("released retained job refused:", err)
	}
	job := func(d r.DurableState) *r.DurableJob { return d.Jobs["request-1-1"] }
	for name, edit := range map[string]func(r.DurableState){
		"nothing changed": func(d r.DurableState) { d.Jobs["request-1-1"] = before() },
		"lease kept":      func(d r.DurableState) { job(d).LeaseToken = "lease" },
		"lease deadline kept": func(d r.DurableState) {
			job(d).LeaseUntil = time.Unix(1000, 0).UTC()
		},
		"reservation kept":          func(d r.DurableState) { job(d).Outstanding = true },
		"queued instead":            func(d r.DurableState) { job(d).State = "queued" },
		"charged session refunded":  func(d r.DurableState) { job(d).Checkpoint.ConsumedSessions = 0 },
		"attempt evidence dropped":  func(d r.DurableState) { job(d).ProviderAttempts = nil },
		"attempt evidence replaced": func(d r.DurableState) { job(d).ProviderAttempts[0].Evidence.Access.Responses = 1 },
		"job changed":               func(d r.DurableState) { job(d).Job.Release = "other" },
		"job vanished":              func(d r.DurableState) { delete(d.Jobs, "request-1-1") },
		"another job released": func(d r.DurableState) {
			j := job(d)
			delete(d.Jobs, j.Job.ID)
			j.Job.ID = "request-1-2"
			d.Jobs[j.Job.ID] = j
		},
		"second job": func(d r.DurableState) {
			d.Jobs["request-2-1"] = &r.DurableJob{Job: r.Job{ID: "request-2-1"}, Scope: e2eScope(2), State: "queued"}
		},
		"write appeared": func(d r.DurableState) {
			d.Writes["w"] = &r.WriteIntent{ID: "w", Kind: "review", Scope: e2eScope(1), State: "prepared"}
		},
		"attempt forgotten": func(d r.DurableState) { job(d).AttemptID = "" },
	} {
		d := released()
		edit(d)
		if e2eReleasedVerdict(before(), d) == nil {
			t.Error(name, "accepted as the released retained job")
		}
	}
	if e2eReleasedVerdict(nil, released()) == nil {
		t.Error("release accepted without the retained job to compare")
	}
}

// TestE2ESourceOrderVerdicts requires each final source record to have been
// sampled live (start recorded and settled, captured and granted, not yet
// revoked or stopped) while the fake upstream served its upload-pack, and to
// be revoked and stopped at the end, so capture precedes revocation and stop.
func TestE2ESourceOrderVerdicts(t *testing.T) {
	head, base := strings.Repeat("1", 40), strings.Repeat("e", 40)
	record := func(attempt string, head string) r.E2ESourceRecord {
		return r.E2ESourceRecord{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", InvocationID: r.Digest(attempt)[:32], Snapshot: r.Snapshot{PR: 1, Head: head, Base: base, MergeBase: base}, StartIntent: true, StartSettled: true, Captured: true, Revoked: true, Stopped: true, GrantHash: r.Digest("grant " + attempt)}
	}
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	judge := func(records []r.E2ESourceRecord, exchanges []e2eGitExchange) []string {
		return e2eFailures(e2eSourceOrderVerdicts("ordered", records, exchanges))
	}
	if f := judge([]r.E2ESourceRecord{record(a, head)}, e2eAcquisition(record(a, head))); len(f) != 0 {
		t.Fatal("ordered acquisition refused:", f)
	}
	two := []r.E2ESourceRecord{record(a, head), record(b, strings.Repeat("2", 40))}
	if f := judge(two, append(e2eAcquisition(two[1]), e2eAcquisition(two[0])...)); len(f) != 0 {
		t.Fatal("two ordered acquisitions refused:", f)
	}
	type input struct {
		records   []r.E2ESourceRecord
		exchanges []e2eGitExchange
	}
	edit := func(change func(*input)) input {
		in := input{[]r.E2ESourceRecord{record(a, head)}, e2eAcquisition(record(a, head))}
		change(&in)
		return in
	}
	sample := func(in *input) *r.E2ESourceRecord { return &in.exchanges[1].Journal[0] }
	for name, in := range map[string]input{
		"no records":                     {nil, e2eAcquisition(record(a, head))},
		"no upload-pack":                 edit(func(in *input) { in.exchanges = in.exchanges[:1] }),
		"upload-pack of another head":    edit(func(in *input) { in.exchanges[1].Wants = []string{strings.Repeat("2", 40), base} }),
		"failed upload-pack":             edit(func(in *input) { in.exchanges[1].Status = 500 }),
		"second upload-pack":             edit(func(in *input) { in.exchanges = append(in.exchanges, in.exchanges[1]) }),
		"journal not sampled":            edit(func(in *input) { in.exchanges[1].Journal = nil }),
		"journal sample failed":          edit(func(in *input) { in.exchanges[1].JournalErr = "closed" }),
		"sample of another attempt only": edit(func(in *input) { sample(in).Attempt = b }),
		"attempt sampled twice":          edit(func(in *input) { in.exchanges[1].Journal = append(in.exchanges[1].Journal, *sample(in)) }),
		"start not yet recorded":         edit(func(in *input) { sample(in).StartIntent = false }),
		"start not yet settled":          edit(func(in *input) { sample(in).StartSettled = false }),
		"not yet captured":               edit(func(in *input) { sample(in).Captured = false }),
		"not yet granted":                edit(func(in *input) { sample(in).GrantHash = "" }),
		"granted another grant":          edit(func(in *input) { sample(in).GrantHash = r.Digest("other grant") }),
		"another invocation":             edit(func(in *input) { sample(in).InvocationID = strings.Repeat("8", 32) }),
		"another unit":                   edit(func(in *input) { sample(in).Unit = "sfl-alpha-" + b + ".service" }),
		"revoked before the upload-pack": edit(func(in *input) { sample(in).Revoked = true }),
		"stopped before the upload-pack": edit(func(in *input) { sample(in).Stopped, sample(in).Revoked = true, true }),
		"final record not revoked":       edit(func(in *input) { in.records[0].Revoked = false }),
		"final record not stopped":       edit(func(in *input) { in.records[0].Stopped = false }),
		"one of two unordered": {two, append(e2eAcquisition(two[0]), func() []e2eGitExchange {
			x := e2eAcquisition(two[1])
			x[1].Journal[0].Revoked = true
			return x
		}()...)},
	} {
		if len(judge(in.records, in.exchanges)) == 0 {
			t.Error(name, "accepted as ordered")
		}
	}
	// A completed case judges the order; an attached restart, whose
	// acquisition the earlier process observed and judged, does not.
	c := e2eCompletedFixture()
	jobs := []*r.DurableJob{e2eJobFor(c.State, 1)}
	c.Exchanges = nil
	if len(e2eFailures(e2eSourceVerdicts("unobserved", c, jobs))) == 0 {
		t.Error("completed source attempt accepted without an observed upload-pack")
	}
	c.Attached = true
	if f := e2eFailures(e2eSourceVerdicts("attached", c, jobs)); len(f) != 0 {
		t.Error("attached restart judged on an acquisition it could not observe:", f)
	}
}

// TestE2EParseUnits refuses a listing line it cannot parse, so a partial or
// malformed line is a failed listing, never a unit silently dropped.
func TestE2EParseUnits(t *testing.T) {
	source, native := "sfl-alpha-"+strings.Repeat("a", 32)+".service", "sfl-alpha-"+strings.Repeat("6", 32)+".service"
	units, err := e2eParseUnits(source + " loaded active running SFL alpha source VM\n" + native + "   loaded failed  failed\n\n")
	if err != nil || len(units) != 2 || units[source] != "loaded/active/running SFL alpha source VM" || units[native] != "loaded/failed/failed " {
		t.Fatal("well-formed listing misparsed", units, err)
	}
	if units, err := e2eParseUnits(""); err != nil || len(units) != 0 {
		t.Fatal("empty listing misparsed", units, err)
	}
	for name, out := range map[string]string{
		"truncated line":       source + " loaded active running source VM\n" + native + " loaded\n",
		"name only":            native + "\n",
		"repeated unit":        native + " loaded active running a\n" + native + " loaded inactive dead a\n",
		"line of another unit": "other.service loaded active running other\n",
	} {
		if units, err := e2eParseUnits(out); err == nil {
			t.Error(name, "parsed as", units)
		}
	}
}

// TestE2EWants keeps every want line of an upload-pack request, so a
// malformed or symbolic want is judged by the upload verdict, not dropped.
func TestE2EWants(t *testing.T) {
	head, base := strings.Repeat("1", 40), strings.Repeat("e", 40)
	body := "0011command=fetch0001000dthin-pack0032want " + head + "\n0032want " + base + "\n0009done\n0000"
	if got := e2eWants([]byte(body)); !slices.Equal(got, []string{head, base}) {
		t.Fatal("wants misparsed", got)
	}
	for name, extra := range map[string]string{
		"short object ID":  "0010want 1234\n",
		"symbolic want":    "0024want-ref refs/heads/main\n",
		"uppercase object": "0032want " + strings.Repeat("A", 40) + "\n",
		"want without ID":  "0009want\n",
	} {
		got := e2eWants([]byte(body + extra))
		if len(got) != 3 || !slices.Equal(got[:2], []string{head, base}) || got[2] == head || got[2] == base {
			t.Error(name, "not kept as a want", got)
		}
		if v := e2eUploadVerdict([]e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Wants: got, Status: 200}}, base, head); v.OK {
			t.Error(name, "accepted as an exact acquisition")
		}
	}
}

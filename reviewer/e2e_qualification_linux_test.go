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
// close-provider-reopen, close-provider-recover, close-publish).
// close-provider-reopen and close-provider-recover are second processes that
// attach to the root retained by close-provider through SFL_E2E_ATTACH, as a
// restarted service would; run one of them per retained root. SFL_E2E_EVIDENCE optionally
// names an existing private directory that receives a nonsecret JSON result.
// Run under the shared VM lock.
func TestE2EQualification(t *testing.T) {
	sourceImage, sourceStub, nativeImage := os.Getenv("SFL_E2E_SOURCE_IMAGE"), os.Getenv("SFL_E2E_SOURCE_STUB"), os.Getenv("SFL_E2E_NATIVE_IMAGE")
	scenario := os.Getenv("SFL_E2E_CASE")
	if sourceImage == "" || sourceStub == "" || nativeImage == "" || scenario == "" {
		t.Skip("explicit source image, exact-head source stub, native image and case required")
	}
	cases := map[string]func(*e2eEnv){"review": e2eReview, "isolation": e2eIsolation, "close-capture": e2eCloseCapture, "close-provider": e2eCloseProvider, "close-provider-reopen": e2eCloseProviderReopen, "close-provider-recover": e2eCloseProviderRecover, "close-publish": e2eClosePublish}
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
// must start (source acquisitions and native launches), whether both
// provider auth stores must end usable, and exactly the role-qualified routes
// ("role METHOD route", e2eRouteName) the fake GitHub must have served. Fence
// cases check their own auth.
type e2eExpect struct {
	SourceUnits, NativeUnits int
	AuthUsable               bool
	Routes                   []string
}

// e2eRoleRoutes is production's credential split, every route each GitHub
// credential role may use: the app JWT only qualifies the installation and
// mints tokens (acquireGitHubInstallation); each installation token first
// requalifies the installation (/installation/repositories, the bot user and
// the repository); then, as GitHubInstallationTransport documents, only the
// reader serves discovery and object acquisition (github_polling.go,
// github_discovery.go, github_triggers.go; Git is reader-only, serveGit) and
// only the publisher serves publication (github_publication.go: post, scan,
// receipt and annotate). A route outside its role's list is reader work done
// with the write-scoped credential, or the reverse.
var e2eRoleRoutes = map[string][]string{
	"app": {"GET /app", "GET /installation", "GET /app/installations/N", "POST /app/installations/N/access_tokens"},
	"reader": {"GET /installation/repositories", "GET /users/sfl-test[bot]", "GET /repo",
		"GET /pulls", "GET /pulls/N", "GET /git/ref/heads/main", "GET /pulls/N/requested_reviewers", "GET /compare", "GET /issues/N/events", "GET /issues/N/comments"},
	"publisher": {"GET /installation/repositories", "GET /users/sfl-test[bot]", "GET /repo",
		"POST /pulls/N/reviews", "POST /issues/N/comments", "GET /pulls/N/reviews", "GET /issues/N/comments", "GET /pulls/N/reviews/N/comments", "GET /pulls/N/reviews/N", "PUT /pulls/N/reviews/N"},
}

// The routes every case serves: both roles' tokens minted and qualified, and
// the reader's polling, PR, base ref, merge base, trigger comment and
// repository reads. A case that publishes adds the review POST and its
// receipt read; one that reconciles an unknown POST adds the review scan.
var (
	e2eQualifiedRoutes = []string{"app GET /app", "app GET /installation", "app GET /app/installations/N", "app POST /app/installations/N/access_tokens",
		"reader GET /installation/repositories", "reader GET /users/sfl-test[bot]", "reader GET /repo", "reader GET /pulls", "reader GET /pulls/N", "reader GET /git/ref/heads/main", "reader GET /compare", "reader GET /issues/N/comments",
		"publisher GET /installation/repositories", "publisher GET /users/sfl-test[bot]", "publisher GET /repo"}
	e2ePublishedRoutes  = append(slices.Clone(e2eQualifiedRoutes), "publisher POST /pulls/N/reviews", "publisher GET /pulls/N/reviews/N/comments")
	e2eReconciledRoutes = append(slices.Clone(e2ePublishedRoutes), "publisher GET /pulls/N/reviews")
)

var e2eExpectations = map[string]e2eExpect{
	"review":                {SourceUnits: 1, NativeUnits: 4, AuthUsable: true, Routes: e2ePublishedRoutes},
	"isolation":             {SourceUnits: 2, NativeUnits: 8, AuthUsable: true, Routes: e2ePublishedRoutes},
	"close-capture":         {SourceUnits: 1, NativeUnits: 4, AuthUsable: true, Routes: e2ePublishedRoutes},
	"close-publish":         {SourceUnits: 1, NativeUnits: 4, AuthUsable: true, Routes: e2eReconciledRoutes},
	"close-provider":        {SourceUnits: 1, NativeUnits: 1, Routes: e2eQualifiedRoutes},
	"close-provider-reopen": {Routes: e2eQualifiedRoutes},
	// Recovery launches nothing; PR 2's review is one acquisition and four
	// sessions, and the re-provisioned Codex auth, a fresh synthetic seed
	// with the original synthetic tokens, is returned by its last one.
	// Recovery uses no GitHub route (RecoverStoppedProvider reads and writes
	// only the store, the native roots and the auth store), so the process
	// serves exactly an attached restart's routes plus PR 2's publication.
	"close-provider-recover": {SourceUnits: 1, NativeUnits: 4, AuthUsable: true, Routes: e2ePublishedRoutes},
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
		hosts[provider] = r.GitReviewerProviderHost{Root: e.native[provider], Auth: store, Transport: model.transport, ProfileHints: e2eProfileHints(provider), Check: model.check}
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
	e.openIdle(label)
	e.startRun(label)
}

// openIdle composes the public startup without refreshing or running, the
// state in which an operator may invoke stopped-provider recovery.
func (e *e2eEnv) openIdle(label string) {
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
}

// startRun explicitly refreshes credentials and starts Run.
func (e *e2eEnv) startRun(label string) {
	e.t.Helper()
	s := e.service
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
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
	e.awaitRun(label, 10*time.Second)
	if err == nil {
		e.service = nil
	}
	e.snapshotJobs(label + " after close")
	if e.store != nil {
		e.closeStore(label, e.store)
		e.store = nil
	}
}

// awaitRun requires Close itself to have stopped the service's Run: it
// waits up to limit for Run to return and judges its result, and only then
// cancels Run's own context. Cancelling first would let a Close that never
// stopped the active controller pass, since that cancellation alone makes
// Run return the expected context.Canceled.
func (e *e2eEnv) awaitRun(label string, limit time.Duration) {
	e.t.Helper()
	defer e.runCancel()
	select {
	case runErr := <-e.runDone:
		e.result.RunErrors = append(e.result.RunErrors, fmt.Sprintf("%s: %v", label, runErr))
		e.check(e2eRunErrorExpected(runErr, e.wedged), "%s: Run returned only the expected shutdown result (wedged %v): %v", label, e.wedged, runErr)
	case <-time.After(limit):
		e.check(false, "%s: Run did not return after Close", label)
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
	e.result.NativeUnits = e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID())
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
		verdicts = append(verdicts, e2eVerdict{u.wholeStop() && u.AuditPresent, fmt.Sprintf("native unit %s started, settled, captured, aborted and stopped with its bound stopped audit", u.Launch)})
	}
	verdicts = append(verdicts, e2eVerdict{len(res.GitHub.Violations) == 0, fmt.Sprintf("fake GitHub saw no role, destination or credential violation: %v", res.GitHub.Violations)})
	verdicts = append(verdicts, e2eRouteVerdict(res.GitHub.Requests, want.Routes))
	verdicts = append(verdicts, e2eCorrelateUnits(res, want, before, attached)...)
	if want.AuthUsable {
		for _, provider := range []r.Provider{r.Codex, r.Claude} {
			verdicts = append(verdicts, e2eVerdict{res.AuthRecoveryClear[string(provider)], fmt.Sprintf("%s auth store is usable after the run", provider)})
		}
	}
	return verdicts
}

// e2eRouteVerdict requires the fake GitHub to have served exactly the case's
// role-qualified routes, so a role that did the other role's work, or work its
// case never needs, fails even where both roles' allowlists admit the route
// (issue comments, the repository and the qualification reads).
func e2eRouteVerdict(requests map[string]int, want []string) e2eVerdict {
	unexpected, missing := []string{}, []string{}
	for route, n := range requests {
		if n > 0 && !slices.Contains(want, route) {
			unexpected = append(unexpected, route)
		}
	}
	for _, route := range want {
		if requests[route] == 0 {
			missing = append(missing, route)
		}
	}
	sort.Strings(unexpected)
	return e2eVerdict{len(want) != 0 && len(unexpected) == 0 && len(missing) == 0, fmt.Sprintf("fake GitHub served exactly the case's per-role routes (unexpected %v, missing %v)", unexpected, missing)}
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
		e.settled()
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

// settled re-reads the fake GitHub and model backends of a finished run once
// every fake server is closed, and httptest's Close has therefore waited for
// every handler to return, and judges them with e2eSettledVerdicts against
// what finish recorded while the servers were still open.
func (e *e2eEnv) settled() {
	if !e.finished || e.github == nil {
		return
	}
	models := map[r.Provider][]e2eModelSession{}
	for provider, model := range e.models {
		models[provider] = model.sessionsCopy()
	}
	e.mu.Lock()
	judged := e.result
	e.mu.Unlock()
	for _, v := range e2eSettledVerdicts(judged, e.github.summary(), models) {
		e.check(v.OK, "%s", v.Message)
	}
}

// e2eSettledVerdicts requires the quiesced fake servers to hold exactly the
// observations res recorded in finish: no later violation, review POST,
// remote review or Git exchange, and no later model session or backend
// request. A handler still running when finish read the servers could
// otherwise add one the judgment never saw. Token mints and the API request
// tally are not compared: a wedged handle keeps its credential owner until
// the process exits.
func e2eSettledVerdicts(res *e2eResult, github e2eGitHubSummary, models map[r.Provider][]e2eModelSession) []e2eVerdict {
	return []e2eVerdict{
		{slices.Equal(github.Violations, res.GitHub.Violations), fmt.Sprintf("the quiesced fake GitHub holds no violation finish did not judge (%v)", github.Violations)},
		{maps.Equal(github.ReviewPosts, res.GitHub.ReviewPosts) && r.Digest(github.Reviews) == r.Digest(res.GitHub.Reviews), fmt.Sprintf("the quiesced fake GitHub holds exactly the review POSTs and reviews finish judged (%v)", github.ReviewPosts)},
		{len(github.GitExchanges) == len(res.GitHub.GitExchanges), fmt.Sprintf("the quiesced fake upstream served exactly the %d Git exchanges finish judged (%d)", len(res.GitHub.GitExchanges), len(github.GitExchanges))},
		{r.Digest(models) == r.Digest(res.Models), "the quiesced fake model backends hold exactly the sessions and requests finish judged"},
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

// observed samples once and counts the units first seen by this run. A
// failed listing is returned, never read as an empty one.
func (o *e2eUnitObserver) observed() (int, error) {
	if _, err := o.sample(); err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.seen), nil
}

// stop first stops and joins the periodic sampler, so no listing still in
// flight can record a unit after the final one, then waits until a successful
// listing of its own shows no unit first seen by this run, and reports. The
// last listing stop makes is therefore the last observation. A failed listing
// is never read as an empty one: it keeps the last successful residual (or,
// before any, every unit this run saw) and, when the grace ends without a
// later successful listing, returns its error.
func (o *e2eUnitObserver) stop(grace time.Duration) ([]e2eSystemdUnit, []string, error) {
	o.stopOnce.Do(func() { close(o.done) })
	<-o.joined
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
	Started, StartSettled, Captured      bool
	Aborted, Stopped                     bool
	Complete, AuthExported, AuthReturned bool
	AuditPresent                         bool
	RequestHash, ReportHash, SourceHash  string
	Stage, Failure                       string
	// Access is the nonsecret provider access receipt of a present audit.
	Access r.ProviderAccess
	// AttemptID is the durable attempt the stopped receipt names, and
	// EvidenceHash the digest of its complete provider evidence (empty
	// without one). Both are set only with AuditPresent.
	AttemptID, EvidenceHash string
	// Binding is the journal's launch binding, Policy the release's egress
	// policy and Audit the present audit, which launch verdicts bind to the
	// session, request, report and source receipt they judge.
	Binding r.AuthBinding     `json:"-"`
	Policy  r.EgressPolicy    `json:"-"`
	Audit   *r.E2ENativeAudit `json:"-"`
}

// e2eBootID is the host's current boot ID as production records it (the
// trimmed /proc boot_id), or "" when unreadable, which no unit journal
// matches.
func e2eBootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// e2eProfileHints are the nonsecret profile hints the harness configures
// for provider's native host.
func e2eProfileHints(provider r.Provider) r.NativeVMProfileHints {
	if provider == r.Claude {
		return r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	return r.NativeVMProfileHints{}
}

// e2eFixturePaths is the sorted source path list of PR pr's job: the paths
// its trusted diff changes, which the job's evidence reader allows.
func e2eFixturePaths(pr int) []string { return slices.Sorted(maps.Keys(e2eFixtureDiff(pr))) }

// e2eSessionFor is the session production's engine builds for job j's
// provider phase in round (Engine.Run): NewSession under the job's protocol
// limits, with every report of the earlier rounds' artifacts, in phase order
// and frozen through JSON as the engine freezes them, as peer reports, and no
// focus IDs, which only a third round derives.
func e2eSessionFor(j *r.DurableJob, provider r.Provider, round int) (r.Session, error) {
	if j == nil || round < 1 || round > 2 {
		return r.Session{}, fmt.Errorf("no derivable session for round %d", round)
	}
	limits, err := r.E2EProtocolLimits(j.Job)
	if err != nil {
		return r.Session{}, err
	}
	var peers []r.Report
	for earlier := 1; earlier < round; earlier++ {
		for _, p := range j.Job.Config.Providers() {
			found := 0
			for _, a := range j.Checkpoint.Artifacts {
				if a.Report.Provider == p && a.Report.Round == earlier {
					peers = append(peers, a.Report)
					found++
				}
			}
			if found != 1 {
				return r.Session{}, fmt.Errorf("job %s holds %d %s R%d artifacts, want one", j.Job.ID, found, p, earlier)
			}
		}
	}
	raw, err := json.Marshal(peers)
	if err != nil {
		return r.Session{}, err
	}
	var frozen []r.Report
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return r.Session{}, err
	}
	return r.NewSession(j.Job.ID, j.Job.Snapshot, j.Job.Config.Hash(), provider, round, j.Job.Packet, frozen, nil, limits)
}

// e2eReadNativeUnits projects every entry of each provider's native root
// except the supervisor's own executables directory, judging each launch
// against that provider's release. An entry is never skipped: a directory
// without a readable unit journal, any other entry, and an unreadable root
// each project as a failed launch, which no launch verdict accepts.
func e2eReadNativeUnits(roots map[r.Provider]string, releases map[r.Provider]r.GitReviewerProviderRelease, limits r.Limits, bootID string) []e2eNativeUnit {
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
			units = append(units, e2eNativeUnitFrom(provider, releases[provider], limits, bootID, entry.Name(), filepath.Join(root, entry.Name())))
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Launch < units[j].Launch })
	return units
}

// e2eNativeUnitFrom projects one native launch directory's unit journal and
// stopped audit receipt. The journal is read only through production's own
// unit journal reader (E2EReadNativeUnitJournal), which refuses a record that
// is not private, strict, of the native-vm payload under the release's stub,
// or of an impossible lifecycle (a capture without a settled start, a stop
// without an abort). It then counts only as version 1 for this directory's
// launch: it names sfl-alpha-<launch>.service, its binding derives the
// containment ID from its session and launch, its manifest hash is the one
// production derives from the release's pins for this launch and session,
// and it, and its captured process, were recorded in this boot (bootID) with
// a systemd invocation and process start time. The audit is read only as
// production's exact receipt encoding (E2EReadNativeAudit) and counts
// (AuditPresent) only as version 2 repeating that exact binding and manifest
// hash and naming a well-formed durable attempt ID, which the launch verdicts
// bind to the job's recorded attempt (a version 1 receipt names no attempt,
// so production recovery refuses it and so does the harness), with the stage
// and failure kind of its completion: a completed phase
// ends in result validation with no failure, an interrupted one records a
// failure kind. Its provider evidence, required once auth was exported or the
// session completed, must be the stopped receipt of the same session under
// the release's policy and pass production's provider receipt validation
// under the session limits: the stopped-receipt invariants always, and the
// completed-receipt validation when the audit records a completed session. A
// copied, foreign, failed or impossible receipt therefore proves nothing. The
// audit's auth lifecycle must be possible: production returns only exported
// auth, and a completed session has both exported and returned it. The
// audit's request, report and source hashes and its guest-reported parts
// need the launch's session and are bound by the launch verdicts.
func e2eNativeUnitFrom(provider r.Provider, release r.GitReviewerProviderRelease, limits r.Limits, bootID, launch, directory string) e2eNativeUnit {
	u := e2eNativeUnit{Provider: string(provider), Launch: launch}
	policy := release.Policy
	if len(release.Stub.SHA256) != 64 {
		return u
	}
	unit, err := r.E2EReadNativeUnitJournal(directory, release.Stub.SHA256)
	if err != nil {
		return u
	}
	b := unit.Binding
	if unit.Version != 1 || b.LaunchID != launch || unit.Unit != "sfl-alpha-"+launch+".service" || len(b.SessionHash) != 64 || b.ContainmentID != r.Digest(struct{ Session, Launch string }{b.SessionHash, launch}) {
		return u
	}
	manifest, err := r.E2ENativeManifestHash(release, provider, b)
	if err != nil || unit.PayloadKind != "native-vm" || unit.StubHash != release.Stub.SHA256 || unit.ManifestHash != manifest {
		return u
	}
	process := unit.Identity
	if bootID == "" || unit.BootID != bootID || process.BootID != bootID || !e2eHexPattern.MatchString(process.InvocationID) || process.StartTicks == 0 {
		return u
	}
	u.SessionHash, u.Captured, u.Stopped, u.ManifestHash = b.SessionHash, unit.Captured, unit.Stopped, unit.ManifestHash
	u.Started, u.StartSettled, u.Aborted, u.Binding, u.Policy = unit.Started, unit.StartSettled, unit.Aborted, b, policy
	audit, err := r.E2EReadNativeAudit(directory)
	if err != nil || audit.Version != 2 || audit.Binding != b || audit.ManifestHash != unit.ManifestHash || !e2eAttemptID.MatchString(audit.AttemptID) {
		return u
	}
	if (audit.AuthReturned && !audit.AuthExported) || (audit.Complete && !audit.AuthReturned) {
		return u
	}
	if audit.Complete && (audit.Stage != "result-validation" || audit.Failure != "") || !audit.Complete && audit.Failure == "" {
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
	u.RequestHash, u.ReportHash, u.SourceHash, u.Stage, u.Failure, u.Audit = audit.RequestHash, audit.ReportHash, audit.SourceHash, audit.Stage, audit.Failure, &audit
	if audit.Provider != nil {
		u.Access = audit.Provider.Access
	}
	u.AttemptID, u.EvidenceHash = audit.AttemptID, e2eEvidenceHash(audit.Provider)
	return u
}

// e2eHexPattern is a systemd invocation ID: 32 lowercase hex digits.
var e2eHexPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// wholeStop reports the production-whole stop of a validated unit journal:
// its start was recorded and settled, its unit captured, and its stop
// recorded after the abort.
func (u e2eNativeUnit) wholeStop() bool {
	return u.Started && u.StartSettled && u.Captured && u.Aborted && u.Stopped
}

// e2eLaunchAudit binds launch u's audit to the phase of job j it ran for:
// production's request, guest-report and failure-kind validation for the
// session j derives for that phase (E2EValidateNativeAudit), under the job's
// source paths, the release's policy and the provider's profile hints, and
// the report and source receipt the phase recorded, compared by digest as
// persistAudit records them. A completed phase records its stored report;
// an interrupted one records the zero report and, as its final stage, the
// wait for its result record, or that record's validation once its source
// receipt arrived. A phase whose exported auth was not returned fails with
// auth-return, which only a stop failure outranks, and a stop failure
// writes no audit.
func e2eLaunchAudit(u e2eNativeUnit, j *r.DurableJob, session r.Session, report any, access *r.SourceAccess) error {
	if u.Audit == nil || j == nil || r.Digest(session) != u.SessionHash || u.Provider != string(session.Identity.Provider) {
		return fmt.Errorf("launch %s has no audit of its phase's session", u.Launch)
	}
	if err := r.E2EValidateNativeAudit(*u.Audit, session, e2eFixturePaths(j.Job.Snapshot.PR), u.Policy, e2eProfileHints(session.Identity.Provider)); err != nil {
		return fmt.Errorf("launch %s audit: %v", u.Launch, err)
	}
	if u.Audit.ReportHash != r.Digest(report) || u.Audit.SourceHash != r.Digest(access) {
		return fmt.Errorf("launch %s audit records another report or source receipt than its phase", u.Launch)
	}
	stage := "result-validation"
	if !u.Audit.Complete && access == nil {
		stage = "result"
	}
	if u.Audit.Stage != stage || u.Audit.Complete != u.Complete || u.Audit.AuthExported && !u.Audit.AuthReturned && u.Audit.Failure != "auth-return" {
		return fmt.Errorf("launch %s audit ends at stage %s with failure %q", u.Launch, u.Audit.Stage, u.Audit.Failure)
	}
	return nil
}

// e2eInterruptedLaunch binds an interrupted launch u to the one attempt of
// job j that durably records its complete audit evidence: the session j
// derives for u's provider in one of its rounds is u's, the audit records
// the zero report, and exactly the source receipt j records for that
// attempt, which production validates for that session, or none when j
// records none. It returns the attempt ID.
func e2eInterruptedLaunch(u e2eNativeUnit, j *r.DurableJob) (string, error) {
	attempt, err := e2eRecordedEvidence(j, u)
	if err != nil {
		return "", err
	}
	var session r.Session
	derived := false
	for round := 1; round <= 2 && !derived; round++ {
		if s, err := e2eSessionFor(j, r.Provider(u.Provider), round); err == nil && r.Digest(s) == u.SessionHash {
			session, derived = s, true
		}
	}
	if !derived {
		return "", fmt.Errorf("launch %s session is not one job %s derives", u.Launch, j.Job.ID)
	}
	var access *r.SourceAccess
	records := 0
	for _, s := range j.SourceAttempts {
		if s.ID == attempt.ID {
			records++
			access = s.Access
		}
	}
	if records > 1 || records == 1 && r.E2EValidateSourceAccess(access, session) != nil {
		return "", fmt.Errorf("attempt %s holds %d source records, or an invalid one", attempt.ID, records)
	}
	if err := e2eLaunchAudit(u, j, session, r.Report{}, access); err != nil {
		return "", err
	}
	return attempt.ID, nil
}

// e2eRetainedLaunches proves the native launch a root attached after
// close-provider must hold, and returns it as the only pre-existing launch:
// exactly one launch, production-whole stopped, with a bound version 2
// stopped audit recording the auth as exported, not returned and the session
// not complete. Its provider and session must be the retained job's
// outstanding attempt, which is the session the job derives for that
// attempt's provider and round and the job's only charged session beyond its
// artifacts, and its receipt must name that attempt's durable ID. The job
// must hold exactly one provider attempt, that one, whose durably recorded
// stopped provider evidence is the audit's complete evidence (by digest), not
// merely another stopped receipt of the same session, and no source attempt
// but at most that attempt's own. The audit must record that attempt's
// request, no report and exactly the source receipt the job recorded for it
// (e2eInterruptedLaunch). A missing launch yields no launch, and an
// unreadable launch directory, a version 1 receipt naming no attempt or any
// other root entry a failed or unaudited launch beside it, so each is refused
// and neither the fence nor recovery can pass without exactly its interrupted
// launch.
func e2eRetainedLaunches(units []e2eNativeUnit, j *r.DurableJob) (map[string]bool, error) {
	if j == nil {
		return nil, errors.New("no retained job")
	}
	if len(units) != 1 {
		return nil, fmt.Errorf("%d native launches retained, want the one interrupted launch", len(units))
	}
	u := units[0]
	if !u.wholeStop() || !u.AuditPresent || !u.AuthExported || u.AuthReturned || u.Complete {
		return nil, fmt.Errorf("retained launch %s lacks a captured whole-unit stop with a stopped audit of exported, unreturned auth", u.Launch)
	}
	if !j.Outstanding || u.Provider != string(j.AttemptProvider) || u.SessionHash != j.AttemptInputHash || !e2eAttemptID.MatchString(u.AttemptID) || u.AttemptID != j.AttemptID {
		return nil, fmt.Errorf("retained launch %s is not job %s's outstanding attempt", u.Launch, j.Job.ID)
	}
	session, err := e2eSessionFor(j, j.AttemptProvider, j.AttemptRound)
	if err != nil || r.Digest(session) != j.AttemptInputHash || j.Checkpoint.ConsumedSessions != len(j.Checkpoint.Artifacts)+1 {
		return nil, fmt.Errorf("job %s's outstanding attempt is not the charged session it derives for %s R%d", j.Job.ID, j.AttemptProvider, j.AttemptRound)
	}
	if j.AttemptID == "" || len(j.ProviderAttempts) != 1 || j.ProviderAttempts[0].ID != j.AttemptID {
		return nil, fmt.Errorf("job %s holds %d provider attempts, want only its outstanding attempt %q", j.Job.ID, len(j.ProviderAttempts), j.AttemptID)
	}
	recorded, err := e2eRecordedEvidence(j, u)
	if err != nil || recorded.ID != j.AttemptID || !recorded.Evidence.Access.Stopped {
		return nil, fmt.Errorf("job %s holds no stopped provider evidence of its outstanding attempt equal to retained launch %s's audit: %v", j.Job.ID, u.Launch, err)
	}
	for _, s := range j.SourceAttempts {
		if s.ID != j.AttemptID {
			return nil, fmt.Errorf("job %s holds source attempt %q of no outstanding attempt", j.Job.ID, s.ID)
		}
	}
	if _, err := e2eInterruptedLaunch(u, j); err != nil {
		return nil, err
	}
	return map[string]bool{u.Launch: true}, nil
}

// e2eArtifactAttempt requires job j to record artifact a's provider evidence
// and source access under one attempt, as the durable reviewer records both
// before storing the artifact: exactly one provider attempt holds a's
// evidence (of a's session and provider), and exactly one source attempt of
// that attempt ID holds a's source access, which is of a's session. The
// artifact must require provider evidence, as every artifact of the audited
// native reviewer does, and pass the validation production runs on every
// stored artifact before recovering or publishing its job
// (E2EValidateArtifactProviderEvidence). It returns the attempt ID.
func e2eArtifactAttempt(j *r.DurableJob, a r.Artifact) (string, error) {
	if a.ProviderEvidence == nil || a.SourceAccess == nil || a.SourceAccess.SessionHash != a.InputHash || !a.ProviderEvidenceRequired {
		return "", fmt.Errorf("artifact %s R%d lacks required provider evidence or source access of its session", a.Report.Provider, a.Report.Round)
	}
	if err := r.E2EValidateArtifactProviderEvidence(j, a); err != nil {
		return "", fmt.Errorf("artifact %s R%d provider evidence: %v", a.Report.Provider, a.Report.Round, err)
	}
	attempt, err := e2eRecordedEvidence(j, e2eNativeUnit{Provider: string(a.Report.Provider), Launch: "of artifact " + a.InputHash, SessionHash: a.InputHash, EvidenceHash: e2eEvidenceHash(a.ProviderEvidence)})
	if err != nil {
		return "", err
	}
	records := 0
	for _, s := range j.SourceAttempts {
		if s.ID == attempt.ID {
			records++
			if s.Access == nil || r.Digest(s.Access) != r.Digest(a.SourceAccess) {
				return "", fmt.Errorf("attempt %s records other source access than artifact %s R%d", attempt.ID, a.Report.Provider, a.Report.Round)
			}
		}
	}
	if records != 1 {
		return "", fmt.Errorf("attempt %s has %d source access records, want one", attempt.ID, records)
	}
	return attempt.ID, nil
}

// e2eAttemptSet requires job j to hold exactly the provider and source
// attempts its artifacts were recorded under (e2eArtifactAttempt) and, for
// each interrupted launch it records (e2eRecordedEvidence), that launch's
// attempt, with at most its one source attempt: no attempt ID repeats or
// lacks a phase, so an unrelated attempt record beside the expected ones is
// refused.
func e2eAttemptSet(j *r.DurableJob, interrupted []e2eNativeUnit) error {
	want := map[string]bool{}
	for _, a := range j.Checkpoint.Artifacts {
		id, err := e2eArtifactAttempt(j, a)
		if err != nil {
			return err
		}
		want[id] = true
	}
	for _, u := range interrupted {
		if attempt, err := e2eRecordedEvidence(j, u); err == nil {
			want[attempt.ID] = true
		}
	}
	providers, sources := map[string]bool{}, map[string]bool{}
	for _, a := range j.ProviderAttempts {
		if !want[a.ID] || providers[a.ID] {
			return fmt.Errorf("job %s holds provider attempt %q of no artifact or interrupted launch, or twice", j.Job.ID, a.ID)
		}
		providers[a.ID] = true
	}
	for _, s := range j.SourceAttempts {
		if !want[s.ID] || sources[s.ID] {
			return fmt.Errorf("job %s holds source attempt %q of no artifact or interrupted launch, or twice", j.Job.ID, s.ID)
		}
		sources[s.ID] = true
	}
	if len(providers) != len(want) {
		return fmt.Errorf("job %s holds %d provider attempts, want %d", j.Job.ID, len(providers), len(want))
	}
	return nil
}

// e2eAttemptSetVerdict applies e2eAttemptSet to every job, with the launches
// of the extra session hashes as the only interrupted launches.
func e2eAttemptSetVerdict(units []e2eNativeUnit, jobs []*r.DurableJob, extra ...string) e2eVerdict {
	interrupted := []e2eNativeUnit{}
	for _, u := range units {
		if slices.Contains(extra, u.SessionHash) && !u.Complete {
			interrupted = append(interrupted, u)
		}
	}
	problems := []string{}
	for _, j := range jobs {
		if j == nil {
			problems = append(problems, "no job")
		} else if err := e2eAttemptSet(j, interrupted); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return e2eVerdict{len(jobs) > 0 && len(problems) == 0, fmt.Sprintf("each of %d jobs holds exactly the provider and source attempts of its artifacts and allowed interrupted launches: %v", len(jobs), problems)}
}

// e2eRecordedEvidence returns the one provider attempt job j durably records
// with launch u's complete audit evidence: equal by digest (as production
// compares a cloned record), of u's session and provider, and, when u's
// receipt names an attempt, of that attempt ID. A launch without projected
// evidence matches nothing.
func e2eRecordedEvidence(j *r.DurableJob, u e2eNativeUnit) (r.ProviderAttempt, error) {
	if j == nil || u.EvidenceHash == "" {
		return r.ProviderAttempt{}, errors.New("no job or no audit evidence to compare")
	}
	found := []r.ProviderAttempt{}
	for _, a := range j.ProviderAttempts {
		if a.Evidence != nil && e2eEvidenceHash(a.Evidence) == u.EvidenceHash {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		return r.ProviderAttempt{}, fmt.Errorf("job %s records launch %s's evidence %d times, want once", j.Job.ID, u.Launch, len(found))
	}
	a := found[0]
	if a.ID == "" || (u.AttemptID != "" && a.ID != u.AttemptID) || a.Evidence.Access.SessionHash != u.SessionHash || string(a.Evidence.Policy.Provider) != u.Provider {
		return r.ProviderAttempt{}, fmt.Errorf("job %s records launch %s's evidence under another attempt, session or provider", j.Job.ID, u.Launch)
	}
	return a, nil
}

// e2eAttemptID is the durable attempt ID format (16 random bytes, hex).
var e2eAttemptID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// e2eEvidenceHash digests provider evidence as the service compares it: a
// copy whose route and header lists are non-nil, as cloneProviderEvidence
// stores them. Nil evidence has no hash.
func e2eEvidenceHash(e *r.ProviderEvidence) string {
	if e == nil {
		return ""
	}
	copy := *e
	copy.Policy.Routes = append([]r.EgressRoute{}, e.Policy.Routes...)
	copy.Policy.Headers = append([]string{}, e.Policy.Headers...)
	return r.Digest(copy)
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

// e2ePullID is the fake upstream's pull request ID of PR n.
func e2ePullID(n int) int64 { return int64(7000 + n) }

// addPR opens PR n. An automatic request needs creation at/after the epoch;
// a PR created before the epoch is reviewed only after an owner comment.
func (g *e2eGitHub) addPR(n int, author int64, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[n] = &e2ePR{Number: n, ID: e2ePullID(n), Author: author, Created: created.UTC().Truncate(time.Second), Head: g.commits[strconv.Itoa(n)], Base: g.baseSHA, MergeBase: g.baseSHA, Comments: []map[string]any{}, Reviews: []map[string]any{}, ReviewComments: map[int64][]map[string]any{}}
}

// addComment adds a comment to PR n and returns the ID the fake upstream
// assigned it.
func (g *e2eGitHub) addComment(n int, author int64, body string, at time.Time) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	at = at.UTC().Truncate(time.Second)
	issue := "https://api.github.com/repos/hemsoft-dev/set-it-free-loop/issues/" + strconv.Itoa(n)
	id := g.nextID.Add(1)
	g.prs[n].Comments = append(g.prs[n].Comments, map[string]any{"id": id, "user": map[string]any{"id": author}, "body": body, "issue_url": issue, "created_at": at, "updated_at": at})
	return id
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
	if !slices.Contains(e2eRoleRoutes[role], q.Method+" "+e2eRouteName(p)) {
		g.violation("%s credential used outside its production capability for %s %s", role, q.Method, p)
		return 403, map[string]any{"message": "outside the credential's capability"}
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
	State   r.DurableState
	Heads   map[int]string
	Base    string
	Release string
	// Config is the configuration the harness opened the store with, which
	// admission must persist at its initial version.
	Config    r.Config
	Source    e2eSourceBinding
	Posts     map[int]int
	Reviews   map[int][]map[string]any
	Records   []r.E2ESourceRecord
	Exports   []r.E2EExportRecord
	SourceErr error
	// Boot is the host's current boot ID, which every source record must
	// have been recorded in.
	Boot     string
	Units    []e2eNativeUnit
	Sessions []e2eModelSession
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
	c := e2eCompletion{State: e.state(), Heads: map[int]string{}, Base: e.github.baseSHA, Release: e.options.Current, Config: e.config, Source: e2eSourceBinding{e.result.ReleaseHash, e.result.SourceRuntimeHash, e.result.SourceStubHash}, Posts: summary.ReviewPosts, Reviews: summary.Reviews, Boot: e2eBootID(), Units: e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID()), Exchanges: summary.GitExchanges, Attached: e.attached}
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
	e.recordSource(c)
	return jobs
}

// recordSource records the source records and exports a judged completion
// read, when they were readable.
func (e *e2eEnv) recordSource(c e2eCompletion) {
	if c.SourceErr == nil {
		e.mu.Lock()
		e.result.SourceRecords = append(e.result.SourceRecords, c.Records)
		e.result.Exports = append(e.result.Exports, c.Exports)
		e.mu.Unlock()
	}
}

// e2eExpectedJob is the job admission (Store.Admit) must persist for PR pr
// from the retained request j names, restated from the harness's inputs
// rather than read back from j: the ID of a request with an origin, the owner
// as requester, the fake upstream's snapshot, the configuration the harness
// opened the store with at its initial version 1 (no case issues a configure
// command), the current release, and the retained request's complete origin,
// which the request verdicts hold to the fake upstream's PR and comment. Only
// the generation (production's per-PR observation counter, which the intent
// must repeat) and the packet (the source capture's output, whose digest the
// staged export must carry) are j's own. The origin is nil unless PR pr
// retains exactly one request of j's request ID and it carries an origin.
func e2eExpectedJob(d r.DurableState, pr int, j *r.DurableJob, snapshot r.Snapshot, config r.Config, release string) r.Job {
	var origin *r.RequestOrigin
	if p := d.PRs[e2eScopeKey(pr)]; p != nil {
		found := 0
		for _, q := range p.Requests {
			if q.ID == j.RequestID {
				found++
				origin = q.Origin
			}
		}
		if found != 1 {
			origin = nil
		}
	}
	if origin != nil {
		copied := *origin
		origin = &copied
	}
	return r.Job{Origin: origin, ID: fmt.Sprintf("request-%d-%d", pr, j.RequestID), RequesterID: r.OwnerID, Generation: j.Job.Generation, Snapshot: snapshot, Config: config, ConfigVersion: 1, Release: release, Packet: j.Job.Packet}
}

// e2eAsExpected is j with its job replaced by expect, so that every later
// expectation (sessions, reports, document, source binding) is derived from
// the harness's inputs, and every record bound to j's job must be bound to
// the expected job instead.
func e2eAsExpected(j *r.DurableJob, expect r.Job) *r.DurableJob {
	copied := *j
	copied.Job = expect
	return &copied
}

// e2eExpectedFor is e2eAsExpected of PR pr's job j under c's inputs.
func e2eExpectedFor(c e2eCompletion, pr int, j *r.DurableJob) *r.DurableJob {
	return e2eAsExpected(j, e2eExpectedJob(c.State, pr, j, e2eCompletionSnapshot(c, pr), c.Config, c.Release))
}

// e2eCompletionSnapshot is the fake upstream's snapshot of PR pr in c.
func e2eCompletionSnapshot(c e2eCompletion, pr int) r.Snapshot {
	return r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: pr, Head: c.Heads[pr], Base: c.Base, MergeBase: c.Base}
}

// e2eReviewedVerdicts judges one PR's completed review. The PR must have
// exactly one durable job, exactly the one admission derives from the
// harness's inputs (e2eExpectedJob): bound to its scope, the fake upstream's
// snapshot, the harness configuration at version 1, the current release and
// its retained request's complete origin, holding exactly the adversarial
// round set of artifacts produced for that expected job, each session checked once by its own
// provider's backend and each holding exactly the canned backend's report.
// Its sole confirmed intent must be bound to that job and to the one remote
// review, whose canonical document is the intent's, and that document must be
// the one rendered from the canned reports, not merely one the intent and the
// remote agree on. The job's own attempt bookkeeping must be a released
// completion's (e2eReleasedAttempts). It returns the PR's job, or nil when
// there is not exactly one.
func e2eReviewedVerdicts(c e2eCompletion, pr int) ([]e2eVerdict, *r.DurableJob) {
	j := e2eJobFor(c.State, pr)
	if j == nil {
		return []e2eVerdict{{false, fmt.Sprintf("PR %d has exactly one durable job", pr)}}, nil
	}
	want := e2eCompletionSnapshot(c, pr)
	expected := e2eExpectedFor(c, pr, j)
	expect := expected.Job
	rounds := e2eArtifactRounds(expected)
	checked := e2eCheckedSessions(c.Sessions, expected)
	v := []e2eVerdict{
		{j.State == "dual_complete", fmt.Sprintf("PR %d job %s reached dual_complete (got %s)", pr, j.Job.ID, j.State)},
		{c.Config.Mode == r.Adversarial && j.Checkpoint.ConsumedSessions == 4 && j.Checkpoint.JobHash == r.Digest(j.Job), fmt.Sprintf("PR %d adversarial engine checkpoint of its own job consumed exactly 4 sessions (consumed %d)", pr, j.Checkpoint.ConsumedSessions)},
		{c.Heads[pr] != "" && c.Base != "" && j.Job.Snapshot == want && j.Scope == e2eScope(pr) && j.Job.Release == c.Release, fmt.Sprintf("PR %d job binds its own scope, head, base and the current release", pr)},
		{expect.Origin != nil && j.Job.Origin != nil && r.Digest(*j.Job.Origin) == r.Digest(*expect.Origin), fmt.Sprintf("PR %d job carries exactly the complete origin of its retained request %d", pr, j.RequestID)},
		{j.Job.Config == c.Config && j.Job.ConfigVersion == 1, fmt.Sprintf("PR %d job holds exactly the configuration the harness opened the store with, at its initial version 1 (version %d)", pr, j.Job.ConfigVersion)},
		{r.Digest(j.Job) == r.Digest(expect), fmt.Sprintf("PR %d job is exactly the one admission derives from its retained request and the harness's inputs (ID %s, requester %d)", pr, j.Job.ID, j.Job.RequesterID)},
		{rounds == nil, fmt.Sprintf("PR %d stored exactly Codex R1, Claude R1, Codex R2 and Claude R2, each produced for its job: %v", pr, rounds)},
		{checked == nil, fmt.Sprintf("PR %d each artifact session was checked once by its own provider's backend for its job and round, and made exactly the fixture's exchanges: %v", pr, checked)},
		{e2eReleasedAttempts(j) == nil, fmt.Sprintf("PR %d job holds no lease, reservation or pending charge, both providers audited, and its last attempt is its Claude R2 artifact's: %v", pr, e2eReleasedAttempts(j))},
	}
	canned := e2eCannedArtifacts(expected)
	v = append(v, e2eVerdict{canned == nil, fmt.Sprintf("PR %d artifacts hold exactly the canned backend's reports for their phases: %v", pr, canned)})
	writes := e2eWritesFor(c.State, pr, "review")
	w := e2eSoleConfirmed(writes)
	v = append(v, e2eVerdict{w != nil, fmt.Sprintf("PR %d has exactly one review intent and it is confirmed (%d writes)", pr, len(writes))})
	if w != nil {
		v = append(v,
			e2eVerdict{w.Scope == j.Scope && w.RequestID == j.RequestID && w.Generation == j.Job.Generation && w.Snapshot == j.Job.Snapshot && w.JobHash == r.Digest(j.Job) && w.CheckpointHash == r.Digest(j.Checkpoint) && w.ReceiptID > 0, fmt.Sprintf("PR %d confirmed intent binds its job's request, generation, snapshot, job and checkpoint, and a receipt", pr)},
			e2eVerdict{w.Marker == "<!-- sfl-alpha:"+w.ID+" -->" && w.BodyHash == r.Digest(w.Document), fmt.Sprintf("PR %d confirmed intent's marker names its own ID and its body hash is its document's", pr)},
			e2eVerdict{len(w.Document.Inline) == 1 && w.Document.Inline[0].Path == "source.go" && w.Document.Inline[0].Line == 1, fmt.Sprintf("PR %d document carries exactly the source-backed inline finding at source.go:1", pr)})
		document := e2eCannedDocument(expect, w)
		v = append(v, e2eVerdict{document == nil, fmt.Sprintf("PR %d confirmed document is exactly the one rendered from the canned reports and states the canned finding: %v", pr, document)})
	}
	v = append(v, e2eVerdict{c.Posts[pr] == 1, fmt.Sprintf("PR %d received exactly one review POST (got %d)", pr, c.Posts[pr])})
	reviews := c.Reviews[pr]
	v = append(v, e2eVerdict{len(reviews) == 1 && reviews[0]["state"] == "COMMENTED" && c.Heads[pr] != "" && reviews[0]["commit_id"] == c.Heads[pr] && reviews[0]["marker"] != "", fmt.Sprintf("PR %d remote review is one marked COMMENT on its head", pr)})
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

// e2eReleasedAttempts requires completed job j's own attempt bookkeeping:
// its lease and deadline released, no reservation outstanding and no charge
// pending, charged on at least one distinct UTC day, created at a recorded
// time, Codex then Claude audited, and its last attempt, which production
// keeps after release, the Claude R2 phase's: that artifact's session and
// attempt, requiring provider evidence. A completed job carries no operator
// recovery receipt.
func e2eReleasedAttempts(j *r.DurableJob) error {
	if j.LeaseToken != "" || !j.LeaseUntil.IsZero() || j.Outstanding || j.AttemptChargeDay != "" || j.CreatedAt.IsZero() {
		return errors.New("lease, deadline, reservation or pending charge kept")
	}
	if len(j.ProviderRecoveries) != 0 {
		return fmt.Errorf("%d operator recovery receipts on a completed job", len(j.ProviderRecoveries))
	}
	days := map[string]bool{}
	for _, day := range j.ChargeDays {
		if _, err := time.Parse("2006-01-02", day); err != nil || days[day] {
			return fmt.Errorf("charge day %q invalid or repeated", day)
		}
		days[day] = true
	}
	if len(days) == 0 || !slices.Equal(j.AuditedProviders, []r.Provider{r.Codex, r.Claude}) {
		return fmt.Errorf("charge days %v, audited providers %v", j.ChargeDays, j.AuditedProviders)
	}
	for _, a := range j.Checkpoint.Artifacts {
		if a.Report.Provider == r.Claude && a.Report.Round == 2 {
			attempt, err := e2eArtifactAttempt(j, a)
			if err != nil || j.AttemptID != attempt || j.AttemptProvider != r.Claude || j.AttemptRound != 2 || j.AttemptInputHash != a.InputHash || !j.AttemptProviderEvidenceRequired {
				return fmt.Errorf("last attempt %q (%s R%d) is not the Claude R2 artifact's: %v", j.AttemptID, j.AttemptProvider, j.AttemptRound, err)
			}
			return nil
		}
	}
	return errors.New("no Claude R2 artifact")
}

// e2eAdversarialRounds is the exact artifact set of an adversarial review.
var e2eAdversarialRounds = map[string]int{"codex R1": 1, "claude R1": 1, "codex R2": 1, "claude R2": 1}

// e2eArtifactRounds requires j's checkpoint to hold exactly one artifact of
// each adversarial provider round, every one produced for j: its job hash,
// report job ID, snapshot hash and config hash are j's, its session is the
// one j derives for its phase (e2eSessionFor, so an R2 session froze exactly
// the stored R1 reports), and no two artifacts share a session.
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
		if session, err := e2eSessionFor(j, a.Report.Provider, a.Report.Round); err != nil || r.Digest(session) != a.InputHash {
			return fmt.Errorf("artifact %s R%d is not the session its job derives: %v", a.Report.Provider, a.Report.Round, err)
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
// (e2eFixtureExchanges) for the session its job derives for the phase. Every
// other check of j must have been held by a fault hook: an unheld check that
// produced no artifact is unexplained provider work.
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
			session, err := e2eSessionFor(j, a.Report.Provider, a.Report.Round)
			if err == nil && r.Digest(session) != a.InputHash {
				err = errors.New("artifact is not the session its job derives")
			}
			if err == nil {
				err = e2eFixtureExchanges(s.Counts, a, session, e2eFixtureFiles(j.Job.Snapshot.PR))
			}
			if err != nil {
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
// source.go must be among them. Before any of that is judged, the source
// receipt must pass production's source receipt validation for the
// artifact's session (E2EValidateSourceAccess: version, session, call, byte
// and read bounds, counter consistency and coverage records), and record no
// exhaustion or omission, which the canned phase never causes. The
// artifact's stopped provider receipt must also count exactly the inference
// and metadata requests the backend observed.
func e2eFixtureExchanges(counts f.Counts, a r.Artifact, session r.Session, files map[string]string) error {
	head := session.Snapshot.Head
	if counts.Inference != 3 || counts.Failures != 0 || counts.Telemetry != 0 {
		return fmt.Errorf("backend counted %d inferences, %d failures and %d telemetry requests, want 3, 0 and 0", counts.Inference, counts.Failures, counts.Telemetry)
	}
	if a.Report.Provider == r.Codex && (!counts.MetadataCaptured || counts.Negotiation != 0) {
		return fmt.Errorf("codex metadata captured %v with %d negotiations reaching the backend", counts.MetadataCaptured, counts.Negotiation)
	}
	if err := r.E2EValidateSourceAccess(a.SourceAccess, session); err != nil {
		return fmt.Errorf("source receipt: %v", err)
	}
	if a.SourceAccess.Exhausted || len(a.SourceAccess.Coverage) != 0 {
		return errors.New("source receipt records exhaustion or an omission")
	}
	if a.SourceAccess.ToolCalls != 2 {
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

// The canned backend's one synthetic finding (nativefixture sourceReport).
const (
	e2eCannedTitle    = "Synthetic finding"
	e2eCannedBody     = "Protocol fixture only."
	e2eCannedSeverity = 2
	e2eCannedReason   = "Synthetic source-backed protocol disposition."
)

// e2eCannedAnchor is the canned finding's anchor at head: source.go line 1,
// quoting that line of the fake upstream's source.go.
func e2eCannedAnchor(head string) r.Anchor {
	return r.Anchor{Path: "source.go", Line: 1, Quote: strings.TrimSpace(e2eSource), Revision: head}
}

// e2eCannedFinding is provider's canned R1 finding at head.
func e2eCannedFinding(provider r.Provider, head string) r.Finding {
	return r.Finding{ID: string(provider) + ":fixture", Title: e2eCannedTitle, Body: e2eCannedBody, Severity: e2eCannedSeverity, Anchor: e2eCannedAnchor(head)}
}

// e2eCannedReports is, in phase order, every report the canned backends
// answer for an adversarial job, restated from the fixture rather than read
// back from the run: in R1 each provider proposes its one synthetic finding
// at source.go:1 and nothing else; in R2 each confirms its peer's R1 finding,
// with that finding's anchor as evidence, and proposes nothing. Every report
// carries the job's immutable headers. Usage is provider accounting, never
// published, and is not part of the canned content.
func e2eCannedReports(job r.Job) []r.Report {
	head := job.Snapshot.Head
	reports := []r.Report{}
	for round := 1; round <= 2; round++ {
		for _, provider := range []r.Provider{r.Codex, r.Claude} {
			report := r.Report{JobID: job.ID, PolicyVersion: r.PolicyVersion, PacketHash: r.Digest(job.Packet), Provider: provider, Round: round, SnapshotHash: r.Digest(job.Snapshot), ConfigHash: job.Config.Hash(), Findings: []r.Finding{}, Judgments: []r.Judgment{}, Retractions: []string{}, Coverage: []r.Coverage{}}
			if round == 1 {
				report.Findings = append(report.Findings, e2eCannedFinding(provider, head))
			} else {
				peer := r.Claude
				if provider == r.Claude {
					peer = r.Codex
				}
				report.Judgments = append(report.Judgments, r.Judgment{FindingID: string(peer) + ":fixture", Verdict: "confirm", Reason: e2eCannedReason, Evidence: []r.Anchor{e2eCannedAnchor(head)}})
			}
			reports = append(reports, report)
		}
	}
	return reports
}

// e2eCannedLedger is the engine's documented adversarial aggregation of
// e2eCannedReports, written out rather than recomputed: each provider's
// finding is corroborated by its peer's R2 confirmation, keeps its own
// severity, and is linked to the other provider's exact duplicate. Entries
// are ordered by severity, then finding ID.
func e2eCannedLedger(job r.Job) []r.Entry {
	head := job.Snapshot.Head
	entry := func(origin, peer r.Provider) r.Entry {
		confirm := r.Judgment{FindingID: string(origin) + ":fixture", Verdict: "confirm", Reason: e2eCannedReason, Evidence: []r.Anchor{e2eCannedAnchor(head)}}
		return r.Entry{Finding: e2eCannedFinding(origin, head), Origin: origin, RaisedRound: 1, Status: "corroborated", History: []r.Assessment{{Provider: peer, Round: 2, Judgment: confirm}}, RelatedIDs: []string{string(peer) + ":fixture"}, SeverityMin: e2eCannedSeverity, SeverityMax: e2eCannedSeverity}
	}
	return []r.Entry{entry(r.Claude, r.Codex), entry(r.Codex, r.Claude)}
}

// e2eFixtureDiff is the trusted diff of PR pr against the fake upstream's
// base: the PR adds source.go and its own one-line file.
func e2eFixtureDiff(pr int) map[string]map[int]bool {
	return map[string]map[int]bool{"source.go": {1: true}, fmt.Sprintf("pr%d-only.go", pr): {1: true}}
}

// e2eCannedArtifacts requires every artifact of j to hold exactly the canned
// backend's report for its provider and round (all findings, judgments,
// retractions, coverage and immutable headers; Usage aside), no engine-added
// coverage, and exactly the engine's one validation read of the canned
// anchor's file: source.go at the head with the fake upstream's bytes, so a
// decoding or merge change cannot alter what the document is built from.
func e2eCannedArtifacts(j *r.DurableJob) error {
	canned := map[string]r.Report{}
	for _, report := range e2eCannedReports(j.Job) {
		canned[fmt.Sprintf("%s R%d", report.Provider, report.Round)] = report
	}
	for _, a := range j.Checkpoint.Artifacts {
		key := fmt.Sprintf("%s R%d", a.Report.Provider, a.Report.Round)
		want, ok := canned[key]
		got := a.Report
		got.Usage = r.Usage{}
		if !ok || r.Digest(got) != r.Digest(want) || len(a.Coverage) != 0 {
			return fmt.Errorf("artifact %s is not the canned backend's report", key)
		}
		read := []r.ReadRecord{{Revision: j.Job.Snapshot.Head, Path: "source.go", Hash: r.Digest([]byte(e2eSource)), Bytes: len(e2eSource)}}
		if !slices.Equal(a.ValidationReads, read) {
			return fmt.Errorf("artifact %s validation reads are not the canned anchor's one read", key)
		}
	}
	return nil
}

// e2eCannedDocument requires the confirmed intent w to carry exactly the
// document production renders for the expected job (e2eExpectedJob, not the
// stored job, so a wrong trigger origin or configuration in the stored job
// cannot shape its own expectation) from the canned reports and their
// expected ledger under the PR's trusted diff, with w's marker. Because that
// render is production's own, the published inline comment must also state
// the canned finding literally: its severity, title, body and source link at
// the head.
func e2eCannedDocument(job r.Job, w *r.WriteIntent) error {
	want, err := r.E2ECompletedDocument(job, e2eCannedReports(job), e2eCannedLedger(job), e2eFixtureDiff(job.Snapshot.PR), w.Marker)
	if err != nil {
		return fmt.Errorf("expected document: %v", err)
	}
	if r.Digest(w.Document) != r.Digest(want) {
		return errors.New("document differs from the one rendered from the canned reports")
	}
	stated := fmt.Sprintf("P%d %s\n\n%s\n\nSource: [source.go:1](https://github.com/hemsoft-dev/set-it-free-loop/blob/%s/source.go#L1).", e2eCannedSeverity, e2eCannedTitle, e2eCannedBody, job.Snapshot.Head)
	if len(w.Document.Inline) != 1 || !strings.Contains(w.Document.Inline[0].Body, stated) || !strings.Contains(w.Document.Summary, stated) {
		return errors.New("document does not state the canned finding")
	}
	return nil
}

// e2eDurableSetVerdicts requires the durable state to hold exactly one job
// and exactly one review intent for each of prs, and no other job or write
// of any kind. A further job or intent could still run or publish later.
func e2eDurableSetVerdicts(label string, d r.DurableState, prs ...int) []e2eVerdict {
	want := map[int]int{}
	for _, pr := range prs {
		want[pr] = 1
	}
	jobs, writes, other := e2eDurableCounts(d)
	return []e2eVerdict{
		{maps.Equal(jobs, want), fmt.Sprintf("%s: durable state holds exactly one job for each of PRs %v and no other job (%v)", label, prs, jobs)},
		{maps.Equal(writes, want) && other == 0, fmt.Sprintf("%s: durable state holds exactly one review intent for each of PRs %v and no other write (%v, %d other)", label, prs, writes, other)},
	}
}

// e2eDurableCountVerdicts requires the durable state to hold exactly
// jobs[pr] jobs and intents[pr] review intents for each PR, none for any PR
// either map omits, and no write of another kind. A case whose PRs did not
// all publish states its exact counts: an extra job or intent could still
// run or publish later.
func e2eDurableCountVerdicts(label string, d r.DurableState, jobs, intents map[int]int) []e2eVerdict {
	gotJobs, gotIntents, other := e2eDurableCounts(d)
	return []e2eVerdict{
		{maps.Equal(gotJobs, jobs), fmt.Sprintf("%s: durable state holds exactly the jobs %v per PR and no other job (%v)", label, jobs, gotJobs)},
		{maps.Equal(gotIntents, intents) && other == 0, fmt.Sprintf("%s: durable state holds exactly the review intents %v per PR and no other write (%v, %d other)", label, intents, gotIntents, other)},
	}
}

// e2eDurableCounts counts the durable jobs and review intents of each PR, and
// the writes of any other kind.
func e2eDurableCounts(d r.DurableState) (jobs, intents map[int]int, other int) {
	jobs, intents = map[int]int{}, map[int]int{}
	for _, j := range d.Jobs {
		jobs[j.Scope.PR]++
	}
	for _, w := range d.Writes {
		if w.Kind == "review" {
			intents[w.Scope.PR]++
		} else {
			other++
		}
	}
	return jobs, intents, other
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
// exactly one of jobs, each job's once. As restore requires, the export's
// staged packet hash must be the digest of that job's packet.
func e2eSourceEvidence(records []r.E2ESourceRecord, exports []r.E2EExportRecord, jobs []*r.DurableJob, bind e2eSourceBinding) error {
	want := len(jobs)
	if want <= 0 || len(records) != want || len(exports) != want {
		return fmt.Errorf("%d records and %d exports, want %d of each", len(records), len(exports), want)
	}
	if bind.Release == "" || bind.Runtime == "" || bind.Stub == "" {
		return errors.New("no current release binding to compare")
	}
	owners, packets := map[r.Snapshot]int{}, map[r.Snapshot]string{}
	for _, j := range jobs {
		if j == nil {
			return errors.New("a source record has no job to bind")
		}
		owners[j.Job.Snapshot]++
		packets[j.Job.Snapshot] = r.Digest(j.Job.Packet)
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
		if x.PacketHash != packets[rec.Snapshot] {
			return fmt.Errorf("source attempt %s export materialized another packet than its job's", rec.Attempt)
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
	e.recordSource(c)
}

// e2eSourceVerdicts requires one source attempt per job, bound to it and the
// current release, each with a staged export of its own and a final journal
// record holding the start intent, settled start, capture, grant, revocation
// and durable stop. That stop is the captured unit's own: the record was made
// in this boot, and StopBoot, which only a later boot's observation of an
// uncaptured instance sets, is empty. A process that acquired the attempts also requires the
// order (e2eSourceOrderVerdicts); an attached restart cannot observe the
// earlier process's acquisition, whose order that process judged.
func e2eSourceVerdicts(label string, c e2eCompletion, jobs []*r.DurableJob) []e2eVerdict {
	if c.SourceErr != nil {
		return []e2eVerdict{{false, fmt.Sprintf("%s: source records readable: %v", label, c.SourceErr)}}
	}
	err := e2eSourceEvidence(c.Records, c.Exports, jobs, c.Source)
	v := []e2eVerdict{{err == nil, fmt.Sprintf("%s: %d source records with matching staged-export entries: %v", label, len(jobs), err)}}
	for _, rec := range c.Records {
		v = append(v, e2eVerdict{rec.StartIntent && rec.StartSettled && rec.Captured && rec.Stopped && rec.Revoked && rec.GrantHash != "" && rec.StopBoot == "" && c.Boot != "" && rec.BootID == c.Boot, fmt.Sprintf("%s: source attempt %s (unit %s) final record holds its start intent, settled start, capture, grant, revocation and durable stop of its captured unit in this boot, not a later boot's proof", label, rec.Attempt, rec.Unit)})
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
// production-whole stop, and no other launch except one interrupted launch
// per listed extra session hash. It keys each artifact session to the
// artifact, its job and the session the job derives for its phase. An
// artifact counts only when it is that derived session and its job records
// its provider evidence and source access under one attempt
// (e2eArtifactAttempt); a launch counts only as its artifact's provider with
// the same evidence, its version 2 receipt must name the attempt that
// recorded it, and its audit must record that phase's request, stored report
// and source receipt (e2eLaunchAudit). An allowed extra launch must be
// stopped, audited, not complete and bound to the one attempt of a job that
// records its evidence (e2eInterruptedLaunch), which its receipt names and no
// artifact used; pass the jobs that own extra launches. An attempt ID that
// the jobs record more than once matches no launch. No job or artifact passes
// vacuously.
func e2eNativeSessionVerdict(units []e2eNativeUnit, jobs []*r.DurableJob, extra ...string) e2eVerdict {
	type phase struct {
		job      *r.DurableJob
		artifact r.Artifact
		session  r.Session
		attempt  string
	}
	expected := map[string]phase{}
	recorded, used := map[string]int{}, map[string]bool{}
	artifacts, unbound := 0, 0
	for _, j := range jobs {
		if j == nil {
			continue
		}
		for _, p := range j.ProviderAttempts {
			recorded[p.ID]++
		}
	}
	for _, j := range jobs {
		if j == nil {
			unbound++
			continue
		}
		for _, a := range j.Checkpoint.Artifacts {
			artifacts++
			session, err := e2eSessionFor(j, a.Report.Provider, a.Report.Round)
			attempt, attemptErr := e2eArtifactAttempt(j, a)
			if attemptErr != nil || err != nil || r.Digest(session) != a.InputHash {
				unbound++
			} else {
				used[attempt] = true
			}
			expected[a.InputHash] = phase{j, a, session, attempt}
		}
	}
	allowed := map[string]int{}
	for _, hash := range extra {
		allowed[hash]++
	}
	interrupted := func(u e2eNativeUnit) bool {
		bound := 0
		for _, j := range jobs {
			if j == nil {
				continue
			}
			if _, err := e2eInterruptedLaunch(u, j); err == nil {
				bound++
			}
		}
		return bound == 1
	}
	seen := map[string]int{}
	unexpected := 0
	for _, u := range units {
		p, ok := expected[u.SessionHash]
		switch {
		case ok && p.artifact.ProviderEvidence != nil && p.attempt != "" && u.AttemptID == p.attempt && recorded[u.AttemptID] == 1 && u.Provider == string(p.artifact.Report.Provider) && u.EvidenceHash == e2eEvidenceHash(p.artifact.ProviderEvidence) && u.wholeStop() && u.AuditPresent && u.Complete && u.AuthExported && u.AuthReturned && e2eLaunchAudit(u, p.job, p.session, p.artifact.Report, p.artifact.SourceAccess) == nil:
			seen[u.SessionHash]++
		case allowed[u.SessionHash] > 0 && u.wholeStop() && u.AuditPresent && !u.Complete && u.AttemptID != "" && recorded[u.AttemptID] == 1 && !used[u.AttemptID] && interrupted(u):
			allowed[u.SessionHash]--
		default:
			unexpected++
		}
	}
	once := len(expected) > 0 && artifacts == len(expected) && unbound == 0 && len(seen) == len(expected)
	for _, n := range seen {
		once = once && n == 1
	}
	return e2eVerdict{once && unexpected == 0, fmt.Sprintf("each of %d job-bound sessions ran in exactly one completed native launch of its artifact's provider and evidence with auth exported and returned, whose receipt names its recorded attempt and whose audit records its request, report and source receipt, and no other launch exists (%d artifacts, %d unbound, %d matched, %d unexpected)", len(expected), artifacts, unbound, len(seen), unexpected)}
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
	verdicts, jobs, expected := e2eDurableSetVerdicts(label, c.State, prs...), []*r.DurableJob{}, []*r.DurableJob{}
	for _, pr := range prs {
		v, j := e2eReviewedVerdicts(c, pr)
		verdicts, jobs = append(verdicts, v...), append(jobs, j)
		if j != nil {
			expected = append(expected, e2eExpectedFor(c, pr, j))
		}
	}
	if slices.Contains(jobs, nil) {
		return verdicts, jobs
	}
	verdicts = append(verdicts, e2eSourceVerdicts(label, c, expected)...)
	return append(verdicts, e2eAttemptSetVerdict(c.Units, expected, extra...), e2eNativeSessionVerdict(c.Units, expected, extra...)), jobs
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
	comment := e.github.addComment(2, r.OwnerID, "@sfl review", e.github.epoch.Add(30*time.Second))
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
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eIsolationRequestVerdicts(d, j1, j2, comment) })
	// Admission: the host checks (session starts) of the two jobs never
	// interleave. That whole sessions do not overlap is the serial admission
	// rule (no job is selected while another holds its lease), not observed.
	// Sessions and source attempts bind each job's PR and the fake upstream's
	// head of that PR, not the head the job records.
	h1, h2 := e.github.commits["1"], e.github.commits["2"]
	sessions := append(e.models[r.Codex].sessionsCopy(), e.models[r.Claude].sessionsCopy()...)
	switches, unbound := e2eSessionsBound(sessions, e2eJob{ID: j1.Job.ID, PR: 1, Head: h1}, e2eJob{ID: j2.Job.ID, PR: 2, Head: h2})
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
	e.check(h1 != "" && h2 != "" && a1.Snapshot.Head == h1 && a2.Snapshot.Head == h2, "each source attempt binds its own PR's fake upstream head")
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
	// The recovered source attempt must bind the job admission derives from
	// the harness's inputs; assertCompleted then judges the job itself.
	if j != nil {
		j = e2eAsExpected(j, e2eExpectedJob(d, 1, j, e.github.snapshot(1), e.config, e.options.Current))
	}
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
	e.wedged = closeErr != nil
	e.awaitRun("close-provider", 10*time.Second)
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
	// Every later expectation of the interrupted job derives from the job
	// admission must have persisted from the harness's inputs, not from j.
	expected := e2eAsExpected(j, e2eExpectedJob(d, 1, j, e.github.snapshot(1), e.config, e.options.Current))
	e.check(r.Digest(j.Job) == r.Digest(expected.Job), "the interrupted job is exactly the one admission derives from its retained request, the fake upstream's snapshot, the harness configuration at version 1 and the current release")
	e.checkUploads(e.github.commits["1"])
	e.note("after provider-session close: job state %s, consumed %d, artifacts %d, lease %v, outstanding %v, provider attempts %d", j.State, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), j.LeaseToken != "", j.Outstanding, len(j.ProviderAttempts))
	e.result.Requests = e2eRequests(d)
	e.checkAll(e2eRequestVerdicts("after provider close", e.result.Requests, map[int]e2eRequest{1: e2eAdmittedOpened(1, j.RequestID)}))
	e.check(j.Checkpoint.ConsumedSessions == 1 && len(j.Checkpoint.Artifacts) == 0, "the interrupted session stays charged and produced no artifact")
	units := e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID())
	e.check(len(units) == 1 && units[0].wholeStop() && !units[0].Complete, "the interrupted native unit was captured and whole-unit stopped without completion")
	returned := len(units) == 1 && units[0].AuthReturned
	e.check(len(units) == 1 && units[0].AuditPresent, "the interrupted native unit has a well-formed stopped audit receipt")
	e.check(len(units) == 1 && units[0].AttemptID == j.AttemptID, "the interrupted launch's stopped receipt names the job's durable attempt")
	for _, u := range units {
		e.note("interrupted native unit %s: audit %v complete %v exported %v returned %v", u.Launch, u.AuditPresent, u.Complete, u.AuthExported, u.AuthReturned)
	}
	auth := map[r.Provider]string{r.Codex: e2eAuthDisposition(e.auth, r.Codex), r.Claude: e2eAuthDisposition(e.auth, r.Claude)}
	e.note("auth disposition after close: codex %s, claude %s", auth[r.Codex], auth[r.Claude])
	e.check(auth[r.Claude] == "usable", "the untouched provider's auth remains usable (%s)", auth[r.Claude])
	e.check((returned && auth[r.Codex] == "usable") || (!returned && auth[r.Codex] == "fenced"), "codex auth is usable exactly when its stopped return was committed, and fenced otherwise (returned %v, %s)", returned, auth[r.Codex])
	e.check(e.github.reviewPosts(1) == 0, "nothing was published for the interrupted job")
	if !e.wedged {
		e.check(j.LeaseToken == "" && j.State == "queued" && returned, "a completed Close leaves a queued job with returned auth (state %s)", j.State)
		// Not yet observed on a host: the stopped receipt the job records for
		// the interrupted attempt is the launch's audit evidence.
		if len(units) == 1 {
			_, err := e2eRecordedEvidence(expected, units[0])
			e.check(err == nil, "the job durably records the interrupted launch's complete stopped audit evidence: %v", err)
		}
		e.open("reopen")
		e.wait("PR 1 review after reopen", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 1) })
		e.snapshotJobs("after reopen")
		// The one allowed extra launch is the Codex R1 session the expected
		// job derives, not whichever session the interrupted launch recorded.
		interrupted := ""
		if session, err := e2eSessionFor(expected, r.Codex, 1); err == nil {
			interrupted = r.Digest(session)
		}
		e.check(interrupted != "" && len(units) == 1 && units[0].SessionHash == interrupted, "the interrupted launch ran the Codex R1 session the expected job derives")
		verdicts, reopened := e2eProviderReopenVerdicts(e.completion(), interrupted)
		e.checkAll(verdicts)
		// Not yet observed on a host: the interrupted launch plus one completed
		// launch per adversarial phase, restated rather than counted from the
		// reopened job's artifacts, still one acquisition, both auth stores
		// usable, and the review published.
		e.expect = e2eExpect{SourceUnits: 1, NativeUnits: 1 + len(e2eAdversarialRounds), AuthUsable: true, Routes: e2ePublishedRoutes}
		if reopened != nil {
			e.result.Jobs = append(e.result.Jobs, e2eJobSummary(reopened))
		}
		e.collectWrites()
		return
	}
	e.check(j.LeaseToken != "" && j.Outstanding && j.State == "running", "the uncertain provider attempt keeps its durable lease and outstanding reservation")
	e.check(!returned && auth[r.Codex] == "fenced", "the unreturned exported auth is fenced for recovery")
	e.check(len(units) == 1 && units[0].AuditPresent && units[0].AuthExported && !units[0].AuthReturned, "the stopped audit records the auth as exported and not returned")
	_, err := e2eRetainedLaunches(units, expected)
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
	comment := e.github.addComment(2, r.OwnerID, "@sfl review", time.Now().Add(-time.Second))
	before, err := e2eAttachedJob(e.reopenState(), e.options.Current, e.github.snapshot(1), e.config)
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted job:", err)
	}
	launches, err := e2eRetainedLaunches(e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID()), before)
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted launch and its stopped audit:", err)
	}
	e.nativeBefore = launches
	e.check(true, "the attached root holds the interrupted launch, captured, stopped and audited with exported unreturned auth, its receipt naming the leased attempt and repeating that attempt's recorded stopped provider evidence")
	codexFenced := e2eAuthDisposition(e.auth, r.Codex) == "fenced"
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
	e.check(e2eAuthDisposition(e.auth, r.Codex) == "fenced" && e2eAuthUsable(e.auth, r.Claude), "codex auth stays fenced; claude auth stays usable")
	// The fence holds every request back; none may have been duplicated
	// behind it, or releasing the lease could admit a second review.
	e.result.Requests = e2eRequests(d)
	e.checkAll(e2eRequestVerdicts("fenced after restart", e.result.Requests, map[int]e2eRequest{1: e2eAdmittedOpened(1, before.RequestID), 2: e2ePendingComment(2, comment)}))
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
// The job must be exactly the one admission derives from its retained request
// and the harness's inputs (e2eExpectedJob, under config), and is returned as
// that expected job, so the retained launch is judged against a session
// derived from the harness's inputs.
func e2eAttachedJob(d r.DurableState, current string, snapshot r.Snapshot, config r.Config) (*r.DurableJob, error) {
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
	expect := e2eExpectedJob(d, 1, j, snapshot, config, current)
	if r.Digest(j.Job) != r.Digest(expect) {
		return nil, fmt.Errorf("retained job %s is not the one admission derives from its retained request %d, the harness configuration at version 1 and the current release", j.Job.ID, j.RequestID)
	}
	return e2eAsExpected(j, expect), nil
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
// admitted request of j2 from the fake upstream's owner comment comment, and
// no PR holds any other request.
func e2eIsolationRequestVerdicts(d r.DurableState, j1, j2 *r.DurableJob, comment int64) []e2eVerdict {
	return e2eRequestVerdicts("after both reviews", e2eRequests(d), map[int]e2eRequest{1: e2eAdmittedOpened(1, e2eRequestOf(j1)), 2: e2eAdmittedComment(2, e2eRequestOf(j2), comment)})
}

// e2eRecoveredRequestVerdicts judges the requests close-provider-recover
// retains once PR 2 is reviewed: PR 1 holds exactly the admitted PR-open
// request of its recovered job j1, which recovery released but did not
// remove, PR 2 exactly the admitted request of j2 from the fake upstream's
// owner comment comment, and no PR holds any other request. A second request
// on either PR would be admitted once PR 2's job ends.
func e2eRecoveredRequestVerdicts(d r.DurableState, j1, j2 *r.DurableJob, comment int64) []e2eVerdict {
	return e2eRequestVerdicts("after recovery review", e2eRequests(d), map[int]e2eRequest{1: e2eAdmittedOpened(1, e2eRequestOf(j1)), 2: e2eAdmittedComment(2, e2eRequestOf(j2), comment)})
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

// e2eRequest is a nonsecret projection of one retained native request. Its
// origin's pull and comment IDs are published in the review's trigger line.
type e2eRequest struct {
	PR                int
	ID, ActorID       int64
	PullID, CommentID int64
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
				s.PullID, s.CommentID = q.Origin.PullID, q.Origin.CommentID
			}
			out = append(out, s)
		}
	}
	return out
}

// e2eAdmittedOpened is the automatic PR-open request of the fake upstream's
// owner-authored PR pr, admitted as the request of job request id.
func e2eAdmittedOpened(pr int, id int64) e2eRequest {
	return e2eRequest{PR: pr, ID: id, ActorID: r.OwnerID, PullID: e2ePullID(pr), Kind: "opened", ActorRole: "pr_author", Admitted: true}
}

// e2eAdmittedComment is the owner @sfl review request of the fake upstream's
// comment on PR pr, admitted as the request of job request id.
func e2eAdmittedComment(pr int, id, comment int64) e2eRequest {
	return e2eRequest{PR: pr, ID: id, ActorID: r.OwnerID, PullID: e2ePullID(pr), CommentID: comment, Kind: "comment", ActorRole: "commenter", Admitted: true}
}

// e2ePendingComment is the owner @sfl review request of the fake upstream's
// comment on PR pr, not yet admitted.
func e2ePendingComment(pr int, comment int64) e2eRequest {
	return e2eRequest{PR: pr, ActorID: r.OwnerID, PullID: e2ePullID(pr), CommentID: comment, Kind: "comment", ActorRole: "commenter"}
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

// close-provider-recover is a fresh process attached to the root retained by
// close-provider. Before Run, the operator lists the leased attempt, is
// refused for a wrong attempt and without consent to retire the lost auth,
// then recovers it. The job must end auth_failed and charged, never
// reviewed. Codex auth is fenced until recovery and retired after it: the
// store holds no state and no lease, as production's retirement leaves it.
// After the operator re-provisions it with exactly a fresh synthetic seed,
// which carries the original synthetic tokens, Run must capture, review and
// publish the next request (PR 2) with exactly one review POST and nothing
// for PR 1. finish then requires both stores usable: Codex holding the
// original synthetic tokens as PR 2's sessions returned the re-supplied
// seed, Claude its untouched original.
func e2eCloseProviderRecover(e *e2eEnv) {
	if !e.attached {
		e.t.Fatal("close-provider-recover requires SFL_E2E_ATTACH")
	}
	e.github.addPR(1, r.OwnerID, e.github.epoch.Add(time.Minute))
	e.github.addPR(2, 4242, e.github.epoch.Add(-time.Minute))
	comment := e.github.addComment(2, r.OwnerID, "@sfl review", time.Now().Add(-time.Second))
	retained := e.reopenState()
	before, err := e2eAttachedJob(retained, e.options.Current, e.github.snapshot(1), e.config)
	if err == nil && !before.Outstanding {
		err = fmt.Errorf("retained job %s has no outstanding attempt", before.Job.ID)
	}
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted job:", err)
	}
	e.check(true, "the attached root holds only PR 1's leased job with an outstanding attempt, exactly the one admission derives from its retained request, PR 1's fake upstream snapshot, the harness configuration at version 1 and the current release")
	e.checkAll(e2eDurableCountVerdicts("attached before recovery", retained, map[int]int{1: 1}, map[int]int{}))
	units := e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID())
	kept, err := e2eRetainedLaunches(units, before)
	if err != nil {
		e.t.Fatal("attached root does not hold the interrupted launch and its stopped audit:", err)
	}
	e.nativeBefore = kept
	interrupted := units[0]
	e.check(interrupted.Provider == string(r.Codex), "the attached root holds the one interrupted Codex launch, captured, stopped and audited with exported unreturned auth, its receipt naming the leased attempt and repeating that attempt's recorded stopped provider evidence")
	auth := map[r.Provider]string{r.Codex: e2eAuthDisposition(e.auth, r.Codex), r.Claude: e2eAuthDisposition(e.auth, r.Claude)}
	e.check(auth[r.Codex] == "fenced" && auth[r.Claude] == "usable", "codex auth is fenced and claude auth usable before recovery (codex %s, claude %s)", auth[r.Codex], auth[r.Claude])
	e.checkAll(e2eOpenedRequestVerdicts("attached before recovery", retained, before))
	e.openIdle("restart")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	candidates, err := e.service.ProviderRecoveryCandidates(ctx)
	// The operator view is exactly the leased attempt's: its job, attempt,
	// provider, round and session, outstanding with recorded stopped
	// evidence, and the one original launch whose receipt names it.
	candidate := r.ProviderRecoveryCandidate{JobID: before.Job.ID, AttemptID: before.AttemptID, Provider: before.AttemptProvider, Round: before.AttemptRound, SessionHash: before.AttemptInputHash, Outstanding: true, ProviderEvidenceRecorded: true, Launches: []string{interrupted.Launch}}
	e.check(err == nil && len(candidates) == 1 && r.Digest(candidates[0]) == r.Digest(candidate), "the operator view lists exactly the leased attempt and its one original launch (%v, %v)", candidates, err)
	request := r.ProviderRecoveryRequest{JobID: before.Job.ID, AttemptID: before.AttemptID, LaunchID: interrupted.Launch}
	wrong := request
	wrong.AttemptID = strings.Repeat("0", 32)
	wrong.RetireUnreturnedAuth = true
	_, err = e.service.RecoverStoppedProvider(ctx, wrong)
	e.check(errors.Is(err, r.ErrFenced), "recovery refuses a mismatched attempt identity: %v", err)
	_, err = e.service.RecoverStoppedProvider(ctx, request)
	e.check(errors.Is(err, r.ErrAuthRecoveryRequired), "recovery refuses lost exported auth without consent to retire it: %v", err)
	unchanged := e2eJobFor(e.state(), 1)
	codexAuth := e2eAuthDisposition(e.auth, r.Codex)
	e.check(unchanged.LeaseToken == before.LeaseToken && unchanged.Outstanding && unchanged.State == "running" && len(unchanged.ProviderRecoveries) == 0 && codexAuth == "fenced", "refused recoveries changed neither the lease nor the auth fence (codex %s)", codexAuth)
	request.RetireUnreturnedAuth = true
	started := time.Now()
	result, err := e.service.RecoverStoppedProvider(ctx, request)
	e.event("recovery", "RecoverStoppedProvider returned %+v, %v in %s", result, err, time.Since(started).Round(time.Millisecond))
	e.check(err == nil && result.Auth == r.ProviderAuthRetired && result.State == "auth_failed" && result.Provider == r.Codex, "operator recovery retired the lost auth and released the lease as auth_failed (%+v, %v)", result, err)
	_, err = e.service.RecoverStoppedProvider(ctx, request)
	e.check(errors.Is(err, r.ErrFenced), "a repeated recovery is refused: %v", err)
	recoveredState := e.state()
	e.checkAll(e2eDurableCountVerdicts("after recovery", recoveredState, map[int]int{1: 1}, map[int]int{}))
	recovered, _ := e2eRecoveredJobVerdicts("after recovery", recoveredState, before, interrupted, e.github.snapshot(1), e.config, e.options.Current)
	e.checkAll(recovered)
	// Production retirement removes the lost auth's state and lease, so the
	// store is neither fenced nor holding any credential, original or not.
	auth = map[r.Provider]string{r.Codex: e2eAuthDisposition(e.auth, r.Codex), r.Claude: e2eAuthDisposition(e.auth, r.Claude)}
	e.note("auth disposition after recovery: codex %s, claude %s", auth[r.Codex], auth[r.Claude])
	e.check(auth[r.Codex] == "retired" && auth[r.Claude] == "usable", "the lost codex auth is retired, holding no state and no lease, never reused; claude auth is untouched and usable (codex %s, claude %s)", auth[r.Codex], auth[r.Claude])
	// Recovery releases the job, not its request: PR 1's request stays
	// admitted to the auth_failed job, so it can never be admitted again.
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eOpenedRequestVerdicts("after recovery", d, before) })
	observed, listing := e.units.observed()
	launches := len(e2eReadNativeUnits(e.native, e.releases, e.config.Limits, e2eBootID()))
	e.check(listing == nil && observed == 0 && launches == 1, "startup and recovery started no source or native unit (%d observed, %d launches, listing %v)", observed, launches, listing)
	// The operator re-establishes synthetic Codex auth outside the reviewer.
	// The re-supplied seed is a fresh synthetic profile, not the retired
	// state, but it carries the original synthetic tokens and chatgpt mode,
	// the only credential the canned Codex backend accepts. The store must
	// then hold exactly the re-supplied bytes.
	codex, err := r.NewAuthStore(r.Codex, e.auth)
	supplied := false
	if err == nil {
		seed := f.Seed(r.Codex)
		resupplied := r.Digest(seed)
		supplied = e2eSyntheticAuth(r.Codex, seed)
		err = codex.Refresh(context.Background(), func(old []byte) ([]byte, error) {
			if len(old) != 0 {
				return nil, errors.New("retired auth still present")
			}
			return seed, nil
		})
		if err == nil {
			err = codex.Refresh(context.Background(), func(current []byte) ([]byte, error) {
				if r.Digest(current) != resupplied {
					return nil, errors.New("codex auth is not the re-supplied seed")
				}
				return current, nil
			})
		}
		clear(seed)
	}
	codexAuth = e2eAuthDisposition(e.auth, r.Codex)
	e.check(err == nil && supplied && codexAuth == "usable", "operator re-provisioned the retired codex store with exactly the re-supplied synthetic seed, carrying the original synthetic tokens (%s): %v", codexAuth, err)
	e.startRun("after recovery")
	// Once Run has admitted PR 2, the service is certainly running.
	if e.wait("PR 2 admitted after recovery", 3*time.Minute, func(d r.DurableState) bool { return e2eJobFor(d, 2) != nil }) {
		_, err = e.service.RecoverStoppedProvider(ctx, request)
		e.check(errors.Is(err, r.ErrLiveBlocked), "recovery is refused while the service runs: %v", err)
	}
	if !e.wait("PR 2 review confirmed after recovery", 6*time.Minute, func(d r.DurableState) bool { return e2eConfirmed(d, 2) }) {
		e.snapshotJobs("timeout")
		return
	}
	e.snapshotJobs("confirmed")
	c := e.completion()
	verdicts, j1, j2 := e2eRecoveredVerdicts(c, before, interrupted)
	e.checkAll(verdicts)
	e.recordSource(c)
	e.checkRequests(func(d r.DurableState) []e2eVerdict { return e2eRecoveredRequestVerdicts(d, j1, j2, comment) })
	posts := map[string]int{}
	for key, n := range e.github.summary().Requests {
		if strings.Contains(key, " POST ") {
			posts[key] = n
		}
	}
	e.check(len(posts) == 2 && posts["app POST /app/installations/N/access_tokens"] == 2 && posts["publisher POST /pulls/N/reviews"] == 1, "the only GitHub POSTs are two role token mints and one review (%v)", posts)
	// PR 2's acquisition is the only upstream exchange: the retained PR 1
	// attempt was captured by close-provider and is never fetched again.
	e.checkUploads(e.github.commits["2"])
	if j1 != nil && j2 != nil {
		e.result.Jobs = append(e.result.Jobs, e2eJobSummary(j1), e2eJobSummary(j2))
	}
	e.collectWrites()
}

// e2eRecoveredJobVerdicts judges PR 1's job after operator recovery against
// the retained job before it, the interrupted launch and the harness's
// inputs: PR 1's fake upstream snapshot, the configuration the harness opened
// the store with and the current release. It must be the sole PR 1 job, the
// retained job itself (same ID, job, scope and request), and exactly the job
// admission derives from its retained request and those inputs
// (e2eExpectedJob), from which every derived expectation (the attempt's
// session, the launch's request and its evidence) is taken. It must be
// released as auth_failed with no lease or outstanding attempt, still charged
// for its one interrupted session, with no artifact and no source access. It
// must record exactly one provider attempt, the leased one, holding the
// interrupted launch's complete stopped evidence, and carry exactly one
// recovery receipt of that attempt, round, launch and evidence digest, with
// the lost Codex auth retired, recorded at a recovery time. As
// releaseRecoveredProviderAttempt changes nothing else, the job must equal
// the retained job, holding the expected job, with only its state, lease,
// deadline, reservation and pending charge day released and that one receipt
// appended, so its attempt
// set is exactly the leased attempt (e2eAttemptSet) with no source attempt.
// The interrupted launch's audit must still bind that attempt
// (e2eInterruptedLaunch): its derived request, the zero report, no source
// receipt, the wait for its result record as its final stage and the
// auth-return failure. It returns the job, or nil when PR 1 has not exactly
// one.
func e2eRecoveredJobVerdicts(label string, d r.DurableState, before *r.DurableJob, interrupted e2eNativeUnit, snapshot r.Snapshot, config r.Config, release string) ([]e2eVerdict, *r.DurableJob) {
	j := e2eJobFor(d, 1)
	if j == nil || before == nil {
		return []e2eVerdict{{false, fmt.Sprintf("%s: PR 1 has exactly one durable job beside its retained one", label)}}, nil
	}
	expect := e2eExpectedJob(d, 1, j, snapshot, config, release)
	expected := e2eAsExpected(j, expect)
	recorded, err := e2eRecordedEvidence(expected, interrupted)
	var receipt r.ProviderRecoveryReceipt
	if len(j.ProviderRecoveries) == 1 {
		receipt = j.ProviderRecoveries[0]
	}
	want := *before
	want.Job = expect
	want.State, want.LeaseToken, want.LeaseUntil, want.Outstanding, want.AttemptChargeDay = "auth_failed", "", time.Time{}, false, ""
	want.ProviderRecoveries = append(slices.Clone(before.ProviderRecoveries), receipt)
	set := e2eAttemptSet(expected, []e2eNativeUnit{interrupted})
	bound, boundErr := e2eInterruptedLaunch(interrupted, expected)
	audit := interrupted.Audit != nil && interrupted.Audit.Stage == "result" && interrupted.Audit.Failure == "auth-return" && interrupted.Audit.ReportHash == r.Digest(r.Report{}) && interrupted.Audit.SourceHash == r.Digest((*r.SourceAccess)(nil)) && interrupted.Audit.AttemptID == before.AttemptID
	return []e2eVerdict{
		{j.Job.ID == before.Job.ID && r.Digest(j.Job) == r.Digest(before.Job) && j.Scope == before.Scope && j.RequestID == before.RequestID, fmt.Sprintf("%s: PR 1's job is the retained job %s, unchanged in identity, snapshot, release and request", label, before.Job.ID)},
		{expect.Origin != nil && j.Job.Origin != nil && r.Digest(*j.Job.Origin) == r.Digest(*expect.Origin) && j.Job.Config == config && j.Job.ConfigVersion == 1 && r.Digest(j.Job) == r.Digest(expect), fmt.Sprintf("%s: PR 1's recovered job is exactly the one admission derives from its retained request %d, the fake upstream's snapshot, the harness configuration at version 1 and the current release (ID %s, requester %d, version %d)", label, j.RequestID, j.Job.ID, j.Job.RequesterID, j.Job.ConfigVersion)},
		{j.State == "auth_failed" && j.LeaseToken == "" && !j.Outstanding, fmt.Sprintf("%s: PR 1's job was released as auth_failed with no lease or outstanding attempt (state %s)", label, j.State)},
		{before.Checkpoint.ConsumedSessions == 1 && j.Checkpoint.ConsumedSessions == 1 && len(j.Checkpoint.Artifacts) == 0 && len(j.SourceAttempts) == 0, fmt.Sprintf("%s: PR 1's job stays charged for its one interrupted session, with no artifact and no source access (consumed %d, %d artifacts, %d source attempts)", label, j.Checkpoint.ConsumedSessions, len(j.Checkpoint.Artifacts), len(j.SourceAttempts))},
		{len(j.ProviderAttempts) == 1 && err == nil && before.AttemptID != "" && recorded.ID == before.AttemptID && interrupted.AttemptID == before.AttemptID, fmt.Sprintf("%s: PR 1's job records only the leased attempt, with the interrupted launch's complete stopped evidence (%d attempts): %v", label, len(j.ProviderAttempts), err)},
		{len(j.ProviderRecoveries) == 1 && receipt.AttemptID == before.AttemptID && receipt.LaunchID == interrupted.Launch && receipt.Provider == r.Codex && receipt.Round == before.AttemptRound && interrupted.EvidenceHash != "" && receipt.EvidenceHash == interrupted.EvidenceHash && receipt.Auth == r.ProviderAuthRetired && receipt.State == "auth_failed" && !receipt.RecoveredAt.IsZero() && !receipt.RecoveredAt.Before(before.CreatedAt), fmt.Sprintf("%s: PR 1's job carries one recovery receipt of the leased attempt, round, interrupted launch and its evidence, with the lost auth retired, at its recovery time (%d receipts)", label, len(j.ProviderRecoveries))},
		{len(before.ProviderRecoveries) == 0 && r.Digest(j) == r.Digest(&want), fmt.Sprintf("%s: PR 1's job is the retained job holding the expected job, with only its state, lease, deadline, reservation and pending charge released and one recovery receipt appended", label)},
		{set == nil && len(j.ProviderAttempts) == 1 && len(j.SourceAttempts) == 0, fmt.Sprintf("%s: PR 1's job holds exactly the interrupted launch's attempt and no source attempt: %v", label, set)},
		{boundErr == nil && bound == before.AttemptID && audit, fmt.Sprintf("%s: the interrupted launch's audit binds PR 1's recovered attempt: its request, the zero report, no source receipt, stage result and the auth-return failure: %v", label, boundErr)},
	}, j
}

// e2eRecoveredVerdicts judges close-provider-recover's end state as every
// completed case is judged, with PR 1 recovered instead of reviewed. Both
// jobs are held to the jobs admission derives from the harness's inputs
// (e2eExpectedJob): PR 1's from its retained open request, PR 2's from the
// fake upstream's owner comment, and every derived expectation (sessions,
// source records, launches) is taken from those expected jobs. Exactly
// one job per PR, no PR 1 intent and one PR 2 intent, and no other write;
// PR 1's recovered job (e2eRecoveredJobVerdicts) with no review POST or remote
// review; PR 2's full review (e2eReviewedVerdicts), whose four sessions must
// each make exactly the fixture's exchanges and be served only the fake
// upstream's bytes; one stopped source record per job, PR 1's retained one
// and PR 2's, each bound to its own job's snapshot, the current release and
// the release's source stub; the interrupted launch still present with its
// unchanged receipt; and PR 2's four completed launches plus that interrupted
// launch, whose receipt must name PR 1's recovered attempt, as the only
// entries of the attached native roots.
//
// This attached process acquired only PR 2's source attempt, so only that
// attempt's lifecycle order is judged here, from the journal sampled at its
// upload-pack; PR 1's retained attempt was acquired, and its order judged,
// by close-provider, and is judged here by its final record only. PR 1's
// interrupted session is never run again: this process's backends must have
// checked exactly PR 2's artifact sessions, none held, and the interrupted
// session is judged by its retained stopped receipt, which must count its one
// held inference as forwarded and never answered. It returns the PR 1 and
// PR 2 jobs, nil for a PR without exactly one.
func e2eRecoveredVerdicts(c e2eCompletion, before *r.DurableJob, interrupted e2eNativeUnit) ([]e2eVerdict, *r.DurableJob, *r.DurableJob) {
	const label = "after recovery review"
	verdicts := e2eDurableCountVerdicts(label, c.State, map[int]int{1: 1, 2: 1}, map[int]int{2: 1})
	recovered, j1 := e2eRecoveredJobVerdicts(label, c.State, before, interrupted, e2eCompletionSnapshot(c, 1), c.Config, c.Release)
	reviewed, j2 := e2eReviewedVerdicts(c, 2)
	verdicts = append(append(verdicts, recovered...), reviewed...)
	verdicts = append(verdicts,
		e2eVerdict{c.Posts[1] == 0 && len(c.Reviews[1]) == 0, fmt.Sprintf("%s: PR 1 received no review POST and holds no remote review (%d POSTs, %d reviews)", label, c.Posts[1], len(c.Reviews[1]))},
		e2eVerdict{interrupted.Launch != "" && e2eLaunchUnchanged(c.Units, interrupted), fmt.Sprintf("%s: the interrupted launch %s is still present with its unchanged stopped receipt", label, interrupted.Launch)})
	held := interrupted.Access
	verdicts = append(verdicts, e2eVerdict{interrupted.AuditPresent && held.InferenceRequests == 1 && held.Failed && !held.Exhausted && held.Forwarded == held.Responses+1, fmt.Sprintf("%s: PR 1's interrupted session's stopped receipt counts exactly its one held inference, forwarded and never answered (%d inferences, %d forwarded, %d responses, failed %v)", label, held.InferenceRequests, held.Forwarded, held.Responses, held.Failed)})
	if j1 == nil || j2 == nil {
		return verdicts, j1, j2
	}
	e1, e2 := e2eExpectedFor(c, 1, j1), e2eExpectedFor(c, 2, j2)
	// The one allowed extra launch is the Codex R1 session PR 1's expected
	// job derives, not whichever session the interrupted launch recorded.
	extra := ""
	if session, err := e2eSessionFor(e1, r.Codex, 1); err == nil {
		extra = r.Digest(session)
	}
	verdicts = append(verdicts, e2eVerdict{extra != "" && interrupted.SessionHash == extra, fmt.Sprintf("%s: the interrupted launch ran the Codex R1 session PR 1's expected job derives", label)})
	others := 0
	for _, s := range c.Sessions {
		if s.JobID != e2.Job.ID || s.PR != 2 || s.HeldInCheck {
			others++
		}
	}
	verdicts = append(verdicts, e2eVerdict{len(c.Sessions) == len(j2.Checkpoint.Artifacts) && others == 0, fmt.Sprintf("%s: this process's backends checked only PR 2's %d artifact sessions, none held, and never ran PR 1's interrupted session again (%d checks, %d others)", label, len(j2.Checkpoint.Artifacts), len(c.Sessions), others)})
	// PR 2's job holds exactly its four artifact attempts: no interrupted
	// launch is its own, and no recovery receipt.
	set := e2eAttemptSet(e2, nil)
	verdicts = append(verdicts, e2eVerdict{set == nil && len(j2.ProviderAttempts) == 4 && len(j2.SourceAttempts) == 4 && len(j2.ProviderRecoveries) == 0, fmt.Sprintf("%s: PR 2's job holds exactly its four artifact attempts and no recovery receipt (%d provider, %d source attempts, %d receipts): %v", label, len(j2.ProviderAttempts), len(j2.SourceAttempts), len(j2.ProviderRecoveries), set)})
	// The final records of both attempts are judged as an attached restart's;
	// only PR 2's acquisition was sampled by this process.
	attached := c
	attached.Attached = true
	verdicts = append(verdicts, e2eSourceVerdicts(label, attached, []*r.DurableJob{e1, e2})...)
	if c.SourceErr == nil {
		own := []r.E2ESourceRecord{}
		for _, rec := range c.Records {
			if rec.Snapshot == e2.Job.Snapshot {
				own = append(own, rec)
			}
		}
		verdicts = append(verdicts, e2eSourceOrderVerdicts(label+" (PR 2's acquisition)", own, c.Exchanges)...)
	}
	jobs := []*r.DurableJob{e1, e2}
	return append(verdicts, e2eAttemptSetVerdict(c.Units, jobs, extra), e2eNativeSessionVerdict(c.Units, jobs, extra)), j1, j2
}

// e2eLaunchUnchanged reports whether units hold launch u exactly once, with
// the same projection and the same stopped audit.
func e2eLaunchUnchanged(units []e2eNativeUnit, u e2eNativeUnit) bool {
	found := 0
	for _, x := range units {
		if x.Launch == u.Launch {
			found++
			if r.Digest(x) != r.Digest(u) || r.Digest(x.Audit) != r.Digest(u.Audit) || r.Digest(x.Policy) != r.Digest(u.Policy) || x.Binding != u.Binding {
				return false
			}
		}
	}
	return found == 1
}

// e2eAuthDisposition judges provider's auth store under directory through a
// refresh that writes the held state back unchanged: "usable" when it holds
// the provider's original synthetic credential; "retired" when the unfenced
// store yields no state at all, which is exactly what production's
// AuthProfile.Retire leaves once operator recovery retires lost exported auth
// (no lease, profile or pending journal, and no state, so new auth must be
// established separately); "invalid" when it yields any other nonempty state,
// since the store itself accepts any nonempty bytes (e2eSyntheticAuth);
// "fenced" only when the store refuses the refresh with
// ErrAuthRecoveryRequired, as it does while an exported profile awaits
// recovery; "unavailable" when the store cannot be opened, or refuses the
// refresh before yielding state for any other reason (a malformed or unsafe
// state file, an invalid lock file, a filesystem error). Retired, invalid and
// unavailable auth are neither usable nor fenced.
func e2eAuthDisposition(directory string, provider r.Provider) string {
	auth, err := r.NewAuthStore(provider, directory)
	if err != nil {
		return "unavailable"
	}
	judged, empty := false, false
	err = auth.Refresh(context.Background(), func(current []byte) ([]byte, error) {
		judged, empty = true, len(current) == 0
		if len(current) == 0 || !e2eSyntheticAuth(provider, current) {
			return nil, errors.New("returned auth is not the provider's synthetic credential")
		}
		return current, nil
	})
	switch {
	case err == nil:
		return "usable"
	case judged && empty:
		return "retired"
	case judged:
		return "invalid"
	case errors.Is(err, r.ErrAuthRecoveryRequired):
		return "fenced"
	default:
		return "unavailable"
	}
}

// e2eAuthUsable reports whether provider's auth store is usable: unfenced
// and holding the provider's original synthetic credential.
func e2eAuthUsable(directory string, provider r.Provider) bool {
	return e2eAuthDisposition(directory, provider) == "usable"
}

// e2eSyntheticAuth reports whether raw is provider's synthetic seed
// credential (f.Seed) as the fake backends leave it, by the hostile fixture's
// rule (cmd/nativevmfixture syntheticTokensReturned): the original synthetic
// access and refresh tokens, and for Codex the chatgpt auth mode.
func e2eSyntheticAuth(provider r.Provider, raw []byte) bool {
	switch provider {
	case r.Codex:
		var profile struct {
			AuthMode string `json:"auth_mode"`
			Tokens   struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			} `json:"tokens"`
		}
		return json.Unmarshal(raw, &profile) == nil && profile.AuthMode == "chatgpt" && profile.Tokens.AccessToken == "sfl-fake-access" && profile.Tokens.RefreshToken == "sfl-fake-refresh"
	case r.Claude:
		var profile struct {
			OAuth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		return json.Unmarshal(raw, &profile) == nil && profile.OAuth.AccessToken == "sfl-fake-access" && profile.OAuth.RefreshToken == "sfl-fake-refresh"
	}
	return false
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
// including the outstanding attempt's stopped evidence, are unchanged. As
// ReleaseStopped changes nothing else, the released job must equal the
// retained job with only its state, lease, deadline, reservation and
// pending charge day replaced.
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
	want := *before
	want.State, want.LeaseToken, want.LeaseUntil, want.Outstanding, want.AttemptChargeDay = "auth_failed", "", time.Time{}, false, ""
	if r.Digest(a) != r.Digest(&want) {
		return fmt.Errorf("released job %s changed more than its state, lease and reservation", a.Job.ID)
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
		return &r.DurableJob{Job: r.Job{Snapshot: snapshot(pr), Packet: fmt.Sprintf("packet %d", pr)}, Scope: r.Scope{PR: pr}}
	}
	bind := e2eSourceBinding{r.Digest("release package"), r.Digest("source VM manifest"), r.Digest("source stub")}
	// Each attempt's source request hash derives from it, so distinct
	// attempts never share one; release and runtime hashes are the release's.
	hash := func(kind, attempt string) string { return r.Digest(kind + attempt) }
	rec := func(attempt string, pr int) r.E2ESourceRecord {
		return r.E2ESourceRecord{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", RequestHash: hash("unit", attempt), SourceRequestHash: hash("source", attempt), StubHash: bind.Stub, ReleaseHash: bind.Release, RuntimeHash: bind.Runtime, Snapshot: snapshot(pr), Stopped: true, Revoked: true, GrantHash: "grant"}
	}
	export := func(attempt string, pr int) r.E2EExportRecord {
		return r.E2EExportRecord{Attempt: attempt, State: "staged", RequestHash: hash("source", attempt), ReleaseHash: bind.Release, RuntimeHash: bind.Runtime, Snapshot: snapshot(pr), PacketHash: r.Digest(fmt.Sprintf("packet %d", pr))}
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
		// Round 10: restore admits a staged export only for the job whose
		// packet it materialized.
		"export of another packet":       pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.PacketHash = r.Digest("packet 2") })),
		"export without its packet hash": pair([]r.E2ESourceRecord{rec(a, 1)}, exportWith(func(x *r.E2EExportRecord) { x.PacketHash = "" })),
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
	res.GitHub.Requests = e2eRouteTally(e2ePublishedRoutes)
	if attempt != "" {
		unit := "sfl-alpha-" + attempt + ".service"
		res.SourceRecords = [][]r.E2ESourceRecord{{{Attempt: attempt, Unit: unit}}, {{Attempt: attempt, Unit: unit}}}
		res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: unit, Source: true})
	}
	for _, launch := range launches {
		res.NativeUnits = append(res.NativeUnits, e2eNativeUnit{Launch: launch, Started: true, StartSettled: true, Captured: true, Aborted: true, Stopped: true, AuditPresent: true})
		res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: "sfl-alpha-" + launch + ".service"})
	}
	return res
}

// e2eRouteTally is a fake GitHub request tally that served each of routes.
func e2eRouteTally(routes []string) map[string]int {
	tally := map[string]int{}
	for _, route := range routes {
		tally[route] += 2
	}
	return tally
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
	reopen.GitHub.Requests = e2eRouteTally(e2eQualifiedRoutes)
	reopen.SystemdUnits = nil
	reopen.SourceRecords = [][]r.E2ESourceRecord{{{Attempt: source, Unit: "sfl-alpha-" + source + ".service"}}}
	reopen.AuthRecoveryClear["codex"] = false
	if f := failures(reopen, e2eExpectations["close-provider-reopen"], map[string]bool{n1: true}, true); len(f) != 0 {
		t.Fatal("attached fenced restart refused:", f)
	}
	fenced := e2eUnitRun(source, n1)
	fenced.GitHub.Requests = e2eRouteTally(e2eQualifiedRoutes)
	fenced.AuthRecoveryClear["codex"] = false
	if f := failures(fenced, e2eExpectations["close-provider"], nil, false); len(f) != 0 {
		t.Fatal("fenced provider close refused:", f)
	}
	// recovered is close-provider-recover's end state: the retained PR 1
	// attempt and interrupted launch beside PR 2's own acquisition and four
	// launches, Codex auth re-provisioned and returned.
	interrupted, next := id("0"), id("b")
	recovered := func() *e2eResult {
		res := e2eUnitRun(next, n1, n2, n3, n4)
		retained := r.E2ESourceRecord{Attempt: source, Unit: "sfl-alpha-" + source + ".service"}
		for i := range res.SourceRecords {
			res.SourceRecords[i] = append([]r.E2ESourceRecord{retained}, res.SourceRecords[i]...)
		}
		res.NativeUnits = append([]e2eNativeUnit{{Launch: interrupted, Started: true, StartSettled: true, Captured: true, Aborted: true, Stopped: true, AuditPresent: true}}, res.NativeUnits...)
		return res
	}
	recovery, retainedLaunch := e2eExpectations["close-provider-recover"], map[string]bool{interrupted: true}
	if f := failures(recovered(), recovery, retainedLaunch, true); len(f) != 0 {
		t.Fatal("attached recovery and next review refused:", f)
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
		// Round 10: a native launch counts only as a production-whole stop.
		"native launch stopped without an abort":         {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.NativeUnits[1].Aborted = false }), review, nil, false},
		"native launch captured without a settled start": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.NativeUnits[3].StartSettled = false }), review, nil, false},
		// Round 11 finding 4: the fake GitHub served exactly the case's
		// per-role routes, so reader work done with the publisher credential
		// fails even where the publisher's own allowlist admits the route.
		"publisher polled pull requests":   {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.GitHub.Requests["publisher GET /pulls"] = 1 }), review, nil, false},
		"publisher read trigger comments":  {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.GitHub.Requests["publisher GET /issues/N/comments"] = 1 }), review, nil, false},
		"reader scanned published reviews": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.GitHub.Requests["reader GET /pulls/N/reviews"] = 1 }), review, nil, false},
		"review never posted": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) {
			delete(res.GitHub.Requests, "publisher POST /pulls/N/reviews")
		}), review, nil, false},
		"reader never polled": {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.GitHub.Requests["reader GET /pulls"] = 0 }), review, nil, false},
		"no request tally":    {mutate(e2eUnitRun(source, n1, n2, n3, n4), func(res *e2eResult) { res.GitHub.Requests = nil }), review, nil, false},
		"fenced provider published": {mutate(e2eUnitRun(source, n1), func(res *e2eResult) {
			res.AuthRecoveryClear["codex"] = false
		}), e2eExpectations["close-provider"], nil, false},
		"recovery launched a native unit": {mutate(recovered(), func(res *e2eResult) {
			res.NativeUnits = append(res.NativeUnits, e2eNativeUnit{Launch: id("5"), Started: true, StartSettled: true, Captured: true, Aborted: true, Stopped: true, AuditPresent: true})
			res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: "sfl-alpha-" + id("5") + ".service"})
		}), recovery, retainedLaunch, true},
		"recovery acquired source again": {mutate(recovered(), func(res *e2eResult) {
			again := r.E2ESourceRecord{Attempt: id("c"), Unit: "sfl-alpha-" + id("c") + ".service"}
			res.SourceRecords[len(res.SourceRecords)-1] = append(res.SourceRecords[len(res.SourceRecords)-1], again)
			res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: again.Unit, Source: true})
		}), recovery, retainedLaunch, true},
		"retained attempt observed in the recovery process": {mutate(recovered(), func(res *e2eResult) {
			res.SystemdUnits = append(res.SystemdUnits, e2eSystemdUnit{Name: "sfl-alpha-" + source + ".service", Source: true})
		}), recovery, retainedLaunch, true},
		"next request never acquired source": {mutate(recovered(), func(res *e2eResult) {
			res.SourceRecords = [][]r.E2ESourceRecord{res.SourceRecords[0][:1]}
			res.SystemdUnits = res.SystemdUnits[1:]
		}), recovery, retainedLaunch, true},
		"interrupted launch claimed by the recovery process": {recovered(), recovery, nil, true},
		"retained launch vanished after recovery": {mutate(recovered(), func(res *e2eResult) {
			res.NativeUnits = res.NativeUnits[1:]
		}), recovery, retainedLaunch, true},
		"retained launch stopped without an abort": {mutate(recovered(), func(res *e2eResult) {
			res.NativeUnits[0].Aborted = false
		}), recovery, retainedLaunch, true},
		"codex auth not re-provisioned after recovery": {mutate(recovered(), func(res *e2eResult) { res.AuthRecoveryClear["codex"] = false }), recovery, retainedLaunch, true},
		// Round 11 finding 4 applied to the recovery process: recovery uses
		// no GitHub route, so the process serves exactly the routes of an
		// attached restart that reviews and publishes PR 2.
		"recovery process never published PR 2": {mutate(recovered(), func(res *e2eResult) {
			res.GitHub.Requests = e2eRouteTally(e2eQualifiedRoutes)
		}), recovery, retainedLaunch, true},
		"recovery process polled with the publisher": {mutate(recovered(), func(res *e2eResult) { res.GitHub.Requests["publisher GET /pulls/N"] = 1 }), recovery, retainedLaunch, true},
		"recovery process scanned reviews":           {mutate(recovered(), func(res *e2eResult) { res.GitHub.Requests["publisher GET /pulls/N/reviews"] = 1 }), recovery, retainedLaunch, true},
		"recovery process commented on PR 1":         {mutate(recovered(), func(res *e2eResult) { res.GitHub.Requests["publisher POST /issues/N/comments"] = 1 }), recovery, retainedLaunch, true},
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

// TestE2EUnitObserverQuiesced is round 9 finding 2: a periodic listing still
// in flight when stop's final listing comes back clean must not record a unit
// that the final listing did not judge. The scripted systemd starts a unit
// right after the first listing stop makes; the periodic listing, entered
// before stop, takes its snapshot only after that, as a slow systemctl would.
func TestE2EUnitObserverQuiesced(t *testing.T) {
	unit := map[string]string{"sfl-alpha-" + strings.Repeat("b", 32) + ".service": "loaded/active/running source VM"}
	var calls atomic.Int32
	var running atomic.Bool
	entered, started := make(chan struct{}), make(chan struct{})
	world := func() map[string]string {
		if running.Load() {
			return unit
		}
		return map[string]string{}
	}
	list := func() (map[string]string, error) {
		switch n := calls.Add(1); n {
		case 1:
			return map[string]string{}, nil
		case 2:
			// The periodic listing: in flight before stop, it observes the
			// world only once the unit started or after a bound.
			close(entered)
			select {
			case <-started:
			case <-time.After(500 * time.Millisecond):
			}
			return world(), nil
		case 3:
			// The first listing stop makes is clean; the unit starts as it
			// returns.
			defer func() { running.Store(true); close(started) }()
			return world(), nil
		default:
			return world(), nil
		}
	}
	o := newE2EUnitObserver(t, list)
	<-entered
	units, residual, err := o.stop(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 0 && len(residual) == 0 {
		t.Error("a unit recorded after the final clean listing was reported observed with no residual", units)
	}
}

// TestE2ESettledVerdicts is the round 9 self-audit of concurrent observers:
// what the fake servers hold once quiesced must be exactly what finish judged.
func TestE2ESettledVerdicts(t *testing.T) {
	judged := func() (*e2eResult, e2eGitHubSummary, map[r.Provider][]e2eModelSession) {
		github := e2eGitHubSummary{ReviewPosts: map[int]int{1: 1}, Reviews: map[int][]map[string]any{1: {{"id": int64(5001), "state": "COMMENTED", "inline": []string{"source.go:1"}}}}, GitExchanges: []e2eGitExchange{{Operation: "advertisement", Status: 200}, {Operation: "upload-pack", Status: 200}}}
		models := map[r.Provider][]e2eModelSession{r.Codex: {{JobID: "request-1-1", Provider: r.Codex, Round: 1, Counts: f.Counts{Inference: 3}}}, r.Claude: nil}
		res := &e2eResult{GitHub: github, Models: models}
		// The settled reads are fresh copies, as summary and sessionsCopy make.
		var settled e2eGitHubSummary
		var copies map[r.Provider][]e2eModelSession
		raw, _ := json.Marshal(github)
		_ = json.Unmarshal(raw, &settled)
		raw, _ = json.Marshal(models)
		_ = json.Unmarshal(raw, &copies)
		return res, settled, copies
	}
	if f := e2eFailures(e2eSettledVerdicts(judged())); len(f) != 0 {
		t.Fatal("settled run refused:", f)
	}
	for name, edit := range map[string]func(*e2eGitHubSummary, map[r.Provider][]e2eModelSession){
		"violation after finish":   func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession) { g.Violations = []string{"late"} },
		"review POST after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession) { g.ReviewPosts[1] = 2 },
		"remote review after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession) {
			g.Reviews[2] = []map[string]any{{"id": int64(5002)}}
		},
		"Git exchange after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession) {
			g.GitExchanges = append(g.GitExchanges, e2eGitExchange{Operation: "upload-pack"})
		},
		"model request after finish": func(_ *e2eGitHubSummary, m map[r.Provider][]e2eModelSession) { m[r.Codex][0].Counts.Inference = 4 },
		"model session after finish": func(_ *e2eGitHubSummary, m map[r.Provider][]e2eModelSession) {
			m[r.Claude] = append(m[r.Claude], e2eModelSession{JobID: "request-1-1", Provider: r.Claude, Round: 1})
		},
	} {
		res, github, models := judged()
		edit(&github, models)
		if len(e2eFailures(e2eSettledVerdicts(res, github, models))) == 0 {
			t.Error(name, "accepted")
		}
	}
}

// e2eProviderReleaseFixture is a well-formed native provider release with
// synthetic pins and the given egress policy.
func e2eProviderReleaseFixture(policy r.EgressPolicy) r.GitReviewerProviderRelease {
	asset := func(path string) r.VMBootAsset { return r.VMBootAsset{Path: path, SHA256: r.Digest(path), Bytes: 4096} }
	return r.GitReviewerProviderRelease{Stub: asset("/fixture/nativevmstub"), QEMU: asset("/fixture/qemu"), Kernel: asset("/fixture/vmlinuz"), Initrd: asset("/fixture/native.cpio.gz"), Firmware: asset("/fixture/bios.bin"), Runtime: r.NativeVMRuntimeSpec{Artifact: asset("/fixture/native-qemu-runtime.bin"), NamespaceHelper: asset("/fixture/namespace-helper"), BootstrapLibraries: []r.VMBootAsset{asset("/lib/x86_64-linux-gnu/libc.so.6"), asset("/lib64/ld-linux-x86-64.so.2")}}, Policy: policy}
}

// e2eTestBootID is the boot ID unit tests run the native projection under.
const e2eTestBootID = "0f8fad5b-d9cb-469f-a165-70867728950e"

// e2eUnitJournalFixture is a production-valid unit journal of a launch that
// was started, settled, captured, aborted and stopped in boot e2eTestBootID.
func e2eUnitJournalFixture(binding r.AuthBinding, stub, manifest string) r.E2ENativeUnitJournal {
	unit := "sfl-alpha-" + binding.LaunchID + ".service"
	j := r.E2ENativeUnitJournal{Version: 1, Binding: binding, Unit: unit, BootID: e2eTestBootID, ManagerOwner: ":1.42", StubHash: stub, ManifestHash: manifest, Started: true, StartSettled: true, Captured: true, Aborted: true, Stopped: true, StartJob: "/org/freedesktop/systemd1/job/7", StopJob: "/org/freedesktop/systemd1/job/8", Identity: r.GateIdentity{InvocationID: strings.Repeat("d", 32), BootID: e2eTestBootID, PID: 4242, StartTicks: 99}, PayloadKind: "native-vm"}
	j.Cgroup.Path, j.Cgroup.Inode = "/user.slice/user-1000.slice/user@1000.service/app.slice/"+unit, 7
	return j
}

func TestE2ENativeUnitFrom(t *testing.T) {
	session, launch, attempt := strings.Repeat("5", 64), strings.Repeat("6", 32), strings.Repeat("a", 32)
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
	// receipts are a launch directory's unit journal, edited as JSON fields
	// (production's reader decodes it strictly but not canonically), and
	// its audit, which must be production's exact encoding: raw, when set,
	// replaces the audit's bytes.
	type receipts struct {
		unit  map[string]any
		audit *r.E2ENativeAudit
		raw   func([]byte) []byte
		mode  os.FileMode
	}
	valid := func(complete bool) receipts {
		var unit map[string]any
		encoded, _ := json.Marshal(e2eUnitJournalFixture(binding(session, launch), release.Stub.SHA256, pinned))
		if err := json.Unmarshal(encoded, &unit); err != nil {
			t.Fatal(err)
		}
		audit := &r.E2ENativeAudit{Version: 2, Binding: binding(session, launch), AttemptID: attempt, RequestHash: r.Digest("request"), ManifestHash: pinned, ReportHash: r.Digest(r.Report{}), SourceHash: r.Digest((*r.SourceAccess)(nil)), Provider: evidence(session, policy, access(complete)), Complete: complete, Stage: "result", AuthExported: true, AuthReturned: complete, Failure: "auth-return"}
		if complete {
			audit.Stage, audit.Failure = "result-validation", ""
		}
		return receipts{unit: unit, audit: audit, mode: 0600}
	}
	project := func(in receipts) e2eNativeUnit {
		directory := t.TempDir()
		write := func(name string, raw []byte) {
			if err := os.WriteFile(filepath.Join(directory, name), raw, in.mode); err != nil {
				t.Fatal(err)
			}
		}
		if in.unit != nil {
			raw, _ := json.Marshal(in.unit)
			write("unit.json", raw)
		}
		if in.audit != nil {
			raw, _ := json.Marshal(in.audit)
			if in.raw != nil {
				raw = in.raw(raw)
			}
			write("native-audit.json", raw)
		}
		return e2eNativeUnitFrom(r.Codex, release, config.Limits, e2eTestBootID, launch, directory)
	}
	whole := func(u e2eNativeUnit) bool { return u.Started && u.StartSettled && u.Captured && u.Aborted && u.Stopped }
	if u := project(valid(true)); !u.AuditPresent || !whole(u) || !u.Complete || !u.AuthReturned || u.SessionHash != session || u.ManifestHash != pinned || u.Access.InferenceRequests != 3 || u.Access.SessionHash != session || u.AttemptID != attempt || u.EvidenceHash != e2eEvidenceHash(evidence(session, policy, access(true))) || u.Audit == nil || u.Binding != binding(session, launch) || r.Digest(u.Policy) != r.Digest(policy) || u.RequestHash != r.Digest("request") || u.Stage != "result-validation" {
		t.Fatal("bound completed receipts refused", u)
	}
	if u := project(valid(false)); !u.AuditPresent || u.Complete || !u.AuthExported || u.AuthReturned || !u.Access.Failed || u.Failure != "auth-return" {
		t.Fatal("bound interrupted receipts refused", u)
	}
	if u := project(receipts{unit: valid(true).unit, mode: 0600}); u.AuditPresent || !whole(u) || u.SessionHash != session || u.AttemptID != "" {
		t.Fatal("unit without an audit misreported", u)
	}
	change := func(complete bool, edit func(receipts)) receipts { in := valid(complete); edit(in); return in }
	// A launch stopped before export and completion needs no evidence.
	if u := project(change(false, func(in receipts) { in.audit.AuthExported, in.audit.Provider = false, nil })); !u.AuditPresent || u.AttemptID != attempt || u.EvidenceHash != "" {
		t.Fatal("unexported interrupted receipt refused", u)
	}
	// fields edits the audit's encoding as JSON fields.
	fields := func(complete bool, edit func(map[string]any)) receipts {
		in := valid(complete)
		in.raw = func(raw []byte) []byte {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			edit(m)
			out, _ := json.Marshal(m)
			return out
		}
		return in
	}
	// receipt edits only the provider access counters of a bound audit.
	receipt := func(complete bool, edit func(*r.ProviderAccess)) receipts {
		a := access(complete)
		edit(&a)
		return change(complete, func(in receipts) { in.audit.Provider = evidence(session, policy, a) })
	}
	l := policy.Limits
	for name, in := range map[string]receipts{
		"audit of another session":                  change(true, func(in receipts) { in.audit.Binding = binding(strings.Repeat("7", 64), launch) }),
		"audit of another launch":                   change(true, func(in receipts) { in.audit.Binding = binding(session, strings.Repeat("8", 32)) }),
		"audit without a binding":                   fields(true, func(m map[string]any) { delete(m, "Binding") }),
		"audit of another version":                  change(true, func(in receipts) { in.audit.Version = 3 }),
		"audit without a version":                   fields(true, func(m map[string]any) { delete(m, "Version") }),
		"version 1 audit":                           fields(true, func(m map[string]any) { m["Version"] = 1; delete(m, "AttemptID") }),
		"version 1 audit naming an attempt":         change(true, func(in receipts) { in.audit.Version = 1 }),
		"audit without an attempt ID":               fields(true, func(m map[string]any) { delete(m, "AttemptID") }),
		"audit with an empty attempt ID":            change(false, func(in receipts) { in.audit.AttemptID = "" }),
		"audit with a malformed attempt ID":         change(true, func(in receipts) { in.audit.AttemptID = strings.Repeat("A", 32) }),
		"audit with a short attempt ID":             change(true, func(in receipts) { in.audit.AttemptID = attempt[:31] }),
		"completed audit without evidence":          change(true, func(in receipts) { in.audit.Provider = nil }),
		"exported audit without evidence":           change(false, func(in receipts) { in.audit.Provider = nil }),
		"evidence of another session":               change(true, func(in receipts) { in.audit.Provider = evidence(strings.Repeat("7", 64), policy, access(true)) }),
		"evidence under another policy":             change(true, func(in receipts) { in.audit.Provider = evidence(session, otherPolicy, access(true)) }),
		"evidence of another provider":              change(true, func(in receipts) { in.audit.Provider = evidence(session, claudePolicy, access(true)) }),
		"completed audit with unreturned auth":      change(true, func(in receipts) { in.audit.AuthReturned = false }),
		"completed audit without exported auth":     change(true, func(in receipts) { in.audit.AuthExported = false }),
		"auth returned but never exported":          change(false, func(in receipts) { in.audit.AuthExported, in.audit.AuthReturned = false, true }),
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
			in.unit["Binding"], in.audit.Binding = binding(session, strings.Repeat("8", 32)), binding(session, strings.Repeat("8", 32))
		}),
		"unit journal naming another unit": change(true, func(in receipts) { in.unit["Unit"] = "sfl-alpha-" + strings.Repeat("8", 32) + ".service" }),
		"unit journal of another version":  change(true, func(in receipts) { in.unit["Version"] = 2 }),
		"containment not derived from launch": change(true, func(in receipts) {
			b := binding(session, launch)
			b.ContainmentID = strings.Repeat("9", 64)
			in.unit["Binding"], in.audit.Binding = b, b
		}),
		// Round 8 finding 1: the journal and audit must be of the pinned
		// native payload and this launch's release manifest.
		"unit journal of the fixture payload":     change(true, func(in receipts) { in.unit["PayloadKind"] = "vm-fixture" }),
		"unit journal without a payload kind":     change(true, func(in receipts) { delete(in.unit, "PayloadKind") }),
		"unit journal of another stub":            change(true, func(in receipts) { in.unit["StubHash"] = r.Digest("another stub") }),
		"unit journal without a stub hash":        change(true, func(in receipts) { delete(in.unit, "StubHash") }),
		"unit journal without a manifest hash":    change(true, func(in receipts) { delete(in.unit, "ManifestHash") }),
		"audit without a manifest hash":           fields(true, func(m map[string]any) { delete(m, "ManifestHash") }),
		"audit of another manifest than its unit": change(true, func(in receipts) { in.audit.ManifestHash = manifest(otherRelease, binding(session, launch)) }),
		"unit journal of another native release": change(true, func(in receipts) {
			in.unit["ManifestHash"], in.audit.ManifestHash = manifest(otherRelease, binding(session, launch)), manifest(otherRelease, binding(session, launch))
		}),
		"manifest of another launch's binding": change(true, func(in receipts) {
			other := manifest(release, binding(session, strings.Repeat("8", 32)))
			in.unit["ManifestHash"], in.audit.ManifestHash = other, other
		}),
		// Round 10 finding 4: the journal must pass production's own unit
		// journal validation, which refuses a capture without a settled
		// start and a stop without an abort, before its flags count.
		"captured unit journal without a start":         change(true, func(in receipts) { in.unit["Started"] = false }),
		"captured unit journal without a settled start": change(true, func(in receipts) { in.unit["StartSettled"] = false }),
		"stopped unit journal without an abort":         change(true, func(in receipts) { in.unit["Aborted"] = false }),
		"captured unit journal without a process":       change(true, func(in receipts) { in.unit["Identity"] = r.GateIdentity{} }),
		"captured unit journal in a foreign cgroup": change(true, func(in receipts) {
			in.unit["Cgroup"] = map[string]any{"Path": "/user.slice/other.service", "Inode": 7, "Hierarchy": map[string]any{"Device": 0, "Inode": 0, "MountNamespace": 0, "CgroupNamespace": 0}}
		}),
		"unit journal without a manager owner": change(true, func(in receipts) { in.unit["ManagerOwner"] = "" }),
		"unit journal with an unknown field":   change(true, func(in receipts) { in.unit["Extra"] = true }),
		"unit journal of another boot":         change(true, func(in receipts) { in.unit["BootID"] = "11111111-2222-4333-8444-555555555555" }),
		"unit process of another boot": change(true, func(in receipts) {
			in.unit["Identity"] = r.GateIdentity{InvocationID: strings.Repeat("d", 32), BootID: "11111111-2222-4333-8444-555555555555", PID: 4242, StartTicks: 99}
		}),
		"unit process without an invocation": change(true, func(in receipts) {
			in.unit["Identity"] = r.GateIdentity{BootID: e2eTestBootID, PID: 4242, StartTicks: 99}
		}),
		"receipts readable by others": func() receipts { in := valid(true); in.mode = 0644; return in }(),
		// Round 10 finding 5: the audit is production's exact receipt, with
		// its stage and failure kind consistent with its completion.
		"audit with an unknown field":              fields(true, func(m map[string]any) { m["Extra"] = true }),
		"audit without its source hash":            fields(true, func(m map[string]any) { delete(m, "SourceHash") }),
		"audit without its report hash":            fields(true, func(m map[string]any) { delete(m, "ReportHash") }),
		"audit without its request hash":           fields(true, func(m map[string]any) { delete(m, "RequestHash") }),
		"audit in another encoding":                fields(true, func(map[string]any) {}),
		"completed audit with a failure kind":      change(true, func(in receipts) { in.audit.Failure = "cancelled" }),
		"completed audit at another stage":         change(true, func(in receipts) { in.audit.Stage = "result" }),
		"interrupted audit without a failure kind": change(false, func(in receipts) { in.audit.Failure = "" }),
	} {
		if u := project(in); u.AuditPresent {
			t.Error(name, "accepted as this launch's audit")
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
	units := e2eReadNativeUnits(map[r.Provider]string{r.Codex: root, r.Claude: filepath.Join(root, "absent")}, releases, r.DefaultConfig().Limits, e2eTestBootID)
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

// e2eFixtureJob is PR 1's adversarial job on a deterministic snapshot before
// any session: no lease, attempt or artifact. It was admitted from the
// owner-authored PR's automatic open request 1 (e2eFixtureOrigin).
func e2eFixtureJob() *r.DurableJob { return e2eFixtureJobFor(1) }

// e2eFixtureJobFor is e2eFixtureJob for PR pr, whose head is pr's digit
// repeated, admitted from request 1 of origin e2eFixtureOriginFor(pr).
func e2eFixtureJobFor(pr int) *r.DurableJob {
	head, base := strings.Repeat(strconv.Itoa(pr), 40), strings.Repeat("e", 40)
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: pr, Head: head, Base: base, MergeBase: base}
	job := &r.DurableJob{Job: r.Job{Origin: e2eFixtureOriginFor(pr), ID: fmt.Sprintf("request-%d-1", pr), RequesterID: r.OwnerID, Generation: 1, Snapshot: snapshot, Config: r.DefaultConfig(), ConfigVersion: 1, Release: strings.Repeat("f", 40), Packet: "synthetic packet"}, Scope: e2eScope(pr), RequestID: 1, CreatedAt: time.Unix(1000, 0).UTC(), ChargeDays: []string{"2026-10-23"}}
	job.Checkpoint.JobHash = r.Digest(job.Job)
	return job
}

// e2eFixtureOrigin is the valid origin of PR 1's automatic open request: the
// owner opened the fake upstream's PR 1 after the trigger policy took effect,
// and polling observed it as its first observation.
func e2eFixtureOrigin() *r.RequestOrigin { return e2eFixtureOriginFor(1) }

// e2eFixtureComment is the fake upstream's ID of the owner's @sfl review
// comment that admits every fixture PR other than PR 1.
const e2eFixtureComment = int64(31)

// e2eFixtureOriginFor is the valid origin of PR pr's request: PR 1's
// automatic open request (e2eFixtureOrigin), and for any other PR, as in
// isolation and close-provider-recover, the owner's @sfl review comment
// e2eFixtureComment on a PR another author opened before the epoch.
func e2eFixtureOriginFor(pr int) *r.RequestOrigin {
	if pr != 1 {
		return &r.RequestOrigin{PolicyEnabledAt: time.Unix(900, 0).UTC(), Kind: "comment", PolicyOwnerID: r.OwnerID, PullID: e2ePullID(pr), ActorID: r.OwnerID, ActorRole: "commenter", CommentID: e2eFixtureComment, BodyHash: r.Digest("@sfl review"), EventAt: time.Unix(950, 0).UTC(), ObservedAt: time.Unix(960, 0).UTC()}
	}
	return &r.RequestOrigin{PolicyEnabledAt: time.Unix(900, 0).UTC(), Kind: "opened", PolicyOwnerID: r.OwnerID, PullID: e2ePullID(1), ActorID: r.OwnerID, ActorRole: "pr_author", EventAt: time.Unix(950, 0).UTC(), ObservedAt: time.Unix(960, 0).UTC(), Sequence: 1}
}

// e2eFixtureRequest is the admitted request PR j.Scope.PR retains for the
// fixture job j, of origin e2eFixtureOriginFor, taken before any edit of j.
func e2eFixtureRequest(j *r.DurableJob) r.NativeRequest {
	origin := e2eFixtureOriginFor(j.Scope.PR)
	return r.NativeRequest{Origin: origin, ID: j.RequestID, RequesterID: r.OwnerID, CreatedAt: origin.EventAt, Snapshot: j.Job.Snapshot, Admitted: true}
}

// e2eFixtureSession is the session job j derives for provider's round,
// which the fixture requires to exist.
func e2eFixtureSession(j *r.DurableJob, provider r.Provider, round int) r.Session {
	s, err := e2eSessionFor(j, provider, round)
	if err != nil {
		panic(err)
	}
	return s
}

// e2eFixtureEvidence is a stopped receipt of session under its provider's
// fixture policy: a completed phase's canned exchanges (Codex also its five
// metadata requests and one local negotiation), or a phase interrupted with
// its first inference forwarded and never answered.
func e2eFixtureEvidence(session r.Session, complete bool) *r.ProviderEvidence {
	a := r.ProviderAccess{Version: 1, Requests: 3, InferenceRequests: 3, Forwarded: 3, Responses: 3, RequestBytes: 4096, ResponseBytes: 8192, Stopped: true}
	if session.Identity.Provider == r.Codex {
		a = r.ProviderAccess{Version: 1, Requests: 9, InferenceRequests: 3, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 8, Responses: 8, RequestBytes: 4096, ResponseBytes: 8192, Stopped: true}
	}
	if !complete {
		a = r.ProviderAccess{Version: 1, Requests: 7, InferenceRequests: 1, MetadataRequests: 5, NegotiationRequests: 1, NegotiationResponses: 1, Forwarded: 6, Responses: 5, RequestBytes: 2048, ResponseBytes: 4096, Failed: true, Stopped: true}
	}
	policy := f.Policy(session)
	a.SessionHash, a.PolicyHash = r.Digest(session), r.Digest(policy)
	return &r.ProviderEvidence{Policy: policy, Access: a}
}

// e2eLaunchFixture is the projected native launch of session under launch,
// whole-unit stopped, with the version 2 audit production writes for it when
// durable attempt attempt runs it: the request
// reviewTaskAudited builds, the report and source receipt the phase
// recorded, its evidence and auth disposition, and the stage and failure kind
// of a completed phase or of one interrupted while awaiting its result.
func e2eLaunchFixture(j *r.DurableJob, session r.Session, launch, attempt string, evidence *r.ProviderEvidence, report any, access *r.SourceAccess, complete, returned bool) e2eNativeUnit {
	provider := session.Identity.Provider
	hash := r.Digest(session)
	binding := r.AuthBinding{SessionHash: hash, LaunchID: launch, ContainmentID: r.Digest(struct{ Session, Launch string }{hash, launch})}
	request, err := r.E2ENativeRequestHash(binding, session, e2eFixturePaths(j.Job.Snapshot.PR), evidence.Policy, e2eProfileHints(provider))
	if err != nil {
		panic(err)
	}
	audit := &r.E2ENativeAudit{Version: 2, Binding: binding, AttemptID: attempt, RequestHash: request, ManifestHash: r.Digest("manifest " + launch), ReportHash: r.Digest(report), SourceHash: r.Digest(access), Provider: evidence, Complete: complete, Stage: "result", AuthExported: true, AuthReturned: returned, Failure: "auth-return"}
	if returned {
		audit.Failure = "cancelled"
	}
	if complete {
		audit.Stage, audit.Failure = "result-validation", ""
		if provider == r.Codex {
			audit.CodeHost = r.E2ECodeHostFixture(session)
		}
	}
	return e2eNativeUnit{Provider: string(provider), Launch: launch, SessionHash: hash, ManifestHash: audit.ManifestHash, Started: true, StartSettled: true, Captured: true, Aborted: true, Stopped: true, Complete: complete, AuthExported: true, AuthReturned: returned, AuditPresent: true, RequestHash: request, ReportHash: audit.ReportHash, SourceHash: audit.SourceHash, Stage: audit.Stage, Failure: audit.Failure, Access: evidence.Access, AttemptID: attempt, EvidenceHash: e2eEvidenceHash(evidence), Binding: binding, Policy: evidence.Policy, Audit: audit}
}

// e2eCompletedFixture is a valid completed PR 1 review as the open service
// reports it: the sole job with the four adversarial artifacts, each bound to
// the job, holding the canned backend's report for its phase and carrying
// provider evidence the job records; one completed native launch with that
// evidence and one unheld backend check per artifact session; the sole
// confirmed intent bound to the job, carrying the document rendered from the
// canned reports; the one remote review projected from that intent's
// document; and one stopped source attempt of the job's snapshot under the
// current release.
func e2eCompletedFixture() e2eCompletion { return e2eReviewFixture(1) }

// e2eReviewFixture is e2eCompletedFixture for PR pr, whose head is pr's digit
// repeated.
func e2eReviewFixture(pr int) e2eCompletion { return e2eReviewFixtureOf(pr, nil) }

// e2eCompletedFixtureOf is e2eCompletedFixture for a job that admission
// persisted with edit applied, every record derived from the job (artifacts,
// attempts, launches, sessions, intent, document, remote review and source
// export) consistently rebuilt from the edited job. The harness's own inputs
// are not edited: the configuration it opened the store with, the release,
// the fake upstream's heads and the retained admitted request.
func e2eCompletedFixtureOf(edit func(*r.Job)) e2eCompletion { return e2eReviewFixtureOf(1, edit) }

// e2eReviewFixtureOf is e2eCompletedFixtureOf for PR pr, whose head is pr's
// digit repeated and whose retained request has e2eFixtureOriginFor(pr).
func e2eReviewFixtureOf(pr int, edit func(*r.Job)) e2eCompletion {
	attempt, id := strings.Repeat("a", 32), strings.Repeat("c", 32)
	job := e2eFixtureJobFor(pr)
	head, snapshot := job.Job.Snapshot.Head, job.Job.Snapshot
	config, release, request := job.Job.Config, job.Job.Release, e2eFixtureRequest(job)
	if edit != nil {
		edit(&job.Job)
		job.Checkpoint.JobHash = r.Digest(job.Job)
	}
	job.State, job.Checkpoint.ConsumedSessions = "dual_complete", 4
	reports := e2eCannedReports(job.Job)
	c := e2eCompletion{Heads: map[int]string{pr: head}, Base: snapshot.Base, Release: release, Config: config, Source: e2eSourceBinding{r.Digest("release package"), r.Digest("source VM manifest"), r.Digest("source stub")}, Posts: map[int]int{pr: 1}, Boot: e2eTestBootID}
	for i, run := range []struct {
		provider r.Provider
		round    int
	}{{r.Codex, 1}, {r.Claude, 1}, {r.Codex, 2}, {r.Claude, 2}} {
		session := e2eFixtureSession(job, run.provider, run.round)
		hash := r.Digest(session)
		// The canned backends' exchange contract: three inferences, and for
		// Codex its five captured metadata requests.
		counts := f.Counts{Inference: 3}
		if run.provider == r.Codex {
			counts.Metadata, counts.MetadataCaptured = 5, true
		}
		evidence := e2eFixtureEvidence(session, true)
		recorded := *evidence
		own := fmt.Sprintf("pr%d-only.go", pr)
		ownBytes := e2eFixtureFiles(pr)[own]
		access := &r.SourceAccess{Version: 1, SessionHash: hash, ToolCalls: 2, ReaderCalls: 3, Bytes: 64, Reads: []r.ReadRecord{{Revision: head, Path: "source.go", Hash: r.Digest([]byte(e2eSource)), Bytes: len(e2eSource)}, {Revision: head, Path: own, Hash: r.Digest([]byte(ownBytes)), Bytes: len(ownBytes)}}, Coverage: []r.Coverage{}}
		read := *access
		attemptID := strings.Repeat(strconv.Itoa(6+i), 32)
		validation := []r.ReadRecord{{Revision: head, Path: "source.go", Hash: r.Digest([]byte(e2eSource)), Bytes: len(e2eSource)}}
		job.Checkpoint.Artifacts = append(job.Checkpoint.Artifacts, r.Artifact{JobHash: r.Digest(job.Job), InputHash: hash, Report: reports[i], ValidationReads: validation, Coverage: []r.Coverage{}, SourceAccess: access, ProviderEvidence: evidence, ProviderEvidenceRequired: true})
		job.ProviderAttempts = append(job.ProviderAttempts, r.ProviderAttempt{ID: attemptID, Evidence: &recorded})
		job.SourceAttempts = append(job.SourceAttempts, r.SourceAttempt{ID: attemptID, Access: &read})
		job.AttemptProvider, job.AttemptRound, job.AttemptID, job.AttemptInputHash, job.AttemptProviderEvidenceRequired = run.provider, run.round, attemptID, hash, true
		if !slices.Contains(job.AuditedProviders, run.provider) {
			job.AuditedProviders = append(job.AuditedProviders, run.provider)
		}
		c.Units = append(c.Units, e2eLaunchFixture(job, session, strings.Repeat(strconv.Itoa(i+1), 32), attemptID, evidence, reports[i], access, true, true))
		c.Sessions = append(c.Sessions, e2eModelSession{JobID: job.Job.ID, Provider: run.provider, Round: run.round, PR: pr, Head: head, SessionHash: hash, Counts: counts})
	}
	marker := "<!-- sfl-alpha:" + id + " -->"
	document, err := r.E2ECompletedDocument(job.Job, reports, e2eCannedLedger(job.Job), e2eFixtureDiff(pr), marker)
	if err != nil {
		panic(err)
	}
	write := &r.WriteIntent{ID: id, Kind: "review", Scope: job.Scope, RequestID: job.RequestID, Generation: job.Job.Generation, JobHash: r.Digest(job.Job), CheckpointHash: r.Digest(job.Checkpoint), Snapshot: job.Job.Snapshot, Document: document, BodyHash: r.Digest(document), Marker: marker, State: "confirmed", ReceiptID: 5001}
	remoteMarker, remoteHash := r.E2ERemoteReview(document.Summary, document.Inline)
	c.Reviews = map[int][]map[string]any{pr: {{"id": int64(5001), "state": "COMMENTED", "commit_id": head, "marker": remoteMarker, "document_hash": remoteHash, "inline": []string{"source.go:1"}}}}
	c.State = r.DurableState{Jobs: map[string]*r.DurableJob{job.Job.ID: job}, Writes: map[string]*r.WriteIntent{write.ID: write}, PRs: map[string]*r.PRRecord{e2eScopeKey(pr): {Scope: e2eScope(pr), Generation: 1, Requests: []r.NativeRequest{request}, CurrentRequest: request.ID}}}
	c.Records = []r.E2ESourceRecord{{Attempt: attempt, Unit: "sfl-alpha-" + attempt + ".service", InvocationID: strings.Repeat("9", 32), RequestHash: strings.Repeat("6", 64), SourceRequestHash: strings.Repeat("7", 64), StubHash: c.Source.Stub, ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot, StartIntent: true, StartSettled: true, Captured: true, Stopped: true, Revoked: true, GrantHash: "grant", BootID: e2eTestBootID}}
	c.Exchanges = e2eAcquisition(c.Records[0])
	c.Exports = []r.E2EExportRecord{{Attempt: attempt, State: "staged", RequestHash: strings.Repeat("7", 64), ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot, PacketHash: r.Digest(job.Job.Packet)}}
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
// version 2 receipt names the attempt under which the job also records its
// stopped evidence, and its held backend check. The interrupted launch is the
// last unit. The interrupted phase returned its auth, read no result and so recorded no
// report and no source receipt.
func e2eReopenFixture() (e2eCompletion, string) {
	c := e2eCompletedFixture()
	job := e2eJobFor(c.State, 1)
	session := e2eFixtureSession(job, r.Codex, 1)
	first := r.Digest(session)
	evidence := e2eFixtureEvidence(session, false)
	interrupted := strings.Repeat("b", 32)
	job.ProviderAttempts = append([]r.ProviderAttempt{{ID: interrupted, Evidence: evidence}}, job.ProviderAttempts...)
	c.Units = append(c.Units, e2eLaunchFixture(job, session, strings.Repeat("9", 32), interrupted, evidence, r.Report{}, nil, false, true))
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
		"second review POST":                        func(c *e2eCompletion) { c.Posts[1] = 2 },
		"remote review without inline":              func(c *e2eCompletion) { c.Reviews[1][0]["inline"] = []string{} },
		"remote review on another head":             func(c *e2eCompletion) { c.Reviews[1][0]["commit_id"] = strings.Repeat("9", 40) },
		"completed launch kept its auth":            func(c *e2eCompletion) { c.Units[1].AuthReturned = false },
		"completed launch audit unbound":            func(c *e2eCompletion) { c.Units[2].AuditPresent = false },
		"completed launch names an unknown attempt": func(c *e2eCompletion) { c.Units[2].AttemptID = strings.Repeat("f", 32) },
		"completed launch names the interrupted attempt": func(c *e2eCompletion) {
			c.Units[0].AttemptID, c.Units[0].EvidenceHash = c.Units[4].AttemptID, c.Units[4].EvidenceHash
		},
		"completed launch names another session's attempt": func(c *e2eCompletion) {
			c.Units[1].AttemptID, c.Units[1].EvidenceHash = c.Units[2].AttemptID, c.Units[2].EvidenceHash
		},
		"completed launch repeats other evidence": func(c *e2eCompletion) { c.Units[3].EvidenceHash = c.Units[4].EvidenceHash },
		"artifact attempt unrecorded": func(c *e2eCompletion) {
			j := e2eJobFor(c.State, 1)
			j.ProviderAttempts = slices.Delete(j.ProviderAttempts, 2, 3)
		},
		"artifact without provider evidence": func(c *e2eCompletion) { e2eJobFor(c.State, 1).Checkpoint.Artifacts[1].ProviderEvidence = nil },
		"second interrupted launch":          func(c *e2eCompletion) { c.Units = append(c.Units, c.Units[4]) },
		"source attempt not stopped":         func(c *e2eCompletion) { c.Records[0].Stopped = false },
		"source records unreadable":          func(c *e2eCompletion) { c.SourceErr = r.ErrLiveBlocked },
		"interrupted session recharged":      func(c *e2eCompletion) { e2eJobFor(c.State, 1).Checkpoint.ConsumedSessions = 5 },
		"interrupted launch completed": func(c *e2eCompletion) {
			// A completed launch with a foreign receipt must not pass as the
			// interrupted one.
			c.Units[4].Complete = true
		},
		"interrupted launch without its audit": func(c *e2eCompletion) { c.Units[4].AuditPresent = false },
		"interrupted launch names a completed attempt": func(c *e2eCompletion) {
			c.Units[4].AttemptID, c.Units[4].EvidenceHash = c.Units[1].AttemptID, c.Units[1].EvidenceHash
		},
		"interrupted launch names an unknown attempt": func(c *e2eCompletion) { c.Units[4].AttemptID = strings.Repeat("f", 32) },
		"interrupted launch repeats other evidence":   func(c *e2eCompletion) { c.Units[4].EvidenceHash = c.Units[1].EvidenceHash },
		"interrupted attempt recorded twice": func(c *e2eCompletion) {
			j := e2eJobFor(c.State, 1)
			j.ProviderAttempts = append(j.ProviderAttempts, j.ProviderAttempts[0])
		},
		// Round 10: the interrupted launch is the job's one interrupted
		// attempt, and its audit records that attempt's request, no report
		// and exactly the source receipt the job recorded for it (none).
		"interrupted attempt never recorded": func(c *e2eCompletion) {
			e2eJobFor(c.State, 1).ProviderAttempts = e2eJobFor(c.State, 1).ProviderAttempts[1:]
		},
		"second interrupted provider attempt": func(c *e2eCompletion) {
			j := e2eJobFor(c.State, 1)
			other := *j.ProviderAttempts[0].Evidence
			other.Access.Responses--
			j.ProviderAttempts = append(j.ProviderAttempts, r.ProviderAttempt{ID: "interrupted-again", Evidence: &other})
		},
		"interrupted audit of a report": func(c *e2eCompletion) {
			c.Units[len(c.Units)-1].Audit.ReportHash = r.Digest(e2eJobFor(c.State, 1).Checkpoint.Artifacts[0].Report)
		},
		"interrupted audit of an unrecorded source receipt": func(c *e2eCompletion) {
			c.Units[len(c.Units)-1].Audit.SourceHash = r.Digest(e2eJobFor(c.State, 1).Checkpoint.Artifacts[0].SourceAccess)
		},
		"interrupted audit of another request": func(c *e2eCompletion) { c.Units[len(c.Units)-1].Audit.RequestHash = r.Digest("another request") },
		"interrupted audit at another stage":   func(c *e2eCompletion) { c.Units[len(c.Units)-1].Audit.Stage = "auth-export" },
		"interrupted audit without a failure":  func(c *e2eCompletion) { c.Units[len(c.Units)-1].Audit.Failure = "" },
		"interrupted unit never aborted":       func(c *e2eCompletion) { c.Units[len(c.Units)-1].Aborted = false },
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
	// republish edits the intent's document and re-derives its hash and the
	// remote review's projection, so intent and remote agree on the edited
	// content and only its binding to the canned reports can refuse it.
	republish := func(c *e2eCompletion, edit func(string) string) {
		d := &write(c).Document
		d.Summary = edit(d.Summary)
		for i := range d.Inline {
			d.Inline[i].Body = edit(d.Inline[i].Body)
		}
		rehash(c)
		_, remote(c)["document_hash"] = r.E2ERemoteReview(d.Summary, d.Inline)
	}
	// rejob replaces the completion with one whose job admission persisted
	// with edit applied and every record derived from the job rebuilt
	// consistently (e2eCompletedFixtureOf), so only a check against the
	// harness's own inputs can refuse it.
	rejob := func(edit func(*r.Job)) func(*e2eCompletion) {
		return func(c *e2eCompletion) { *c = e2eCompletedFixtureOf(edit) }
	}
	// recheckpoint rebinds the intent to an edited checkpoint, so only the
	// artifacts' binding to the canned reports can refuse the edit.
	recheckpoint := func(c *e2eCompletion) { write(c).CheckpointHash = r.Digest(job(c).Checkpoint) }
	finding := func(c *e2eCompletion, i int) *r.Finding { return &artifact(c, i).Report.Findings[0] }
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
		"launch naming another artifact's attempt":     "job-bound sessions",
		"launch naming no attempt":                     "job-bound sessions",
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
		// Round 11 findings 2 and 3: the job admission persisted is held to
		// the harness's inputs, and the expected document is rendered from
		// those inputs, so a consistently rebuilt wrong job is refused.
		"job of another origin":                        "carries exactly the complete origin",
		"document of another origin":                   "confirmed document is exactly the one rendered",
		"job of another pull origin":                   "carries exactly the complete origin",
		"job without an origin":                        "carries exactly the complete origin",
		"retained request of another origin":           "carries exactly the complete origin",
		"retained request lost":                        "carries exactly the complete origin",
		"job of another provider timeout":              "configuration the harness opened the store with",
		"document of another configuration":            "confirmed document is exactly the one rendered",
		"job of another quota wait":                    "configuration the harness opened the store with",
		"job of another daily review limit":            "configuration the harness opened the store with",
		"job of another retention":                     "configuration the harness opened the store with",
		"job of another configuration version":         "configuration the harness opened the store with",
		"document of another configuration version":    "confirmed document is exactly the one rendered",
		"job of another ID":                            "exactly the one admission derives",
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
		"report with another finding title":            "canned backend's reports",
		"report with another finding body":             "canned backend's reports",
		"report with another severity":                 "canned backend's reports",
		"report with another quote":                    "canned backend's reports",
		"report with another anchor revision":          "canned backend's reports",
		"report without its peer confirmation":         "canned backend's reports",
		"report rejecting its peer's finding":          "canned backend's reports",
		"report with a retraction":                     "canned backend's reports",
		"report with coverage":                         "canned backend's reports",
		"artifact with engine-added coverage":          "canned backend's reports",
		"report of another packet":                     "canned backend's reports",
		"report of another policy":                     "canned backend's reports",
		"document with another finding title":          "rendered from the canned reports",
		"document with another finding body":           "rendered from the canned reports",
		"document with another severity":               "rendered from the canned reports",
		"document with another source revision":        "rendered from the canned reports",
		"document with another summary":                "rendered from the canned reports",
		"document without the peer confirmation":       "rendered from the canned reports",
		// Round 10.
		"fifth provider attempt":                        "exactly the provider and source attempts",
		"fifth source attempt":                          "exactly the provider and source attempts",
		"source receipt of another version":             "fixture's exchanges",
		"source receipt with negative reader calls":     "fixture's exchanges",
		"source receipt over the byte limit":            "fixture's exchanges",
		"source receipt exhausted below the call limit": "fixture's exchanges",
		"source receipt with malformed coverage":        "fixture's exchanges",
		"source receipt with omitted coverage":          "fixture's exchanges",
		"launch audit of another request":               "job-bound sessions",
		"launch audit of another report":                "job-bound sessions",
		"launch audit of another source receipt":        "job-bound sessions",
		"launch audit of another stage":                 "job-bound sessions",
		"codex launch audit without its code host":      "job-bound sessions",
		"launch unit never aborted":                     "job-bound sessions",
		"launch unit start never settled":               "job-bound sessions",
		"launch audit absent":                           "job-bound sessions",
		"artifact not requiring provider evidence":      "job-bound sessions",
		"artifact evidence over the protocol limits":    "job-bound sessions",
		"R1 report changed after R2 froze it":           "exactly Codex R1",
		"artifact of an underived session":              "exactly Codex R1",
		"artifact with other validation reads":          "canned backend's reports",
		"artifact without validation reads":             "canned backend's reports",
		"completed job still leased":                    "holds no lease",
		"completed job lease deadline kept":             "holds no lease",
		"completed job still outstanding":               "holds no lease",
		"completed job with a pending charge":           "holds no lease",
		"completed job without a charge day":            "holds no lease",
		"completed job's last attempt another's":        "holds no lease",
		"completed job audited one provider":            "holds no lease",
		"completed job without required evidence":       "holds no lease",
		"source record stopped through a later boot":    "final record holds",
		"source record of another boot":                 "final record holds",
		"staged export of another packet":               "source records with matching",
	}
	// source edits one artifact's source receipt and the job's record of it
	// alike, so only production's receipt validation can refuse the edit.
	source := func(c *e2eCompletion, i int, edit func(*r.SourceAccess)) {
		edit(artifact(c, i).SourceAccess)
		edit(job(c).SourceAttempts[i].Access)
	}
	for name, edit := range map[string]func(*e2eCompletion){
		// Round 10 finding 1: the job holds exactly its artifacts' attempts.
		"fifth provider attempt": func(c *e2eCompletion) {
			job(c).ProviderAttempts = append(job(c).ProviderAttempts, r.ProviderAttempt{ID: "unrelated", Evidence: e2eFixtureEvidence(e2eFixtureSession(job(c), r.Codex, 1), false)})
		},
		"fifth source attempt": func(c *e2eCompletion) {
			extra := *job(c).SourceAttempts[0].Access
			job(c).SourceAttempts = append(job(c).SourceAttempts, r.SourceAttempt{ID: "unrelated", Access: &extra})
		},
		// Round 10 finding 2: production's source receipt validation for
		// the artifact's session.
		"source receipt of another version":         func(c *e2eCompletion) { source(c, 0, func(a *r.SourceAccess) { a.Version = 2 }) },
		"source receipt with negative reader calls": func(c *e2eCompletion) { source(c, 1, func(a *r.SourceAccess) { a.ReaderCalls = -1 }) },
		"source receipt over the byte limit": func(c *e2eCompletion) {
			source(c, 2, func(a *r.SourceAccess) { a.Bytes = job(c).Job.Config.Limits.ReaderBytes + 1 })
		},
		"source receipt exhausted below the call limit": func(c *e2eCompletion) { source(c, 3, func(a *r.SourceAccess) { a.Exhausted = true }) },
		"source receipt with malformed coverage": func(c *e2eCompletion) {
			source(c, 0, func(a *r.SourceAccess) { a.Coverage = []r.Coverage{{Path: "../escape", Reason: ""}} })
		},
		"source receipt with omitted coverage": func(c *e2eCompletion) {
			source(c, 1, func(a *r.SourceAccess) { a.Coverage = []r.Coverage{{Path: "README.md", Reason: "not read"}} })
		},
		// Round 10 finding 5: each completed launch's audit records the
		// request, report and source receipt of its artifact's phase.
		"launch audit of another request": func(c *e2eCompletion) { c.Units[0].Audit.RequestHash = r.Digest("another request") },
		"launch audit of another report":  func(c *e2eCompletion) { c.Units[1].Audit.ReportHash = r.Digest(r.Report{}) },
		"launch audit of another source receipt": func(c *e2eCompletion) {
			c.Units[2].Audit.SourceHash = r.Digest((*r.SourceAccess)(nil))
		},
		"launch audit of another stage":            func(c *e2eCompletion) { c.Units[3].Audit.Stage = "result" },
		"codex launch audit without its code host": func(c *e2eCompletion) { c.Units[0].Audit.CodeHost = nil },
		"launch unit never aborted":                func(c *e2eCompletion) { c.Units[1].Aborted = false },
		"launch unit start never settled":          func(c *e2eCompletion) { c.Units[2].StartSettled = false },
		"launch audit absent":                      func(c *e2eCompletion) { c.Units[3].Audit = nil },
		// Production's artifact evidence validation and the audited
		// reviewer's evidence requirement.
		"artifact not requiring provider evidence": func(c *e2eCompletion) { artifact(c, 0).ProviderEvidenceRequired = false },
		"artifact evidence over the protocol limits": func(c *e2eCompletion) {
			for _, e := range []*r.ProviderEvidence{artifact(c, 1).ProviderEvidence, job(c).ProviderAttempts[1].Evidence} {
				e.Access.RequestBytes = e.Policy.Limits.TotalBytes + 2
			}
			c.Units[1].EvidenceHash, c.Units[1].Access = e2eEvidenceHash(artifact(c, 1).ProviderEvidence), artifact(c, 1).ProviderEvidence.Access
		},
		// Each artifact is the session its job derives: R2 sessions freeze
		// the R1 reports.
		"R1 report changed after R2 froze it": func(c *e2eCompletion) { artifact(c, 0).Report.Usage.Turns = 7; recheckpoint(c) },
		"artifact of an underived session":    func(c *e2eCompletion) { artifact(c, 3).InputHash = r.Digest("underived session") },
		// The engine's validation reads are exactly the one canned anchor's.
		"artifact with other validation reads": func(c *e2eCompletion) {
			artifact(c, 0).ValidationReads = append(artifact(c, 0).ValidationReads, r.ReadRecord{Revision: c.Heads[1], Path: "pr1-only.go", Hash: r.Digest([]byte("pull request 1 only\n")), Bytes: 20})
			recheckpoint(c)
		},
		"artifact without validation reads": func(c *e2eCompletion) { artifact(c, 1).ValidationReads = []r.ReadRecord{}; recheckpoint(c) },
		// A completed job released its lease and reservation; its last
		// attempt is its Claude R2 phase's, and both providers were audited.
		"completed job still leased":              func(c *e2eCompletion) { job(c).LeaseToken = "lease" },
		"completed job lease deadline kept":       func(c *e2eCompletion) { job(c).LeaseUntil = time.Unix(2000, 0).UTC() },
		"completed job still outstanding":         func(c *e2eCompletion) { job(c).Outstanding = true },
		"completed job with a pending charge":     func(c *e2eCompletion) { job(c).AttemptChargeDay = "2026-10-23" },
		"completed job without a charge day":      func(c *e2eCompletion) { job(c).ChargeDays = nil },
		"completed job's last attempt another's":  func(c *e2eCompletion) { job(c).AttemptID = strings.Repeat("6", 32) },
		"completed job audited one provider":      func(c *e2eCompletion) { job(c).AuditedProviders = []r.Provider{r.Codex} },
		"completed job without required evidence": func(c *e2eCompletion) { job(c).AttemptProviderEvidenceRequired = false },
		// Source: a stop proved by a later boot is not this run's
		// captured stop; restore binds a staged export to its job's packet.
		"source record stopped through a later boot": func(c *e2eCompletion) { c.Records[0].StopBoot = "later boot" },
		"source record of another boot":              func(c *e2eCompletion) { c.Records[0].BootID = "earlier boot" },
		"staged export of another packet":            func(c *e2eCompletion) { c.Exports[0].PacketHash = r.Digest("another packet") },
		// Finding 2: a completed launch must have exported the auth it returned.
		"completed launch never exported its auth": func(c *e2eCompletion) { c.Units[0].AuthExported = false },
		// Finding 3: a launch must be the artifact's provider and carry the
		// artifact's evidence, not merely its session hash.
		"launch of another provider": func(c *e2eCompletion) { c.Units[0].Provider = "claude" },
		"launch with another receipt": func(c *e2eCompletion) {
			c.Units[0].EvidenceHash = r.Digest("another receipt")
		},
		// The version 2 receipt must name the attempt that recorded the
		// artifact's evidence.
		"launch naming another artifact's attempt": func(c *e2eCompletion) { c.Units[0].AttemptID = c.Units[1].AttemptID },
		"launch naming no attempt":                 func(c *e2eCompletion) { c.Units[0].AttemptID = "" },
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
			write(c).Marker, write(c).Document.Summary = marker, strings.Replace(write(c).Document.Summary, write(c).Marker, marker, 1)
			rehash(c)
			remote(c)["marker"], remote(c)["document_hash"] = r.E2ERemoteReview(write(c).Document.Summary, write(c).Document.Inline)
		},
		"intent body hash not its document": func(c *e2eCompletion) { write(c).BodyHash = r.Digest("other") },
		"intent of another job":             func(c *e2eCompletion) { write(c).JobHash = r.Digest("another job") },
		"intent of another checkpoint":      func(c *e2eCompletion) { write(c).CheckpointHash = r.Digest("another checkpoint") },
		"intent of another request":         func(c *e2eCompletion) { write(c).RequestID = 2 },
		"intent of another generation":      func(c *e2eCompletion) { write(c).Generation = 2 },
		"job of another origin":             rejob(func(j *r.Job) { j.Origin.Sequence = 2 }),
		"document of another origin":        rejob(func(j *r.Job) { j.Origin.Sequence = 2 }),
		"job of another pull origin":        rejob(func(j *r.Job) { j.Origin.PullID = e2ePullID(2) }),
		"job without an origin":             rejob(func(j *r.Job) { j.Origin = nil }),
		"retained request of another origin": func(c *e2eCompletion) {
			c.State.PRs[e2eScopeKey(1)].Requests[0].Origin.Sequence = 2
		},
		"retained request lost":                     func(c *e2eCompletion) { c.State.PRs[e2eScopeKey(1)].Requests = nil },
		"job of another provider timeout":           rejob(func(j *r.Job) { j.Config.Limits.Timeout = time.Minute }),
		"document of another configuration":         rejob(func(j *r.Job) { j.Config.Limits.Timeout = time.Minute }),
		"job of another quota wait":                 rejob(func(j *r.Job) { j.Config.QuotaWait = time.Hour }),
		"job of another daily review limit":         rejob(func(j *r.Job) { j.Config.DailyReviews = 2 }),
		"job of another retention":                  rejob(func(j *r.Job) { j.Config.RetentionDays = 3 }),
		"job of another configuration version":      rejob(func(j *r.Job) { j.ConfigVersion = 2 }),
		"document of another configuration version": rejob(func(j *r.Job) { j.ConfigVersion = 2 }),
		"job of another ID":                         rejob(func(j *r.Job) { j.ID = "native-1-1" }),
		"intent of another base":                    func(c *e2eCompletion) { write(c).Snapshot.Base = strings.Repeat("9", 40) },
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
		// Round 9 finding 1: the stored reports are the canned backend's
		// known output, not merely reports at the right location.
		"report with another finding title": func(c *e2eCompletion) { finding(c, 0).Title = "Altered finding"; recheckpoint(c) },
		"report with another finding body":  func(c *e2eCompletion) { finding(c, 1).Body = "Altered body."; recheckpoint(c) },
		"report with another severity":      func(c *e2eCompletion) { finding(c, 0).Severity = 1; recheckpoint(c) },
		"report with another quote":         func(c *e2eCompletion) { finding(c, 1).Anchor.Quote = "immutable"; recheckpoint(c) },
		"report with another anchor revision": func(c *e2eCompletion) {
			finding(c, 0).Anchor.Revision = strings.Repeat("9", 40)
			recheckpoint(c)
		},
		"report without its peer confirmation": func(c *e2eCompletion) {
			artifact(c, 2).Report.Judgments = []r.Judgment{}
			recheckpoint(c)
		},
		"report rejecting its peer's finding": func(c *e2eCompletion) {
			artifact(c, 3).Report.Judgments[0].Verdict = "reject"
			recheckpoint(c)
		},
		"report with a retraction": func(c *e2eCompletion) {
			artifact(c, 2).Report.Retractions = []string{"codex:fixture"}
			recheckpoint(c)
		},
		"report with coverage": func(c *e2eCompletion) {
			artifact(c, 1).Report.Coverage = []r.Coverage{{Path: "README.md", Reason: "not read"}}
			recheckpoint(c)
		},
		"artifact with engine-added coverage": func(c *e2eCompletion) {
			artifact(c, 0).Coverage = []r.Coverage{{Path: "README.md", Reason: "not read"}}
			recheckpoint(c)
		},
		"report of another packet": func(c *e2eCompletion) { artifact(c, 0).Report.PacketHash = r.Digest("another packet"); recheckpoint(c) },
		"report of another policy": func(c *e2eCompletion) { artifact(c, 3).Report.PolicyVersion = "alpha-policy-0"; recheckpoint(c) },
		// The intent and the remote review agree on an altered document.
		"document with another finding title": func(c *e2eCompletion) {
			republish(c, func(s string) string { return strings.ReplaceAll(s, "Synthetic finding", "Altered finding") })
		},
		"document with another finding body": func(c *e2eCompletion) {
			republish(c, func(s string) string { return strings.ReplaceAll(s, "Protocol fixture only.", "Altered body.") })
		},
		"document with another severity": func(c *e2eCompletion) {
			republish(c, func(s string) string { return strings.ReplaceAll(s, "P2 Synthetic", "P1 Synthetic") })
		},
		"document with another source revision": func(c *e2eCompletion) {
			republish(c, func(s string) string { return strings.ReplaceAll(s, c.Heads[1], strings.Repeat("9", 40)) })
		},
		"document with another summary": func(c *e2eCompletion) {
			d := &write(c).Document
			d.Summary = strings.Replace(d.Summary, "## Coverage", "Additional unreviewed claim.\n\n## Coverage", 1)
			rehash(c)
			_, remote(c)["document_hash"] = r.E2ERemoteReview(d.Summary, d.Inline)
		},
		"document without the peer confirmation": func(c *e2eCompletion) {
			republish(c, func(s string) string {
				return strings.ReplaceAll(s, "confirm: Synthetic source-backed protocol disposition.", "")
			})
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

// TestE2ENativeSessionRecovery judges close-provider-recover's launches: PR
// 1's interrupted launch names its recovered attempt, and PR 2's four
// completed launches name their own recorded attempts.
func TestE2ENativeSessionRecovery(t *testing.T) {
	type recovery struct {
		units  []e2eNativeUnit
		j1, j2 *r.DurableJob
		jobs   []*r.DurableJob
	}
	fixture := func() *recovery {
		c := e2eCompletedFixture()
		j2 := e2eJobFor(c.State, 1)
		// j1 is another job, so its derived sessions differ from j2's.
		j1, recoveredAttempt, launch := e2eFixtureJob(), strings.Repeat("b", 32), strings.Repeat("0", 32)
		j1.Job.ID = "request-0-1"
		j1.Checkpoint.JobHash = r.Digest(j1.Job)
		session := e2eFixtureSession(j1, r.Codex, 1)
		evidence := e2eFixtureEvidence(session, false)
		recorded := *evidence
		j1.State, j1.Checkpoint.ConsumedSessions = "auth_failed", 1
		j1.AttemptProvider, j1.AttemptRound, j1.AttemptID, j1.AttemptInputHash = r.Codex, 1, recoveredAttempt, r.Digest(session)
		j1.ProviderAttempts = []r.ProviderAttempt{{ID: recoveredAttempt, Evidence: &recorded}}
		j1.ProviderRecoveries = []r.ProviderRecoveryReceipt{{AttemptID: recoveredAttempt, LaunchID: launch, Provider: r.Codex, Auth: r.ProviderAuthRetired, State: "auth_failed"}}
		units := append([]e2eNativeUnit{e2eLaunchFixture(j1, session, launch, recoveredAttempt, evidence, r.Report{}, nil, false, false)}, c.Units...)
		return &recovery{units, j1, j2, []*r.DurableJob{j1, j2}}
	}
	verdict := func(c *recovery) e2eVerdict { return e2eNativeSessionVerdict(c.units, c.jobs, c.units[0].SessionHash) }
	if v := verdict(fixture()); !v.OK {
		t.Fatal("recovered launch beside the next review refused:", v.Message)
	}
	for name, edit := range map[string]func(*recovery){
		"interrupted launch judged without its job": func(c *recovery) { c.jobs = []*r.DurableJob{c.j2} },
		"interrupted launch names a PR 2 attempt": func(c *recovery) {
			c.units[0].AttemptID, c.units[0].EvidenceHash = c.units[1].AttemptID, c.units[1].EvidenceHash
		},
		"PR 2 launch names the recovered attempt": func(c *recovery) {
			c.units[1].AttemptID, c.units[1].EvidenceHash = c.units[0].AttemptID, c.units[0].EvidenceHash
		},
		"recovered attempt evidence unrecorded": func(c *recovery) { c.j1.ProviderAttempts = nil },
		"recovered attempt recorded by PR 2's job too": func(c *recovery) {
			c.j2.ProviderAttempts = append(c.j2.ProviderAttempts, c.j1.ProviderAttempts[0])
		},
		"second interrupted launch": func(c *recovery) { c.units = append(c.units, c.units[0]) },
	} {
		c := fixture()
		edit(c)
		if verdict(c).OK {
			t.Error(name, "accepted")
		}
	}
}

// e2eRecoveredFixture is a valid close-provider-recover end state: PR 2's
// completed review (e2eReviewFixture) beside PR 1's recovered job, which is
// close-provider's retained job (e2eRetainedFixture) released as auth_failed
// with one recovery receipt appended, so it records only its leased attempt
// with the interrupted launch's stopped evidence; the interrupted launch,
// whose version 2 receipt names that attempt, counts its one held inference,
// forwarded and never answered, and records the attempt's request, the zero
// report and no source receipt; and PR 1's retained source record and
// export, acquired by the earlier process in this boot. The attached
// process's only Git exchanges are PR 2's acquisition, sampled while PR 2's
// attempt was live. It returns the end state, the retained job as the
// attached root held it, and the interrupted launch.
func e2eRecoveredFixture() (e2eCompletion, *r.DurableJob, e2eNativeUnit) {
	return e2eRecoveredFixtureOf(nil, nil)
}

// e2eRecoveredFixtureOf is e2eRecoveredFixture for PR 1 and PR 2 jobs that
// admission persisted with edit1 and edit2 applied, every record derived from
// either job consistently rebuilt from it (e2eRetainedFixtureOf,
// e2eReviewFixtureOf). The harness's own inputs are not edited: the
// configuration, the release, the fake upstream's heads and the retained
// admitted requests.
func e2eRecoveredFixtureOf(edit1, edit2 func(*r.Job)) (e2eCompletion, *r.DurableJob, e2eNativeUnit) {
	c := e2eReviewFixtureOf(2, edit2)
	units, before := e2eRetainedFixtureOf(edit1)
	_, j1 := e2eRetainedFixtureOf(edit1)
	interrupted, head, source := units[0], before.Job.Snapshot.Head, strings.Repeat("d", 32)
	snapshot := before.Job.Snapshot
	j1.State, j1.LeaseToken, j1.LeaseUntil, j1.Outstanding, j1.AttemptChargeDay = "auth_failed", "", time.Time{}, false, ""
	j1.ProviderRecoveries = []r.ProviderRecoveryReceipt{{AttemptID: before.AttemptID, LaunchID: interrupted.Launch, Provider: r.Codex, Round: 1, EvidenceHash: interrupted.EvidenceHash, Auth: r.ProviderAuthRetired, State: "auth_failed", RecoveredAt: time.Unix(6000, 0).UTC()}}
	c.State.Jobs[j1.Job.ID] = j1
	// Recovery released the job, not the admitted request it came from.
	request := e2eFixtureRequest(e2eFixtureJob())
	c.State.PRs[e2eScopeKey(1)] = &r.PRRecord{Scope: e2eScope(1), Generation: 1, Requests: []r.NativeRequest{request}, CurrentRequest: request.ID}
	c.Heads[1], c.Posts[1] = head, 0
	c.Units = append(c.Units, interrupted)
	c.Attached = true
	c.Records = append(c.Records, r.E2ESourceRecord{Attempt: source, Unit: "sfl-alpha-" + source + ".service", InvocationID: strings.Repeat("8", 32), RequestHash: strings.Repeat("4", 64), SourceRequestHash: strings.Repeat("5", 64), StubHash: c.Source.Stub, ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot, StartIntent: true, StartSettled: true, Captured: true, Stopped: true, Revoked: true, GrantHash: "grant", BootID: e2eTestBootID})
	c.Exports = append(c.Exports, r.E2EExportRecord{Attempt: source, State: "staged", RequestHash: strings.Repeat("5", 64), ReleaseHash: c.Source.Release, RuntimeHash: c.Source.Runtime, Snapshot: snapshot, PacketHash: r.Digest(j1.Job.Packet)})
	return c, before, interrupted
}

// TestE2ERecoveredVerdicts holds close-provider-recover's end state to the
// completed-case bindings: exact jobs and intents per PR, PR 1's recovered
// job bound to its retained job, attempt, launch and evidence, PR 2's full
// review, both source records and every launch bound to its recorded attempt.
func TestE2ERecoveredVerdicts(t *testing.T) {
	type recovery struct {
		c           e2eCompletion
		before      *r.DurableJob
		interrupted e2eNativeUnit
	}
	failures := func(in recovery) []string {
		verdicts, _, _ := e2eRecoveredVerdicts(in.c, in.before, in.interrupted)
		return e2eFailures(verdicts)
	}
	fixture := func() recovery {
		c, before, interrupted := e2eRecoveredFixture()
		return recovery{c, before, interrupted}
	}
	if f := failures(fixture()); len(f) != 0 {
		t.Fatal("recovered end state refused:", f)
	}
	if _, j1, j2 := e2eRecoveredVerdicts(fixture().c, fixture().before, fixture().interrupted); j1 == nil || j2 == nil || j1.Scope.PR != 1 || j2.Scope.PR != 2 {
		t.Fatal("recovered jobs not returned", j1, j2)
	}
	j1 := func(in *recovery) *r.DurableJob { return in.c.State.Jobs["request-1-1"] }
	j2 := func(in *recovery) *r.DurableJob { return in.c.State.Jobs["request-2-1"] }
	w2 := func(in *recovery) *r.WriteIntent {
		for _, w := range in.c.State.Writes {
			if w.Scope == e2eScope(2) {
				return w
			}
		}
		return nil
	}
	last := func(in *recovery) int { return len(in.c.Units) - 1 }
	refusal := map[string]string{
		"PR 1 review intent":                      "exactly the review intents",
		"PR 1 acknowledgement":                    "exactly the review intents",
		"second PR 2 intent":                      "exactly the review intents",
		"second PR 1 job":                         "exactly the jobs",
		"job for another PR":                      "exactly the jobs",
		"PR 1 job of another snapshot":            "is the retained job",
		"PR 1 job of another request":             "is the retained job",
		"PR 1 job still leased":                   "released as auth_failed",
		"PR 1 job requeued":                       "released as auth_failed",
		"PR 1 attempt still outstanding":          "released as auth_failed",
		"interrupted session refunded":            "stays charged",
		"PR 1 artifact":                           "stays charged",
		"PR 1 source access invented":             "stays charged",
		"second PR 1 provider attempt":            "records only the leased attempt",
		"PR 1 evidence differs from the launch":   "records only the leased attempt",
		"PR 1 attempt of another ID":              "records only the leased attempt",
		"no recovery receipt":                     "recovery receipt",
		"second recovery receipt":                 "recovery receipt",
		"recovery receipt of another launch":      "recovery receipt",
		"recovery receipt of other evidence":      "recovery receipt",
		"recovery receipt of another round":       "recovery receipt",
		"recovery receipt with auth returned":     "recovery receipt",
		"PR 1 review POSTed":                      "PR 1 received no review POST",
		"PR 1 remote review":                      "PR 1 received no review POST",
		"interrupted launch gone":                 "still present",
		"interrupted receipt rewritten":           "still present",
		"retained source record missing":          "source records with matching",
		"retained source record of another head":  "source records with matching",
		"retained source record of a release":     "source records with matching",
		"retained source record of a runtime":     "source records with matching",
		"retained source record not stopped":      "final record holds its start intent",
		"interrupted launch names a PR 2 attempt": "job-bound sessions",
		"PR 2 launch names the recovered attempt": "job-bound sessions",
		"second interrupted launch":               "job-bound sessions",
		"PR 2 review never POSTed":                "PR 2 received exactly one review POST",
		"PR 2 checked as another round":           "checked once by its own provider's backend",
		"PR 2 missing a round":                    "exactly Codex R1",
		// Round 8 rules applied to the recovery case.
		"PR 2 phase served PR 1's file":                 "made exactly the fixture's exchanges",
		"PR 2 phase served other bytes":                 "made exactly the fixture's exchanges",
		"PR 2 backend counted another inference":        "made exactly the fixture's exchanges",
		"PR 2 receipt differs from its backend":         "made exactly the fixture's exchanges",
		"PR 2 codex metadata not captured":              "made exactly the fixture's exchanges",
		"PR 1 interrupted session checked again":        "checked only PR 2's",
		"PR 2 session held in the recovery process":     "checked only PR 2's",
		"interrupted receipt answered its inference":    "one held inference",
		"interrupted receipt counts two inferences":     "one held inference",
		"retained source record of another stub":        "source records with matching",
		"PR 2 source record of another stub":            "source records with matching",
		"PR 2 acquisition never sampled":                "PR 2's acquisition",
		"PR 2 acquisition sample failed":                "PR 2's acquisition",
		"PR 2 acquisition sampled after revocation":     "PR 2's acquisition",
		"PR 2 acquisition sampled before its grant":     "PR 2's acquisition",
		"PR 2 acquisition fetched twice":                "PR 2's acquisition",
		"stray entry in the attached native root":       "job-bound sessions",
		"unreadable launch in the attached native root": "job-bound sessions",
		// Round 9 rules applied to the recovery case: PR 2's stored reports
		// and published document are the canned ones.
		"PR 2 report with another finding title":   "PR 2 artifacts hold exactly the canned backend's reports",
		"PR 2 report rejecting its peer's finding": "PR 2 artifacts hold exactly the canned backend's reports",
		"PR 2 document with another finding body":  "PR 2 confirmed document is exactly the one rendered from the canned reports",
		"PR 2 document without its coverage":       "PR 2 confirmed document is exactly the one rendered from the canned reports",
		// Round 10 rules applied to the recovery case: the recovered job is
		// the retained job with only the released fields and one receipt
		// changed, each job holds exactly its own attempts, the interrupted
		// launch's audit binds the recovered attempt, and every source
		// record and export is this boot's and its job's.
		"PR 1 job keeps its pending charge day":       "only its state, lease, deadline, reservation and pending charge released",
		"PR 1 job lost its charge day":                "only its state, lease, deadline, reservation and pending charge released",
		"PR 1 job audited another provider":           "only its state, lease, deadline, reservation and pending charge released",
		"recovery receipt without a recovery time":    "recovery receipt",
		"PR 1 source attempt of its leased attempt":   "the interrupted launch's attempt and no source attempt",
		"interrupted audit of a report":               "the interrupted launch's audit binds",
		"interrupted audit of a source receipt":       "the interrupted launch's audit binds",
		"interrupted audit of another request":        "the interrupted launch's audit binds",
		"interrupted audit at the validation stage":   "the interrupted launch's audit binds",
		"interrupted audit of another failure":        "the interrupted launch's audit binds",
		"interrupted audit names another attempt":     "the interrupted launch's audit binds",
		"PR 2 holds the interrupted attempt":          "PR 2's job holds exactly its four artifact attempts",
		"PR 2 holds an unrelated source attempt":      "PR 2's job holds exactly its four artifact attempts",
		"PR 2 recovery receipt":                       "PR 2's job holds exactly its four artifact attempts",
		"PR 2 job keeps its lease":                    "PR 2 job holds no lease, reservation or pending charge",
		"PR 2 export of another packet":               "source records with matching",
		"retained source record of another boot":      "final record holds its start intent",
		"retained source record proved by later boot": "final record holds its start intent",
		// Round 11 rules applied to the recovery case: both jobs are held to
		// the jobs admission derives from the harness's inputs, PR 1's from
		// its retained open request and PR 2's from the fake upstream's owner
		// comment, so a consistently rebuilt wrong job is refused, and PR 2's
		// remote review is bound to the fake upstream's head.
		"PR 1 job of another origin":                "PR 1's recovered job is exactly the one admission derives",
		"PR 1 job without an origin":                "PR 1's recovered job is exactly the one admission derives",
		"PR 1 job of another provider timeout":      "PR 1's recovered job is exactly the one admission derives",
		"PR 1 job of another quota wait":            "PR 1's recovered job is exactly the one admission derives",
		"PR 1 job of another configuration version": "PR 1's recovered job is exactly the one admission derives",
		"PR 1 job of another requester":             "PR 1's recovered job is exactly the one admission derives",
		"PR 1 retained request of another origin":   "PR 1's recovered job is exactly the one admission derives",
		"PR 1 retained request lost":                "PR 1's recovered job is exactly the one admission derives",
		"PR 2 job of another comment":               "PR 2 job carries exactly the complete origin",
		"PR 2 job of another pull origin":           "PR 2 job carries exactly the complete origin",
		"PR 2 job of an automatic origin":           "PR 2 job carries exactly the complete origin",
		"PR 2 job of another quota wait":            "configuration the harness opened the store with",
		"PR 2 job of another configuration version": "configuration the harness opened the store with",
		"PR 2 job of another ID":                    "exactly the one admission derives",
		"PR 2 retained request of another comment":  "PR 2 job carries exactly the complete origin",
		"PR 2 remote review on another commit":      "PR 2 remote review is one marked COMMENT on its head",
		"PR 2 reviewed at a head upstream lacks":    "PR 2 job binds its own scope, head",
	}
	rejob := func(edit1, edit2 func(*r.Job)) func(*recovery) {
		return func(in *recovery) {
			c, before, interrupted := e2eRecoveredFixtureOf(edit1, edit2)
			*in = recovery{c, before, interrupted}
		}
	}
	for name, edit := range map[string]func(*recovery){
		"PR 1 job of another origin":                rejob(func(j *r.Job) { j.Origin.Sequence = 2 }, nil),
		"PR 1 job without an origin":                rejob(func(j *r.Job) { j.Origin = nil }, nil),
		"PR 1 job of another provider timeout":      rejob(func(j *r.Job) { j.Config.Limits.Timeout = time.Minute }, nil),
		"PR 1 job of another quota wait":            rejob(func(j *r.Job) { j.Config.QuotaWait = time.Hour }, nil),
		"PR 1 job of another configuration version": rejob(func(j *r.Job) { j.ConfigVersion = 2 }, nil),
		"PR 1 job of another requester":             rejob(func(j *r.Job) { j.RequesterID++ }, nil),
		"PR 1 retained request of another origin": func(in *recovery) {
			in.c.State.PRs[e2eScopeKey(1)].Requests[0].Origin.Sequence = 2
		},
		"PR 1 retained request lost":                func(in *recovery) { in.c.State.PRs[e2eScopeKey(1)].Requests = nil },
		"PR 2 job of another comment":               rejob(nil, func(j *r.Job) { j.Origin.CommentID++ }),
		"PR 2 job of another pull origin":           rejob(nil, func(j *r.Job) { j.Origin.PullID = e2ePullID(1) }),
		"PR 2 job of an automatic origin":           rejob(nil, func(j *r.Job) { j.Origin = e2eFixtureOrigin() }),
		"PR 2 job of another quota wait":            rejob(nil, func(j *r.Job) { j.Config.QuotaWait = time.Hour }),
		"PR 2 job of another configuration version": rejob(nil, func(j *r.Job) { j.ConfigVersion = 2 }),
		"PR 2 job of another ID":                    rejob(nil, func(j *r.Job) { j.ID = "native-2-1" }),
		"PR 2 retained request of another comment": func(in *recovery) {
			in.c.State.PRs[e2eScopeKey(2)].Requests[0].Origin.CommentID++
		},
		"PR 2 remote review on another commit":   func(in *recovery) { in.c.Reviews[2][0]["commit_id"] = strings.Repeat("9", 40) },
		"PR 2 reviewed at a head upstream lacks": func(in *recovery) { in.c.Heads[2] = strings.Repeat("9", 40) },
		"PR 1 review intent": func(in *recovery) {
			in.c.State.Writes["w1"] = &r.WriteIntent{ID: "w1", Kind: "review", Scope: e2eScope(1), State: "prepared"}
		},
		"PR 1 acknowledgement": func(in *recovery) {
			in.c.State.Writes["w1"] = &r.WriteIntent{ID: "w1", Kind: "ack", Scope: e2eScope(1), State: "confirmed"}
		},
		"second PR 2 intent": func(in *recovery) {
			in.c.State.Writes["w2"] = &r.WriteIntent{ID: "w2", Kind: "review", Scope: e2eScope(2), State: "unknown"}
		},
		"second PR 1 job": func(in *recovery) {
			in.c.State.Jobs["request-1-2"] = &r.DurableJob{Job: r.Job{ID: "request-1-2"}, Scope: e2eScope(1), State: "queued"}
		},
		"job for another PR": func(in *recovery) {
			in.c.State.Jobs["request-3-1"] = &r.DurableJob{Job: r.Job{ID: "request-3-1"}, Scope: e2eScope(3), State: "queued"}
		},
		"PR 1 job of another snapshot":   func(in *recovery) { j1(in).Job.Snapshot.Head = strings.Repeat("9", 40) },
		"PR 1 job of another request":    func(in *recovery) { j1(in).RequestID = 2 },
		"PR 1 job still leased":          func(in *recovery) { j1(in).LeaseToken = "lease" },
		"PR 1 job requeued":              func(in *recovery) { j1(in).State = "queued" },
		"PR 1 attempt still outstanding": func(in *recovery) { j1(in).Outstanding = true },
		"interrupted session refunded":   func(in *recovery) { j1(in).Checkpoint.ConsumedSessions = 0 },
		"PR 1 artifact": func(in *recovery) {
			j1(in).Checkpoint.Artifacts = []r.Artifact{{InputHash: in.interrupted.SessionHash}}
		},
		"PR 1 source access invented": func(in *recovery) {
			j1(in).SourceAttempts = []r.SourceAttempt{{ID: in.before.AttemptID, Access: &r.SourceAccess{Version: 1, SessionHash: in.interrupted.SessionHash}}}
		},
		"second PR 1 provider attempt": func(in *recovery) {
			j1(in).ProviderAttempts = append(j1(in).ProviderAttempts, r.ProviderAttempt{ID: strings.Repeat("f", 32), Evidence: &r.ProviderEvidence{Access: r.ProviderAccess{Version: 1, Stopped: true}}})
		},
		"PR 1 evidence differs from the launch": func(in *recovery) {
			j1(in).ProviderAttempts[0].Evidence.Access.InferenceRequests = 2
		},
		"PR 1 attempt of another ID": func(in *recovery) { j1(in).ProviderAttempts[0].ID = strings.Repeat("f", 32) },
		"no recovery receipt":        func(in *recovery) { j1(in).ProviderRecoveries = nil },
		"second recovery receipt": func(in *recovery) {
			j1(in).ProviderRecoveries = append(j1(in).ProviderRecoveries, j1(in).ProviderRecoveries[0])
		},
		"recovery receipt of another launch": func(in *recovery) { j1(in).ProviderRecoveries[0].LaunchID = "other" },
		"recovery receipt of other evidence": func(in *recovery) { j1(in).ProviderRecoveries[0].EvidenceHash = r.Digest("other") },
		"recovery receipt of another round":  func(in *recovery) { j1(in).ProviderRecoveries[0].Round = 2 },
		"recovery receipt with auth returned": func(in *recovery) {
			j1(in).ProviderRecoveries[0].Auth = r.ProviderAuthReturned
		},
		"PR 1 review POSTed": func(in *recovery) { in.c.Posts[1] = 1 },
		"PR 1 remote review": func(in *recovery) {
			in.c.Reviews[1] = []map[string]any{{"state": "COMMENTED", "commit_id": in.c.Heads[1]}}
		},
		"interrupted launch gone":       func(in *recovery) { in.c.Units = in.c.Units[:last(in)] },
		"interrupted receipt rewritten": func(in *recovery) { in.c.Units[last(in)].AuthReturned = true },
		"retained source record missing": func(in *recovery) {
			in.c.Records, in.c.Exports = in.c.Records[:1], in.c.Exports[:1]
		},
		"retained source record of another head": func(in *recovery) {
			in.c.Records[1].Snapshot.Head, in.c.Exports[1].Snapshot.Head = strings.Repeat("9", 40), strings.Repeat("9", 40)
		},
		"retained source record of a release": func(in *recovery) {
			in.c.Records[1].ReleaseHash, in.c.Exports[1].ReleaseHash = r.Digest("earlier release"), r.Digest("earlier release")
		},
		"retained source record of a runtime": func(in *recovery) {
			in.c.Records[1].RuntimeHash, in.c.Exports[1].RuntimeHash = r.Digest("earlier runtime"), r.Digest("earlier runtime")
		},
		"retained source record not stopped": func(in *recovery) { in.c.Records[1].Stopped = false },
		"interrupted launch names a PR 2 attempt": func(in *recovery) {
			in.c.Units[last(in)].AttemptID, in.c.Units[last(in)].EvidenceHash = in.c.Units[0].AttemptID, in.c.Units[0].EvidenceHash
			in.interrupted = in.c.Units[last(in)]
		},
		"PR 2 launch names the recovered attempt": func(in *recovery) {
			in.c.Units[0].AttemptID, in.c.Units[0].EvidenceHash = in.interrupted.AttemptID, in.interrupted.EvidenceHash
		},
		"second interrupted launch":     func(in *recovery) { in.c.Units = append(in.c.Units, in.interrupted) },
		"PR 2 review never POSTed":      func(in *recovery) { in.c.Posts[2] = 0 },
		"PR 2 checked as another round": func(in *recovery) { in.c.Sessions[0].Round = 2 },
		"PR 2 missing a round": func(in *recovery) {
			j2(in).Checkpoint.Artifacts = j2(in).Checkpoint.Artifacts[1:]
		},
		"PR 2 phase served PR 1's file": func(in *recovery) {
			j2(in).Checkpoint.Artifacts[0].SourceAccess.Reads[1] = r.ReadRecord{Revision: in.c.Heads[2], Path: "pr1-only.go", Hash: r.Digest([]byte("pull request 1 only\n")), Bytes: 20}
		},
		"PR 2 phase served other bytes": func(in *recovery) {
			j2(in).Checkpoint.Artifacts[1].SourceAccess.Reads[0].Hash = r.Digest("other bytes")
		},
		"PR 2 backend counted another inference": func(in *recovery) { in.c.Sessions[2].Counts.Inference = 4 },
		"PR 2 receipt differs from its backend": func(in *recovery) {
			j2(in).Checkpoint.Artifacts[0].ProviderEvidence.Access.MetadataRequests = 4
		},
		"PR 2 codex metadata not captured": func(in *recovery) { in.c.Sessions[0].Counts.MetadataCaptured = false },
		"PR 1 interrupted session checked again": func(in *recovery) {
			in.c.Sessions = append(in.c.Sessions, e2eModelSession{JobID: in.before.Job.ID, Provider: r.Codex, Round: 1, PR: 1, Head: in.c.Heads[1], SessionHash: in.interrupted.SessionHash, HeldInCheck: true})
		},
		"PR 2 session held in the recovery process": func(in *recovery) {
			held := in.c.Sessions[0]
			held.HeldInCheck = true
			in.c.Sessions = append(in.c.Sessions, held)
		},
		"interrupted receipt answered its inference": func(in *recovery) {
			in.interrupted.Access.Responses++
			in.c.Units[last(in)] = in.interrupted
		},
		"interrupted receipt counts two inferences": func(in *recovery) {
			in.interrupted.Access.InferenceRequests++
			in.c.Units[last(in)] = in.interrupted
		},
		"retained source record of another stub": func(in *recovery) { in.c.Records[1].StubHash = r.Digest("another stub") },
		"PR 2 source record of another stub":     func(in *recovery) { in.c.Records[0].StubHash = "" },
		"PR 2 acquisition never sampled":         func(in *recovery) { in.c.Exchanges = nil },
		"PR 2 acquisition sample failed": func(in *recovery) {
			in.c.Exchanges[1].Journal, in.c.Exchanges[1].JournalErr = nil, "journal sample timed out"
		},
		"PR 2 acquisition sampled after revocation": func(in *recovery) { in.c.Exchanges[1].Journal[0].Revoked = true },
		"PR 2 acquisition sampled before its grant": func(in *recovery) { in.c.Exchanges[1].Journal[0].GrantHash = "" },
		"PR 2 acquisition fetched twice": func(in *recovery) {
			in.c.Exchanges = append(in.c.Exchanges, in.c.Exchanges[1])
		},
		"stray entry in the attached native root": func(in *recovery) {
			in.c.Units = append(in.c.Units, e2eNativeUnit{Provider: string(r.Codex), Launch: "stray"})
		},
		"unreadable launch in the attached native root": func(in *recovery) {
			in.c.Units = append(in.c.Units, e2eNativeUnit{Provider: string(r.Claude), Launch: strings.Repeat("e", 32)})
		},
		// The intent is rebound to the edited checkpoint, so only the
		// artifacts' binding to the canned reports can refuse it.
		"PR 2 report with another finding title": func(in *recovery) {
			j2(in).Checkpoint.Artifacts[0].Report.Findings[0].Title = "Altered finding"
			w2(in).CheckpointHash = r.Digest(j2(in).Checkpoint)
		},
		"PR 2 report rejecting its peer's finding": func(in *recovery) {
			j2(in).Checkpoint.Artifacts[3].Report.Judgments[0].Verdict = "reject"
			w2(in).CheckpointHash = r.Digest(j2(in).Checkpoint)
		},
		// The intent and the remote review agree on the altered document, so
		// only its binding to the canned reports can refuse it.
		"PR 2 document with another finding body": func(in *recovery) {
			d := &w2(in).Document
			d.Summary = strings.ReplaceAll(d.Summary, e2eCannedBody, "Altered body.")
			for i := range d.Inline {
				d.Inline[i].Body = strings.ReplaceAll(d.Inline[i].Body, e2eCannedBody, "Altered body.")
			}
			w2(in).BodyHash = r.Digest(*d)
			_, in.c.Reviews[2][0]["document_hash"] = r.E2ERemoteReview(d.Summary, d.Inline)
		},
		"PR 2 document without its coverage": func(in *recovery) {
			d := &w2(in).Document
			d.Summary = strings.Replace(d.Summary, "## Coverage", "## Omitted", 1)
			w2(in).BodyHash = r.Digest(*d)
			_, in.c.Reviews[2][0]["document_hash"] = r.E2ERemoteReview(d.Summary, d.Inline)
		},
		"PR 1 job keeps its pending charge day": func(in *recovery) { j1(in).AttemptChargeDay = "2026-10-23" },
		"PR 1 job lost its charge day":          func(in *recovery) { j1(in).ChargeDays = nil },
		"PR 1 job audited another provider": func(in *recovery) {
			j1(in).AuditedProviders = append(j1(in).AuditedProviders, r.Claude)
		},
		"recovery receipt without a recovery time": func(in *recovery) { j1(in).ProviderRecoveries[0].RecoveredAt = time.Time{} },
		"PR 1 source attempt of its leased attempt": func(in *recovery) {
			j1(in).SourceAttempts = []r.SourceAttempt{{ID: in.before.AttemptID, Access: &r.SourceAccess{Version: 1, SessionHash: in.interrupted.SessionHash, Reads: []r.ReadRecord{}, Coverage: []r.Coverage{}}}}
		},
		// The interrupted unit and its projection in c.Units share the
		// audit, so each edit leaves the launch unchanged between them.
		"interrupted audit of a report": func(in *recovery) {
			in.interrupted.Audit.ReportHash = r.Digest(e2eCannedReports(j1(in).Job)[0])
		},
		"interrupted audit of a source receipt": func(in *recovery) {
			in.interrupted.Audit.SourceHash = r.Digest(&r.SourceAccess{Version: 1, SessionHash: in.interrupted.SessionHash})
		},
		"interrupted audit of another request":      func(in *recovery) { in.interrupted.Audit.RequestHash = r.Digest("another request") },
		"interrupted audit at the validation stage": func(in *recovery) { in.interrupted.Audit.Stage = "result-validation" },
		"interrupted audit of another failure":      func(in *recovery) { in.interrupted.Audit.Failure = "cancelled" },
		"interrupted audit names another attempt": func(in *recovery) {
			in.interrupted.AttemptID, in.interrupted.Audit.AttemptID = strings.Repeat("c", 32), strings.Repeat("c", 32)
			in.c.Units[last(in)] = in.interrupted
		},
		"PR 2 holds the interrupted attempt": func(in *recovery) {
			j2(in).ProviderAttempts = append(j2(in).ProviderAttempts, j1(in).ProviderAttempts[0])
		},
		"PR 2 holds an unrelated source attempt": func(in *recovery) {
			j2(in).SourceAttempts = append(j2(in).SourceAttempts, r.SourceAttempt{ID: strings.Repeat("f", 32), Access: j2(in).SourceAttempts[0].Access})
		},
		"PR 2 recovery receipt": func(in *recovery) {
			j2(in).ProviderRecoveries = []r.ProviderRecoveryReceipt{j1(in).ProviderRecoveries[0]}
		},
		"PR 2 job keeps its lease":                    func(in *recovery) { j2(in).LeaseToken = "lease" },
		"PR 2 export of another packet":               func(in *recovery) { in.c.Exports[0].PacketHash = r.Digest("another packet") },
		"retained source record of another boot":      func(in *recovery) { in.c.Records[1].BootID = "another boot" },
		"retained source record proved by later boot": func(in *recovery) { in.c.Records[1].StopBoot = "later boot" },
	} {
		in := fixture()
		edit(&in)
		f, target := failures(in), refusal[name]
		if target == "" || !slices.ContainsFunc(f, func(m string) bool { return strings.Contains(m, target) }) {
			t.Error(name, "not refused by its target check", target, f)
		}
	}
}

// TestE2ESettledRecoveredVerdicts applies the round 9 settled re-read to
// close-provider-recover. The attached process's fake servers are its own:
// they hold only PR 2's acquisition, its four sessions and its one review
// POST, and nothing of what close-provider observed, since PR 1's retained
// attempt and interrupted session were judged by that process. Once
// quiesced they must hold exactly what finish recorded, so a late rerun of
// PR 1's interrupted session, a late PR 1 review or refetch, or a late PR 2
// request is refused.
func TestE2ESettledRecoveredVerdicts(t *testing.T) {
	judged := func() (*e2eResult, e2eGitHubSummary, map[r.Provider][]e2eModelSession, e2eNativeUnit) {
		c, _, interrupted := e2eRecoveredFixture()
		github := e2eGitHubSummary{ReviewPosts: c.Posts, Reviews: c.Reviews, GitExchanges: c.Exchanges}
		models := map[r.Provider][]e2eModelSession{}
		for _, s := range c.Sessions {
			models[s.Provider] = append(models[s.Provider], s)
		}
		res := &e2eResult{GitHub: github, Models: models}
		// The settled reads are fresh copies, as summary and sessionsCopy make.
		var settled e2eGitHubSummary
		var copies map[r.Provider][]e2eModelSession
		raw, _ := json.Marshal(github)
		_ = json.Unmarshal(raw, &settled)
		raw, _ = json.Marshal(models)
		_ = json.Unmarshal(raw, &copies)
		return res, settled, copies, interrupted
	}
	res, github, models, _ := judged()
	if len(github.GitExchanges) == 0 || len(models[r.Codex]) != 2 || len(models[r.Claude]) != 2 || github.ReviewPosts[2] != 1 || github.ReviewPosts[1] != 0 {
		t.Fatal("recovered observations are not PR 2's acquisition, four sessions and one review POST", github.ReviewPosts, len(github.GitExchanges), len(models[r.Codex]), len(models[r.Claude]))
	}
	if f := e2eFailures(e2eSettledVerdicts(res, github, models)); len(f) != 0 {
		t.Fatal("settled recovery refused:", f)
	}
	for name, edit := range map[string]func(*e2eGitHubSummary, map[r.Provider][]e2eModelSession, e2eNativeUnit){
		"PR 1 interrupted session rerun after finish": func(_ *e2eGitHubSummary, m map[r.Provider][]e2eModelSession, u e2eNativeUnit) {
			m[r.Codex] = append(m[r.Codex], e2eModelSession{JobID: "request-1-1", Provider: r.Codex, Round: 1, PR: 1, Head: strings.Repeat("1", 40), SessionHash: u.SessionHash})
		},
		"PR 1 review POST after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession, _ e2eNativeUnit) {
			g.ReviewPosts[1] = 1
		},
		"PR 1 remote review after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession, _ e2eNativeUnit) {
			g.Reviews[1] = []map[string]any{{"id": int64(5002)}}
		},
		"PR 1 refetched after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession, _ e2eNativeUnit) {
			g.GitExchanges = append(g.GitExchanges, e2eGitExchange{Operation: "upload-pack", Wants: []string{strings.Repeat("1", 40), strings.Repeat("e", 40)}, Status: 200})
		},
		"second PR 2 review POST after finish": func(g *e2eGitHubSummary, _ map[r.Provider][]e2eModelSession, _ e2eNativeUnit) {
			g.ReviewPosts[2] = 2
		},
		"PR 2 inference after finish": func(_ *e2eGitHubSummary, m map[r.Provider][]e2eModelSession, _ e2eNativeUnit) {
			m[r.Claude][1].Counts.Inference++
		},
	} {
		res, github, models, interrupted := judged()
		edit(&github, models, interrupted)
		if len(e2eFailures(e2eSettledVerdicts(res, github, models))) == 0 {
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

// e2eRetainedFixture is close-provider's retained state: PR 1's job leased
// with its Codex R1 attempt outstanding and recorded with the interrupted
// launch's stopped evidence, and that launch, whose exported auth was never
// returned and whose audit records the attempt's request, no report and no
// source receipt.
func e2eRetainedFixture() ([]e2eNativeUnit, *r.DurableJob) { return e2eRetainedFixtureOf(nil) }

// e2eRetainedFixtureOf is e2eRetainedFixture for a job that admission
// persisted with edit applied, its attempt, session, evidence and launch
// consistently derived from the edited job.
func e2eRetainedFixtureOf(edit func(*r.Job)) ([]e2eNativeUnit, *r.DurableJob) {
	j := e2eFixtureJob()
	if edit != nil {
		edit(&j.Job)
		j.Checkpoint.JobHash = r.Digest(j.Job)
	}
	j.State, j.LeaseToken, j.LeaseUntil, j.Outstanding, j.Checkpoint.ConsumedSessions = "running", "lease", time.Unix(5000, 0).UTC(), true, 1
	session := e2eFixtureSession(j, r.Codex, 1)
	evidence := e2eFixtureEvidence(session, false)
	j.AttemptProvider, j.AttemptRound, j.AttemptChargeDay, j.AttemptID, j.AttemptInputHash, j.AttemptProviderEvidenceRequired = r.Codex, 1, "2026-10-23", strings.Repeat("b", 32), r.Digest(session), true
	j.AuditedProviders = []r.Provider{r.Codex}
	recorded := *evidence
	j.ProviderAttempts = []r.ProviderAttempt{{ID: j.AttemptID, Evidence: &recorded}}
	return []e2eNativeUnit{e2eLaunchFixture(j, session, strings.Repeat("6", 32), j.AttemptID, evidence, r.Report{}, nil, false, false)}, j
}

func TestE2ERetainedLaunches(t *testing.T) {
	units, j := e2eRetainedFixture()
	launch := units[0].Launch
	if before, err := e2eRetainedLaunches(units, j); err != nil || !maps.Equal(before, map[string]bool{launch: true}) {
		t.Fatal("retained interrupted launch refused", before, err)
	}
	type input struct {
		units []e2eNativeUnit
		job   *r.DurableJob
	}
	edit := func(u func(*e2eNativeUnit), edit func(*r.DurableJob)) input {
		units, j := e2eRetainedFixture()
		if u != nil {
			u(&units[0])
		}
		if edit != nil {
			edit(j)
		}
		return input{units, j}
	}
	_, job := e2eRetainedFixture()
	unit := func() e2eNativeUnit { units, _ := e2eRetainedFixture(); return units[0] }
	for name, in := range map[string]input{
		"launch directory missing":     {nil, job},
		"launch directory unreadable":  {[]e2eNativeUnit{{Provider: "codex", Launch: launch}}, job},
		"second retained launch":       {[]e2eNativeUnit{unit(), unit()}, job},
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
			other := *j.ProviderAttempts[0].Evidence
			other.Access.Responses = 1
			j.ProviderAttempts = append(j.ProviderAttempts, r.ProviderAttempt{ID: j.AttemptID, Evidence: &other})
		}),
		"outstanding attempt without an ID": edit(nil, func(j *r.DurableJob) { j.AttemptID, j.ProviderAttempts[0].ID = "", "" }),
		// Round 10: the retained job holds exactly the interrupted attempt,
		// the unit's lifecycle is production-whole, and the audit records
		// the attempt's derived request, no report and exactly the source
		// receipt the job recorded for it.
		"second provider attempt": edit(nil, func(j *r.DurableJob) {
			other := *j.ProviderAttempts[0].Evidence
			other.Access.Responses = 1
			j.ProviderAttempts = append(j.ProviderAttempts, r.ProviderAttempt{ID: "earlier", Evidence: &other})
		}),
		"source attempt the audit never recorded": edit(nil, func(j *r.DurableJob) {
			j.SourceAttempts = []r.SourceAttempt{{ID: j.AttemptID, Access: &r.SourceAccess{Version: 1, SessionHash: j.AttemptInputHash, Reads: []r.ReadRecord{}, Coverage: []r.Coverage{}}}}
		}),
		"source attempt of another attempt": edit(nil, func(j *r.DurableJob) {
			j.SourceAttempts = []r.SourceAttempt{{ID: "earlier", Access: &r.SourceAccess{Version: 1, SessionHash: j.AttemptInputHash, Reads: []r.ReadRecord{}, Coverage: []r.Coverage{}}}}
		}),
		"attempt session not the job's derivation": func() input {
			// Unit, attempt and recorded evidence agree on a session the
			// job does not derive.
			units, j := e2eRetainedFixture()
			j.AttemptInputHash = strings.Repeat("7", 64)
			j.ProviderAttempts[0].Evidence.Access.SessionHash = j.AttemptInputHash
			units[0].SessionHash, units[0].EvidenceHash = j.AttemptInputHash, e2eEvidenceHash(j.ProviderAttempts[0].Evidence)
			return input{units, j}
		}(),
		"unit start never settled":    edit(func(u *e2eNativeUnit) { u.StartSettled = false }, nil),
		"unit never aborted":          edit(func(u *e2eNativeUnit) { u.Aborted = false }, nil),
		"audit of another request":    edit(func(u *e2eNativeUnit) { u.Audit.RequestHash = r.Digest("another request") }, nil),
		"audit of a report":           edit(func(u *e2eNativeUnit) { u.Audit.ReportHash = r.Digest(e2eCannedReports(job.Job)[0]) }, nil),
		"audit of a source receipt":   edit(func(u *e2eNativeUnit) { u.Audit.SourceHash = r.Digest(&r.SourceAccess{}) }, nil),
		"audit at another stage":      edit(func(u *e2eNativeUnit) { u.Audit.Stage = "auth-export" }, nil),
		"audit of another failure":    edit(func(u *e2eNativeUnit) { u.Audit.Failure = "cancelled" }, nil),
		"audit without a failure":     edit(func(u *e2eNativeUnit) { u.Audit.Failure = "" }, nil),
		"audit absent from the unit":  edit(func(u *e2eNativeUnit) { u.Audit = nil }, nil),
		"audit of another round":      edit(nil, func(j *r.DurableJob) { j.AttemptRound = 2 }),
		"interrupted attempt charged": edit(nil, func(j *r.DurableJob) { j.Checkpoint.ConsumedSessions = 2 }),
		// The version 2 receipt must name the outstanding attempt, and the
		// job must record that attempt's evidence under no other ID.
		"receipt naming no attempt":      edit(func(u *e2eNativeUnit) { u.AttemptID = "" }, nil),
		"receipt naming another attempt": edit(func(u *e2eNativeUnit) { u.AttemptID = strings.Repeat("c", 32) }, nil),
		"attempt ID cleared on both sides": edit(func(u *e2eNativeUnit) { u.AttemptID = "" }, func(j *r.DurableJob) {
			j.AttemptID, j.ProviderAttempts[0].ID = "", ""
		}),
		"receipt repeats other evidence":             edit(func(u *e2eNativeUnit) { u.EvidenceHash = strings.Repeat("7", 64) }, nil),
		"recorded evidence differs from the receipt": edit(nil, func(j *r.DurableJob) { j.ProviderAttempts[0].Evidence.Access.Requests = 2 }),
		"evidence of another attempt beside the job's": edit(nil, func(j *r.DurableJob) {
			j.ProviderAttempts[0].ID = strings.Repeat("c", 32)
			j.ProviderAttempts = append(j.ProviderAttempts, r.ProviderAttempt{ID: j.AttemptID, Evidence: &r.ProviderEvidence{Access: r.ProviderAccess{Version: 1, SessionHash: j.AttemptInputHash, Stopped: true}}})
		}),
	} {
		if _, err := e2eRetainedLaunches(in.units, in.job); err == nil {
			t.Error(name, "accepted as the retained interrupted launch")
		}
	}
}

func TestE2ERequestVerdicts(t *testing.T) {
	const comment = int64(31)
	origin := func(kind, role string, actor int64, pr int) *r.RequestOrigin {
		o := &r.RequestOrigin{Kind: kind, ActorID: actor, ActorRole: role, PullID: e2ePullID(pr)}
		if kind == "comment" {
			o.CommentID = comment
		}
		return o
	}
	state := func() r.DurableState {
		return r.DurableState{PRs: map[string]*r.PRRecord{
			e2eScopeKey(1): {Scope: r.Scope{PR: 1}, Requests: []r.NativeRequest{{ID: 1, Admitted: true, Origin: origin("opened", "pr_author", r.OwnerID, 1)}}},
			e2eScopeKey(2): {Scope: r.Scope{PR: 2}, Requests: []r.NativeRequest{{ID: 1, Origin: origin("comment", "commenter", r.OwnerID, 2)}}},
		}}
	}
	want := map[int]e2eRequest{1: e2eAdmittedOpened(1, 1), 2: e2ePendingComment(2, comment)}
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
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("synchronize", "pr_author", r.OwnerID, 1)})
		},
		"repeated open request on PR 1": func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("opened", "pr_author", r.OwnerID, 1)})
		},
		"second owner comment on PR 2": func(d r.DurableState) {
			pr(d, 2).Requests = append(pr(d, 2).Requests, r.NativeRequest{ID: 2, Origin: origin("comment", "commenter", r.OwnerID, 2)})
		},
		"PR 1 request not the leased job's": func(d r.DurableState) { pr(d, 1).Requests[0].ID = 3 },
		"PR 1 request without origin":       func(d r.DurableState) { pr(d, 1).Requests[0].Origin = nil },
		"PR 1 request removed":              func(d r.DurableState) { pr(d, 1).Requests[0].Removed = true },
		"PR 2 request admitted":             func(d r.DurableState) { pr(d, 2).Requests[0].Admitted = true },
		"PR 2 request automatic":            func(d r.DurableState) { pr(d, 2).Requests[0].Origin = origin("opened", "pr_author", 4242, 2) },
		"PR 2 comment by another actor":     func(d r.DurableState) { pr(d, 2).Requests[0].Origin.ActorID = 4242 },
		// Round 11 sweep: the origin's pull and comment IDs are published in
		// the trigger line, so they must be the fake upstream's.
		"PR 1 request of another pull":    func(d r.DurableState) { pr(d, 1).Requests[0].Origin.PullID = e2ePullID(2) },
		"PR 2 comment of another pull":    func(d r.DurableState) { pr(d, 2).Requests[0].Origin.PullID = e2ePullID(1) },
		"PR 2 comment of another comment": func(d r.DurableState) { pr(d, 2).Requests[0].Origin.CommentID = comment + 1 },
		"PR 2 request lost":               func(d r.DurableState) { pr(d, 2).Requests = nil },
		"PR 2 record lost":                func(d r.DurableState) { delete(d.PRs, e2eScopeKey(2)) },
		"request on another PR": func(d r.DurableState) {
			d.PRs[e2eScopeKey(3)] = &r.PRRecord{Scope: r.Scope{PR: 3}, Requests: []r.NativeRequest{{ID: 1, Origin: origin("comment", "commenter", r.OwnerID, 3)}}}
		},
	} {
		d := state()
		edit(d)
		if len(failures(d)) == 0 {
			t.Error(name, "accepted")
		}
	}
	// close-provider-recover's end: recovery released PR 1's job but kept its
	// admitted request, and PR 2's owner comment became its reviewed job.
	recovered := func() r.DurableState {
		return r.DurableState{PRs: map[string]*r.PRRecord{
			e2eScopeKey(1): {Scope: r.Scope{PR: 1}, Requests: []r.NativeRequest{{ID: 1, Admitted: true, Origin: origin("opened", "pr_author", r.OwnerID, 1)}}},
			e2eScopeKey(2): {Scope: r.Scope{PR: 2}, Requests: []r.NativeRequest{{ID: 7, Admitted: true, Origin: origin("comment", "commenter", r.OwnerID, 2)}}},
		}}
	}
	job := func(pr int, id int64) *r.DurableJob {
		return &r.DurableJob{Job: r.Job{ID: fmt.Sprintf("request-%d-%d", pr, id)}, Scope: r.Scope{PR: pr}, RequestID: id}
	}
	j1, j2 := job(1, 1), job(2, 7)
	judged := func(d r.DurableState, j1, j2 *r.DurableJob) int {
		n := 0
		for _, v := range e2eRecoveredRequestVerdicts(d, j1, j2, comment) {
			if !v.OK {
				n++
			}
		}
		return n
	}
	recoveredFailures := func(d r.DurableState) int { return judged(d, j1, j2) }
	if n := recoveredFailures(recovered()); n != 0 {
		t.Fatal("recovered end state refused:", n)
	}
	// A missing job, or one without a request ID, matches no retained request.
	for name, jobs := range map[string][2]*r.DurableJob{
		"PR 2 job missing":              {j1, nil},
		"PR 2 job without a request ID": {j1, job(2, 0)},
		"PR 1 job missing":              {nil, j2},
		"PR 1 job without a request ID": {job(1, 0), j2},
	} {
		if judged(recovered(), jobs[0], jobs[1]) == 0 {
			t.Error("recovered:", name, "accepted")
		}
	}
	for name, edit := range map[string]func(r.DurableState){
		"PR 1 request no longer admitted":  func(d r.DurableState) { pr(d, 1).Requests[0].Admitted = false },
		"PR 1 request removed by recovery": func(d r.DurableState) { pr(d, 1).Requests[0].Removed = true },
		"PR 1 opened again after recovery": func(d r.DurableState) {
			pr(d, 1).Requests = append(pr(d, 1).Requests, r.NativeRequest{ID: 2, Origin: origin("opened", "pr_author", r.OwnerID, 1)})
		},
		// Round 11: the origins' pull and comment IDs are the fake
		// upstream's, as the trigger line publishes them.
		"PR 1 request of another pull":    func(d r.DurableState) { pr(d, 1).Requests[0].Origin.PullID = e2ePullID(2) },
		"PR 2 comment of another pull":    func(d r.DurableState) { pr(d, 2).Requests[0].Origin.PullID = e2ePullID(1) },
		"PR 2 comment of another comment": func(d r.DurableState) { pr(d, 2).Requests[0].Origin.CommentID = comment + 1 },
		"PR 2 comment never admitted":     func(d r.DurableState) { pr(d, 2).Requests[0].Admitted = false },
		"PR 2 request not the job's":      func(d r.DurableState) { pr(d, 2).Requests[0].ID = 8 },
		"PR 2 comment repeated": func(d r.DurableState) {
			pr(d, 2).Requests = append(pr(d, 2).Requests, r.NativeRequest{ID: 8, Origin: origin("comment", "commenter", r.OwnerID, 2)})
		},
		"PR 2 automatic request recorded": func(d r.DurableState) { pr(d, 2).Requests[0].Origin = origin("opened", "pr_author", r.OwnerID, 2) },
		"request on another PR": func(d r.DurableState) {
			d.PRs[e2eScopeKey(3)] = &r.PRRecord{Scope: r.Scope{PR: 3}, Requests: []r.NativeRequest{{ID: 1, Origin: origin("comment", "commenter", r.OwnerID, 3)}}}
		},
	} {
		d := recovered()
		edit(d)
		if recoveredFailures(d) == 0 {
			t.Error("recovered:", name, "accepted")
		}
	}
}

func TestE2EAttachedJob(t *testing.T) {
	current, earlier := strings.Repeat("a", 40), strings.Repeat("b", 40)
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("1", 40), Base: strings.Repeat("e", 40), MergeBase: strings.Repeat("e", 40)}
	config := r.DefaultConfig()
	// The retained job is the fixture's admitted job (e2eFixtureJob: its
	// origin, owner requester, configuration and version) under the current
	// release, with PR 1 retaining the request it was admitted from.
	states := func(edit func(*r.DurableJob), request func(*r.NativeRequest)) r.DurableState {
		j := e2eFixtureJob()
		j.Job.Release, j.LeaseToken, j.Outstanding = current, "lease", true
		q := r.NativeRequest{Origin: e2eFixtureOrigin(), ID: j.RequestID, Snapshot: snapshot, Admitted: true}
		if edit != nil {
			edit(j)
		}
		if request != nil {
			request(&q)
		}
		return r.DurableState{Jobs: map[string]*r.DurableJob{j.Job.ID: j}, PRs: map[string]*r.PRRecord{e2eScopeKey(1): {Scope: e2eScope(1), Requests: []r.NativeRequest{q}}}}
	}
	state := func(edit func(*r.DurableJob)) r.DurableState { return states(edit, nil) }
	if j, err := e2eAttachedJob(state(nil), current, snapshot, config); err != nil || j == nil || j.Job.ID != "request-1-1" || j.LeaseToken != "lease" {
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
		// Round 11 findings 2 and 3: the retained job is the one admission
		// derives from its retained request and the harness's configuration.
		"job of another origin":                state(func(j *r.DurableJob) { j.Job.Origin.Sequence = 2 }),
		"job without an origin":                state(func(j *r.DurableJob) { j.Job.Origin = nil }),
		"retained request of another origin":   states(nil, func(q *r.NativeRequest) { q.Origin.PullID = e2ePullID(2) }),
		"retained request lost":                states(nil, func(q *r.NativeRequest) { q.ID = 2 }),
		"job of another provider timeout":      state(func(j *r.DurableJob) { j.Job.Config.Limits.Timeout = time.Minute }),
		"job of another quota wait":            state(func(j *r.DurableJob) { j.Job.Config.QuotaWait = time.Hour }),
		"job of another configuration version": state(func(j *r.DurableJob) { j.Job.ConfigVersion = 2 }),
		"job of another ID":                    state(func(j *r.DurableJob) { j.Job.ID = "native-1-1" }),
		"job of another requester":             state(func(j *r.DurableJob) { j.Job.RequesterID++ }),
	} {
		if j, err := e2eAttachedJob(d, current, snapshot, config); err == nil {
			t.Error(name, "accepted as the attached root's interrupted job", j)
		}
	}
}

// TestE2EConfirmedRequestVerdicts covers the request set a confirmed case
// leaves: each reviewed PR retains exactly the admitted request its job came
// from, of the expected origin, and no PR retains any other request. A
// further request could otherwise be admitted later as a duplicate review.
func TestE2EConfirmedRequestVerdicts(t *testing.T) {
	const commentID = int64(31)
	origin := func(kind, role string, actor int64) *r.RequestOrigin {
		return &r.RequestOrigin{Kind: kind, ActorID: actor, ActorRole: role}
	}
	opened := func(id int64) r.NativeRequest {
		o := origin("opened", "pr_author", r.OwnerID)
		o.PullID = e2ePullID(1)
		return r.NativeRequest{ID: id, Admitted: true, Origin: o}
	}
	comment := func(id int64, admitted bool) r.NativeRequest {
		o := origin("comment", "commenter", r.OwnerID)
		o.PullID, o.CommentID = e2ePullID(2), commentID
		return r.NativeRequest{ID: id, Admitted: admitted, Origin: o}
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
	isolation := func(d r.DurableState) []string { return failures(e2eIsolationRequestVerdicts(d, j1, j2, commentID)) }
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
		"single: request of another pull":   {1, single, func(d r.DurableState) { pr(d, 1).Requests[0].Origin.PullID = e2ePullID(2) }},
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
		"isolation: PR 2 comment of another pull":  {2, isolation, func(d r.DurableState) { pr(d, 2).Requests[0].Origin.PullID = e2ePullID(1) }},
		"isolation: PR 2 another comment's request": {2, isolation, func(d r.DurableState) {
			pr(d, 2).Requests[0].Origin.CommentID = commentID + 1
		}},
		"isolation: PR 2 record lost": {2, isolation, func(d r.DurableState) { delete(d.PRs, e2eScopeKey(2)) }},
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
// TestE2EAuthUsable is round 9 finding 3: an auth store holding bytes that
// are not the provider's original synthetic credential is not usable, even
// though the store accepts any nonempty state.
func TestE2EAuthUsable(t *testing.T) {
	edit := func(provider r.Provider, change func(map[string]any)) []byte {
		var value map[string]any
		if err := json.Unmarshal(f.Seed(provider), &value); err != nil {
			t.Fatal(err)
		}
		change(value)
		raw, _ := json.Marshal(value)
		return raw
	}
	cases := []struct {
		name     string
		provider r.Provider
		state    []byte
		usable   bool
	}{
		{"codex synthetic seed", r.Codex, f.Seed(r.Codex), true},
		{"claude synthetic seed", r.Claude, f.Seed(r.Claude), true},
		{"codex corrupted", r.Codex, []byte(`{"auth_mode":"chatgpt","tokens":`), false},
		{"claude corrupted", r.Claude, []byte("not json"), false},
		{"codex holding claude auth", r.Codex, f.Seed(r.Claude), false},
		{"claude holding codex auth", r.Claude, f.Seed(r.Codex), false},
		{"codex api key mode", r.Codex, edit(r.Codex, func(v map[string]any) { v["auth_mode"] = "apikey" }), false},
		{"codex other access token", r.Codex, edit(r.Codex, func(v map[string]any) { v["tokens"].(map[string]any)["access_token"] = "sfl-other-access" }), false},
		{"codex without refresh token", r.Codex, edit(r.Codex, func(v map[string]any) { delete(v["tokens"].(map[string]any), "refresh_token") }), false},
		{"claude other refresh token", r.Claude, edit(r.Claude, func(v map[string]any) { v["claudeAiOauth"].(map[string]any)["refreshToken"] = "sfl-other-refresh" }), false},
		{"claude without access token", r.Claude, edit(r.Claude, func(v map[string]any) { delete(v["claudeAiOauth"].(map[string]any), "accessToken") }), false},
	}
	private := func() string {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	for _, c := range cases {
		directory := private()
		store, err := r.NewAuthStore(c.provider, directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Refresh(context.Background(), func([]byte) ([]byte, error) { return c.state, nil }); err != nil {
			t.Fatal(c.name, err)
		}
		if got := e2eAuthUsable(directory, c.provider); got != c.usable {
			t.Errorf("%s: usable %v, want %v", c.name, got, c.usable)
		}
		// Invalid auth must not pass as a recovery fence either.
		if got := e2eAuthDisposition(directory, c.provider); !c.usable && got != "invalid" {
			t.Errorf("%s: disposition %s, want invalid", c.name, got)
		}
		// A pending recovery lease fences even the synthetic credential.
		if err := os.WriteFile(filepath.Join(directory, string(c.provider), "lease.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := e2eAuthDisposition(directory, c.provider); got != "fenced" {
			t.Errorf("%s with a recovery lease: disposition %s, want fenced", c.name, got)
		}
	}
	// A store that holds nothing is retired, neither usable, invalid nor
	// fenced.
	if got := e2eAuthDisposition(private(), r.Codex); got != "retired" {
		t.Error("empty auth store judged", got)
	}
	// Round 11 finding 1: a refusal before the store yields state is a
	// recovery fence only when it is ErrAuthRecoveryRequired. Every other
	// pending recovery path fences; a malformed or unsafe state file, an
	// invalid lock file or another filesystem error leaves the store
	// unavailable, not fenced.
	for name, c := range map[string]struct {
		damage func(provider string) error
		want   string
	}{
		"pending recovery journal":      {func(p string) error { return os.WriteFile(filepath.Join(p, "pending.json"), []byte("{}"), 0600) }, "fenced"},
		"exposed profile":               {func(p string) error { return os.Mkdir(filepath.Join(p, "profile"), 0700) }, "fenced"},
		"state file readable by others": {func(p string) error { return os.Chmod(filepath.Join(p, "state.json"), 0644) }, "unavailable"},
		"empty state file":              {func(p string) error { return os.WriteFile(filepath.Join(p, "state.json"), nil, 0600) }, "unavailable"},
		"state file is a directory": {func(p string) error {
			if err := os.Remove(filepath.Join(p, "state.json")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(p, "state.json"), 0700)
		}, "unavailable"},
		"state file linked elsewhere":  {func(p string) error { return os.Link(filepath.Join(p, "state.json"), filepath.Join(p, "linked.json")) }, "unavailable"},
		"lock file readable by others": {func(p string) error { return os.Chmod(filepath.Join(p, "refresh.lock"), 0644) }, "unavailable"},
		"lock file is a directory": {func(p string) error {
			if err := os.Remove(filepath.Join(p, "refresh.lock")); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(p, "refresh.lock"), 0700)
		}, "unavailable"},
	} {
		directory := private()
		store, err := r.NewAuthStore(r.Codex, directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Refresh(context.Background(), func([]byte) ([]byte, error) { return f.Seed(r.Codex), nil }); err != nil {
			t.Fatal(name, err)
		}
		if err := c.damage(filepath.Join(directory, string(r.Codex))); err != nil {
			t.Fatal(name, err)
		}
		if got := e2eAuthDisposition(directory, r.Codex); got != c.want {
			t.Errorf("%s: disposition %s, want %s", name, got, c.want)
		}
	}
	// A store left as production's AuthProfile.Retire leaves it, with neither
	// state nor lease (TestProviderRecoveryRetiresLostAuthAndReleasesChargedLease),
	// is retired; re-provisioning the synthetic seed makes it usable again,
	// and a pending lease fences it.
	for _, provider := range []r.Provider{r.Codex, r.Claude} {
		directory := private()
		store, err := r.NewAuthStore(provider, directory)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Refresh(context.Background(), func([]byte) ([]byte, error) { return f.Seed(provider), nil }); err != nil {
			t.Fatal(provider, err)
		}
		if err := os.Remove(filepath.Join(directory, string(provider), "state.json")); err != nil {
			t.Fatal(err)
		}
		if got := e2eAuthDisposition(directory, provider); got != "retired" {
			t.Errorf("%s retired store judged %s", provider, got)
		}
		if err := store.Refresh(context.Background(), func(old []byte) ([]byte, error) {
			if len(old) != 0 {
				return nil, errors.New("retired auth still present")
			}
			return f.Seed(provider), nil
		}); err != nil {
			t.Fatal(provider, err)
		}
		if got := e2eAuthDisposition(directory, provider); got != "usable" {
			t.Errorf("%s re-provisioned store judged %s", provider, got)
		}
		if err := os.Remove(filepath.Join(directory, string(provider), "state.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, string(provider), "lease.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := e2eAuthDisposition(directory, provider); got != "fenced" {
			t.Errorf("%s retired store with a recovery lease judged %s", provider, got)
		}
	}
}

// TestE2EAwaitRun requires Close itself to stop Run: a Run that returns
// only once its own context is cancelled is reported as not returning after
// Close, and Run's context is cancelled only after the wait.
func TestE2EAwaitRun(t *testing.T) {
	for _, stopsOnClose := range []bool{true, false} {
		tb := &e2eCleanupTB{T: t}
		run, cancel := context.WithCancel(context.Background())
		closed, done := make(chan struct{}), make(chan error, 1)
		if stopsOnClose {
			close(closed)
		}
		go func() {
			select {
			case <-closed:
			case <-run.Done():
			}
			done <- context.Canceled
		}()
		e := &e2eEnv{t: tb, result: &e2eResult{}, runCancel: cancel, runDone: done}
		e.awaitRun("probe", 200*time.Millisecond)
		if tb.Failed() == stopsOnClose {
			t.Errorf("Run stopped by Close %v judged with errors %v", stopsOnClose, tb.errors)
		}
		if run.Err() == nil {
			t.Error("Run's context not cancelled after the wait")
		}
	}
}

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
		// Round 10: ReleaseStopped changes only the lease, deadline,
		// reservation, pending charge and state; every other field of the
		// released job is the retained job's.
		"source attempt appeared": func(d r.DurableState) {
			job(d).SourceAttempts = []r.SourceAttempt{{ID: "attempt", Access: &r.SourceAccess{Version: 1}}}
		},
		"attempt round changed":        func(d r.DurableState) { job(d).AttemptRound = 2 },
		"attempt session changed":      func(d r.DurableState) { job(d).AttemptInputHash = strings.Repeat("7", 64) },
		"attempt provider changed":     func(d r.DurableState) { job(d).AttemptProvider = r.Claude },
		"audited providers changed":    func(d r.DurableState) { job(d).AuditedProviders = []r.Provider{r.Claude} },
		"creation time changed":        func(d r.DurableState) { job(d).CreatedAt = time.Unix(7, 0).UTC() },
		"scope changed":                func(d r.DurableState) { job(d).Scope.InstallationID++ },
		"request changed":              func(d r.DurableState) { job(d).RequestID = 2 },
		"evidence requirement dropped": func(d r.DurableState) { job(d).AttemptProviderEvidenceRequired = false },
		"charge days changed":          func(d r.DurableState) { job(d).ChargeDays = []string{"2026-10-24"} },
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
// TestE2EGitHubRoleRoutes is round 11 finding 4: the fake GitHub holds each
// credential role to production's capability split (e2eRoleRoutes), so
// discovery or polling routed through the publisher credential, on its own
// publisher transport, is a violation and is refused, as is publication work
// done with the reader credential.
func TestE2EGitHubRoleRoutes(t *testing.T) {
	g := &e2eGitHub{prs: map[int]*e2ePR{}, baseSHA: strings.Repeat("e", 40), tokens: map[string]string{"reader-token": "reader", "publisher-token": "publisher"}, mints: map[string]int{}, requests: map[string]int{}}
	call := func(role, method, path string) (int, []string) {
		q := httptest.NewRequest(method, "https://api.github.com"+path, nil)
		q.Header.Set("Authorization", "Bearer "+role+"-token")
		if role == "app" {
			q.Header.Set("Authorization", "Bearer synthetic-app-jwt")
		}
		transport := role
		if role == "app" {
			transport = "reader"
		}
		before := len(g.violations)
		status, _ := g.api(q, transport)
		return status, slices.Clone(g.violations[before:])
	}
	for _, c := range []struct{ role, method, path string }{
		{"app", "GET", "/app"},
		{"app", "GET", e2eRepoPath + "/installation"},
		{"reader", "GET", "/installation/repositories"},
		{"reader", "GET", "/users/sfl-test[bot]"},
		{"reader", "GET", e2eRepoPath},
		{"reader", "GET", e2eRepoPath + "/pulls?per_page=100&page=1"},
		{"reader", "GET", e2eRepoPath + "/pulls/1"},
		{"reader", "GET", e2eRepoPath + "/git/ref/heads/main"},
		{"reader", "GET", e2eRepoPath + "/issues/1/comments"},
		{"publisher", "GET", "/installation/repositories"},
		{"publisher", "GET", "/users/sfl-test[bot]"},
		{"publisher", "GET", e2eRepoPath},
		{"publisher", "GET", e2eRepoPath + "/pulls/1/reviews"},
		{"publisher", "GET", e2eRepoPath + "/pulls/1/reviews/5/comments"},
	} {
		if _, violations := call(c.role, c.method, c.path); len(violations) != 0 {
			t.Errorf("%s %s %s within production's capability refused: %v", c.role, c.method, c.path, violations)
		}
	}
	for _, c := range []struct{ role, method, path string }{
		{"publisher", "GET", e2eRepoPath + "/pulls?per_page=100&page=1"},
		{"publisher", "GET", e2eRepoPath + "/pulls/1"},
		{"publisher", "GET", e2eRepoPath + "/git/ref/heads/main"},
		{"publisher", "GET", e2eRepoPath + "/compare/" + strings.Repeat("e", 40) + "..." + strings.Repeat("1", 40) + "?per_page=1&page=2"},
		{"publisher", "GET", e2eRepoPath + "/issues/1/events"},
		{"publisher", "GET", e2eRepoPath + "/pulls/1/requested_reviewers"},
		{"reader", "GET", e2eRepoPath + "/pulls/1/reviews"},
		{"reader", "GET", e2eRepoPath + "/pulls/1/reviews/5/comments"},
		{"reader", "GET", e2eRepoPath + "/pulls/1/reviews/5"},
	} {
		status, violations := call(c.role, c.method, c.path)
		if len(violations) == 0 || status != 403 {
			t.Errorf("%s %s %s outside production's capability served (status %d, violations %v)", c.role, c.method, c.path, status, violations)
		}
	}
}

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

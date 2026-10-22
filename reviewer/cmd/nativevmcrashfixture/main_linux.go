//go:build linux

// nativevmcrashfixture kills its own fake-only supervisor after a validated
// synthetic refresh and inference request. It never reads an existing profile.
package main

import (
	"bufio"
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
	"sync"
	"syscall"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	f "github.com/hemsoft-dev/set-it-free-loop/reviewer/internal/nativefixture"
)

var sourceHead, sourceTree string

type config struct {
	Version                              int
	Purpose, ImageSourceHead, Root       string
	Provider                             r.Provider
	QEMU, Kernel, Initrd, Stub, Firmware r.VMBootAsset
	Runtime                              r.NativeVMRuntimeSpec
}
type crashPoint struct {
	Version                                             int
	RunnerSourceHead, RunnerSourceTree, ImageSourceHead string
	SessionHash, PolicyHash, SeedHash                   string
	RootDevice, RootInode                               uint64
	Counts                                              f.Counts
}
type unitRecord struct {
	Binding                         r.AuthBinding
	Captured, Stopped, Aborted      bool
	PayloadKind, Unit, ManifestHash string
	Identity                        r.GateIdentity
}

func hash(raw []byte) string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func fixedSession(c config) (r.Session, error) {
	limits := r.DefaultLimits()
	limits.Timeout = 75 * time.Second
	snapshot := r.Snapshot{RepositoryID: r.RepositoryID, HeadRepositoryID: r.RepositoryID, PR: 1, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), MergeBase: strings.Repeat("c", 40)}
	return r.NewSession("native-fake-supervisor-loss", snapshot, r.Digest("synthetic-supervisor-loss"), c.Provider, 1, "immutable source evidence", nil, nil, limits)
}
func validConfig(c config) bool {
	return c.Version == 1 && c.Purpose == "native-process-loss-fake-only" && (c.Provider == r.Codex || c.Provider == r.Claude) && filepath.IsAbs(c.Root) && filepath.Clean(c.Root) == c.Root && len(c.ImageSourceHead) == 40
}
func main() {
	if len(os.Args) != 3 || os.Getenv("SFL_NATIVE_VM_CRASH_FAKE_ONLY") != "1" || len(sourceHead) != 40 || len(sourceTree) != 40 || (os.Args[1] != "--run-fake-only" && os.Args[1] != "--owned-child") {
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[2])
	if err != nil || len(raw) > 16384 {
		os.Exit(2)
	}
	c, err := r.DecodeStrict[config](raw, 16384)
	if err != nil || !validConfig(c) {
		os.Exit(2)
	}
	if os.Args[1] == "--owned-child" {
		err = runChild(c)
	} else {
		err = runParent(c, os.Args[2])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-only supervisor-loss qualification failed")
		os.Exit(1)
	}
}
func rootIdentity(root string) (uint64, uint64, error) {
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return 0, 0, errors.New("private root required")
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || raw.Uid != uint32(os.Geteuid()) {
		return 0, 0, errors.New("owned root required")
	}
	return uint64(raw.Dev), raw.Ino, nil
}
func sendCrashPoint(file *os.File, p crashPoint) error {
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > 8192 {
		return errors.New("bounded point required")
	}
	_, err = file.Write(append(raw, '\n'))
	return err
}

// All dialing is fixed to the one local TLS server. Proxy and ambient provider
// credentials are absent. The host route guard still validates captured routes.
func loopbackTransport(handler http.Handler) (http.RoundTripper, func(), error) {
	server := httptest.NewTLSServer(handler)
	target, err := url.Parse(server.URL)
	if err != nil {
		server.Close()
		return nil, nil, err
	}
	base := server.Client().Transport.(*http.Transport).Clone()
	base.Proxy = nil
	base.ForceAttemptHTTP2 = false
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != target.Host {
			return nil, errors.New("outside fake peer denied")
		}
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	}
	return fixedLocal{target, base}, func() { base.CloseIdleConnections(); server.Close() }, nil
}

type fixedLocal struct {
	target *url.URL
	base   *http.Transport
}

func (t fixedLocal) RoundTrip(q *http.Request) (*http.Response, error) {
	if q.URL.Scheme != "https" {
		return nil, errors.New("fake HTTPS required")
	}
	switch q.URL.Host {
	case "fixture.provider.example", "auth.openai.com", "platform.claude.com", "api.anthropic.com":
	default:
		return nil, errors.New("outside fake authority denied")
	}
	copy := q.Clone(q.Context())
	u := *q.URL
	u.Host = t.target.Host
	copy.URL = &u
	return t.base.RoundTrip(copy)
}
func runChild(c config) error {
	// The child itself exclusively creates the root before creating synthetic
	// state. An external invocation cannot adopt an existing auth directory.
	if err := os.Mkdir(c.Root, 0700); err != nil {
		return errors.New("fresh root required")
	}
	for _, name := range []string{"auth", "launches"} {
		if err := os.Mkdir(filepath.Join(c.Root, name), 0700); err != nil {
			return err
		}
	}
	pointFD := os.NewFile(3, "private-crash-point")
	if pointFD == nil {
		return errors.New("private point pipe required")
	}
	syscall.CloseOnExec(3)
	defer pointFD.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	session, err := fixedSession(c)
	if err != nil {
		return err
	}
	auth, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return err
	}
	seed := f.ExpiredSeed(c.Provider)
	seedHash := hash(seed)
	err = auth.Refresh(ctx, func(old []byte) ([]byte, error) {
		if len(old) != 0 {
			return nil, errors.New("existing seed denied")
		}
		return seed, nil
	})
	clear(seed)
	if err != nil {
		return err
	}
	backend, policy, err := f.NewRefresh(session, false)
	if err != nil {
		return err
	}
	var once sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// Run the actual canned producer to completion before examining its counters.
		// Its native-client validators run before a successful inference response.
		reply := httptest.NewRecorder()
		backend.ServeHTTP(reply, q)
		if reply.Code == http.StatusOK && reply.Header().Get("Content-Type") == "text/event-stream" {
			counts := backend.Counts()
			if counts.Inference == 1 && counts.Refreshes == 1 && counts.Failures == 0 && counts.RefreshFailures == 0 && counts.Telemetry == 0 {
				once.Do(func() {
					dev, ino, e := rootIdentity(c.Root)
					if e == nil {
						e = sendCrashPoint(pointFD, crashPoint{1, sourceHead, sourceTree, c.ImageSourceHead, r.Digest(session), r.Digest(policy), seedHash, dev, ino, counts})
					}
					if e != nil {
						cancel()
					}
				})
				<-q.Context().Done()
				return
			}
		}
		for key, values := range reply.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(reply.Code)
		_, _ = w.Write(reply.Body.Bytes())
	})
	transport, closeBackend, err := loopbackTransport(handler)
	if err != nil {
		return err
	}
	defer closeBackend()
	reader, err := r.NewReader(f.Store{}, session.Snapshot, []string{"source.go"}, session.Limits)
	if err != nil {
		return err
	}
	hints := r.NativeVMProfileHints{}
	if c.Provider == r.Claude {
		hints = r.NativeVMProfileHints{ClaudeAccountUUID: "11111111-1111-4111-8111-111111111111", ClaudeOrganizationUUID: "22222222-2222-4222-8222-222222222222", ClaudeEmail: "fixture@example.invalid"}
	}
	worker, err := r.NewNativeVMReviewer(r.NativeVMOptions{Provider: c.Provider, Root: filepath.Join(c.Root, "launches"), Stub: c.Stub.Path, StubHash: c.Stub.SHA256, QEMU: c.QEMU, Kernel: c.Kernel, Initrd: c.Initrd, Firmware: c.Firmware, Runtime: c.Runtime, Auth: auth, Reader: reader, Policy: policy, Transport: transport, ProfileHints: hints, Check: func(_ context.Context, got r.Session) error {
		if r.Digest(got) != r.Digest(session) {
			return errors.New("foreign fake session")
		}
		return nil
	}})
	if err != nil {
		return err
	}
	_, _, _, _ = worker.ReviewAudited(ctx, session)
	return errors.New("child returned instead of being killed")
}
func readUnit(root string) (unitRecord, error) {
	paths, err := filepath.Glob(filepath.Join(root, "launches", "*", "unit.json"))
	if err != nil || len(paths) != 1 {
		return unitRecord{}, errors.New("one original unit required")
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil || len(raw) > 65536 {
		return unitRecord{}, errors.New("bounded unit required")
	}
	var record unitRecord
	err = json.Unmarshal(raw, &record)
	return record, err
}
func pointValid(c config, p crashPoint) bool {
	session, err := fixedSession(c)
	if err != nil {
		return false
	}
	_, policy, err := f.NewRefresh(session, false)
	if err != nil {
		return false
	}
	dev, ino, err := rootIdentity(c.Root)
	return err == nil && p.Version == 1 && p.RunnerSourceHead == sourceHead && p.RunnerSourceTree == sourceTree && p.ImageSourceHead == c.ImageSourceHead && p.SessionHash == r.Digest(session) && p.PolicyHash == r.Digest(policy) && len(p.SeedHash) == 64 && dev == p.RootDevice && ino == p.RootInode && p.Counts.Inference == 1 && p.Counts.Refreshes == 1 && p.Counts.Failures == 0 && p.Counts.RefreshFailures == 0 && p.Counts.Telemetry == 0
}
func stopOriginal(c config, sessionHash string) (unitRecord, error) {
	record, err := readUnit(c.Root)
	if err != nil || !capturedUnitBound(record, sessionHash) {
		return record, errors.New("original native capture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	supervisor, err := r.OpenNativeVMSupervisor(ctx, filepath.Join(c.Root, "launches"), c.Stub.Path, c.Stub.SHA256)
	if err != nil {
		return record, err
	}
	defer supervisor.Close()
	if err = supervisor.ConfirmAuthStopped(ctx, record.Binding); err != nil {
		return record, err
	}
	if err = supervisor.CleanupRuntime(ctx, record.Binding); err != nil {
		return record, err
	}
	return readUnit(c.Root)
}
func runParent(c config, configuration string) (err error) {
	if _, e := os.Lstat(c.Root); !os.IsNotExist(e) {
		return errors.New("fresh root required")
	}
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pipeRead.Close()
	program, err := os.Executable()
	if err != nil {
		pipeWrite.Close()
		return err
	}
	child := exec.Command(program, "--owned-child", configuration)
	child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "SFL_NATIVE_VM_CRASH_FAKE_ONLY=1"}
	child.ExtraFiles = []*os.File{pipeWrite}
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err = child.Start(); err != nil {
		pipeWrite.Close()
		return err
	}
	pipeWrite.Close()
	joined := false
	defer func() {
		if !joined {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		if err != nil {
			session, e := fixedSession(c)
			if e == nil {
				_, _ = stopOriginal(c, r.Digest(session))
			}
		}
	}()
	if err = pipeRead.SetReadDeadline(time.Now().Add(105 * time.Second)); err != nil {
		return err
	}
	point, pointErr := readCrashPoint(pipeRead)
	if pointErr != nil || !pointValid(c, point) {
		return errors.New("wrong crash point")
	}
	before, err := readUnit(c.Root)
	if err != nil || !capturedUnitBound(before, point.SessionHash) || before.Stopped {
		return errors.New("live captured unit required")
	}
	killedAt := time.Now()
	if err = child.Process.Kill(); err != nil {
		return err
	}
	waitErr := child.Wait()
	joined = true
	var exit *exec.ExitError
	statusOK := errors.As(waitErr, &exit)
	var status syscall.WaitStatus
	if statusOK {
		status, statusOK = exit.Sys().(syscall.WaitStatus)
	}
	if !statusOK || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return errors.New("actual SIGKILL required")
	}
	auth, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return err
	}
	callback := false
	refreshErr := auth.Refresh(context.Background(), func(current []byte) ([]byte, error) { callback = true; return current, nil })
	blocked := errors.Is(refreshErr, r.ErrAuthRecoveryRequired) && !callback && retainedSeedMatches(c, point.SeedHash)
	profile, err := auth.RecoverProfile(context.Background(), before.Binding)
	if err != nil {
		return err
	}
	defer profile.Close()
	// Complete is intentionally attempted only after fresh physical stop. A lost
	// return cannot make the stale exported seed valid again.
	stopped, err := stopOriginal(c, point.SessionHash)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	supervisor, err := r.OpenNativeVMSupervisor(ctx, filepath.Join(c.Root, "launches"), c.Stub.Path, c.Stub.SHA256)
	if err != nil {
		return err
	}
	defer supervisor.Close()
	completeRefused := errors.Is(profile.Complete(ctx, supervisor), r.ErrAuthRecoveryRequired) && retainedSeedMatches(c, point.SeedHash)
	if !blocked || !completeRefused {
		return errors.New("lost return was accepted")
	}
	if err = profile.Close(); err != nil {
		return err
	}
	reopened, err := r.NewAuthStore(c.Provider, filepath.Join(c.Root, "auth"))
	if err != nil {
		return err
	}
	again := false
	secondErr := reopened.Refresh(ctx, func(current []byte) ([]byte, error) { again = true; return current, nil })
	persisted := errors.Is(secondErr, r.ErrAuthRecoveryRequired) && !again
	profile, err = reopened.RecoverProfile(ctx, before.Binding)
	if err != nil {
		return err
	}
	defer profile.Close()
	// Explicit fixture-only retirement invalidates the synthetic credential. It
	// supplies no permission to recreate an actual account credential.
	if !persisted {
		return errors.New("reopened recovery fence lost")
	}
	if err = profile.Retire(ctx, supervisor); err != nil {
		return err
	}
	empty := false
	err = reopened.Refresh(ctx, func(current []byte) ([]byte, error) {
		empty = len(current) == 0
		return nil, errors.New("no reseeding")
	})
	if err == nil || !empty {
		return errors.New("old seed survived retirement")
	}
	after, err := readUnit(c.Root)
	if err != nil {
		return err
	}
	roots, _ := filepath.Glob(filepath.Join(c.Root, "launches", "*", "qemu-runtime"))
	same := before.Binding == after.Binding && before.Unit == after.Unit && before.ManifestHash == after.ManifestHash && before.Identity == after.Identity
	receipt := struct {
		Version                                                                                                                                                           int
		Purpose, RunnerSourceHead, RunnerSourceTree, ImageSourceHead                                                                                                      string
		Provider                                                                                                                                                          r.Provider
		Passed, FakeOnly, RuntimeQualified, ProviderEligibilityQualified                                                                                                  bool
		SupervisorSIGKILL, AuthRecoveryBlocked, CompleteWithoutReturnRefused, ReopenedAuthFenced, ExplicitSyntheticRetirement, RuntimeRootsRemoved, OriginalUnitPreserved bool
		RecoveryMS                                                                                                                                                        int64
		Point                                                                                                                                                             crashPoint
		Unit                                                                                                                                                              unitRecord
	}{1, "native-process-loss-fake-only", sourceHead, sourceTree, c.ImageSourceHead, c.Provider, blocked && completeRefused && persisted && empty && len(roots) == 0 && same && stopped.Stopped && stopped.Aborted, true, false, false, true, blocked, completeRefused, persisted, empty, len(roots) == 0, same, time.Since(killedAt).Milliseconds(), point, after}
	bytes, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(c.Root, "result.json"), append(bytes, '\n'), 0600); err != nil {
		return err
	}
	if !receipt.Passed {
		return errors.New("process loss proof incomplete")
	}
	return nil
}

// The child writes one bounded line. Partial pipe reads must not invalidate a
// valid producer record or let trailing bytes qualify as another point.
func readCrashPoint(reader io.Reader) (crashPoint, error) {
	raw, err := bufio.NewReader(io.LimitReader(reader, 8193)).ReadBytes('\n')
	if err != nil || len(raw) > 8192 {
		return crashPoint{}, errors.New("bounded complete crash point required")
	}
	return r.DecodeStrict[crashPoint](raw, 8192)
}

// Compare only the fixture-created canonical state; auth bytes never enter a receipt.
func retainedSeedMatches(c config, expected string) bool {
	raw, err := os.ReadFile(filepath.Join(c.Root, "auth", string(c.Provider), "state.json"))
	defer clear(raw)
	return err == nil && len(raw) > 0 && len(raw) <= 100<<10 && hash(raw) == expected
}

func capturedUnitBound(record unitRecord, sessionHash string) bool {
	id := record.Identity
	return record.Captured && record.PayloadKind == "native-vm" && record.Binding.SessionHash == sessionHash && len(record.Binding.LaunchID) == 32 && record.Unit == "sfl-alpha-"+record.Binding.LaunchID+".service" && len(record.ManifestHash) == 64 && len(id.InvocationID) == 32 && len(id.BootID) == 36 && id.PID > 0 && id.StartTicks > 0
}

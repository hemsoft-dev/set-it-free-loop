//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func promotionRegistryFixture(t *testing.T) (*ReleasePromotionRegistry, GitReviewerRelease) {
	t.Helper()
	options, _, _, _ := gitReviewerStartupFixture(t)
	root := nativeRuntimePrivateParent(t)
	registry, err := CreateReleasePromotionRegistry(context.Background(), root, alphaEnrollment())
	if err != nil {
		t.Fatal(err)
	}
	return registry, options.Releases[options.Current]
}
func promotionRequest(t *testing.T, s ReleasePromotionState, release GitReviewerRelease, action string, n int) ReleasePromotionRequest {
	t.Helper()
	_, hash, err := EncodeGitReviewerRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := s.Current()
	at := time.Now().UTC().Add(-time.Hour).Add(time.Duration(n) * time.Second)
	request := ReleasePromotionRequest{OperationID: strings.Repeat("0", 31) + strconv.Itoa(n), ExpectedStateHash: s.Hash(), Action: action, PreviousRevision: current, Revision: release.Revision, PackageHash: hash, OwnerID: OwnerID, At: at, DecisionEvidence: "synthetic explicit owner decision, not an actual approval", StoppedEvidenceHash: Digest("synthetic stopped service evidence")}
	if action != "rollback" {
		request.Approval = ReleaseApproval{Revision: release.Revision, PackageHash: hash, OwnerID: OwnerID, ReviewPR: 1, ReviewURL: "https://github.com/hemsoft-dev/set-it-free-loop/pull/1#pullrequestreview-1", BuildHash: Digest("synthetic build"), ValidationHash: Digest("synthetic qualification"), OwnerDecision: "synthetic manual bootstrap or release approval", ReviewedAt: at.Add(-time.Minute), ApprovedAt: at}
	}
	return request
}
func allowPromotion(context.Context, Enrollment, ReleasePromotionRequest) error { return nil }
func TestReleasePromotionExplicitBootstrapPromotionRollbackAndReopen(t *testing.T) {
	ctx := context.Background()
	registry, first := promotionRegistryFixture(t)
	state, err := registry.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Check(ctx, alphaEnrollment(), first.Revision, Digest("unknown")) == nil {
		t.Fatal("empty registry qualified release")
	}
	bootstrap := promotionRequest(t, state, first, "bootstrap", 1)
	calls := 0
	authorizer := func(_ context.Context, e Enrollment, c ReleasePromotionRequest) error {
		calls++
		if e != alphaEnrollment() || c.OwnerID != OwnerID {
			t.Fatal("wrong authorization scope")
		}
		return nil
	}
	state, err = registry.Apply(ctx, bootstrap, first, authorizer)
	if err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if registry.Check(ctx, alphaEnrollment(), first.Revision, bootstrap.PackageHash) != nil {
		t.Fatal("approved tuple denied")
	}
	duplicate, err := registry.Apply(ctx, bootstrap, first, authorizer)
	if err != nil || duplicate.Hash() != state.Hash() || calls != 1 {
		t.Fatal("same operation replayed authorization or changed state", err, calls)
	}
	second := first
	second.Revision = strings.Repeat("e", 40)
	promote := promotionRequest(t, state, second, "promote", 2)
	state, err = registry.Apply(ctx, promote, second, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Check(ctx, alphaEnrollment(), first.Revision, bootstrap.PackageHash) != nil || registry.Check(ctx, alphaEnrollment(), second.Revision, promote.PackageHash) != nil {
		t.Fatal("promotion lost retained immutable job release")
	}
	wrong := promote
	wrong.OperationID = strings.Repeat("3", 32)
	if _, err = registry.Apply(ctx, wrong, second, authorizer); !errors.Is(err, ErrFenced) || calls != 2 {
		t.Fatal("stale CAS reached authorizer", err, calls)
	}
	rollback := promotionRequest(t, state, first, "rollback", 3)
	state, err = registry.Apply(ctx, rollback, first, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	revision, hash := state.Current()
	if revision != first.Revision || hash != bootstrap.PackageHash || registry.Check(ctx, alphaEnrollment(), second.Revision, promote.PackageHash) == nil || registry.Check(ctx, alphaEnrollment(), first.Revision, hash) != nil {
		t.Fatal("rollback did not revoke replaced release or restore original tuple")
	}
	reopened, err := OpenReleasePromotionRegistry(registry.directory, alphaEnrollment())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Snapshot(ctx)
	if err != nil || loaded.Hash() != state.Hash() || len(loaded.Changes) != 3 {
		t.Fatal("reopen lost history", err)
	}
	if reopened.Check(ctx, alphaEnrollment(), second.Revision, promote.PackageHash) == nil {
		t.Fatal("reopen forgot revocation")
	}
	changed := first
	changed.Revision = second.Revision
	revive := promotionRequest(t, state, changed, "rollback", 4)
	if _, err = reopened.Apply(ctx, revive, changed, authorizer); !errors.Is(err, ErrFenced) {
		t.Fatal("revoked package revived", err)
	}
}
func TestReleasePromotionDenialsMakeNoChange(t *testing.T) {
	ctx := context.Background()
	registry, release := promotionRegistryFixture(t)
	state, err := registry.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := promotionRequest(t, state, release, "bootstrap", 1)
	original, err := os.ReadFile(filepath.Join(registry.directory, "promotion.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Apply(ctx, request, release, nil); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("missing authorization admitted", err)
	}
	for name, mutate := range map[string]func(*ReleasePromotionRequest){
		"wrong-owner": func(c *ReleasePromotionRequest) { c.OwnerID++ }, "wrong-review-owner": func(c *ReleasePromotionRequest) { c.Approval.OwnerID++ }, "wrong-hash": func(c *ReleasePromotionRequest) { c.PackageHash = Digest("other package") }, "wrong-state": func(c *ReleasePromotionRequest) { c.ExpectedStateHash = Digest("other state") }, "unreviewed-head": func(c *ReleasePromotionRequest) { c.Approval.Revision = strings.Repeat("c", 40) }, "foreign-review": func(c *ReleasePromotionRequest) {
			c.Approval.ReviewURL = "https://github.com/other/repo/pull/1#pullrequestreview-1"
		}, "model-review": func(c *ReleasePromotionRequest) {
			c.Approval.ReviewURL = "https://github.com/hemsoft-dev/set-it-free-loop/pull/1#issuecomment-1"
		}, "missing-stop": func(c *ReleasePromotionRequest) { c.StoppedEvidenceHash = "" }, "future-decision": func(c *ReleasePromotionRequest) { c.At = time.Now().Add(time.Hour); c.Approval.ApprovedAt = c.At }, "unencodable-time": func(c *ReleasePromotionRequest) {
			c.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			c.Approval.ApprovedAt = c.At
		}, "invalid-utf8": func(c *ReleasePromotionRequest) { c.DecisionEvidence = "owner \xff" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			mutate(&changed)
			calls := 0
			if _, err := registry.Apply(ctx, changed, release, func(context.Context, Enrollment, ReleasePromotionRequest) error { calls++; return nil }); err == nil || calls != 0 {
				t.Fatal("invalid request reached authorizer", err, calls)
			}
		})
	}
	if _, err = registry.Apply(ctx, request, release, func(context.Context, Enrollment, ReleasePromotionRequest) error { return ErrLiveBlocked }); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(registry.directory, "promotion.json"))
	if err != nil || string(after) != string(original) {
		t.Fatal("rejection changed durable approval", err)
	}
}
func TestReleasePromotionCanonicalHistoryIdentityAndLossRefuse(t *testing.T) {
	ctx := context.Background()
	registry, release := promotionRegistryFixture(t)
	state, _ := registry.Snapshot(ctx)
	request := promotionRequest(t, state, release, "bootstrap", 1)
	state, err := registry.Apply(ctx, request, release, allowPromotion)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(registry.directory, "promotion.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{append(append([]byte{}, raw...), ' '), []byte("null"), []byte(`{"Version":1,"unknown":true}`), []byte(strings.Repeat("x", releasePromotionBytes+1))} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("inject corrupt history")
		}
		if _, err = registry.Snapshot(ctx); !errors.Is(err, ErrFenced) {
			t.Fatal("corrupt state accepted", err)
		}
		if registry.Check(ctx, alphaEnrollment(), release.Revision, request.PackageHash) == nil {
			t.Fatal("corrupt state qualified work")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("restore exact state")
	}
	foreign := alphaEnrollment()
	foreign.InstallationID++
	if _, err = OpenReleasePromotionRegistry(registry.directory, foreign); err == nil {
		t.Fatal("foreign enrollment accepted")
	}
	if os.Remove(path) != nil {
		t.Fatal("inject lost history")
	}
	if _, err = OpenReleasePromotionRegistry(registry.directory, alphaEnrollment()); err == nil {
		t.Fatal("lost history silently reinitialized")
	}
	if _, err = registry.Apply(ctx, promotionRequest(t, state, release, "rollback", 2), release, allowPromotion); err == nil {
		t.Fatal("lost history promoted")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("lost history rewritten", err)
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("restore state")
	}
	old := registry.directory + "-original"
	if os.Rename(registry.directory, old) != nil {
		t.Fatal("move original registry")
	}
	t.Cleanup(func() { os.RemoveAll(old) })
	if os.Mkdir(registry.directory, 0700) != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("inject replaced directory")
	}
	if _, err = registry.Snapshot(ctx); !errors.Is(err, ErrFenced) {
		t.Fatal("replacement directory adopted", err)
	}
}
func TestReleasePromotionConcurrentCASAndCancelledAuthorization(t *testing.T) {
	ctx := context.Background()
	registry, release := promotionRegistryFixture(t)
	state, _ := registry.Snapshot(ctx)
	request := promotionRequest(t, state, release, "bootstrap", 1)
	entered, unblock := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := registry.Apply(ctx, request, release, func(context.Context, Enrollment, ReleasePromotionRequest) error {
			close(entered)
			<-unblock
			return nil
		})
		done <- err
	}()
	<-entered
	other, err := newReleasePromotionRegistry(registry.directory, alphaEnrollment())
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	if _, err = other.Snapshot(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("second owner bypassed lock", err)
	}
	cancel()
	close(unblock)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	second := release
	second.Revision = strings.Repeat("e", 40)
	latest, _ := registry.Snapshot(ctx)
	next := promotionRequest(t, latest, second, "promote", 2)
	call, cancel := context.WithCancel(ctx)
	if _, err = registry.Apply(call, next, second, func(context.Context, Enrollment, ReleasePromotionRequest) error { cancel(); return nil }); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("cancelled authorizer committed", err)
	}
	after, _ := registry.Snapshot(ctx)
	if after.Hash() != latest.Hash() {
		t.Fatal("cancel changed release")
	}
}
func TestReleasePromotionStartupGateKeepsSigningClosed(t *testing.T) {
	options, _, opener, signs := gitReviewerStartupFixture(t)
	e := options.Credentials.Journal.enrollment
	registry, err := CreateReleasePromotionRegistry(context.Background(), nativeRuntimePrivateParent(t), e)
	if err != nil {
		t.Fatal(err)
	}
	options.Check = registry.Check
	if service, err := openGitReviewerService(context.Background(), options, opener); err == nil || service != nil || signs.Load() != 0 {
		t.Fatal("unapproved package opened startup", err)
	}
	state, _ := registry.Snapshot(context.Background())
	release := options.Releases[options.Current]
	request := promotionRequest(t, state, release, "bootstrap", 1)
	if _, err = registry.Apply(context.Background(), request, release, allowPromotion); err != nil {
		t.Fatal(err)
	}
	service, err := openGitReviewerService(context.Background(), options, opener)
	if err != nil || signs.Load() != 0 {
		t.Fatal("approved construction signed or failed", err)
	}
	defer service.Close(context.Background())
	if service.Run(context.Background()) == nil || signs.Load() != 0 {
		t.Fatal("promotion supplied credential qualification")
	}
	second := release
	second.Revision = strings.Repeat("e", 40)
	state, _ = registry.Snapshot(context.Background())
	next := promotionRequest(t, state, second, "promote", 2)
	stoppedGate := func(context.Context, Enrollment, ReleasePromotionRequest) error {
		if !service.closed {
			return ErrWorkerStopPending
		}
		return nil
	}
	if _, err = registry.Apply(context.Background(), next, second, stoppedGate); !errors.Is(err, ErrLiveBlocked) {
		t.Fatal("undrained prior service switched", err)
	}
	if service.Close(context.Background()) != nil {
		t.Fatal("close original service")
	}
	if _, err = registry.Apply(context.Background(), next, second, stoppedGate); err != nil || signs.Load() != 0 {
		t.Fatal("explicit drained switch failed or signed", err)
	}
}
func TestReleasePromotionWriteFailurePreservesDecisionAndRetryIdentity(t *testing.T) {
	registry, release := promotionRegistryFixture(t)
	t.Cleanup(func() { os.Chmod(registry.directory, 0700) })
	state, _ := registry.Snapshot(context.Background())
	request := promotionRequest(t, state, release, "bootstrap", 1)
	path := filepath.Join(registry.directory, "promotion.json")
	raw, _ := os.ReadFile(path)
	calls := 0
	if _, err := registry.Apply(context.Background(), request, release, func(context.Context, Enrollment, ReleasePromotionRequest) error {
		calls++
		// This test-only fault occurs after a valid record has been read and
		// authorization reached. The actual atomic replacement must fail.
		return os.Chmod(registry.directory, 0500)
	}); !errors.Is(err, ErrReleasePromotionUnknown) || calls != 1 {
		t.Fatal("actual write failure lost uncertainty", err, calls)
	}
	if err := os.Chmod(registry.directory, 0700); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(raw) {
		t.Fatal("failed replace changed committed state", err)
	}
	if _, err = registry.Snapshot(context.Background()); err != nil {
		t.Fatal("reconciliation could not read original state", err)
	}
	if _, err := registry.Apply(context.Background(), request, release, allowPromotion); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved ReleasePromotionState
	if json.Unmarshal(data, &saved) != nil || len(saved.Changes) != 1 {
		t.Fatal("reconciliation duplicated decision")
	}
}

func TestReleasePromotionIdempotencyRejectsUnencodableMutation(t *testing.T) {
	registry, release := promotionRegistryFixture(t)
	state, _ := registry.Snapshot(context.Background())
	request := promotionRequest(t, state, release, "bootstrap", 1)
	if _, err := registry.Apply(context.Background(), request, release, allowPromotion); err != nil {
		t.Fatal(err)
	}
	request.At = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := registry.Apply(context.Background(), request, release, allowPromotion); err == nil {
		t.Fatal("unencodable idempotency mutation accepted")
	}
}

func TestReleasePromotionRefusesUnknownDirectoryBeforeMutation(t *testing.T) {
	root := nativeRuntimePrivateParent(t)
	path := filepath.Join(root, "state.json")
	canary := []byte("synthetic credential-store ownership canary")
	if os.WriteFile(path, canary, 0600) != nil {
		t.Fatal("fixture")
	}
	if _, err := CreateReleasePromotionRegistry(context.Background(), root, alphaEnrollment()); !errors.Is(err, ErrFenced) {
		t.Fatal("foreign directory adopted", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatal("foreign directory mutated", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(canary) {
		t.Fatal("foreign contents changed", err)
	}
}

func TestReleasePromotionStartupBindsCurrentSelectionAndDecisionEpoch(t *testing.T) {
	ctx := context.Background()
	options, _, opener, signs := gitReviewerStartupFixture(t)
	enrollment := options.Credentials.Journal.enrollment
	registry, err := CreateReleasePromotionRegistry(ctx, nativeRuntimePrivateParent(t), enrollment)
	if err != nil {
		t.Fatal(err)
	}
	first := options.Releases[options.Current]
	state, _ := registry.Snapshot(ctx)
	bootstrap := promotionRequest(t, state, first, "bootstrap", 1)
	state, err = registry.Apply(ctx, bootstrap, first, allowPromotion)
	if err != nil {
		t.Fatal(err)
	}
	wrong := options
	wrong.Current = strings.Repeat("f", 40)
	opened := 0
	counted := func(ctx context.Context, a, b, c string) (*gitUnitOwner, error) {
		opened++
		return opener(ctx, a, b, c)
	}
	if service, err := openPromotedGitReviewerService(ctx, registry, wrong, counted); err == nil || service != nil || opened != 0 || signs.Load() != 0 {
		t.Fatal("stale selection opened a manager", err, opened)
	}
	checks := 0
	options.Check = func(context.Context, Enrollment, string, string) error { checks++; return nil }
	service, err := openPromotedGitReviewerService(ctx, registry, options, counted)
	if err != nil || checks == 0 || signs.Load() != 0 {
		t.Fatal("promoted construction skipped independent gate or signed", err, checks)
	}
	defer service.Close(ctx)
	if err = service.inner.check(ctx, enrollment, first.Revision); err != nil {
		t.Fatal("current decision fenced before change", err)
	}
	second := first
	second.Revision = strings.Repeat("e", 40)
	promote := promotionRequest(t, state, second, "promote", 2)
	// Synthetic adversarial authorizer intentionally omits its required stop
	// check. The service's bound epoch must still refuse any subsequent work.
	if _, err = registry.Apply(ctx, promote, second, allowPromotion); err != nil {
		t.Fatal(err)
	}
	if registry.Check(ctx, enrollment, first.Revision, bootstrap.PackageHash) != nil {
		t.Fatal("old package must remain approved for retained jobs in a new startup")
	}
	if err = service.inner.check(ctx, enrollment, first.Revision); !errors.Is(err, ErrLiveBlocked) || signs.Load() != 0 {
		t.Fatal("service continued under changed decision epoch", err)
	}
}
func TestReleasePromotionEpochRechecksAfterIndependentGate(t *testing.T) {
	ctx := context.Background()
	options, _, opener, _ := gitReviewerStartupFixture(t)
	enrollment := options.Credentials.Journal.enrollment
	registry, err := CreateReleasePromotionRegistry(ctx, nativeRuntimePrivateParent(t), enrollment)
	if err != nil {
		t.Fatal(err)
	}
	first := options.Releases[options.Current]
	state, _ := registry.Snapshot(ctx)
	state, err = registry.Apply(ctx, promotionRequest(t, state, first, "bootstrap", 1), first, allowPromotion)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Revision = strings.Repeat("e", 40)
	promote := promotionRequest(t, state, second, "promote", 2)
	options.Check = func(context.Context, Enrollment, string, string) error {
		// A noncooperating synthetic writer bypasses the lifecycle lock. Keep the
		// epoch recheck effective even for this invalid read-only callback.
		updated := state
		updated.Changes = append(append([]ReleasePromotionRequest{}, state.Changes...), promote)
		raw, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(registry.directory, "promotion.json"), raw, 0600)
	}
	opened := 0
	counted := func(ctx context.Context, a, b, c string) (*gitUnitOwner, error) {
		opened++
		return opener(ctx, a, b, c)
	}
	if service, err := openPromotedGitReviewerService(ctx, registry, options, counted); err == nil || service != nil || opened != 0 {
		t.Fatal("decision changed inside eligibility gate admitted startup", err, opened)
	}
}

func awaitPromotionLockWaiter(t *testing.T, identity os.FileInfo) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
			if err == nil && os.SameFile(info, identity) {
				count++
			}
		}
		if count >= 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("second operation did not open the held lock")
}
func TestReleasePromotionRevalidatesDirectoryAfterLockWait(t *testing.T) {
	for _, kind := range []string{"foreign-entry", "public-mode", "replaced-path"} {
		t.Run(kind, func(t *testing.T) {
			registry, _ := promotionRegistryFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			held, err := registry.lock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer held.close()
			identity, err := held.file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { _, err := registry.Snapshot(ctx); result <- err }()
			awaitPromotionLockWaiter(t, identity)
			temporary := filepath.Join(registry.directory, ".replace-"+strings.Repeat("a", 32))
			if os.WriteFile(temporary, []byte("synthetic retained fragment"), 0600) != nil {
				t.Fatal("temp fixture")
			}
			switch kind {
			case "foreign-entry":
				if os.WriteFile(filepath.Join(registry.directory, "foreign.json"), []byte("foreign ownership canary"), 0600) != nil {
					t.Fatal("foreign fixture")
				}
			case "public-mode":
				if os.Chmod(registry.directory, 0755) != nil {
					t.Fatal("mode fixture")
				}
				t.Cleanup(func() { os.Chmod(registry.directory, 0700) })
			case "replaced-path":
				old := registry.directory + "-old"
				if os.Rename(registry.directory, old) != nil || os.Mkdir(registry.directory, 0700) != nil {
					t.Fatal("path fixture")
				}
				temporary = filepath.Join(old, filepath.Base(temporary))
				t.Cleanup(func() { os.RemoveAll(old) })
			}
			if err = held.close(); err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, ErrFenced) {
				t.Fatal("post-lock foreign directory accepted", kind, err)
			}
			if raw, err := os.ReadFile(temporary); err != nil || string(raw) != "synthetic retained fragment" {
				t.Fatal("refusal cleaned foreign-owner evidence", kind, err)
			}
		})
	}
}
func TestReleasePromotionStartupRefusesUnapprovedOrRevokedCatalog(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(strconv.FormatBool(revoked), func(t *testing.T) {
			ctx := context.Background()
			options, _, opener, _ := gitReviewerStartupFixture(t)
			e := options.Credentials.Journal.enrollment
			registry, err := CreateReleasePromotionRegistry(ctx, nativeRuntimePrivateParent(t), e)
			if err != nil {
				t.Fatal(err)
			}
			first := options.Releases[options.Current]
			state, _ := registry.Snapshot(ctx)
			state, err = registry.Apply(ctx, promotionRequest(t, state, first, "bootstrap", 1), first, allowPromotion)
			if err != nil {
				t.Fatal(err)
			}
			second := first
			second.Revision = strings.Repeat("e", 40)
			if revoked {
				state, err = registry.Apply(ctx, promotionRequest(t, state, second, "promote", 2), second, allowPromotion)
				if err != nil {
					t.Fatal(err)
				}
				state, err = registry.Apply(ctx, promotionRequest(t, state, first, "rollback", 3), first, allowPromotion)
				if err != nil {
					t.Fatal(err)
				}
			}
			options.Releases[second.Revision] = second
			options.Providers[second.Revision] = options.Providers[first.Revision]
			opened := 0
			counted := func(ctx context.Context, a, b, c string) (*gitUnitOwner, error) {
				opened++
				return opener(ctx, a, b, c)
			}
			service, err := openPromotedGitReviewerService(ctx, registry, options, counted)
			if service != nil {
				defer service.Close(ctx)
			}
			if err == nil || opened != 0 {
				t.Fatal("unapproved or revoked catalog opened startup", revoked, opened, err)
			}
		})
	}
}
func TestReleasePromotionSerializesThroughCompleteServiceConstruction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options, _, opener, _ := gitReviewerStartupFixture(t)
	e := options.Credentials.Journal.enrollment
	registry, err := CreateReleasePromotionRegistry(ctx, nativeRuntimePrivateParent(t), e)
	if err != nil {
		t.Fatal(err)
	}
	first := options.Releases[options.Current]
	state, _ := registry.Snapshot(ctx)
	state, err = registry.Apply(ctx, promotionRequest(t, state, first, "bootstrap", 1), first, allowPromotion)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Revision = strings.Repeat("e", 40)
	request := promotionRequest(t, state, second, "promote", 2)
	entered, release := make(chan struct{}), make(chan struct{})
	type startupResult struct {
		service *GitReviewerService
		err     error
	}
	started := make(chan startupResult, 1)
	blocked := func(ctx context.Context, a, b, c string) (*gitUnitOwner, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return opener(ctx, a, b, c)
	}
	go func() {
		service, err := openPromotedGitReviewerService(ctx, registry, options, blocked)
		started <- startupResult{service, err}
	}()
	<-entered
	applying := make(chan struct{})
	applied := make(chan error, 1)
	go func() {
		close(applying)
		_, err := registry.Apply(ctx, request, second, allowPromotion)
		applied <- err
	}()
	<-applying
	early := false
	var applyErr error
	select {
	case applyErr = <-applied:
		early = true
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	out := <-started
	if out.service != nil {
		defer out.service.Close(context.Background())
	}
	if !early {
		applyErr = <-applied
	}
	if early {
		t.Fatal("promotion committed while startup still owned partial resources", applyErr, out.err)
	}
	if out.err != nil || out.service == nil || applyErr != nil {
		t.Fatal("serialized construction/promotion failed", out.err, applyErr)
	}
	if out.service.inner.check(ctx, e, first.Revision) == nil {
		t.Fatal("old service not fenced after subsequent decision")
	}
}

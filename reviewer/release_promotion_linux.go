//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var ErrReleasePromotionUnknown = errors.New("release decision persistence uncertain; reconcile the original operation before retrying")

const releasePromotionBytes = 256 << 10

// ReleaseApproval records independently verified owner/manual-review and build
// provenance. These fields are claims until the explicit trusted authorizer
// verifies them. Model output and a merged PR cannot authorize promotion.
type ReleaseApproval struct {
	Revision, PackageHash                               string
	OwnerID                                             int64
	ReviewPR                                            int
	ReviewURL, BuildHash, ValidationHash, OwnerDecision string
	ReviewedAt, ApprovedAt                              time.Time
}
type ReleasePromotionRequest struct {
	OperationID, ExpectedStateHash                  string
	Action, PreviousRevision, Revision, PackageHash string
	OwnerID                                         int64
	At                                              time.Time
	DecisionEvidence, StoppedEvidenceHash           string
	Approval                                        ReleaseApproval
}
type ReleasePromotionState struct {
	Version    int
	Enrollment Enrollment
	Changes    []ReleasePromotionRequest
}

func (s ReleasePromotionState) Hash() string { return Digest(s) }
func (s ReleasePromotionState) Current() (revision, packageHash string) {
	if len(s.Changes) > 0 {
		c := s.Changes[len(s.Changes)-1]
		return c.Revision, c.PackageHash
	}
	return "", ""
}
func (s ReleasePromotionState) catalog() (map[string]ReleaseApproval, map[string]bool) {
	approved := map[string]ReleaseApproval{}
	revoked := map[string]bool{}
	for _, c := range s.Changes {
		if c.Action == "rollback" {
			revoked[c.PreviousRevision] = true
		} else {
			approved[c.Revision] = c.Approval
		}
	}
	return approved, revoked
}
func promotionTime(at time.Time) bool {
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return false
	}
	_, err := json.Marshal(at)
	return err == nil
}
func promotionText(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}
func promotionID(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == value
}
func (a ReleaseApproval) valid(c ReleasePromotionRequest) bool {
	u, err := url.Parse(a.ReviewURL)
	if err != nil {
		return false
	}
	reviewID, reviewErr := strconv.ParseInt(strings.TrimPrefix(u.Fragment, "pullrequestreview-"), 10, 64)
	return a.Revision == c.Revision && a.PackageHash == c.PackageHash && a.OwnerID == OwnerID && a.ReviewPR > 0 && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Path == "/hemsoft-dev/set-it-free-loop/pull/"+strconv.Itoa(a.ReviewPR) && strings.HasPrefix(u.Fragment, "pullrequestreview-") && reviewErr == nil && reviewID > 0 && len(a.ReviewURL) <= 512 && validAuthHash(a.BuildHash) && validAuthHash(a.ValidationHash) && promotionText(a.OwnerDecision) && promotionTime(a.ReviewedAt) && a.ApprovedAt.Equal(c.At) && !a.ReviewedAt.After(a.ApprovedAt)
}
func validatePromotionStep(s ReleasePromotionState, c ReleasePromotionRequest) error {
	if !promotionID(c.OperationID) || c.ExpectedStateHash != s.Hash() || c.OwnerID != OwnerID || !objectID.MatchString(c.Revision) || !validAuthHash(c.PackageHash) || !promotionTime(c.At) || !promotionText(c.DecisionEvidence) || !validAuthHash(c.StoppedEvidenceHash) || len(s.Changes) >= 64 {
		return ErrFenced
	}
	previous, _ := s.Current()
	if c.PreviousRevision != previous {
		return ErrFenced
	}
	for _, old := range s.Changes {
		if old.OperationID == c.OperationID {
			return ErrFenced
		}
	}
	if len(s.Changes) > 0 && !c.At.After(s.Changes[len(s.Changes)-1].At) {
		return ErrFenced
	}
	approved, revoked := s.catalog()
	switch c.Action {
	case "bootstrap", "promote":
		if (c.Action == "bootstrap") != (len(s.Changes) == 0) || len(approved) >= 8 || approved[c.Revision].Revision != "" || revoked[c.Revision] || !c.Approval.valid(c) {
			return ErrFenced
		}
	case "rollback":
		a, ok := approved[c.Revision]
		if !ok || c.Revision == previous || revoked[c.Revision] || a.PackageHash != c.PackageHash || c.Approval != (ReleaseApproval{}) {
			return ErrFenced
		}
	default:
		return ErrFenced
	}
	return nil
}
func (s ReleasePromotionState) validate(enrollment Enrollment) error {
	if s.Version != 1 || s.Enrollment != enrollment || s.Enrollment.Validate() != nil || s.Changes == nil || len(s.Changes) > 64 {
		return ErrFenced
	}
	prefix := ReleasePromotionState{Version: 1, Enrollment: enrollment, Changes: []ReleasePromotionRequest{}}
	for _, c := range s.Changes {
		if validatePromotionStep(prefix, c) != nil {
			return ErrFenced
		}
		prefix.Changes = append(prefix.Changes, c)
	}
	return nil
}

// ReleasePromotionRegistry has no credentials, GitHub writer, runtime or model
// capability. Its directory must be preexisting, private and dedicated solely
// to this registry. It holds a cross-process lock only during each operation.
type ReleasePromotionRegistry struct {
	directory  string
	enrollment Enrollment
	identity   os.FileInfo
}

func newReleasePromotionRegistry(directory string, enrollment Enrollment) (*ReleasePromotionRegistry, error) {
	canonical, err := filepath.EvalSymlinks(directory)
	if enrollment.Validate() != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || err != nil || canonical != directory || privateDirectory(directory) != nil {
		return nil, ErrLiveBlocked
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		return nil, ErrLiveBlocked
	}
	return &ReleasePromotionRegistry{directory, enrollment, identity}, nil
}
func OpenReleasePromotionRegistry(directory string, enrollment Enrollment) (*ReleasePromotionRegistry, error) {
	registry, err := newReleasePromotionRegistry(directory, enrollment)
	if err != nil {
		return nil, err
	}
	if _, err := registry.Snapshot(context.Background()); err != nil {
		return nil, err
	}
	return registry, nil
}

// Create writes only an empty, nonqualifying registry. It refuses an existing
// record. Open never recreates a missing or lost promotion history.
func CreateReleasePromotionRegistry(ctx context.Context, directory string, enrollment Enrollment) (*ReleasePromotionRegistry, error) {
	r, err := newReleasePromotionRegistry(directory, enrollment)
	if err != nil {
		return nil, err
	}
	held, err := r.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer held.close()
	if _, err = held.root.Lstat("promotion.json"); !os.IsNotExist(err) {
		return nil, ErrFenced
	}
	state := ReleasePromotionState{Version: 1, Enrollment: enrollment, Changes: []ReleasePromotionRequest{}}
	raw, _ := json.Marshal(state)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err = writeAuth(held.root, "promotion.json", raw); err != nil {
		return nil, errors.Join(ErrReleasePromotionUnknown, err)
	}
	return r, nil
}
func (r *ReleasePromotionRegistry) read(root *os.Root) (ReleasePromotionState, error) {
	empty := ReleasePromotionState{Version: 1, Enrollment: r.enrollment, Changes: []ReleasePromotionRequest{}}
	file, err := openRootNoFollow(root, "promotion.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return empty, ErrFenced
	}
	defer file.Close()
	if privateAuthFile(file) != nil {
		return empty, ErrFenced
	}
	raw, err := io.ReadAll(io.LimitReader(file, releasePromotionBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > releasePromotionBytes {
		return empty, ErrFenced
	}
	var state ReleasePromotionState
	if json.Unmarshal(raw, &state) != nil || state.validate(r.enrollment) != nil {
		return empty, ErrFenced
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, raw) {
		return empty, ErrFenced
	}
	return state, nil
}
func (r *ReleasePromotionRegistry) validateDirectory(root *os.Root) error {
	actual, err := root.Stat(".")
	if err != nil || r.identity == nil || !os.SameFile(actual, r.identity) {
		return ErrFenced
	}
	owner, ok := actual.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || actual.Mode().Perm()&0077 != 0 {
		return ErrFenced
	}
	canonical, err := filepath.EvalSymlinks(r.directory)
	named, namedErr := os.Lstat(r.directory)
	if err != nil || canonical != r.directory || namedErr != nil || !os.SameFile(named, r.identity) {
		return ErrFenced
	}
	directory, err := root.Open(".")
	if err != nil {
		return ErrFenced
	}
	entries, readErr := directory.ReadDir(129)
	directory.Close()
	if (readErr != nil && readErr != io.EOF) || len(entries) > 128 {
		return ErrFenced
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "promotion.json" && name != "promotion.lock" && name != "startup.lock" && !(strings.HasPrefix(name, ".replace-") && promotionID(strings.TrimPrefix(name, ".replace-"))) {
			return ErrFenced
		}
	}
	return nil
}
func (r *ReleasePromotionRegistry) lock(ctx context.Context) (*authLock, error) {
	return r.lockFile(ctx, "promotion.lock")
}
func (r *ReleasePromotionRegistry) lockFile(ctx context.Context, name string) (*authLock, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || (name != "promotion.lock" && name != "startup.lock") {
		return nil, ErrLiveBlocked
	}
	root, err := os.OpenRoot(r.directory)
	if err != nil {
		return nil, err
	}
	// Refuse a foreign owner before creating a lock or cleaning any fragment.
	if r.validateDirectory(root) != nil {
		root.Close()
		return nil, ErrFenced
	}
	file, err := openRootNoFollow(root, name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		root.Close()
		return nil, ErrFenced
	}
	held := &authLock{root, file}
	if privateAuthFile(file) != nil {
		held.close()
		return nil, ErrFenced
	}
	for {
		if ctx.Err() != nil {
			held.close()
			return nil, ctx.Err()
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Waiting can span a rename, chmod or another directory owner's arrival.
			// Repeat every invariant before any cleanup or registry operation.
			if ctx.Err() != nil {
				held.close()
				return nil, ctx.Err()
			}
			if r.validateDirectory(root) != nil {
				held.close()
				return nil, ErrFenced
			}
			if name == "promotion.lock" && clearAuthTemps(root) != nil {
				held.close()
				return nil, ErrFenced
			}
			return held, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			held.close()
			return nil, ErrFenced
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			held.close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func (r *ReleasePromotionRegistry) Snapshot(ctx context.Context) (ReleasePromotionState, error) {
	held, err := r.lock(ctx)
	if err != nil {
		return ReleasePromotionState{}, err
	}
	defer held.close()
	state, err := r.read(held.root)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return state, err
}

// Apply is an explicit operator action, never a merge hook. Authorize must
// independently verify owner authority, exact package/build/manual-review
// evidence and complete shutdown of the prior service. It must honor context
// and must not reenter this registry while its lock is held. A missing callback
// denies before opening the directory. It supplies no activation permission.
func (r *ReleasePromotionRegistry) Apply(ctx context.Context, request ReleasePromotionRequest, release GitReviewerRelease, authorize func(context.Context, Enrollment, ReleasePromotionRequest) error) (ReleasePromotionState, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || authorize == nil {
		return ReleasePromotionState{}, ErrLiveBlocked
	}
	requestRaw, requestErr := json.Marshal(request)
	if requestErr != nil || len(requestRaw) > 8192 {
		return ReleasePromotionState{}, ErrFenced
	}
	_, hash, err := EncodeGitReviewerRelease(release)
	if err != nil || release.Revision != request.Revision || hash != request.PackageHash || len(release.Providers) != 2 {
		return ReleasePromotionState{}, ErrLiveBlocked
	}
	phase, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Lock order is startup then metadata. Snapshot/epoch checks take only the
	// metadata lock, so they remain usable during an owned construction.
	lifecycle, err := r.lockFile(phase, "startup.lock")
	if err != nil {
		return ReleasePromotionState{}, err
	}
	defer lifecycle.close()
	held, err := r.lock(phase)
	if err != nil {
		return ReleasePromotionState{}, err
	}
	defer held.close()
	state, err := r.read(held.root)
	if err != nil {
		return ReleasePromotionState{}, err
	}
	for _, old := range state.Changes {
		if old.OperationID == request.OperationID {
			if Digest(old) != Digest(request) {
				return state, ErrFenced
			}
			return state, nil
		}
	}
	if validatePromotionStep(state, request) != nil || request.At.After(time.Now()) {
		return state, ErrFenced
	}
	if phase.Err() != nil {
		return state, ErrLiveBlocked
	}
	if authorize(phase, r.enrollment, request) != nil || phase.Err() != nil {
		return state, ErrLiveBlocked
	}
	next := state
	next.Changes = append(append([]ReleasePromotionRequest{}, state.Changes...), request)
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > releasePromotionBytes || next.validate(r.enrollment) != nil {
		return state, ErrFenced
	}
	if phase.Err() != nil {
		return state, ErrLiveBlocked
	}
	if err = writeAuth(held.root, "promotion.json", raw); err != nil {
		return state, errors.Join(ErrReleasePromotionUnknown, err)
	}
	return next, nil
}

// Check matches GitReviewerServiceOptions.Check. Retained immutable jobs may
// use older approved packages. Rollback revokes the replaced release, including
// retained jobs; it cannot silently run those jobs with the rollback target.
// Compose this with independent owner activation and eligibility checks.
func (r *ReleasePromotionRegistry) Check(ctx context.Context, enrollment Enrollment, revision, packageHash string) error {
	if r == nil || enrollment != r.enrollment {
		return ErrLiveBlocked
	}
	state, err := r.Snapshot(ctx)
	if err != nil {
		return ErrLiveBlocked
	}
	approved, revoked := state.catalog()
	a, ok := approved[revision]
	if !ok || revoked[revision] || a.PackageHash != packageHash || ctx.Err() != nil {
		return ErrLiveBlocked
	}
	return nil
}

// OpenPromotedGitReviewerService requires the registry's committed selection
// and binds the whole startup to that decision epoch. It preserves the caller's
// independent activation/eligibility gate and every existing startup safeguard.
func OpenPromotedGitReviewerService(ctx context.Context, registry *ReleasePromotionRegistry, options GitReviewerServiceOptions) (*GitReviewerService, error) {
	return openPromotedGitReviewerService(ctx, registry, options, openGitUnitOwner)
}
func openPromotedGitReviewerService(ctx context.Context, registry *ReleasePromotionRegistry, options GitReviewerServiceOptions, openUnit func(context.Context, string, string, string) (*gitUnitOwner, error)) (*GitReviewerService, error) {
	if ctx == nil || ctx.Err() != nil || registry == nil || options.Check == nil || !options.Credentials.Journal.valid() || options.Credentials.Journal.enrollment != registry.enrollment {
		return nil, ErrLiveBlocked
	}
	phase, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	lifecycle, err := registry.lockFile(phase, "startup.lock")
	if err != nil {
		return nil, ErrLiveBlocked
	}
	defer lifecycle.close()
	state, err := registry.Snapshot(phase)
	if err != nil {
		return nil, ErrLiveBlocked
	}
	revision, hash := state.Current()
	release, ok := options.Releases[revision]
	if !ok || options.Current != revision {
		return nil, ErrLiveBlocked
	}
	_, actual, err := EncodeGitReviewerRelease(release)
	if err != nil || actual != hash {
		return nil, ErrLiveBlocked
	}
	approved, revoked := state.catalog()
	if len(options.Releases) < 1 || len(options.Releases) > 8 {
		return nil, ErrLiveBlocked
	}
	for revision, configured := range options.Releases {
		_, digest, encodeErr := EncodeGitReviewerRelease(configured)
		approval, known := approved[revision]
		if encodeErr != nil || configured.Revision != revision || !known || revoked[revision] || approval.PackageHash != digest {
			return nil, ErrLiveBlocked
		}
	}
	epoch := state.Hash()
	independent := options.Check
	options.Check = func(ctx context.Context, enrollment Enrollment, revision, hash string) error {
		if registry.checkEpoch(ctx, enrollment, revision, hash, epoch) != nil {
			return ErrLiveBlocked
		}
		if independent(ctx, enrollment, revision, hash) != nil || ctx.Err() != nil {
			return ErrLiveBlocked
		}
		// A slow independent gate must not admit a decision that changed while it
		// was checking local eligibility. Publication/runtime checks repeat this.
		return registry.checkEpoch(ctx, enrollment, revision, hash, epoch)
	}
	return openGitReviewerService(phase, options, openUnit)
}
func (r *ReleasePromotionRegistry) checkEpoch(ctx context.Context, enrollment Enrollment, revision, hash, epoch string) error {
	if r == nil || enrollment != r.enrollment {
		return ErrLiveBlocked
	}
	state, err := r.Snapshot(ctx)
	if err != nil || state.Hash() != epoch {
		return ErrLiveBlocked
	}
	approved, revoked := state.catalog()
	approval, ok := approved[revision]
	if !ok || revoked[revision] || approval.PackageHash != hash || ctx.Err() != nil {
		return ErrLiveBlocked
	}
	return nil
}

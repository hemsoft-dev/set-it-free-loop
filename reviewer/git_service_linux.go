//go:build linux

package reviewer

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

// All inputs belong to the trusted service/release layer. Runtime must select
// the immutable job's explicitly qualified release, never candidate settings.
// Store/journal, helper and HTTP transports remain caller-owned. Unit, archive
// and runtime transfer only when the concrete factory returns a nonnil handle.
type gitReviewerServiceOptions struct {
	Credentials                     GitHubCredentialOwnerOptions
	Unit                            *gitUnitOwner
	Archive                         *gitExportArchive
	RuntimeRoot                     *gitRuntimeRoot
	Helper                          *os.File
	Manifest                        gitGrantManifest
	Triggers                        TriggerPolicy
	Release, ReleaseHash, Directory string
	// SourceVM selects the measured source VM mechanism. It is exclusive with
	// the host acquisition owners above and comes from a version 2 release.
	SourceVM       *gitVMServiceSource
	NativeReleases map[string]map[Provider]NativeVMOptions
	// Runtime is an explicit trusted fixture/alternate runtime seam. Supplying
	// it together with the concrete native release catalog is rejected.
	Runtime                           func(context.Context, Job, *JobEvidence) (Engine, error)
	Check                             func(context.Context, Enrollment, string) error
	Interval, MaxBackoff, TickTimeout time.Duration
}

// gitVMServiceSource transfers to the source VM factory only when it returns
// a nonnil handle, like the host owners.
type gitVMServiceSource struct {
	Owner    *gitVMUnitOwner
	Archive  *gitVMExportArchive
	Assets   *gitVMAssets
	Manifest gitVMManifest
}

// gitServiceSource is the private seam between the service and either source
// factory. holdReady runs fn under the factory's operation token only while
// the factory is ready, so Capture cannot interleave with credential refresh.
type gitServiceSource interface {
	PollSource
	Recover(context.Context) error
	Close(context.Context) error
	holdReady(context.Context, func() error) error
}

func (f *gitSourceFactory) holdReady(ctx context.Context, fn func() error) error {
	if !f.enter(ctx) {
		return ErrLiveBlocked
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil {
		return ErrLiveBlocked
	}
	return fn()
}
func (f *gitVMSourceFactory) holdReady(ctx context.Context, fn func() error) error {
	if !f.enter(ctx) {
		return ErrLiveBlocked
	}
	defer f.leave()
	if f.closed || !f.ready || f.checkRoot() != nil {
		return ErrLiveBlocked
	}
	return fn()
}

// This owner never installs credentials, chooses an account or starts work at
// construction. A nonnil handle returned with an error must be retained for
// explicit Recover/Close. No method silently recreates or replaces its owners.
type gitReviewerService struct {
	options     gitReviewerServiceOptions
	credentials *GitHubCredentialOwner
	source      gitServiceSource
	controller  *PollingController
	operation   chan struct{}
	mu          sync.Mutex
	startup     bool
	closing     bool
	closed      bool
	ran         bool
	phaseCancel context.CancelFunc
	runCancel   context.CancelFunc
	runDone     chan struct{}
}

func (*gitReviewerService) String() string   { return "gitReviewerService{redacted}" }
func (*gitReviewerService) GoString() string { return "gitReviewerService{redacted}" }
func (*gitReviewerService) MarshalJSON() ([]byte, error) {
	return []byte(`"gitReviewerService{redacted}"`), nil
}

func newGitReviewerService(ctx context.Context, o gitReviewerServiceOptions) (*gitReviewerService, error) {
	host := o.Unit != nil && o.Archive != nil && o.RuntimeRoot != nil && o.Helper != nil
	noHost := o.Unit == nil && o.Archive == nil && o.RuntimeRoot == nil && o.Helper == nil
	vm := o.SourceVM != nil && o.SourceVM.Owner != nil && o.SourceVM.Archive != nil && o.SourceVM.Assets != nil
	if ctx == nil || ctx.Err() != nil || !o.Credentials.Journal.valid() || o.Credentials.Check == nil || !(host && o.SourceVM == nil || noHost && vm) || o.Triggers.validate() != nil || !objectID.MatchString(o.Release) || !validAuthHash(o.ReleaseHash) || (o.Runtime == nil) == (o.NativeReleases == nil) || o.Check == nil || o.Interval < time.Second || o.Interval > 5*time.Minute || o.MaxBackoff < o.Interval || o.MaxBackoff > time.Hour || o.TickTimeout < time.Second || o.TickTimeout > 5*time.Minute {
		return nil, ErrLiveBlocked
	}
	s := &gitReviewerService{options: o, operation: make(chan struct{}, 1)}
	s.operation <- struct{}{}
	if o.NativeReleases != nil {
		if o.NativeReleases[o.Release] == nil {
			return nil, ErrLiveBlocked
		}
		runtime, err := newGitServiceNativeRuntime(o.NativeReleases, func(ctx context.Context, release string) error {
			return s.check(ctx, o.Credentials.Journal.enrollment, release)
		})
		if err != nil {
			return nil, err
		}
		s.options.Runtime = runtime
	}
	credentials := o.Credentials
	credentials.Check = func(ctx context.Context, e Enrollment) error { return s.check(ctx, e, o.Release) }
	var err error
	s.credentials, err = OpenGitHubCredentialOwner(ctx, credentials)
	if err != nil {
		return nil, err
	}
	if o.SourceVM != nil {
		factory, factoryErr := newGitVMSourceFactory(ctx, s.credentials, o.SourceVM.Owner, o.SourceVM.Archive, o.SourceVM.Assets, o.SourceVM.Manifest, o.ReleaseHash, o.Directory)
		err = factoryErr
		if factory != nil {
			s.source = factory
		}
	} else {
		factory, factoryErr := newGitSourceFactory(ctx, s.credentials, o.Unit, o.Archive, o.RuntimeRoot, o.Helper, o.Manifest, o.Release, o.ReleaseHash, o.Directory)
		err = factoryErr
		if factory != nil {
			s.source = factory
		}
	}
	if s.source == nil {
		// No factory ownership transferred, and this unexposed credential owner
		// has never refreshed or admitted work. Dispose only that local owner.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if closeErr := s.credentials.Close(cleanup); closeErr != nil {
			return s, errors.Join(err, closeErr)
		}
		return nil, err
	}
	if err != nil {
		return s, err
	}
	if err = s.composeController(ctx); err != nil {
		return s, err
	}
	s.startup = true
	return s, nil
}

func (s *gitReviewerService) composeController(ctx context.Context) error {
	if s.controller != nil {
		return nil
	}
	o := s.options
	e := o.Credentials.Journal.enrollment
	reader, err := NewGitHubTriggerDiscoveryHTTP(e, o.Credentials.Journal.store, s.credentials.Reader(), o.Triggers)
	if err != nil {
		return err
	}
	publisher, err := NewGitHubPublicationHTTP(e, reader, s.credentials.Publisher())
	if err != nil {
		return err
	}
	s.controller, err = OpenPollingController(ctx, PollOptions{
		Store: o.Credentials.Journal.store, Enrollment: e, Release: o.Release,
		Discovery: reader, Publisher: Publisher{Store: o.Credentials.Journal.store, Remote: publisher},
		Source: s.source, Runtime: o.Runtime, Check: s.check,
		Now: o.Credentials.Journal.now, Interval: o.Interval, MaxBackoff: o.MaxBackoff, TickTimeout: o.TickTimeout,
	})
	return err
}

func (s *gitReviewerService) check(ctx context.Context, e Enrollment, release string) error {
	if s == nil || ctx == nil || ctx.Err() != nil || e != s.options.Credentials.Journal.enrollment || !objectID.MatchString(release) {
		return ErrLiveBlocked
	}
	s.mu.Lock()
	ready := s.startup && !s.closing && !s.closed
	s.mu.Unlock()
	// Do not take the factory token here: Capture already holds it when it
	// calls credential Ready. Current factory state fences Capture and Refresh.
	if !ready || s.options.Credentials.Check(ctx, e) != nil || ctx.Err() != nil || s.options.Check(ctx, e, release) != nil || ctx.Err() != nil {
		return ErrLiveBlocked
	}
	return nil
}

func (s *gitReviewerService) enter(ctx context.Context) bool {
	if s == nil || ctx == nil || ctx.Err() != nil || s.operation == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.operation:
		if ctx.Err() != nil {
			s.leave()
			return false
		}
		return true
	}
}
func (s *gitReviewerService) leave() { s.operation <- struct{}{} }

func (s *gitReviewerService) begin(ctx context.Context, limit time.Duration, allowClosing bool) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, ErrLiveBlocked
	}
	phase, cancel := context.WithTimeout(ctx, limit)
	if !s.enter(phase) {
		err := phase.Err()
		cancel()
		return nil, nil, errors.Join(ErrWorkerStopPending, err)
	}
	s.mu.Lock()
	if s.closed || s.closing && !allowClosing {
		s.mu.Unlock()
		s.leave()
		cancel()
		return nil, nil, ErrFenced
	}
	s.phaseCancel = cancel
	s.mu.Unlock()
	return phase, func() {
		cancel()
		s.mu.Lock()
		s.phaseCancel = nil
		s.mu.Unlock()
		s.leave()
	}, nil
}

// Recover is local, explicit and never refreshes credentials. During shutdown
// it may finish old stop evidence but cannot reopen admission or a controller.
func (s *gitReviewerService) Recover(ctx context.Context) error {
	phase, finish, err := s.begin(ctx, 3*time.Minute, true)
	if err != nil {
		return err
	}
	defer finish()
	if s.source == nil || s.source.Recover(phase) != nil {
		return ErrWorkerStopPending
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return nil
	}
	if err := s.composeController(phase); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || phase.Err() != nil {
		return ErrFenced
	}
	s.startup = true
	return nil
}

// Refresh is the only signing/minting entry point. Keeping the concrete factory
// token through qualification prevents Capture from introducing uncertainty
// between the readiness check and signing. A run's stop path needs no such token.
func (s *gitReviewerService) Refresh(ctx context.Context) error {
	phase, finish, err := s.begin(ctx, 3*time.Minute, false)
	if err != nil {
		return err
	}
	defer finish()
	if s.source == nil {
		return ErrLiveBlocked
	}
	return s.source.holdReady(phase, func() error {
		if s.check(phase, s.options.Credentials.Journal.enrollment, s.options.Release) != nil {
			return ErrLiveBlocked
		}
		return s.credentials.Refresh(phase)
	})
}

// Run never refreshes. The caller must explicitly qualify credentials first.
// Once a run starts, its controller is not restarted; retained owners still need
// Close, including after Run returns an uncertain worker/recovery error.
func (s *gitReviewerService) Run(ctx context.Context) error {
	phase, finish, err := s.begin(ctx, 5*time.Second, false)
	if err != nil {
		return err
	}
	if s.controller == nil || s.credentials.Ready(phase) != nil {
		finish()
		return ErrLiveBlocked
	}
	s.mu.Lock()
	if s.ran || s.closing || phase.Err() != nil {
		s.mu.Unlock()
		finish()
		return ErrFenced
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.ran, s.runCancel, s.runDone = true, cancel, done
	s.mu.Unlock()
	finish()
	defer func() {
		cancel()
		s.mu.Lock()
		s.runCancel = nil
		close(done)
		s.mu.Unlock()
	}()
	return s.controller.Run(run)
}

func (s *gitReviewerService) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrWorkerStopPending
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.mu.Lock()
	s.closing, s.startup = true, false
	if s.phaseCancel != nil {
		s.phaseCancel()
	}
	if s.runCancel != nil {
		s.runCancel()
	}
	done := s.runDone
	s.mu.Unlock()
	if !s.enter(ctx) {
		return errors.Join(ErrWorkerStopPending, ctx.Err())
	}
	defer s.leave()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(ErrWorkerStopPending, ctx.Err())
		}
	}
	if s.controller != nil {
		if err := s.controller.Close(ctx); err != nil {
			return err
		}
	}
	if s.source != nil {
		if err := s.source.Close(ctx); err != nil {
			return err
		}
	}
	if s.credentials != nil {
		if err := s.credentials.Close(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

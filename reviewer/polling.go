package reviewer

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PollSchedule is controller-owned bounded metadata. It never contains remote
// error bodies, source/model text, tokens or provider transcripts.
type PollSchedule struct {
	Version  int
	Next     time.Time
	Failures int
	Reason   string
}

type PollDiscovery interface {
	OpenScopes(context.Context) ([]Scope, error)
	PollScope(context.Context, Scope) (RemoteState, Discovery, error)
}

type PollOptions struct {
	Store      *Store
	Enrollment Enrollment
	Release    string
	Discovery  PollDiscovery
	Publisher  Publisher
	Capture    func(context.Context, Snapshot) (*SnapshotEvidence, error)
	Restore    func(context.Context, Job) (*JobEvidence, error)
	Runtime    func(context.Context, Job, *JobEvidence) (Engine, error)
	// Check is a trusted startup/phase gate, not self-attestation by a model.
	// Production composition must supply separately qualified installation,
	// provider, isolation, billing and activation evidence. Tests use fakes.
	Check                             func(context.Context, Enrollment, string) error
	Now                               func() time.Time
	Interval, MaxBackoff, TickTimeout time.Duration
}

type PollResult struct {
	Next             time.Time
	Scopes           int
	Active           bool
	RecoveryRequired bool
}

type pollWork struct {
	scope                    Scope
	request                  int64
	generation               int64
	job                      string
	snapshot                 Snapshot
	cancel                   context.CancelFunc
	done                     chan struct{}
	err                      error
	qualificationInterrupted bool
}

// PollingController holds one OS owner lock for this SQLite service directory.
// It polls while a single stopped-worker invocation runs in the background.
// Construction does not start a daemon, provider, HTTP request or deployment.
type PollingController struct {
	options    PollOptions
	owner      *authLock
	tick       chan struct{}
	mu         sync.Mutex
	closed     bool
	active     *pollWork
	pollCancel context.CancelFunc
}

func OpenPollingController(ctx context.Context, o PollOptions) (*PollingController, error) {
	if o.Store == nil || o.Store.db == nil || !filepath.IsAbs(o.Store.path) || o.Enrollment.Validate() != nil || !objectID.MatchString(o.Release) || o.Discovery == nil || o.Publisher.Store != o.Store || o.Publisher.Remote == nil || o.Capture == nil || o.Restore == nil || o.Runtime == nil || o.Check == nil {
		return nil, ErrLiveBlocked
	}
	if o.Interval < time.Second || o.Interval > 5*time.Minute || o.MaxBackoff < o.Interval || o.MaxBackoff > time.Hour || o.TickTimeout < time.Second || o.TickTimeout > 5*time.Minute {
		return nil, ErrIgnored
	}
	d, err := o.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if d.Enrollment != o.Enrollment {
		return nil, ErrIgnored
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	owner, err := (&AuthStore{directory: filepath.Dir(o.Store.path)}).lock(lockCtx)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Publisher.Now = o.Now
	tick := make(chan struct{}, 1)
	tick <- struct{}{}
	return &PollingController{options: o, owner: owner, tick: tick}, nil
}

func (c *PollingController) now() time.Time {
	if c.options.Now != nil {
		return c.options.Now()
	}
	return time.Now()
}

func (c *PollingController) schedule(ctx context.Context, now time.Time, cause error) (time.Time, error) {
	var next time.Time
	err := c.options.Store.update(ctx, func(d *DurableState) error {
		if d.Enrollment != c.options.Enrollment {
			return ErrIgnored
		}
		p := d.Polling
		if p == nil {
			p = &PollSchedule{Version: 1}
			d.Polling = p
		}
		if p.Version != 1 || p.Failures < 0 || p.Failures > 16 {
			return ErrDeferred
		}
		delay := c.options.Interval
		p.Reason = ""
		if cause == nil {
			p.Failures = 0
		} else {
			if p.Failures < 16 {
				p.Failures++
			}
			for i := 1; i < p.Failures && delay < c.options.MaxBackoff; i++ {
				delay = min(delay*2, c.options.MaxBackoff)
			}
			p.Reason = "deferred"
			if errors.Is(cause, ErrLiveBlocked) {
				p.Reason = "qualification_required"
			}
			delay = max(delay, githubRetryDelay(cause))
		}
		next = now.Add(delay)
		p.Next = next
		return nil
	})
	return next, err
}

// githubRetryDelay honors every joined response. If an error graph exceeds the
// bounded traversal, defer for the maximum accepted server delay rather than
// retrying early or following a cyclic error forever.
func githubRetryDelay(cause error) time.Duration {
	pending := []error{cause}
	var delay time.Duration
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 256 {
			return time.Hour
		}
		err := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err == nil {
			continue
		}
		if status, ok := err.(*GitHubHTTPError); ok && status != nil && status.RetryAfter > delay && status.RetryAfter <= time.Hour {
			delay = status.RetryAfter
		}
		switch e := err.(type) {
		case interface{ Unwrap() []error }:
			children := e.Unwrap()
			if len(children) > 256-len(pending) {
				return time.Hour
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			pending = append(pending, e.Unwrap())
		}
	}
	return delay
}

// fenceRemote never infers removal from an empty pending list. Snapshot/access
// changes pause the existing request and require a fresh native request. Known
// cancellation/replacement is committed by Discover before this check.
func (c *PollingController) fenceRemote(ctx context.Context, scope Scope, remote RemoteState) error {
	return c.options.Store.update(ctx, func(d *DurableState) error {
		pr := d.PRs[scope.key()]
		if pr == nil {
			return nil
		}
		invalidate := !remote.Access || !remote.Open
		if remote.Access && remote.Open {
			if remote.Snapshot.Validate() != nil || remote.Snapshot.PR != scope.PR {
				return ErrDeferred
			}
			if remote.Request.ID != 0 && (remote.Request.RequesterID != d.Enrollment.OwnerID || remote.Request.BotID != d.Enrollment.BotID) {
				return ErrIgnored
			}
			for _, request := range pr.Requests {
				if request.ID == pr.CurrentRequest && request.Snapshot != remote.Snapshot {
					invalidate = true
				}
			}
		}
		if invalidate && (!pr.Paused || pr.PauseRequestID != pr.CurrentRequest) {
			pr.Paused = true
			pr.PauseRequestID = pr.CurrentRequest
			pr.Generation++
			retireFencedJobs(d, pr)
		}
		return nil
	})
}

func (c *PollingController) observe(ctx context.Context, scope Scope) (RemoteState, error) {
	remote, batch, err := c.options.Discovery.PollScope(ctx, scope)
	if err != nil {
		return RemoteState{}, err
	}
	if remote.Access && remote.Open {
		if batch.Scope != scope || !batch.Complete || batch.Through.IsZero() {
			return RemoteState{}, ErrDeferred
		}
		if err := c.options.Store.Discover(ctx, batch); err != nil {
			return RemoteState{}, err
		}
	}
	if err := c.fenceRemote(ctx, scope, remote); err != nil {
		return RemoteState{}, err
	}
	return remote, nil
}

func (c *PollingController) cancelFenced(d DurableState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.active
	if w == nil {
		return
	}
	pr := d.PRs[w.scope.key()]
	if pr == nil || pr.Paused || pr.Generation != w.generation || pr.CurrentRequest != w.request {
		w.cancel()
	}
}

func (c *PollingController) drainWork() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return nil
	}
	select {
	case <-c.active.done:
		err := c.active.err
		c.active = nil
		return err
	default:
		return nil
	}
}

func pollWrites(d DurableState) []*WriteIntent {
	items := []*WriteIntent{}
	for _, w := range d.Writes {
		if w.State == "prepared" || w.State == "unknown" || w.State == "dispatched" || w.State == "postcheck_pending" || w.State == "annotation_pending" {
			items = append(items, w)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items
}

func (c *PollingController) flushWrites(ctx context.Context) (bool, error) {
	d, err := c.options.Store.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	var failures error
	pending := false
	acks := 0
	for _, w := range pollWrites(d) {
		var err error
		if w.State == "prepared" && w.Kind == "ack" {
			if acks >= 1 {
				pending = true
				continue
			}
			acks++
		}
		if w.State == "prepared" {
			err = c.options.Publisher.Dispatch(ctx, w.ID)
		} else {
			err = c.options.Publisher.Reconcile(ctx, w.ID)
		}
		if err != nil {
			failures = errors.Join(failures, err)
		}
		if ctx.Err() != nil {
			return pending, errors.Join(failures, ctx.Err())
		}
	}
	return pending, failures
}

func (c *PollingController) selectWork(d DurableState, remote map[string]RemoteState) *pollWork {
	// A durable lease survives expiry and controller restart. Only a separately
	// qualified stopped-worker recovery may release it, never this polling loop.
	for _, job := range d.Jobs {
		if job.LeaseToken != "" {
			return nil
		}
	}
	jobs := []*DurableJob{}
	for _, job := range d.Jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].Job.ID < jobs[j].Job.ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	for _, j := range jobs {
		pr := d.PRs[j.Scope.key()]
		r, ok := remote[j.Scope.key()]
		if !ok || !r.Access || !r.Open || pr == nil || pr.Paused || pr.CurrentRequest != j.RequestID || pr.Generation != j.Job.Generation || r.Snapshot != j.Job.Snapshot {
			continue
		}
		completed := j.State == "dual_complete" || j.State == "single_complete"
		if completed {
			written := false
			for _, w := range d.Writes {
				if w.Kind == "review" && w.JobHash == Digest(j.Job) {
					written = true
				}
			}
			if written {
				continue
			}
		} else if j.State != "queued" && j.State != "quota_wait" {
			continue
		}
		if !completed && r.Request.ID != j.RequestID {
			continue
		}
		return &pollWork{scope: j.Scope, request: j.RequestID, generation: j.Job.Generation, job: j.Job.ID, snapshot: j.Job.Snapshot, done: make(chan struct{})}
	}
	type candidate struct {
		request NativeRequest
		scope   Scope
	}
	requests := []candidate{}
	for _, pr := range d.PRs {
		r, ok := remote[pr.Scope.key()]
		if !ok || !r.Access || !r.Open || pr.Paused || r.Request.ID == 0 || r.Request.ID != pr.CurrentRequest {
			continue
		}
		for _, request := range pr.Requests {
			if request.ID == pr.CurrentRequest && !request.Removed && !request.Admitted && request.Snapshot == r.Snapshot {
				requests = append(requests, candidate{request, pr.Scope})
			}
		}
	}
	sort.Slice(requests, func(i, j int) bool {
		a, b := requests[i].request, requests[j].request
		if a.CreatedAt.Equal(b.CreatedAt) {
			if a.ID == b.ID {
				return requests[i].scope.PR < requests[j].scope.PR
			}
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	if len(requests) == 0 {
		return nil
	}
	r := requests[0].request
	scope := requests[0].scope
	pr := d.PRs[scope.key()]
	return &pollWork{scope: scope, request: r.ID, generation: pr.Generation, snapshot: r.Snapshot, done: make(chan struct{})}
}

func (c *PollingController) beforeWork(ctx context.Context, w *pollWork) (*JobEvidence, Job, error) {
	if err := c.options.Check(ctx, c.options.Enrollment, c.options.Release); err != nil {
		return nil, Job{}, errors.Join(ErrLiveBlocked, err)
	}
	var evidence *SnapshotEvidence
	if w.job == "" {
		var captureErr error
		evidence, captureErr = c.options.Capture(ctx, w.snapshot)
		if captureErr != nil {
			return nil, Job{}, captureErr
		}
		if evidence == nil {
			return nil, Job{}, ErrDeferred
		}
	}
	remote, err := c.observe(ctx, w.scope)
	if err != nil {
		return nil, Job{}, err
	}
	if !remote.Access || !remote.Open || remote.Snapshot != w.snapshot {
		return nil, Job{}, ErrFenced
	}
	var job Job
	if w.job != "" {
		d, err := c.options.Store.Snapshot(ctx)
		if err != nil {
			return nil, Job{}, err
		}
		j := d.Jobs[w.job]
		if j == nil {
			return nil, Job{}, ErrFenced
		}
		job = j.Job
	} else {
		if remote.Request.ID != w.request {
			return nil, Job{}, ErrFenced
		}
		job, err = c.options.Store.Admit(ctx, w.scope, w.request, c.options.Release, evidence.Packet(), c.now())
		if err != nil {
			return nil, Job{}, err
		}
	}
	// Admit may also return an existing immutable job. Always restore its exact
	// persisted packet instead of substituting a new capture with different omissions.
	bound, err := c.options.Restore(ctx, job)
	if err == nil {
		err = bound.validateJob(job)
	}
	if err == nil && job.Release != c.options.Release {
		if qualification := c.options.Check(ctx, c.options.Enrollment, job.Release); qualification != nil {
			return nil, Job{}, errors.Join(ErrLiveBlocked, qualification)
		}
	}
	return bound, job, err
}

func (c *PollingController) runWork(ctx context.Context, w *pollWork) {
	defer close(w.done)
	bound, job, err := c.beforeWork(ctx, w)
	if err == nil {
		d, readErr := c.options.Store.Snapshot(ctx)
		err = readErr
		if err == nil {
			j := d.Jobs[job.ID]
			if j == nil {
				err = ErrFenced
			} else {
				var out Outcome
				if j.State == "dual_complete" || j.State == "single_complete" {
					out, err = c.options.Store.RecoverCompleted(ctx, job.ID, bound.Reader)
				} else {
					var engine Engine
					engine, err = c.options.Runtime(ctx, job, bound)
					if err == nil {
						for _, provider := range job.Config.Providers() {
							if engine.Reviewers[provider] == nil {
								err = ErrLiveBlocked
								break
							}
						}
					}
					if err != nil {
						state := phaseState(err)
						c.mu.Lock()
						shuttingDown := c.closed
						qualificationInterrupted := w.qualificationInterrupted
						c.mu.Unlock()
						saveErr := c.options.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error {
							j := d.Jobs[job.ID]
							if j == nil || j.LeaseToken != "" || Digest(j.Job) != Digest(job) {
								return ErrFenced
							}
							pr := d.PRs[j.Scope.key()]
							if pr == nil {
								return ErrFenced
							}
							if state == "cancelled" && !pr.Paused && pr.Generation == j.Job.Generation && pr.CurrentRequest == j.RequestID {
								if qualificationInterrupted {
									state = "blocked"
								} else if shuttingDown {
									state = "queued"
								}
							}
							j.State = state
							retireFencedJobs(d, pr)
							return nil
						})
						err = errors.Join(err, saveErr)
					}
					if err == nil {
						engine.Reader = bound.Reader
						out, err = c.options.Store.RunStopped(ctx, job.ID, engine, c.now)
						// Service shutdown may requeue known-stopped work, preserving
						// its artifacts and consumed budget. Qualification interruption
						// instead blocks for explicit recovery. Owner cancellation or
						// uncertain physical stop can never take this path.
						c.mu.Lock()
						shuttingDown := c.closed
						qualificationInterrupted := w.qualificationInterrupted
						c.mu.Unlock()
						if (shuttingDown || qualificationInterrupted) && errors.Is(err, context.Canceled) {
							saveErr := c.options.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error {
								j := d.Jobs[job.ID]
								if j == nil {
									return ErrFenced
								}
								pr := d.PRs[j.Scope.key()]
								if j.LeaseToken == "" && !j.Outstanding && j.State == "cancelled" && pr != nil && !pr.Paused && pr.Generation == j.Job.Generation && pr.CurrentRequest == j.RequestID {
									j.State = "queued"
									if qualificationInterrupted {
										j.State = "blocked"
									}
								}
								return nil
							})
							err = errors.Join(err, saveErr)
						}
					}
				}
				if err == nil {
					_, err = c.options.Publisher.PrepareEvidence(ctx, out, bound)
				}
			}
		}
	}
	c.mu.Lock()
	w.err = err
	c.mu.Unlock()
}

// Poll performs one scheduled cycle and may start one background worker. It
// never waits for that worker, so later polls can discover cancel/replacement.
func (c *PollingController) Poll(ctx context.Context) (result PollResult, err error) {
	select {
	case <-c.tick:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	defer func() { c.tick <- struct{}{} }()
	ctx, cancel := context.WithTimeout(ctx, c.options.TickTimeout)
	defer cancel()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return result, ErrFenced
	}
	c.pollCancel = cancel
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.pollCancel = nil; c.mu.Unlock() }()
	now := c.now()
	d, err := c.options.Store.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	if d.Enrollment != c.options.Enrollment {
		return result, ErrIgnored
	}
	c.cancelFenced(d)
	workErr := c.drainWork()
	if workErr != nil {
		if latest, readErr := c.options.Store.Snapshot(ctx); readErr == nil {
			for _, j := range latest.Jobs {
				if j.LeaseToken != "" {
					result.RecoveryRequired = true
				}
			}
		} else {
			workErr = errors.Join(workErr, readErr)
		}
		result.Next, err = c.schedule(ctx, now, workErr)
		return result, errors.Join(workErr, err)
	}
	if d.Polling != nil {
		if d.Polling.Version != 1 || d.Polling.Failures < 0 || d.Polling.Failures > 16 || d.Polling.Next.After(now.Add(time.Hour)) {
			return result, ErrDeferred
		}
		if now.Before(d.Polling.Next) {
			result.Next = d.Polling.Next
			c.mu.Lock()
			result.Active = c.active != nil
			c.mu.Unlock()
			if !result.Active {
				for _, j := range d.Jobs {
					if j.LeaseToken != "" {
						result.RecoveryRequired = true
					}
				}
			}
			return result, nil
		}
	}
	defer func() {
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		next, saveErr := c.schedule(save, c.now(), err)
		result.Next = next
		err = errors.Join(err, saveErr)
	}()
	if err = c.options.Check(ctx, c.options.Enrollment, c.options.Release); err != nil {
		c.mu.Lock()
		if c.active != nil {
			c.active.qualificationInterrupted = true
			c.active.cancel()
		}
		c.mu.Unlock()
		return result, errors.Join(ErrLiveBlocked, err)
	}
	scopes, listErr := c.options.Discovery.OpenScopes(ctx)
	if listErr != nil {
		return result, listErr
	}
	all := map[string]Scope{}
	for _, scope := range scopes {
		if !d.Enrollment.accepts(scope) {
			return result, ErrIgnored
		}
		if _, duplicate := all[scope.key()]; duplicate {
			return result, ErrDeferred
		}
		all[scope.key()] = scope
	}
	for key, pr := range d.PRs {
		if !d.Enrollment.accepts(pr.Scope) {
			return result, ErrIgnored
		}
		all[key] = pr.Scope
	}
	if len(all) > 64 {
		return result, ErrDeferred
	}
	ordered := []Scope{}
	for _, scope := range all {
		ordered = append(ordered, scope)
	}
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	sort.Slice(ordered, func(i, j int) bool {
		if active != nil && (ordered[i] == active.scope || ordered[j] == active.scope) {
			return ordered[i] == active.scope
		}
		return ordered[i].PR < ordered[j].PR
	})
	remote := map[string]RemoteState{}
	var failures error
	for _, scope := range ordered {
		r, readErr := c.observe(ctx, scope)
		if readErr != nil {
			failures = errors.Join(failures, readErr)
			continue
		}
		remote[scope.key()] = r
		result.Scopes++
		current, readErr := c.options.Store.Snapshot(ctx)
		if readErr != nil {
			return result, readErr
		}
		c.cancelFenced(current)
	}
	if failures != nil {
		return result, failures
	}
	pending, writeErr := c.flushWrites(ctx)
	if writeErr != nil {
		return result, writeErr
	}
	if pending {
		return result, nil
	}
	if err = c.options.Store.Prune(ctx, c.now()); err != nil {
		return result, err
	}
	d, err = c.options.Store.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	c.cancelFenced(d)
	for _, j := range d.Jobs {
		if j.LeaseToken != "" {
			result.RecoveryRequired = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return result, ErrFenced
	}
	if c.active != nil {
		result.Active = true
		result.RecoveryRequired = false
		return result, nil
	}
	w := c.selectWork(d, remote)
	if w != nil {
		worker, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		c.active = w
		result.Active = true
		result.RecoveryRequired = false
		go c.runWork(worker, w)
	}
	return result, nil
}

func (c *PollingController) Run(ctx context.Context) (result error) {
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result = errors.Join(result, c.Close(cleanup))
	}()
	for {
		poll, err := c.Poll(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Errors are retained as bounded durable backoff, never busy-looped.
		if err != nil && poll.Next.IsZero() {
			return err
		}
		delay := poll.Next.Sub(c.now())
		if delay < time.Millisecond {
			delay = time.Millisecond
		}
		timer := time.NewTimer(delay)
		c.mu.Lock()
		var done <-chan struct{}
		if c.active != nil {
			done = c.active.done
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-done:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Close cancels/drains the active invocation before releasing controller
// ownership. An uncertain physical worker keeps its durable lease for explicit
// supervisor recovery. A Close timeout keeps the owner lock held for a retry.
func (c *PollingController) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	if c.pollCancel != nil {
		c.pollCancel()
	}
	active := c.active
	if active != nil {
		active.cancel()
	}
	c.mu.Unlock()
	if active != nil {
		select {
		case <-active.done:
		case <-ctx.Done():
			return errors.Join(ErrWorkerStopPending, ctx.Err())
		}
	}
	select {
	case <-c.tick:
	case <-ctx.Done():
		return errors.Join(ErrWorkerStopPending, ctx.Err())
	}
	defer func() { c.tick <- struct{}{} }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != nil {
		c.owner.close()
		c.owner = nil
	}
	d, err := c.options.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, job := range d.Jobs {
		if job.LeaseToken != "" {
			return ErrWorkerStopPending
		}
	}
	return nil
}

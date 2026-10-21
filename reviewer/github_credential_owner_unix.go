//go:build linux || darwin

package reviewer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

var ErrGitHubCredentialOwner = errors.New("GitHub credential generation qualification required")

// GitHubCredentialOwnerOptions contains only explicitly supplied capabilities.
// JWT transfers one bounded fresh buffer to the owner, which clears it. Check
// must establish current trusted installation/activation eligibility with a
// read-only local check, without
// starting acquisition. Neither callback may retry or hide credential fallback.
// Both callbacks and the supplied transports must honor bounded contexts.
type GitHubCredentialOwnerOptions struct {
	Journal                             *GitHubInstallationJournal
	JWT                                 func(context.Context) ([]byte, error)
	ReaderTransport, PublisherTransport http.RoundTripper
	Check                               func(context.Context, Enrollment) error
}

type githubCredentialPair struct {
	reader, publisher *GitHubInstallationTransport
}

func (p *githubCredentialPair) close() {
	if p != nil {
		if p.reader != nil {
			_ = p.reader.Close()
		}
		if p.publisher != nil {
			_ = p.publisher.Close()
		}
	}
}
func (p *githubCredentialPair) transport(role GitHubCredentialRole) *GitHubInstallationTransport {
	if p == nil {
		return nil
	}
	if role == GitHubReader {
		return p.reader
	}
	if role == GitHubPublisher {
		return p.publisher
	}
	return nil
}

// GitHubCredentialOwner holds a distinct service-directory flock for its whole
// lifetime. It starts unqualified on every process restart. Only explicit Refresh
// can obtain a pair; neither construction, readiness nor requests renew it.
type GitHubCredentialOwner struct{ state *githubCredentialOwnerState }

type githubCredentialOwnerState struct {
	options                            GitHubCredentialOwnerOptions
	mu                                 sync.Mutex
	held                               *authLock
	current                            *githubCredentialPair
	ready, refreshing, closing, closed bool
	refreshCancel                      context.CancelFunc
	active                             map[uint64]context.CancelFunc
	acquisitions                       map[uint64]*githubReaderGeneration
	serial                             uint64
	changed                            chan struct{}
}

func (GitHubCredentialOwner) String() string   { return "GitHubCredentialOwner{redacted}" }
func (GitHubCredentialOwner) GoString() string { return "GitHubCredentialOwner{redacted}" }
func (GitHubCredentialOwner) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubCredentialOwner{redacted}"`), nil
}

func nilGitHubCredentialTransport(transport http.RoundTripper) bool {
	if transport == nil {
		return true
	}
	value := reflect.ValueOf(transport)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

func OpenGitHubCredentialOwner(ctx context.Context, o GitHubCredentialOwnerOptions) (*GitHubCredentialOwner, error) {
	if ctx == nil || o.Journal == nil || !o.Journal.valid() || o.JWT == nil || nilGitHubCredentialTransport(o.ReaderTransport) || nilGitHubCredentialTransport(o.PublisherTransport) || o.Check == nil {
		return nil, ErrGitHubCredentialOwner
	}
	directory := filepath.Join(filepath.Dir(o.Journal.store.path), "github-credential-owner")
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return nil, ErrGitHubCredentialOwner
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory || privateDirectory(directory) != nil {
		return nil, ErrGitHubCredentialOwner
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	held, err := (&AuthStore{directory: directory}).lock(lockCtx)
	if err != nil {
		return nil, ErrGitHubCredentialOwner
	}
	return &GitHubCredentialOwner{&githubCredentialOwnerState{options: o, held: held, active: map[uint64]context.CancelFunc{}, acquisitions: map[uint64]*githubReaderGeneration{}, changed: make(chan struct{})}}, nil
}

func (o *GitHubCredentialOwner) Ready(ctx context.Context) error {
	if o == nil || o.state == nil {
		return ErrGitHubCredentialOwner
	}
	return o.state.Ready(ctx)
}
func (o *GitHubCredentialOwner) Refresh(ctx context.Context) error {
	if o == nil || o.state == nil {
		return ErrGitHubCredentialOwner
	}
	return o.state.Refresh(ctx)
}
func (o *GitHubCredentialOwner) Close(ctx context.Context) error {
	if o == nil || o.state == nil {
		return ErrGitHubCredentialOwner
	}
	return o.state.Close(ctx)
}

// GitHubCredentialRoleTransport is a stable opaque capability bound to one role.
// It cannot select endpoints outside the existing scoped installation transport.
type GitHubCredentialRoleTransport struct {
	owner *githubCredentialOwnerState
	role  GitHubCredentialRole
}

func (GitHubCredentialRoleTransport) String() string {
	return "GitHubCredentialRoleTransport{redacted}"
}
func (GitHubCredentialRoleTransport) GoString() string {
	return "GitHubCredentialRoleTransport{redacted}"
}
func (GitHubCredentialRoleTransport) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubCredentialRoleTransport{redacted}"`), nil
}
func (o *GitHubCredentialOwner) Reader() *GitHubCredentialRoleTransport {
	if o == nil {
		return &GitHubCredentialRoleTransport{}
	}
	return &GitHubCredentialRoleTransport{o.state, GitHubReader}
}
func (o *GitHubCredentialOwner) Publisher() *GitHubCredentialRoleTransport {
	if o == nil {
		return &GitHubCredentialRoleTransport{}
	}
	return &GitHubCredentialRoleTransport{o.state, GitHubPublisher}
}

func (o *githubCredentialOwnerState) signal() { close(o.changed); o.changed = make(chan struct{}) }
func (o *githubCredentialOwnerState) usable() bool {
	if !o.ready || o.refreshing || o.closing || o.closed || o.current == nil {
		return false
	}
	now := o.options.Journal.now()
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		t := o.current.transport(role)
		if t == nil {
			return false
		}
		receipt := t.Receipt()
		if receipt.Enrollment != o.options.Journal.enrollment || receipt.Role != role || now.Before(receipt.CheckedAt) || !now.Before(receipt.ExpiresAt.Add(-time.Minute)) {
			return false
		}
	}
	return true
}

// Ready checks current eligibility and the in-memory qualified pair. Persisted
// journal receipts alone never make a restarted owner ready.
func (o *githubCredentialOwnerState) Ready(ctx context.Context) error {
	if o == nil || ctx == nil || ctx.Err() != nil {
		return ErrGitHubCredentialOwner
	}
	o.mu.Lock()
	usable := o.usable()
	o.mu.Unlock()
	if !usable {
		return ErrGitHubCredentialOwner
	}
	if o.options.Check(ctx, o.options.Journal.enrollment) != nil || ctx.Err() != nil {
		return ErrGitHubCredentialOwner
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.usable() || ctx.Err() != nil {
		return ErrGitHubCredentialOwner
	}
	return nil
}
func (o *githubCredentialOwnerState) waitIdle(ctx context.Context, includeRefresh bool) error {
	for {
		o.mu.Lock()
		idle := len(o.active) == 0 && len(o.acquisitions) == 0 && (!includeRefresh || !o.refreshing)
		changed := o.changed
		o.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrGitHubCredentialOwner
		case <-changed:
		}
	}
}
func ownerContextError(ctx context.Context) error {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return errors.Join(ErrGitHubCredentialOwner, context.Canceled)
		case context.DeadlineExceeded:
			return errors.Join(ErrGitHubCredentialOwner, context.DeadlineExceeded)
		}
	}
	return ErrGitHubCredentialOwner
}

// Refresh closes new admission, drains already admitted local requests, checks
// the durable role latches and qualifies both roles before one atomic handover.
// Failure keeps readiness closed. It never restores an old pair as fallback,
// retries a token POST, invokes recovery, or starts a provider session.
func (o *githubCredentialOwnerState) Refresh(ctx context.Context) error {
	if o == nil || ctx == nil || ctx.Err() != nil || httptrace.ContextClientTrace(ctx) != nil {
		return ownerContextError(ctx)
	}
	phase, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	o.mu.Lock()
	if o.closed || o.closing || o.refreshing {
		o.mu.Unlock()
		return ErrGitHubCredentialOwner
	}
	o.ready = false
	o.refreshing = true
	o.refreshCancel = cancel
	o.signal()
	o.mu.Unlock()
	defer func() { o.mu.Lock(); o.refreshing = false; o.refreshCancel = nil; o.signal(); o.mu.Unlock() }()
	if o.waitIdle(phase, false) != nil {
		o.cancelAcquisitions()
		return ownerContextError(phase)
	}
	if o.options.Check(phase, o.options.Journal.enrollment) != nil || phase.Err() != nil {
		return ownerContextError(phase)
	}
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		latest, err := o.options.Journal.Latest(phase, role)
		if err != nil {
			return ErrGitHubCredentialOwner
		}
		if latest.ID != "" && latest.State != "qualified" && latest.State != "pre_mint_failed" && latest.State != "retry_authorized" {
			return ErrGitHubCredentialOwner
		}
	}
	jwt, err := o.options.JWT(phase)
	if jwt != nil {
		defer clear(jwt)
	}
	if err != nil || !githubCredential(jwt) || phase.Err() != nil {
		return ownerContextError(phase)
	}
	next := &githubCredentialPair{}
	defer func() { next.close() }()
	next.reader, err = o.options.Journal.Acquire(phase, GitHubInstallationOptions{o.options.Journal.enrollment, GitHubReader, jwt, o.options.ReaderTransport, o.options.Journal.now})
	if err != nil {
		return ownerContextError(phase)
	}
	next.publisher, err = o.options.Journal.Acquire(phase, GitHubInstallationOptions{o.options.Journal.enrollment, GitHubPublisher, jwt, o.options.PublisherTransport, o.options.Journal.now})
	if err != nil {
		return ownerContextError(phase)
	}
	if o.options.Check(phase, o.options.Journal.enrollment) != nil || phase.Err() != nil {
		return ownerContextError(phase)
	}
	o.mu.Lock()
	if o.closing || o.closed || phase.Err() != nil {
		o.mu.Unlock()
		return ownerContextError(phase)
	}
	old := o.current
	o.current = next
	next = nil
	o.ready = true
	o.signal()
	o.mu.Unlock()
	old.close()
	return nil
}

func (t *GitHubCredentialRoleTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	delegated := false
	defer func() {
		if !delegated && q != nil && q.Body != nil {
			_ = q.Body.Close()
		}
	}()
	if t == nil || t.owner == nil || q == nil || q.URL == nil || t.owner.Ready(q.Context()) != nil {
		return nil, ErrGitHubCredentialOwner
	}
	o := t.owner
	o.mu.Lock()
	if !o.usable() || len(o.active) >= 64 || q.Context().Err() != nil {
		o.mu.Unlock()
		return nil, ErrGitHubCredentialOwner
	}
	transport := o.current.transport(t.role)
	if transport == nil {
		o.mu.Unlock()
		return nil, ErrGitHubCredentialOwner
	}
	o.serial++
	id := o.serial
	ctx, cancel := context.WithCancel(q.Context())
	o.active[id] = cancel
	o.mu.Unlock()
	var once sync.Once
	finish := func() { once.Do(func() { cancel(); o.mu.Lock(); delete(o.active, id); o.signal(); o.mu.Unlock() }) }
	delegated = true
	response, err := transport.RoundTrip(q.Clone(ctx))
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		finish()
		return nil, ErrGitHubCredentialOwner
	}
	response.Request = q
	response.Body = &githubOwnedResponseBody{inner: response.Body, finish: finish}
	return response, nil
}

type githubOwnedResponseBody struct {
	inner     io.ReadCloser
	finish    func()
	closeOnce sync.Once
	closeErr  error
}

func (b *githubOwnedResponseBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if err != nil {
		b.finish()
	}
	return n, err
}
func (b *githubOwnedResponseBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.inner.Close(); b.finish() })
	return b.closeErr
}

// Close rejects new work, cancels admitted requests and qualification, and waits
// for local response bodies and the qualification callback. Timeout retains the private owner
// lock and token ownership for an explicit retry. It is not remote revocation.
func (o *githubCredentialOwnerState) Close(ctx context.Context) error {
	if o == nil || ctx == nil {
		return ErrGitHubCredentialOwner
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closing = true
	o.ready = false
	cancellations := make([]context.CancelFunc, 0, len(o.active)+1)
	if o.refreshCancel != nil {
		cancellations = append(cancellations, o.refreshCancel)
	}
	for _, cancel := range o.active {
		cancellations = append(cancellations, cancel)
	}
	for _, acquisition := range o.acquisitions {
		cancellations = append(cancellations, acquisition.cancel)
	}
	o.signal()
	o.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
	if o.waitIdle(ctx, true) != nil {
		return ownerContextError(ctx)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.current.close()
	o.current = nil
	if err := o.held.close(); err != nil {
		return ErrGitHubCredentialOwner
	}
	o.held = nil
	o.closed = true
	o.signal()
	return nil
}

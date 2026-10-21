//go:build linux || darwin

package reviewer

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"
)

// Private factory ownership, not a model tool or serialized qualification.
// The immutable stop capability must revoke/join the broker and prove the exact
// acquisition unit stopped. Cancellation, HTTP EOF and a returned worker alone
// never release this generation. Only stop can release it after that capability
// succeeds and all borrowed requests/bodies return. No raw token is exposed.
type githubReaderGeneration struct {
	owner     *githubCredentialOwnerState
	pair      *githubCredentialPair
	id        uint64
	ctx       context.Context
	cancel    context.CancelFunc
	stopper   func(context.Context) error
	operation chan struct{}
	borrows   int
	released  bool
}

func (githubReaderGeneration) String() string   { return "githubReaderGeneration{redacted}" }
func (githubReaderGeneration) GoString() string { return "githubReaderGeneration{redacted}" }
func (githubReaderGeneration) MarshalJSON() ([]byte, error) {
	return []byte(`"githubReaderGeneration{redacted}"`), nil
}

// Caller supplies a trusted stop capability before anything can start. There
// is deliberately no public metadata/bool-based release or renewal operation.
func (o *GitHubCredentialOwner) retainReaderGeneration(ctx context.Context, stopped func(context.Context) error) (*githubReaderGeneration, error) {
	if o == nil || o.state == nil || ctx == nil || ctx.Err() != nil || httptrace.ContextClientTrace(ctx) != nil || stopped == nil {
		return nil, ErrGitHubCredentialOwner
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if o.Ready(check) != nil {
		return nil, ownerContextError(check)
	}
	s := o.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.usable() || len(s.acquisitions) != 0 || ctx.Err() != nil {
		return nil, ErrGitHubCredentialOwner
	}
	window := 2 * time.Minute
	now := s.options.Journal.now()
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		remaining := s.current.transport(role).Receipt().ExpiresAt.Add(-time.Minute).Sub(now)
		if remaining < window {
			window = remaining
		}
	}
	if window <= 0 {
		return nil, ErrGitHubCredentialOwner
	}
	phase, cancelPhase := context.WithTimeout(ctx, window)
	if phase.Err() != nil {
		cancelPhase()
		return nil, ownerContextError(phase)
	}
	s.serial++
	g := &githubReaderGeneration{owner: s, pair: s.current, id: s.serial, ctx: phase, cancel: cancelPhase, stopper: stopped, operation: make(chan struct{}, 1)}
	g.operation <- struct{}{}
	s.acquisitions[g.id] = g
	s.signal()
	return g, nil
}

func (o *githubCredentialOwnerState) cancelAcquisitions() {
	o.mu.Lock()
	cancellations := make([]context.CancelFunc, 0, len(o.acquisitions))
	for _, g := range o.acquisitions {
		cancellations = append(cancellations, g.cancel)
	}
	o.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
}

func (g *githubReaderGeneration) usableLocked() bool {
	o := g.owner
	if g.released || o.closed || o.closing || g.ctx.Err() != nil || o.current != g.pair || o.acquisitions[g.id] != g || (!o.ready && !o.refreshing) {
		return false
	}
	now := o.options.Journal.now()
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		transport := g.pair.transport(role)
		if transport == nil {
			return false
		}
		receipt := transport.Receipt()
		if receipt.Role != role || receipt.Enrollment != o.options.Journal.enrollment || now.Before(receipt.CheckedAt) || !now.Before(receipt.ExpiresAt.Add(-time.Minute)) {
			return false
		}
	}
	return true
}

// Factory-only borrow for the pinned reader. The composing typed Git exchange
// must use the returned context and hold finish until its response body ends.
// A whole-generation acquisition lease remains after every such body finishes.
func (g *githubReaderGeneration) borrow(ctx context.Context) (*GitHubInstallationTransport, context.Context, func(), error) {
	if g == nil || g.owner == nil || ctx == nil || ctx.Err() != nil || httptrace.ContextClientTrace(ctx) != nil {
		return nil, nil, nil, ErrGitHubCredentialOwner
	}
	o := g.owner
	o.mu.Lock()
	usable := g.usableLocked()
	o.mu.Unlock()
	if !usable {
		g.cancel()
		return nil, nil, nil, ErrGitHubCredentialOwner
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := o.options.Check(check, o.options.Journal.enrollment)
	checkErr := check.Err()
	cancel()
	if err != nil || checkErr != nil {
		g.cancel()
		return nil, nil, nil, ownerContextError(ctx)
	}
	o.mu.Lock()
	if !g.usableLocked() {
		o.mu.Unlock()
		g.cancel()
		return nil, nil, nil, ErrGitHubCredentialOwner
	}
	if len(o.active) >= 64 || ctx.Err() != nil {
		o.mu.Unlock()
		return nil, nil, nil, ErrGitHubCredentialOwner
	}
	o.serial++
	id := o.serial
	phase, cancelPhase := context.WithCancel(ctx)
	stopCancellation := context.AfterFunc(g.ctx, cancelPhase)
	o.active[id] = cancelPhase
	g.borrows++
	reader := g.pair.reader
	o.mu.Unlock()
	var once sync.Once
	finish := func() {
		once.Do(func() {
			stopCancellation()
			cancelPhase()
			o.mu.Lock()
			delete(o.active, id)
			g.borrows--
			o.signal()
			o.mu.Unlock()
		})
	}
	return reader, phase, finish, nil
}

// stop is idempotent and serialized. A failed or unreturned stop capability
// keeps both the generation and service lock owned. Close cancels work and
// waits; it cannot manufacture this stopped result or discard a live lease.
func (g *githubReaderGeneration) stop(ctx context.Context) error {
	if g == nil || g.owner == nil || ctx == nil {
		return ErrGitHubCredentialOwner
	}
	g.cancel()
	phase, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-phase.Done():
		return ownerContextError(phase)
	case <-g.operation:
	}
	proofOwnsOperation := false
	defer func() {
		if !proofOwnsOperation {
			g.operation <- struct{}{}
		}
	}()
	o := g.owner
	for {
		o.mu.Lock()
		if g.released {
			o.mu.Unlock()
			return nil
		}
		if o.acquisitions[g.id] != g || o.current != g.pair {
			o.mu.Unlock()
			return ErrGitHubCredentialOwner
		}
		idle := g.borrows == 0
		changed := o.changed
		o.mu.Unlock()
		if idle {
			break
		}
		select {
		case <-phase.Done():
			return ownerContextError(phase)
		case <-changed:
		}
	}
	if phase.Err() != nil {
		return ownerContextError(phase)
	}
	// A caller may time out while an uncooperative trusted proof is still
	// running. That proof keeps the serialization token until it returns.
	// No caller may replay it or release ownership in the meantime.
	proofOwnsOperation = true
	result := make(chan error, 1)
	decision := make(chan struct{})
	defer close(decision)
	go func() {
		// A returned proof remains single-flight until its caller accepts or
		// rejects it. A timed-out caller releases this token only after the
		// callback itself returns, without allowing a late success to commit.
		defer func() { <-decision; g.operation <- struct{}{} }()
		if g.stopper(phase) != nil || phase.Err() != nil {
			result <- ownerContextError(phase)
			return
		}
		result <- nil
	}()
	select {
	case err := <-result:
		if phase.Err() != nil {
			return ownerContextError(phase)
		}
		if err != nil {
			return err
		}
		// Only the caller commits the accepted proof. After this locked
		// acceptance point it returns success, even if cancellation arrives
		// during the commit. No deadline branch can report failure after the
		// generation has been released by another goroutine.
		o.mu.Lock()
		defer o.mu.Unlock()
		if g.borrows != 0 || o.acquisitions[g.id] != g || o.current != g.pair || phase.Err() != nil {
			return ownerContextError(phase)
		}
		delete(o.acquisitions, g.id)
		g.released = true
		g.pair = nil
		g.stopper = nil
		o.signal()
		return nil
	case <-phase.Done():
		return ownerContextError(phase)
	}
}

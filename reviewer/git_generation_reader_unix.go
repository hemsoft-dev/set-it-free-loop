//go:build linux || darwin

package reviewer

import (
	"context"
	"io"
	"sync"
)

// Private factory wiring: the broker receives only this typed exchanger,
// never the owner, a role handle, a credential, or a refresh operation.
type gitGenerationReader struct{ generation *githubReaderGeneration }

func (gitGenerationReader) String() string   { return "gitGenerationReader{redacted}" }
func (gitGenerationReader) GoString() string { return "gitGenerationReader{redacted}" }
func (gitGenerationReader) MarshalJSON() ([]byte, error) {
	return []byte(`"gitGenerationReader{redacted}"`), nil
}

func (r *gitGenerationReader) Exchange(ctx context.Context, request GitHubGitReadRequest) (io.ReadCloser, error) {
	closeInput := func() {
		if request.Body != nil {
			_ = request.Body.Close()
		}
	}
	if r == nil || r.generation == nil {
		closeInput()
		return nil, gitReadError(ctx)
	}
	installation, phase, finish, err := r.generation.borrow(ctx)
	if err != nil {
		closeInput()
		return nil, gitReadError(ctx)
	}
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		closeInput()
		failure := gitReadError(phase)
		finish()
		return nil, failure
	}
	// Exchange owns the input body on all paths from this point onward.
	stream, err := reader.Exchange(phase, request)
	if err != nil || stream == nil {
		failure := gitReadError(phase)
		finish()
		return nil, failure
	}
	b := &gitGenerationBody{state: &gitGenerationBodyState{inner: stream, finish: finish}}
	s := b.state
	s.mu.Lock()
	// Register under the same lock used by Close, so even immediate context
	// cancellation cannot race the retained callback or its disposal.
	s.stop = context.AfterFunc(phase, func() { _ = b.Close() })
	s.mu.Unlock()
	return b, nil
}

type gitGenerationBody struct{ state *gitGenerationBodyState }
type gitGenerationBodyState struct {
	mu        sync.Mutex
	inner     io.ReadCloser
	finish    func()
	stop      func() bool
	readers   int
	closing   bool
	closeDone bool
	closeOnce sync.Once
	closeErr  error
}

func (gitGenerationBody) String() string   { return "gitGenerationBody{redacted}" }
func (gitGenerationBody) GoString() string { return "gitGenerationBody{redacted}" }
func (gitGenerationBody) MarshalJSON() ([]byte, error) {
	return []byte(`"gitGenerationBody{redacted}"`), nil
}

// Called with mu held. Closing a body is insufficient while an already
// admitted Read is still running. Both must return before the borrow retires.
func (s *gitGenerationBodyState) retireLocked() func() {
	if !s.closeDone || s.readers != 0 || s.finish == nil {
		return nil
	}
	finish, stop := s.finish, s.stop
	s.finish, s.stop = nil, nil
	return func() {
		if stop != nil {
			stop()
		}
		finish()
	}
}

func (b *gitGenerationBody) Close() error {
	s := b.state
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.closeOnce.Do(func() {
		err := s.inner.Close()
		s.mu.Lock()
		if err != nil {
			s.closeErr = ErrGitHubGitRead
		}
		s.closeDone = true
		finish := s.retireLocked()
		s.mu.Unlock()
		if finish != nil {
			finish()
		}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

func (b *gitGenerationBody) Read(p []byte) (int, error) {
	s := b.state
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return 0, ErrGitHubGitRead
	}
	s.readers++
	s.mu.Unlock()
	n, err := s.inner.Read(p)
	s.mu.Lock()
	s.readers--
	finish := s.retireLocked()
	s.mu.Unlock()
	if finish != nil {
		finish()
	}
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

var ErrGitHubGitRead = errors.New("scoped Git read refused")

// GitHubGitOperation selects one fixed smart-HTTP route. No caller URL, header,
// credential, receive-pack operation or arbitrary repository is representable.
type GitHubGitOperation string

const (
	GitHubGitAdvertisement      GitHubGitOperation = "advertisement"
	GitHubGitUploadPack         GitHubGitOperation = "upload-pack"
	githubGitRequestLimit       int64              = 4 << 20
	githubGitAdvertisementLimit int64              = 4 << 20
	githubGitResponseLimit      int64              = 64 << 20
	githubGitRepository                            = "https://github.com/hemsoft-dev/set-it-free-loop.git"
)

// GitHubGitReadRequest transfers Body ownership to Exchange on every path.
// Protocol is exactly 0 or 2. Advertisement has no Body; upload-pack requires a
// nonempty Body whose Close unblocks Read. The caller supplies decoded bytes.
type GitHubGitReadRequest struct {
	Operation GitHubGitOperation
	Protocol  int
	Body      io.ReadCloser
}

// GitHubGitReader shares an already qualified reader credential. It never
// acquires, renews, recovers, exposes or owns disposal of that credential.
// Calls and bodies need an outer owner lease before generation disposal.
type GitHubGitReader struct{ state *githubInstallationState }

func (GitHubGitReader) String() string   { return "GitHubGitReader{redacted}" }
func (GitHubGitReader) GoString() string { return "GitHubGitReader{redacted}" }
func (GitHubGitReader) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubGitReader{redacted}"`), nil
}

func NewGitHubGitReader(installation *GitHubInstallationTransport) (*GitHubGitReader, error) {
	if installation == nil || installation.state == nil {
		return nil, ErrGitHubGitRead
	}
	state := installation.state
	if state.receipt.Role != GitHubReader || state.receipt.Enrollment.Validate() != nil || state.now == nil || state.inner == nil {
		return nil, ErrGitHubGitRead
	}
	state.mu.Lock()
	available := len(state.token) > 0
	state.mu.Unlock()
	if !available {
		return nil, ErrGitHubGitRead
	}
	return &GitHubGitReader{state}, nil
}
func gitReadError(ctx context.Context) error {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return errors.Join(ErrGitHubGitRead, context.Canceled)
		case context.DeadlineExceeded:
			return errors.Join(ErrGitHubGitRead, context.DeadlineExceeded)
		}
	}
	return ErrGitHubGitRead
}

// Exchange constructs a fresh fixed HTTPS request and performs exactly one
// RoundTrip. Only bounded opaque Git bytes return, never upstream headers,
// request objects, authentication challenges, redirect targets or error bodies.
// The full operation, including its response body, is bounded to 90 seconds.
func (r *GitHubGitReader) Exchange(ctx context.Context, request GitHubGitReadRequest) (io.ReadCloser, error) {
	var bodyClose sync.Once
	closeInput := func() {
		bodyClose.Do(func() {
			if request.Body != nil {
				_ = request.Body.Close()
			}
		})
	}
	defer closeInput()
	if r == nil || r.state == nil || ctx == nil || ctx.Err() != nil || httptrace.ContextClientTrace(ctx) != nil || (request.Protocol != 0 && request.Protocol != 2) {
		return nil, gitReadError(ctx)
	}
	method, endpoint, media, limit := "", "", "", githubGitResponseLimit
	switch request.Operation {
	case GitHubGitAdvertisement:
		if request.Body != nil {
			return nil, ErrGitHubGitRead
		}
		method, endpoint, media, limit = http.MethodGet, "/info/refs?service=git-upload-pack", "application/x-git-upload-pack-advertisement", githubGitAdvertisementLimit
	case GitHubGitUploadPack:
		if request.Body == nil {
			return nil, ErrGitHubGitRead
		}
		method, endpoint, media = http.MethodPost, "/git-upload-pack", "application/x-git-upload-pack-result"
	default:
		return nil, ErrGitHubGitRead
	}
	phase, cancel := context.WithTimeout(ctx, 90*time.Second)
	returned := false
	defer func() {
		if !returned {
			cancel()
		}
	}()
	stopInput := context.AfterFunc(phase, closeInput)
	defer stopInput()
	var data []byte
	if request.Body != nil {
		var err error
		data, err = io.ReadAll(io.LimitReader(request.Body, githubGitRequestLimit+1))
		closeInput()
		if err != nil || len(data) == 0 || int64(len(data)) > githubGitRequestLimit || phase.Err() != nil {
			return nil, gitReadError(phase)
		}
	}
	var input io.ReadCloser
	if data != nil {
		input = io.NopCloser(bytes.NewReader(data))
	}
	q, err := http.NewRequestWithContext(phase, method, githubGitRepository+endpoint, input)
	if err != nil {
		if input != nil {
			_ = input.Close()
		}
		return nil, ErrGitHubGitRead
	}
	q.Header.Set("Accept", media)
	q.Header.Set("Accept-Encoding", "identity")
	q.Header.Set("User-Agent", "sfl-offline-alpha-git-read")
	if method == http.MethodPost {
		q.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		q.ContentLength = int64(len(data))
	}
	if request.Protocol == 2 {
		q.Header.Set("Git-Protocol", "version=2")
	}
	state := r.state
	state.mu.Lock()
	now := state.now()
	if len(state.token) == 0 || now.Before(state.receipt.CheckedAt) || !now.Before(state.receipt.ExpiresAt.Add(-time.Minute)) || phase.Err() != nil {
		state.mu.Unlock()
		if input != nil {
			_ = input.Close()
		}
		return nil, gitReadError(phase)
	}
	q.SetBasicAuth("x-access-token", string(state.token))
	state.mu.Unlock()
	response, err := state.inner.RoundTrip(q)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, gitReadError(phase)
	}
	validType := false
	for name, values := range response.Header {
		if strings.EqualFold(name, "Content-Type") {
			if validType || len(values) != 1 || values[0] != media {
				_ = response.Body.Close()
				return nil, ErrGitHubGitRead
			}
			validType = true
		}
		if strings.EqualFold(name, "Content-Encoding") && (len(values) != 1 || (values[0] != "" && values[0] != "identity")) {
			_ = response.Body.Close()
			return nil, ErrGitHubGitRead
		}
	}
	if response.StatusCode != http.StatusOK || !validType || response.Uncompressed || response.ContentLength > limit || phase.Err() != nil {
		_ = response.Body.Close()
		return nil, gitReadError(phase)
	}
	output := &githubGitReadBody{&githubGitReadBodyState{inner: response.Body, ctx: phase, cancel: cancel, remaining: limit}}
	context.AfterFunc(phase, func() { _ = output.Close() })
	returned = true
	return output, nil
}

type githubGitReadBody struct{ state *githubGitReadBodyState }
type githubGitReadBodyState struct {
	inner     io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	remaining int64
	once      sync.Once
	closeErr  error
}

func (githubGitReadBody) String() string   { return "GitHubGitReadBody{redacted}" }
func (githubGitReadBody) GoString() string { return "GitHubGitReadBody{redacted}" }
func (githubGitReadBody) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubGitReadBody{redacted}"`), nil
}
func (b *githubGitReadBody) Close() error {
	state := b.state
	state.once.Do(func() {
		if state.inner.Close() != nil {
			state.closeErr = ErrGitHubGitRead
		}
		state.cancel()
	})
	return state.closeErr
}
func (b *githubGitReadBody) Read(p []byte) (int, error) {
	state := b.state
	if state.ctx.Err() != nil {
		_ = b.Close()
		return 0, gitReadError(state.ctx)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if state.remaining == 0 {
		var probe [1]byte
		n, err := state.inner.Read(probe[:])
		if n > 0 || (err != nil && err != io.EOF) {
			_ = b.Close()
			return 0, ErrGitHubGitRead
		}
		if err == io.EOF {
			_ = b.Close()
			return 0, io.EOF
		}
		return 0, nil
	}
	if int64(len(p)) > state.remaining {
		p = p[:state.remaining]
	}
	n, err := state.inner.Read(p)
	state.remaining -= int64(n)
	if err != nil {
		_ = b.Close()
		if err != io.EOF {
			return n, ErrGitHubGitRead
		}
	}
	return n, err
}

//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gitVMReadTestBinding() gitVMReadBinding {
	return gitVMReadBinding{1, "source-vm-git-read", gitMaterializationBinding{strings.Repeat("a", 32), Digest("release"), Digest("runtime"), testSnapshot()}, strings.Repeat("b", 32), 1}
}

type gitVMReadTestResult struct {
	audit gitVMReadAudit
	err   error
}

func startGitVMReadTest(t *testing.T, source GitReadExchanger, check func(context.Context, gitVMReadBinding) error) (*gitVMReadClient, context.CancelFunc, <-chan gitVMReadTestResult) {
	t.Helper()
	host, guest := net.Pipe()
	client, err := newGitVMReadClient(guest, gitVMReadTestBinding())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan gitVMReadTestResult, 1)
	go func() {
		audit, err := serveGitVMRead(ctx, host, gitVMReadTestBinding(), source, check)
		done <- gitVMReadTestResult{audit, err}
	}()
	t.Cleanup(func() { cancel(); client.Close() })
	return client, cancel, done
}

func finishGitVMReadTest(t *testing.T, done <-chan gitVMReadTestResult) gitVMReadTestResult {
	t.Helper()
	select {
	case result := <-done:
		if !result.audit.Stopped {
			t.Fatal("channel returned without stopped audit")
		}
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not drain")
	}
	return gitVMReadTestResult{}
}

func admitGitVMReadTest(ctx context.Context, b gitVMReadBinding) error {
	if ctx.Err() != nil || b != gitVMReadTestBinding() {
		return errGitVMRead
	}
	return nil
}

func TestGitVMReadExactOperationsThroughActualTLS(t *testing.T) {
	var seen atomic.Int32
	installation, _ := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		seen.Add(1)
		operation := GitHubGitAdvertisement
		if q.Method == "POST" {
			operation = GitHubGitUploadPack
			data, err := io.ReadAll(q.Body)
			if err != nil || !bytes.Equal(data, []byte{0, 1, 2, 255}) {
				t.Error("opaque upload body changed", data, err)
			}
		}
		w.Header().Set("Content-Type", gitReadMedia(operation))
		w.Write([]byte{0, 1, 255, 2})
	}))
	reader, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	client, _, done := startGitVMReadTest(t, reader, admitGitVMReadTest)
	for _, protocol := range []int{0, 2} {
		for _, op := range []GitHubGitOperation{GitHubGitAdvertisement, GitHubGitUploadPack} {
			q := GitHubGitReadRequest{Operation: op, Protocol: protocol}
			if op == GitHubGitUploadPack {
				q.Body = io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 255}))
			}
			body, err := client.Exchange(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil || !bytes.Equal(data, []byte{0, 1, 255, 2}) {
				t.Fatal("opaque response changed", data, err)
			}
		}
	}
	client.Close()
	result := finishGitVMReadTest(t, done)
	if result.err != nil || result.audit.Requests != 4 || seen.Load() != 4 || result.audit.RequestBytes != 8 || result.audit.ResponseBytes != 16 {
		t.Fatal("wrong bounded exchange audit", result, seen.Load())
	}
}

func TestGitVMReadRejectsForeignAndExtendedRequestsBeforeUpstream(t *testing.T) {
	for _, kind := range []string{"binding", "sequence", "operation", "protocol", "empty-upload", "advertisement-body", "unknown-url", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			host, guest := net.Pipe()
			defer guest.Close()
			var calls atomic.Int32
			done := make(chan gitVMReadTestResult, 1)
			go func() {
				a, e := serveGitVMRead(context.Background(), host, gitVMReadTestBinding(), gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
					calls.Add(1)
					return io.NopCloser(strings.NewReader("unexpected")), nil
				}), admitGitVMReadTest)
				done <- gitVMReadTestResult{a, e}
			}()
			q := gitVMReadRequest{1, Digest(gitVMReadTestBinding()), 1, GitHubGitAdvertisement, 2, nil}
			switch kind {
			case "binding":
				q.BindingHash = Digest("foreign")
			case "sequence":
				q.Sequence = 2
			case "operation":
				q.Operation = "receive-pack"
			case "protocol":
				q.Protocol = 1
			case "empty-upload":
				q.Operation = GitHubGitUploadPack
			case "advertisement-body":
				q.Body = []byte("data")
			}
			raw, _ := json.Marshal(q)
			if kind == "unknown-url" {
				raw = append([]byte(`{"URL":"https://foreign.invalid",`), raw[1:]...)
			}
			if kind == "duplicate" {
				raw = append([]byte(`{"Version":1,`), raw[1:]...)
			}
			var prefix [4]byte
			binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
			_, _ = guest.Write(append(prefix[:], raw...))
			result := finishGitVMReadTest(t, done)
			if !errors.Is(result.err, errGitVMRead) || calls.Load() != 0 {
				t.Fatal("invalid request reached upstream", result, calls.Load())
			}
		})
	}
}

func TestGitVMReadRejectsStaleAdmissionWithoutPartialResponse(t *testing.T) {
	for _, failAt := range []int32{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			var checks, calls atomic.Int32
			client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				return io.NopCloser(strings.NewReader("partial source")), nil
			}), func(context.Context, gitVMReadBinding) error {
				if checks.Add(1) == failAt {
					return errors.New("private admission metadata")
				}
				return nil
			})
			body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
			if body != nil || !errors.Is(err, errGitVMRead) || strings.Contains(err.Error(), "private") {
				t.Fatal("stale response accepted or leaked", body, err)
			}
			result := finishGitVMReadTest(t, done)
			if result.err == nil || calls.Load() != failAt-1 {
				t.Fatal("wrong admission order", result, calls.Load())
			}
		})
	}
}

func TestGitVMReadMalformedRepliesSpendClient(t *testing.T) {
	for _, kind := range []string{"binding", "sequence", "chunk", "failed", "done-data", "empty", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			host, guest := net.Pipe()
			client, err := newGitVMReadClient(guest, gitVMReadTestBinding())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer host.Close()
				q, e := readVMRecord[gitVMReadRequest](host, gitVMReadRequestFrame)
				if e != nil {
					return
				}
				r := gitVMReadReply{1, q.BindingHash, q.Sequence, 0, nil, true, false}
				switch kind {
				case "binding":
					r.BindingHash = Digest("foreign")
				case "sequence":
					r.Sequence++
				case "chunk":
					r.Chunk++
				case "failed":
					r.Failed = true
				case "done-data":
					r.Body = []byte("unexpected")
				case "empty":
					r.Done = false
				case "oversized":
					r.Done = false
					r.Body = make([]byte, gitVMReadChunk+1)
				}
				_ = writeVMRecord(host, r, gitVMReadReplyFrame)
			}()
			body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
			if body != nil || !errors.Is(err, errGitVMRead) {
				t.Fatal("malformed reply accepted", body, err)
			}
			if again, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); again != nil || err == nil {
				t.Fatal("spent client retried")
			}
			<-done
		})
	}
}

type gitVMReadBlockedBody struct {
	entered, closed, retire chan struct{}
	once                    sync.Once
}

func (b *gitVMReadBlockedBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.closed
	<-b.retire
	return 0, context.Canceled
}
func (b *gitVMReadBlockedBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestGitVMReadCancellationJoinsOwnedBodyRead(t *testing.T) {
	body := &gitVMReadBlockedBody{make(chan struct{}), make(chan struct{}), make(chan struct{}), sync.Once{}}
	client, cancel, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) { return body, nil }), admitGitVMReadTest)
	returned := make(chan error, 1)
	go func() {
		r, e := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
		if r != nil {
			r.Close()
		}
		returned <- e
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("read not entered")
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("body not closed")
	}
	select {
	case result := <-done:
		t.Fatal("channel released while owned Read remained active", result)
	case <-time.After(30 * time.Millisecond):
	}
	close(body.retire)
	result := finishGitVMReadTest(t, done)
	if result.err == nil {
		t.Fatal("cancellation accepted")
	}
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("client accepted cancelled read")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not unblock")
	}
}

func TestGitVMReadAdmissionAndClientLimits(t *testing.T) {
	var calls atomic.Int32
	client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(_ context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
		calls.Add(1)
		if q.Body != nil {
			_, _ = io.Copy(io.Discard, q.Body)
			q.Body.Close()
		}
		return io.NopCloser(strings.NewReader("ok")), nil
	}), admitGitVMReadTest)
	for range gitVMReadRequests {
		body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	if body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
		t.Fatal("request count exceeded")
	}
	result := finishGitVMReadTest(t, done)
	if calls.Load() != gitVMReadRequests || result.audit.Requests != gitVMReadRequests {
		t.Fatal("unbounded calls", calls.Load(), result)
	}
}

func TestGitVMReadRefusesOversizedInputAndResponse(t *testing.T) {
	for _, requestOversize := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "request"}[requestOversize], func(t *testing.T) {
			var calls atomic.Int32
			client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				return io.NopCloser(io.LimitReader(gitReadZeros{}, githubGitAdvertisementLimit+1)), nil
			}), admitGitVMReadTest)
			q := GitHubGitReadRequest{Operation: GitHubGitAdvertisement}
			if requestOversize {
				q.Operation = GitHubGitUploadPack
				q.Body = io.NopCloser(io.LimitReader(gitReadZeros{}, githubGitRequestLimit+1))
			}
			body, err := client.Exchange(context.Background(), q)
			if body != nil || err == nil {
				t.Fatal("oversized data accepted")
			}
			client.Close()
			result := finishGitVMReadTest(t, done)
			want := int32(1)
			if requestOversize {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("wrong upstream call count", calls.Load(), result)
			}
		})
	}
}

type gitVMReadCloseError struct{ io.Reader }

func (gitVMReadCloseError) Close() error { return errors.New("private close metadata") }

func TestGitVMReadCloseErrorsNeverCompleteExchange(t *testing.T) {
	for _, input := range []bool{false, true} {
		t.Run(map[bool]string{false: "response", true: "input"}[input], func(t *testing.T) {
			var calls atomic.Int32
			client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
				calls.Add(1)
				return gitVMReadCloseError{strings.NewReader("partial")}, nil
			}), admitGitVMReadTest)
			q := GitHubGitReadRequest{Operation: GitHubGitAdvertisement}
			if input {
				q.Operation = GitHubGitUploadPack
				q.Body = gitVMReadCloseError{strings.NewReader("input")}
			}
			body, err := client.Exchange(context.Background(), q)
			if body != nil || !errors.Is(err, errGitVMRead) || strings.Contains(err.Error(), "private") {
				t.Fatal("close error accepted or leaked", body, err)
			}
			client.Close()
			finishGitVMReadTest(t, done)
			want := int32(1)
			if input {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("wrong upstream count", calls.Load())
			}
		})
	}
}

func TestGitVMReadInputCancellationRetainsReadAndNeverDispatches(t *testing.T) {
	var calls atomic.Int32
	client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(strings.NewReader("ok")), nil
	}), admitGitVMReadTest)
	body := &gitVMReadBlockedBody{make(chan struct{}), make(chan struct{}), make(chan struct{}), sync.Once{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := client.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: body})
		returned <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("input read not entered")
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("input not closed")
	}
	select {
	case err := <-returned:
		t.Fatal("returned with owned input Read active", err)
	case <-time.After(30 * time.Millisecond):
	}
	if calls.Load() != 0 {
		t.Fatal("unfinished input dispatched")
	}
	close(body.retire)
	select {
	case err := <-returned:
		if !errors.Is(err, errGitVMRead) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("input Read not drained")
	}
	response, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
	if err != nil {
		t.Fatal("undispatched input cancellation spent unrelated channel", err)
	}
	response.Close()
	client.Close()
	if result := finishGitVMReadTest(t, done); result.err != nil || calls.Load() != 1 {
		t.Fatal(result, calls.Load())
	}
}

func TestGitVMReadQueuedCallerCancellationDoesNotCancelActiveExchange(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		close(entered)
		<-release
		return io.NopCloser(strings.NewReader("ok")), nil
	}), admitGitVMReadTest)
	first := make(chan error, 1)
	go func() {
		body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
		if body != nil {
			body.Close()
		}
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("active exchange not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if body, err := client.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitAdvertisement}); body != nil || err == nil {
		t.Fatal("queued cancellation accepted")
	}
	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal("queued caller poisoned active exchange", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active exchange did not finish")
	}
	client.Close()
	if result := finishGitVMReadTest(t, done); result.err != nil || result.audit.Requests != 1 {
		t.Fatal(result)
	}
}

func TestGitVMReadAggregateInputBudgetStopsBeforeFifthUpload(t *testing.T) {
	var calls atomic.Int32
	client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(_ context.Context, q GitHubGitReadRequest) (io.ReadCloser, error) {
		calls.Add(1)
		n, err := io.Copy(io.Discard, q.Body)
		q.Body.Close()
		if err != nil || n != githubGitRequestLimit {
			return nil, errors.New("input differed")
		}
		return io.NopCloser(strings.NewReader("ok")), nil
	}), admitGitVMReadTest)
	for range 4 {
		body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: io.NopCloser(io.LimitReader(gitReadZeros{}, githubGitRequestLimit))})
		if err != nil {
			t.Fatal(err)
		}
		body.Close()
	}
	if body, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: io.NopCloser(io.LimitReader(gitReadZeros{}, 1))}); body != nil || err == nil {
		t.Fatal("aggregate input budget exceeded")
	}
	result := finishGitVMReadTest(t, done)
	if calls.Load() != 4 || result.audit.RequestBytes != gitVMReadInputBytes {
		t.Fatal("aggregate input count differed", calls.Load(), result)
	}
}

func TestGitVMReadClientTimeoutCancelsHostBorrow(t *testing.T) {
	body := &gitVMReadBlockedBody{make(chan struct{}), make(chan struct{}), make(chan struct{}), sync.Once{}}
	client, cancelHost, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) { return body, nil }), admitGitVMReadTest)
	defer cancelHost()
	var retired sync.Once
	retire := func() { retired.Do(func() { close(body.retire) }) }
	defer retire()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := client.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
		returned <- err
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("upstream read did not start")
	}
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("timed-out client succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client deadline did not return")
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("client timeout retained host upstream borrow")
	}
	select {
	case result := <-done:
		t.Fatal("released host before upstream Read drained", result)
	case <-time.After(20 * time.Millisecond):
	}
	retire()
	if result := finishGitVMReadTest(t, done); result.err == nil {
		t.Fatal("peer cancellation accepted as success")
	}
}

func TestGitVMReadPrefetchIsBoundedAndCancelsUpstream(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &gitVMReadBlockedBody{make(chan struct{}), make(chan struct{}), make(chan struct{}), sync.Once{}}
	var retired sync.Once
	retire := func() { retired.Do(func() { close(body.retire) }) }
	defer retire()
	var calls atomic.Int32
	done := make(chan gitVMReadTestResult, 1)
	go func() {
		a, err := serveGitVMRead(ctx, host, gitVMReadTestBinding(), gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
			calls.Add(1)
			return body, nil
		}), admitGitVMReadTest)
		done <- gitVMReadTestResult{a, err}
	}()
	request := gitVMReadRequest{1, Digest(gitVMReadTestBinding()), 1, GitHubGitAdvertisement, 0, nil}
	if writeVMRecord(guest, request, gitVMReadRequestFrame) != nil {
		t.Fatal("first request")
	}
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("upstream read missing")
	}
	for _, sequence := range []int{2, 3} {
		request.Sequence = sequence
		if writeVMRecord(guest, request, gitVMReadRequestFrame) != nil {
			t.Fatal("prefetch request")
		}
	}
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("overfull prefetch did not cancel upstream")
	}
	retire()
	result := finishGitVMReadTest(t, done)
	if result.err == nil || calls.Load() != 1 {
		t.Fatal("prefetch dispatched or succeeded", result, calls.Load())
	}
}

type gitVMReadInputSpy struct {
	reads, closed atomic.Int32
}

func (s *gitVMReadInputSpy) Read(p []byte) (int, error) {
	s.reads.Add(1)
	return copy(p, "queued"), io.EOF
}
func (s *gitVMReadInputSpy) Close() error { s.closed.Add(1); return nil }

func TestGitVMReadQueuedUploadDoesNotAllocateInput(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	client, _, done := startGitVMReadTest(t, gitExchangeFunc(func(context.Context, GitHubGitReadRequest) (io.ReadCloser, error) {
		close(entered)
		<-release
		return io.NopCloser(strings.NewReader("ok")), nil
	}), admitGitVMReadTest)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first := make(chan error, 1)
	go func() {
		r, err := client.Exchange(context.Background(), GitHubGitReadRequest{Operation: GitHubGitAdvertisement})
		if r != nil {
			r.Close()
		}
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first operation did not start")
	}
	body := &gitVMReadInputSpy{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if r, err := client.Exchange(ctx, GitHubGitReadRequest{Operation: GitHubGitUploadPack, Body: body}); r != nil || err == nil {
		t.Fatal("queued cancellation succeeded")
	}
	if body.reads.Load() != 0 || body.closed.Load() != 1 {
		t.Fatalf("queued input read/closed %d/%d", body.reads.Load(), body.closed.Load())
	}
	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active operation stuck")
	}
	client.Close()
	if result := finishGitVMReadTest(t, done); result.err != nil {
		t.Fatal(result)
	}
}

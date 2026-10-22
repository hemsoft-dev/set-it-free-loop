//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errGitVMRead = errors.New("VM source read channel refused")

const (
	gitVMReadRequestFrame = 6 << 20
	gitVMReadReplyFrame   = 48 << 10
	gitVMReadChunk        = 32 << 10
	gitVMReadRequests     = 32
	gitVMReadInputBytes   = 16 << 20
	gitVMReadOutputBytes  = 128 << 20
)

// Only the trusted source factory creates this channel binding. Generation is
// the retained host credential-owner generation, never a token. This purpose
// cannot represent a provider session, auth return or physical-stop proof.
type gitVMReadBinding struct {
	Version    int
	Purpose    string
	Source     gitMaterializationBinding
	ChannelID  string
	Generation uint64
}

func (b gitVMReadBinding) valid() bool {
	return b.Version == 1 && b.Purpose == "source-vm-git-read" && b.Source.valid() && gitQuarantineHex(b.ChannelID, 32) && b.Generation > 0
}

// No URL, headers, credential, arbitrary path or write operation is encoded.
type gitVMReadRequest struct {
	Version     int
	BindingHash string
	Sequence    int
	Operation   GitHubGitOperation
	Protocol    int
	Body        []byte
}

func (q gitVMReadRequest) valid(hash string, sequence int) bool {
	if q.Version != 1 || q.BindingHash != hash || q.Sequence != sequence || (q.Protocol != 0 && q.Protocol != 2) {
		return false
	}
	switch q.Operation {
	case GitHubGitAdvertisement:
		return q.Body == nil
	case GitHubGitUploadPack:
		return len(q.Body) > 0 && int64(len(q.Body)) <= githubGitRequestLimit
	}
	return false
}

type gitVMReadReply struct {
	Version      int
	BindingHash  string
	Sequence     int
	Chunk        int
	Body         []byte
	Done, Failed bool
}

type gitVMReadAudit struct {
	Requests                    int
	RequestBytes, ResponseBytes int64
	Stopped, Failed             bool
}

// The caller supplies an already authenticated original-VM channel and a
// retained generation reader. check must revalidate that original VM, channel,
// binding and generation. A matching wire hash is not admission evidence.
// Construction/serving cannot create, refresh or dispose of credentials.
func serveGitVMRead(ctx context.Context, channel io.ReadWriteCloser, binding gitVMReadBinding, source GitReadExchanger, check func(context.Context, gitVMReadBinding) error) (audit gitVMReadAudit, result error) {
	defer func() {
		if channel != nil {
			_ = channel.Close()
		}
		audit.Stopped = true
		audit.Failed = result != nil
	}()
	if ctx == nil || ctx.Err() != nil || channel == nil || !binding.valid() || source == nil || check == nil {
		return audit, errGitVMRead
	}
	phase, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	join := gitBrokerAfterFunc(phase, func() { _ = channel.Close() })
	defer join()
	// Keep reading the duplex channel while upstream work owns a response
	// body. Peer closure must cancel that borrow immediately, rather than
	// waiting for the channel-wide deadline. One prefetched request is allowed;
	// further pipelining refuses the channel instead of growing a queue.
	incoming := make(chan gitVMReadRequest, 1)
	ended := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		var result error
		defer func() { ended <- result; cancel(); close(readerDone) }()
		for {
			q, err := readVMRecord[gitVMReadRequest](channel, gitVMReadRequestFrame)
			if err != nil {
				result = err
				return
			}
			select {
			case incoming <- q:
			default:
				result = errGitVMRead
				return
			}
		}
	}()
	defer func() { cancel(); _ = channel.Close(); <-readerDone }()
	deadline, _ := phase.Deadline()
	hash := Digest(binding)
	for sequence := 1; ; sequence++ {
		var q gitVMReadRequest
		var err error
		select {
		case q = <-incoming:
		case err = <-ended:
		case <-phase.Done():
			select {
			case err = <-ended:
			default:
				err = errGitVMRead
			}
		}
		if err == io.EOF && ctx.Err() == nil && time.Now().Before(deadline) && len(incoming) == 0 {
			return audit, nil
		}
		if err != nil || !q.valid(hash, sequence) || sequence > gitVMReadRequests || phase.Err() != nil || audit.RequestBytes+int64(len(q.Body)) > gitVMReadInputBytes {
			return audit, errGitVMRead
		}
		audit.Requests++
		audit.RequestBytes += int64(len(q.Body))
		chunk := 0
		send := func(data []byte, done, failed bool) error {
			reply := gitVMReadReply{1, hash, sequence, chunk, data, done, failed}
			if writeVMRecord(channel, reply, gitVMReadReplyFrame) != nil {
				return errGitVMRead
			}
			chunk++
			return nil
		}
		if check(phase, binding) != nil || phase.Err() != nil {
			_ = send(nil, true, true)
			return audit, errGitVMRead
		}
		request := GitHubGitReadRequest{Operation: q.Operation, Protocol: q.Protocol}
		if q.Operation == GitHubGitUploadPack {
			request.Body = io.NopCloser(bytes.NewReader(q.Body))
		}
		body, exchangeErr := source.Exchange(phase, request)
		if request.Body != nil {
			_ = request.Body.Close()
		}
		clear(q.Body)
		if exchangeErr != nil || body == nil {
			if body != nil {
				_ = body.Close()
			}
			_ = send(nil, true, true)
			return audit, errGitVMRead
		}
		// This join includes any in-flight Close. The body remains borrowed by
		// its host generation until its Read and Close have both returned.
		var closeOnce sync.Once
		var closeErr error
		closeBody := func() { closeOnce.Do(func() { closeErr = body.Close() }) }
		joinBody := gitBrokerAfterFunc(phase, closeBody)
		limit := githubGitResponseLimit
		if q.Operation == GitHubGitAdvertisement {
			limit = githubGitAdvertisementLimit
		}
		buffer := make([]byte, gitVMReadChunk)
		var count int64
		var readErr error
		idle := 0
		for {
			n, err := body.Read(buffer)
			if n < 0 || n > len(buffer) || count+int64(n) > limit || audit.ResponseBytes+int64(n) > gitVMReadOutputBytes || phase.Err() != nil {
				readErr = errGitVMRead
				break
			}
			if n > 0 {
				count += int64(n)
				audit.ResponseBytes += int64(n)
				idle = 0
				if send(buffer[:n], false, false) != nil {
					readErr = errGitVMRead
					break
				}
			} else {
				idle++
			}
			if err == io.EOF {
				break
			}
			if err != nil || idle > 8 {
				readErr = errGitVMRead
				break
			}
		}
		clear(buffer)
		closeBody()
		joinBody()
		if readErr != nil || closeErr != nil || check(phase, binding) != nil || phase.Err() != nil {
			_ = send(nil, true, true)
			return audit, errGitVMRead
		}
		if send(nil, true, false) != nil {
			return audit, errGitVMRead
		}
	}
}

// The guest broker receives this typed exchanger after independently checking
// its real helper peer. It buffers one bounded response before giving Git any
// bytes. Partial/error replies therefore never become a successful response.
// Any channel/protocol failure spends this client. There is no replay/retry.
type gitVMReadClient struct {
	channel       io.ReadWriteCloser
	binding       gitVMReadBinding
	operation     chan struct{}
	mu            sync.Mutex
	closed        bool
	sequence      int
	input, output int64
}

func newGitVMReadClient(channel io.ReadWriteCloser, binding gitVMReadBinding) (*gitVMReadClient, error) {
	if channel == nil || !binding.valid() {
		if channel != nil {
			_ = channel.Close()
		}
		return nil, errGitVMRead
	}
	c := &gitVMReadClient{channel: channel, binding: binding, operation: make(chan struct{}, 1)}
	c.operation <- struct{}{}
	return c, nil
}

func (c *gitVMReadClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.channel.Close()
}

func gitVMReadInput(ctx context.Context, q GitHubGitReadRequest) ([]byte, error) {
	var once sync.Once
	var closeErr error
	closeInput := func() {
		if q.Body != nil {
			once.Do(func() { closeErr = q.Body.Close() })
		}
	}
	if q.Body != nil {
		defer closeInput()
	}
	if ctx == nil || ctx.Err() != nil || (q.Protocol != 0 && q.Protocol != 2) {
		return nil, errGitVMRead
	}
	if q.Operation == GitHubGitAdvertisement && q.Body == nil {
		return nil, nil
	}
	if q.Operation != GitHubGitUploadPack || q.Body == nil {
		return nil, errGitVMRead
	}
	join := gitBrokerAfterFunc(ctx, closeInput)
	defer join()
	data, err := io.ReadAll(io.LimitReader(q.Body, githubGitRequestLimit+1))
	closeInput()
	if err != nil || closeErr != nil || len(data) == 0 || int64(len(data)) > githubGitRequestLimit || ctx.Err() != nil {
		clear(data)
		return nil, errGitVMRead
	}
	return data, nil
}

func (c *gitVMReadClient) Exchange(ctx context.Context, request GitHubGitReadRequest) (io.ReadCloser, error) {
	if c == nil || ctx == nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, errGitVMRead
	}
	phase, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	closePendingInput := func() {
		if request.Body != nil {
			_ = request.Body.Close()
		}
	}
	select {
	case <-phase.Done():
		closePendingInput()
		return nil, errGitVMRead
	case <-c.operation:
	}
	defer func() { c.operation <- struct{}{} }()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed || phase.Err() != nil {
		closePendingInput()
		return nil, errGitVMRead
	}
	if c.sequence >= gitVMReadRequests || request.Operation == GitHubGitUploadPack && c.input >= gitVMReadInputBytes {
		closePendingInput()
		_ = c.Close()
		return nil, errGitVMRead
	}
	// Serialize intake as well as exchange. Queued callers retain their own
	// deadlines and close unread inputs without allocating another body buffer.
	data, err := gitVMReadInput(phase, request)
	if err != nil {
		return nil, errGitVMRead
	}
	defer clear(data)
	if c.input+int64(len(data)) > gitVMReadInputBytes {
		_ = c.Close()
		return nil, errGitVMRead
	}
	join := gitBrokerAfterFunc(phase, func() { _ = c.Close() })
	defer join()
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	c.sequence++
	c.input += int64(len(data))
	hash := Digest(c.binding)
	q := gitVMReadRequest{1, hash, c.sequence, request.Operation, request.Protocol, data}
	if writeVMRecord(c.channel, q, gitVMReadRequestFrame) != nil {
		return nil, errGitVMRead
	}
	limit := githubGitResponseLimit
	if request.Operation == GitHubGitAdvertisement {
		limit = githubGitAdvertisementLimit
	}
	var output []byte
	defer func() {
		if !ok {
			clear(output)
		}
	}()
	for chunk := 0; ; chunk++ {
		reply, err := readVMRecord[gitVMReadReply](c.channel, gitVMReadReplyFrame)
		if err != nil || reply.Version != 1 || reply.BindingHash != hash || reply.Sequence != c.sequence || reply.Chunk != chunk || reply.Failed || phase.Err() != nil {
			return nil, errGitVMRead
		}
		if reply.Done {
			if reply.Body != nil {
				return nil, errGitVMRead
			}
			ok = true
			return io.NopCloser(bytes.NewReader(output)), nil
		}
		if len(reply.Body) == 0 || len(reply.Body) > gitVMReadChunk || int64(len(output))+int64(len(reply.Body)) > limit || c.output+int64(len(reply.Body)) > gitVMReadOutputBytes {
			return nil, errGitVMRead
		}
		c.output += int64(len(reply.Body))
		output = append(output, reply.Body...)
		clear(reply.Body)
	}
}

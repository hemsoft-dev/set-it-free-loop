package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ErrWorkerStopPending prevents lease release while a supervisor cannot attest
// that the physical worker stopped. Expiry is not permission to start another.
var ErrWorkerStopPending = errors.New("worker termination requires supervisor recovery before lease release")

// Worker is an invocation-specific channel to a trusted worker wrapper. Stop
// must close its pipes and confirm termination/reaping of the entire worker,
// including descendants, even after Wait succeeded. It must honor its context.
// The launcher must persist its own job/worker recovery identity before launch.
// A provider's report or exit code cannot supply this attestation.
type Worker interface {
	Input() io.WriteCloser
	Output() io.ReadCloser
	Wait() error
	Stop(context.Context) error
}

// WorkerLauncher is a trusted supervisor boundary, not model input. Inspect
// checks actual client/model/subscription identity without inference. Start must
// return a Worker whenever a process may exist, including on error. A nil Worker
// on failure attests no work started. It must launch promptly and honor ctx.
// No production launcher is supplied: host identity, auth, filesystem, memory,
// inference/tool and network enforcement still require separate qualification.
type WorkerLauncher interface {
	Inspect(context.Context, Invocation) (Identity, error)
	Start(context.Context, Invocation) (Worker, error)
}

// WorkerDriver transports a bounded JSONL protocol to a trusted wrapper, which
// will in turn drive an official client. It is not a direct CLI or MCP protocol.
// Worker output can request only session-bound Read/Search, report, or failure.
// It cannot select an executable, credentials, job, identity or GitHub endpoint.
type WorkerDriver struct{ Launcher WorkerLauncher }

// Keep only canonical causes and a bounded phase classification. Joining the
// original launcher error could disclose host paths or credentials; dropping
// context sentinels would turn cancellation into provider_unavailable later.
func workerLauncherError(err error, beforeWork bool) error {
	phase := PhaseError{State: phaseState(err), BeforeWork: beforeWork}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.Join(phase, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return errors.Join(phase, context.Canceled)
	case errors.Is(err, ErrLiveBlocked):
		return errors.Join(phase, ErrLiveBlocked)
	default:
		return phase
	}
}

func (d WorkerDriver) Inspect(ctx context.Context, inv Invocation) (Identity, error) {
	if d.Launcher == nil {
		return Identity{}, ErrLiveBlocked
	}
	if err := inv.Limits.Validate(); err != nil {
		return Identity{}, adapterValidation(err, true)
	}
	identity, err := d.Launcher.Inspect(ctx, inv)
	if err != nil {
		return Identity{}, workerLauncherError(err, true)
	}
	return identity, nil
}

// WorkerMessage is one JSON object per line. Version is always 1. Each kind
// accepts only its own fields. Worker failure never attests BeforeWork/refund.
type WorkerMessage struct {
	Version    int             `json:"version"`
	Kind       string          `json:"kind"`
	Revision   string          `json:"revision,omitempty"`
	Path       string          `json:"path,omitempty"`
	Needle     string          `json:"needle,omitempty"`
	MaxResults int             `json:"max_results,omitempty"`
	Report     json.RawMessage `json:"report,omitempty"`
	State      string          `json:"state,omitempty"`
}

// WorkerReply never includes raw controller errors or store paths. A denial is
// visible to the worker; authoritative coverage comes from the source broker.
type WorkerReply struct {
	Version int      `json:"version"`
	Text    string   `json:"text,omitempty"`
	Matches []Anchor `json:"matches,omitempty"`
	Denied  bool     `json:"denied"`
}

func workerInvalid() error {
	return adapterValidation(errors.New("invalid or incomplete bounded worker protocol"), false)
}

func decodeWorkerMessage(line []byte) (WorkerMessage, error) {
	var m WorkerMessage
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(&struct{}{}) != io.EOF || m.Version != 1 {
		return m, workerInvalid()
	}
	// Empty but explicitly present unrelated fields are rejected too. This also
	// rejects duplicate keys rather than letting encoding/json pick the last one.
	allowed := map[string]bool{"version": true, "kind": true}
	switch m.Kind {
	case "read":
		allowed["revision"], allowed["path"] = true, true
		if m.Revision == "" || m.Path == "" {
			return m, workerInvalid()
		}
	case "search":
		allowed["revision"], allowed["needle"], allowed["max_results"] = true, true, true
		if m.Revision == "" || m.Needle == "" || m.MaxResults < 1 {
			return m, workerInvalid()
		}
	case "report":
		allowed["report"] = true
		if len(m.Report) == 0 || bytes.Equal(m.Report, []byte("null")) {
			return m, workerInvalid()
		}
	case "failure":
		allowed["state"] = true
		switch m.State {
		case "quota_wait", "auth_failed", "provider_unavailable", "invalid_output", "incomplete_expired":
		default:
			return m, workerInvalid()
		}
	default:
		return m, workerInvalid()
	}
	d = json.NewDecoder(bytes.NewReader(line))
	_, _ = d.Token()
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return m, workerInvalid()
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return m, workerInvalid()
		}
	}
	return m, nil
}

func encodeWorkerInvocation(inv Invocation) ([]byte, error) {
	// Invocation JSON may escape each input byte up to sixfold, and contains
	// bounded trusted argument/schema metadata in addition to the prompt.
	if len(inv.Input) > inv.Limits.InputBytes || len(inv.Args) > 128 || len(inv.Environment) > 8 || len(inv.ReadOnlyFiles) > 1 {
		return nil, workerInvalid()
	}
	initial, err := json.Marshal(inv)
	if err != nil || len(initial) > 6*inv.Limits.InputBytes+(128<<10) {
		return nil, workerInvalid()
	}
	return initial, nil
}

func workerExchange(ctx context.Context, worker Worker, inv Invocation, initial []byte) ([]byte, error) {
	if _, err := worker.Input().Write(append(initial, '\n')); err != nil {
		return nil, workerInvalid()
	}
	frameLimit := inv.OutputLimit + 4096
	totalLimit := inv.OutputLimit + inv.Limits.ReaderCalls*2048 + 8192
	scan := bufio.NewScanner(io.LimitReader(worker.Output(), int64(totalLimit+1)))
	scan.Buffer(make([]byte, 4096), frameLimit+1)
	count, used, replyBytes := 0, 0, 0
	var report []byte
	var failure error
	terminal := false
	for scan.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		line := scan.Bytes()
		used += len(line) + 1
		count++
		if terminal || used > totalLimit || len(line) > frameLimit || count > inv.Limits.ReaderCalls+1 {
			return nil, workerInvalid()
		}
		m, err := decodeWorkerMessage(line)
		if err != nil {
			return nil, err
		}
		switch m.Kind {
		case "report":
			if len(m.Report) > inv.OutputLimit {
				return nil, workerInvalid()
			}
			report, terminal = append([]byte{}, m.Report...), true
		case "failure":
			failure, terminal = PhaseError{State: m.State}, true
		default:
			if inv.Source == nil {
				return nil, workerInvalid()
			}
			reply := WorkerReply{Version: 1}
			if m.Kind == "read" {
				reply.Text, err = inv.Source.Read(ctx, m.Revision, m.Path)
			} else {
				reply.Matches, err = inv.Source.Search(ctx, m.Revision, m.Needle, m.MaxResults)
			}
			if err != nil {
				reply = WorkerReply{Version: 1, Denied: true}
			}
			data, err := json.Marshal(reply)
			replyBytes += len(data) + 1
			if err != nil || replyBytes > 6*inv.Limits.ReaderBytes+inv.Limits.ReaderCalls*8192 {
				return nil, workerInvalid()
			}
			if _, err = worker.Input().Write(append(data, '\n')); err != nil {
				return nil, workerInvalid()
			}
		}
		if terminal {
			// End the request channel and require stdout EOF. A valid report
			// followed by arbitrary data, a second report or a failed process
			// never completes a phase.
			_ = worker.Input().Close()
		}
	}
	if scan.Err() != nil || !terminal {
		return nil, workerInvalid()
	}
	if err := worker.Wait(); err != nil {
		return nil, workerInvalid()
	}
	return report, failure
}

func (d WorkerDriver) Execute(ctx context.Context, inv Invocation) ([]byte, Identity, error) {
	if d.Launcher == nil {
		return nil, Identity{}, ErrLiveBlocked
	}
	if err := inv.Limits.Validate(); err != nil || inv.OutputLimit != inv.Limits.OutputBytes || !inv.Provider.Valid() {
		return nil, Identity{}, adapterValidation(errors.New("invalid worker invocation limits"), true)
	}
	ctx, cancel := context.WithTimeout(ctx, inv.Limits.Timeout)
	defer cancel()
	initial, err := encodeWorkerInvocation(inv)
	if err != nil {
		return nil, Identity{}, adapterValidation(err, true)
	}
	if ctx.Err() != nil {
		return nil, Identity{}, errors.Join(ctx.Err(), PhaseError{State: phaseState(ctx.Err()), BeforeWork: true})
	}
	worker, startErr := d.Launcher.Start(ctx, inv)
	if worker == nil {
		if startErr == nil {
			startErr = errors.New("launcher returned no worker")
		}
		return nil, Identity{}, workerLauncherError(startErr, true)
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	exchangeCtx, exchangeCancel := context.WithCancel(ctx)
	defer exchangeCancel()
	if startErr == nil {
		go func() { data, err := workerExchange(exchangeCtx, worker, inv, initial); done <- result{data, err} }()
	} else {
		done <- result{err: workerLauncherError(startErr, false)}
	}
	var r result
	collected := false
	select {
	case r = <-done:
		collected = true
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	exchangeCancel()
	// Closing our ends must not depend on a successful OS stop attestation.
	// This unblocks bounded protocol I/O while the supervisor recovers a worker.
	_ = worker.Input().Close()
	_ = worker.Output().Close()
	// Stop has its own small deadline even if the phase was cancelled. A
	// supervisor failure is never converted to a stopped-worker success.
	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopCancel()
	stopped := make(chan error, 1)
	go func() { stopped <- worker.Stop(stopCtx) }()
	var stopErr error
	select {
	case stopErr = <-stopped:
	case <-stopCtx.Done():
		stopErr = stopCtx.Err()
	}
	if stopErr != nil {
		return nil, Identity{}, errors.Join(ErrWorkerStopPending, r.err)
	}
	if !collected {
		select {
		case <-done:
		case <-stopCtx.Done():
			return nil, Identity{}, errors.Join(ErrWorkerStopPending, r.err, stopCtx.Err())
		}
	}
	if r.err != nil {
		return nil, Identity{}, r.err
	}
	if ctx.Err() != nil {
		return nil, Identity{}, ctx.Err()
	}
	identity, err := d.Launcher.Inspect(ctx, inv)
	if err != nil {
		return nil, Identity{}, workerLauncherError(err, false)
	}
	return r.data, identity, err
}

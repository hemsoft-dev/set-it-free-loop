package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// NativeDriver speaks the official clients' stdin/stdout protocol directly.
// Its launcher must supply qualified identity, isolation, source IPC and full
// process-tree termination. No host executable or live launcher is selected here.
type NativeDriver struct {
	Launcher WorkerLauncher
	observe  func(nativeResultObservation)
}

// Private diagnostics carry only bounded classes and digests, never a native
// transcript or an arbitrary client/validator error message.
type nativeResultObservation struct {
	Bytes      int
	Hash, Kind string
	ExitFailed bool
	Trailer    *nativeCodexTrailer
}

type nativeCodexTrailer struct{ Type, Item, Server, Tool, Status, ErrorClass, IO string }

func nativeErrorClass(message string) (string, string) {
	message = strings.ToLower(message)
	class, kind := "other", "other"
	for _, entry := range []struct{ word, class string }{{"git", "git"}, {"rollout", "persistence"}, {"persist", "persistence"}, {"sqlite", "state"}, {"database", "state"}, {"shutdown", "shutdown"}, {"mcp", "mcp"}, {"permission", "filesystem"}, {"read-only", "filesystem"}, {"token", "auth"}, {"auth", "auth"}} {
		if strings.Contains(message, entry.word) {
			class = entry.class
			break
		}
	}
	for _, entry := range []struct{ word, kind string }{{"permission denied", "permission"}, {"operation not permitted", "denied"}, {"read-only file system", "read-only"}, {"no such file or directory", "missing"}, {"broken pipe", "pipe"}, {"connection reset", "connection"}, {"not supported", "unsupported"}} {
		if strings.Contains(message, entry.word) {
			kind = entry.kind
			break
		}
	}
	return class, kind
}

func nativeEventClass(value string) string {
	switch value {
	case "", "thread.started", "thread.closed", "thread.stopped", "turn.started", "turn.completed", "turn.failed", "error", "item.started", "item.updated", "item.completed", "reasoning", "agent_message", "mcp_tool_call", "command_execution", "file_change", "code_execution", "completed", "in_progress", "failed", "sfl-source", "read", "search":
		return value
	}
	return "other"
}

func nativeTrailer(raw []byte) *nativeCodexTrailer {
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 4096), len(raw)+1)
	completed := false
	for scan.Scan() {
		var event struct {
			Type    string                                      `json:"type"`
			Message string                                      `json:"message"`
			Item    struct{ Type, Server, Tool, Status string } `json:"item"`
		}
		if json.Unmarshal(scan.Bytes(), &event) != nil {
			return nil
		}
		if completed {
			class, kind := "", ""
			if event.Type == "error" {
				class, kind = nativeErrorClass(event.Message)
			}
			return &nativeCodexTrailer{nativeEventClass(event.Type), nativeEventClass(event.Item.Type), nativeEventClass(event.Item.Server), nativeEventClass(event.Item.Tool), nativeEventClass(event.Item.Status), class, kind}
		}
		completed = event.Type == "turn.completed" || event.Type == "turn.failed" || event.Type == "error"
	}
	return nil
}

func nativeResultKind(kind string) bool {
	switch kind {
	case "complete", "exit", "report-bound", "invalid-report", "unsupported-item", "unapproved-mcp", "unsupported-event", "after-completion", "incomplete", "missing-report", "provider_unavailable", "auth_failed", "quota_wait", "incomplete_expired", "invalid_output":
		return true
	}
	return false
}

func (o *nativeResultObservation) valid(s Session) bool {
	if o == nil || !nativeResultKind(o.Kind) || len(o.Hash) != 64 || !objectID.MatchString(o.Hash) || o.Bytes < 0 || o.Bytes > NativeTransportLimit(s)+1 {
		return false
	}
	if o.Trailer != nil {
		for _, value := range []string{o.Trailer.Type, o.Trailer.Item, o.Trailer.Server, o.Trailer.Tool, o.Trailer.Status} {
			if value != "other" && nativeEventClass(value) != value {
				return false
			}
		}
		switch o.Trailer.ErrorClass {
		case "", "other", "git", "persistence", "state", "shutdown", "mcp", "filesystem", "auth":
		default:
			return false
		}
		switch o.Trailer.IO {
		case "", "other", "permission", "denied", "read-only", "missing", "pipe", "connection", "unsupported":
		default:
			return false
		}
	}
	return true
}

// Match concrete execution implementations. A marker method can be promoted
// through embedding while an outer type overrides Execute and skips the guard.
func sourceInitializedNative(driver Driver) bool {
	switch value := driver.(type) {
	case NativeDriver:
		return true
	case *NativeDriver:
		return value != nil
	default:
		return false
	}
}

func (d NativeDriver) Inspect(ctx context.Context, inv Invocation) (Identity, error) {
	return (WorkerDriver{Launcher: d.Launcher}).Inspect(ctx, inv)
}

func nativeSession(inv Invocation) (Session, error) {
	var s Session
	if inv.Limits.Validate() != nil || inv.OutputLimit != inv.Limits.OutputBytes || len(inv.Input) > inv.Limits.InputBytes || !inv.Provider.Valid() {
		return s, errors.New("invalid native invocation limits")
	}
	// Adapter's fixed task prefix and captured Session share one stdin prompt.
	// The session is controller input; provider output cannot select its identity.
	_, raw, ok := bytes.Cut(inv.Input, []byte{'\n'})
	if !ok {
		return s, errors.New("missing native session")
	}
	// Sessions are generated by json.Marshal, including nullable unused peer
	// slices. Require that exact canonical encoding rather than Report's shape
	// decoder, which deliberately forbids null collections in model output.
	err := json.Unmarshal(raw, &s)
	canonical, encodeErr := json.Marshal(s)
	if err != nil || encodeErr != nil || !bytes.Equal(canonical, raw) || s.Validate() != nil || s.Identity.Provider != inv.Provider || s.Limits != inv.Limits {
		return Session{}, errors.New("native session differs from captured invocation")
	}
	return s, nil
}

func (d NativeDriver) Execute(ctx context.Context, inv Invocation) ([]byte, Identity, error) {
	if d.Launcher == nil {
		return nil, Identity{}, ErrLiveBlocked
	}
	s, err := nativeSession(inv)
	if err != nil {
		return nil, Identity{}, adapterValidation(err, true)
	}
	ctx, cancel := context.WithTimeout(ctx, inv.Limits.Timeout)
	defer cancel()
	return executeWorker(ctx, d.Launcher, inv, func(_ context.Context, worker Worker) ([]byte, error) {
		if inv.Provider == Claude && inv.SourceEndpoint != nil {
			return claudeSourceExchange(ctx, worker, inv, s)
		}
		// EOF completes the prompt. Raw client output never becomes a wrapper
		// tool request, and never selects source paths or controller operations.
		if n, err := worker.Input().Write(inv.Input); err != nil || n != len(inv.Input) {
			return nil, workerInvalid()
		}
		if worker.Input().Close() != nil {
			return nil, workerInvalid()
		}
		transportLimit := NativeTransportLimit(s)
		raw, err := io.ReadAll(io.LimitReader(worker.Output(), int64(transportLimit)+1))
		if err != nil || len(raw) > transportLimit {
			return nil, workerInvalid()
		}
		waitErr := worker.Wait()
		data, err, observed := normalizeNativeObservedResult(s, raw, waitErr)
		if d.observe != nil {
			d.observe(observed)
		}
		return data, err
	})
}

func normalizeNativeResult(s Session, raw []byte, waitErr error) ([]byte, error) {
	data, err, _ := normalizeNativeObservedResult(s, raw, waitErr)
	return data, err
}

func normalizeNativeObservedResult(s Session, raw []byte, waitErr error) ([]byte, error, nativeResultObservation) {
	sum := sha256.Sum256(raw)
	observed := nativeResultObservation{Bytes: len(raw), Hash: hex.EncodeToString(sum[:]), Kind: "invalid-report", ExitFailed: waitErr != nil}
	r, err := NormalizeCLIOutput(s, raw)
	if err != nil {
		switch err.Error() {
		case "forbidden or unknown client tool/item":
			observed.Kind = "unsupported-item"
		case "unapproved MCP tool in client output":
			observed.Kind = "unapproved-mcp"
		case "client error or unsupported event":
			observed.Kind = "unsupported-event"
		case "unexpected client data after completion":
			observed.Kind = "after-completion"
			observed.Trailer = nativeTrailer(raw)
		case "incomplete Codex transport":
			observed.Kind = "incomplete"
		case "missing Codex report":
			observed.Kind = "missing-report"
		}
		var failure PhaseError
		if errors.As(err, &failure) {
			// Native failure is after possible work and cannot refund a
			// reservation, even if a failed client exits with status zero.
			observed.Kind = phaseState(failure)
			return nil, PhaseError{State: failure.State}, observed
		}
		return nil, adapterValidation(errors.New("invalid or incomplete native client output"), false), observed
	}
	if waitErr != nil {
		observed.Kind = "exit"
		return nil, workerInvalid(), observed
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > s.Limits.OutputBytes {
		observed.Kind = "report-bound"
		return nil, workerInvalid(), observed
	}
	observed.Kind = "complete"
	return data, nil, observed
}

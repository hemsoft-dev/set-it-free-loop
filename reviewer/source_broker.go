package reviewer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// SourceTools is the complete source capability handed to a trusted worker
// transport. The transport must bind it to this invocation, never a global job
// selector. No shell, arbitrary object repository or filesystem API is exposed.
type SourceTools interface {
	Read(context.Context, string, string) (string, error)
	Search(context.Context, string, string, int) ([]Anchor, error)
}

// SourceAccess records broker activity, not a model's claimed tool history.
// It does not prove that an official client has no other source/host capability.
// Nil means no broker receipt was supplied, never a measured zero-read session.
type SourceAccess struct {
	Version     int          `json:"version"`
	SessionHash string       `json:"session_hash"`
	ToolCalls   int          `json:"tool_calls"`
	ReaderCalls int          `json:"reader_calls"`
	Bytes       int          `json:"bytes"`
	Exhausted   bool         `json:"exhausted"`
	Reads       []ReadRecord `json:"reads"`
	Coverage    []Coverage   `json:"coverage"`
}

func cloneSourceAccess(a *SourceAccess) *SourceAccess {
	if a == nil {
		return nil
	}
	copy := *a
	copy.Reads = append([]ReadRecord{}, a.Reads...)
	copy.Coverage = append([]Coverage{}, a.Coverage...)
	return &copy
}

func (a *SourceAccess) validate(s Session) error {
	return a.validateCaptured(s.Snapshot, s.Limits, Digest(s))
}

func (a *SourceAccess) validateCaptured(snapshot Snapshot, limits Limits, inputHash string) error {
	if a == nil {
		return nil
	}
	if a.Version != 1 || a.SessionHash != inputHash || a.ToolCalls < 0 || a.ToolCalls > limits.ReaderCalls || a.ReaderCalls < 0 || a.ReaderCalls > limits.ReaderCalls || a.Bytes < 0 || a.Bytes > limits.ReaderBytes || len(a.Reads) > a.ReaderCalls || len(a.Coverage) > 256 {
		return errors.New("source receipt differs from captured session or limits")
	}
	total := 0
	for _, r := range a.Reads {
		if !validPath(r.Path) || (r.Revision != snapshot.Head && r.Revision != snapshot.Base && r.Revision != snapshot.MergeBase) || len(r.Hash) != 64 || !objectID.MatchString(r.Hash) || r.Bytes < 0 || r.Bytes > a.Bytes-total {
			return errors.New("invalid source read receipt")
		}
		total += r.Bytes
	}
	if (a.ToolCalls == 0 && a.ReaderCalls != 0) || (a.ReaderCalls == 0 && a.Bytes != 0) || (a.Exhausted && a.ToolCalls != limits.ReaderCalls) {
		return errors.New("inconsistent source receipt counters")
	}
	for _, c := range a.Coverage {
		if (c.Path != "" && !validPath(c.Path)) || len(c.Reason) == 0 || len(c.Reason) > 1024 {
			return errors.New("invalid source omission receipt")
		}
	}
	return nil
}

// SourceReviewer returns controller-owned access evidence alongside the model
// report. Report JSON has no field that can populate this evidence channel.
type SourceReviewer interface {
	ReviewSource(context.Context, Session) (Report, *SourceAccess, error)
}

func reviewSource(ctx context.Context, reviewer Reviewer, s Session) (Report, *SourceAccess, error) {
	if worker, ok := reviewer.(SourceReviewer); ok {
		r, access, err := worker.ReviewSource(ctx, s)
		return sourceReviewResult(r, access, err, s)
	}
	r, err := reviewer.Review(ctx, s)
	return r, nil, err
}

func sourceReviewResult(r Report, access *SourceAccess, err error, s Session) (Report, *SourceAccess, error) {
	if auditErr := access.validate(s); auditErr != nil {
		return Report{}, nil, errors.Join(err, adapterValidation(auditErr, false))
	}
	if err == nil && access != nil {
		var mergeErr error
		r, mergeErr = mergeCoverageRecords(r, access.Coverage)
		if mergeErr != nil {
			return Report{}, cloneSourceAccess(access), adapterValidation(mergeErr, false)
		}
	}
	return r, cloneSourceAccess(access), err
}

type sourceBroker struct {
	mu        sync.Mutex
	ctx       context.Context
	reader    *Reader
	session   Session
	calls     int
	sealed    bool
	exhausted bool
	// transportDenied is set by the MCP transport for a call it denied past
	// the bound. It never takes mu, which an active Read or Search holds
	// across store I/O; the next holder of mu folds it into the audit.
	transportDenied atomic.Bool
	// transportRecorded means that omission is already listed once.
	transportRecorded bool
	denials           []Coverage
}

// newSourceBroker forks a fresh read budget. Citation validation on the supplied
// Reader must not consume or appear in the worker's independent source audit.
func newSourceBroker(ctx context.Context, s Session, verifier *Reader) (*sourceBroker, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if verifier == nil || verifier.snapshot != s.Snapshot {
		return nil, errors.New("source broker requires the captured snapshot")
	}
	paths := make([]string, 0, len(verifier.paths))
	for p := range verifier.paths {
		paths = append(paths, p)
	}
	r, err := NewReader(verifier.store, s.Snapshot, paths, s.Limits)
	if err != nil {
		return nil, err
	}
	r.initialCoverage = append([]Coverage(nil), verifier.initialCoverage...)
	r.coverage = append([]Coverage(nil), verifier.initialCoverage...)
	return &sourceBroker{ctx: ctx, reader: r, session: s}, nil
}

func (b *sourceBroker) begin(ctx context.Context) error {
	if b.sealed {
		return errors.New("source session is sealed")
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.foldTransportDenial()
	if b.calls >= b.session.Limits.ReaderCalls {
		if !b.exhausted && !b.transportRecorded {
			b.denials = append(b.denials, Coverage{Reason: "source tool-call limit exhausted"})
		}
		b.exhausted = true
		return errors.New("source tool-call limit exhausted")
	}
	b.calls++
	return nil
}

// recordToolCallExhaustion is called by the MCP transport for a call it denied
// past the tool-call bound. It only marks the denial, so the transport's intake
// loop never waits on source I/O.
func (b *sourceBroker) recordToolCallExhaustion() { b.transportDenied.Store(true) }

// foldTransportDenial records a transport denial once. Exhausted is claimed
// only when the broker itself served the full bound; calls that the transport
// rejected as invalid never count toward it. Called with mu held.
func (b *sourceBroker) foldTransportDenial() {
	if b.sealed || !b.transportDenied.Swap(false) {
		return
	}
	if !b.exhausted && !b.transportRecorded {
		b.denials = append(b.denials, Coverage{Reason: "source tool-call limit exhausted"})
		b.transportRecorded = true
	}
	b.exhausted = b.exhausted || b.calls >= b.session.Limits.ReaderCalls
}

func (b *sourceBroker) Read(ctx context.Context, revision, path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.begin(ctx); err != nil {
		return "", err
	}
	call, done := b.callContext(ctx)
	defer done()
	return b.reader.Read(call, revision, path)
}

func (b *sourceBroker) Search(ctx context.Context, revision, needle string, maxResults int) ([]Anchor, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	if needle == "" || len(needle) > 256 || strings.ContainsAny(needle, "\r\n") || maxResults < 1 || maxResults > 100 {
		if len(b.denials) < 128 {
			b.denials = append(b.denials, Coverage{Reason: "invalid bounded source search"})
		}
		return nil, errors.New("invalid bounded source search")
	}
	call, done := b.callContext(ctx)
	defer done()
	return b.reader.Search(call, revision, needle, maxResults)
}

func (b *sourceBroker) callContext(worker context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(b.ctx)
	stop := context.AfterFunc(worker, cancel)
	return ctx, func() { stop(); cancel() }
}

func (b *sourceBroker) seal() *SourceAccess {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.foldTransportDenial()
	b.sealed = true
	reads, coverage := b.reader.Evidence()
	b.reader.mu.Lock()
	calls, used := b.reader.calls, b.reader.used
	b.reader.mu.Unlock()
	coverage = compactSourceCoverage(append(coverage, b.denials...))
	return &SourceAccess{Version: 1, SessionHash: Digest(b.session), ToolCalls: b.calls, ReaderCalls: calls, Bytes: used, Exhausted: b.exhausted, Reads: append([]ReadRecord{}, reads...), Coverage: append([]Coverage{}, coverage...)}
}

// Group controller-owned reasons by allowed path, including global denials.
// Inventory omissions and repeated reads cannot consume separate slots for
// the same path and crowd out the one-time exhaustion fact. Unknown/unsafe
// attempted paths are sanitized to the global path by Reader.Read.
func compactSourceCoverage(records []Coverage) []Coverage {
	if len(records) <= 256 {
		return records
	}
	out := []Coverage{}
	indices := map[string]int{}
	seen := map[Coverage]bool{}
	for _, record := range records {
		if seen[record] {
			continue
		}
		seen[record] = true
		if index, ok := indices[record.Path]; ok {
			out[index].Reason += "; " + record.Reason
		} else {
			indices[record.Path] = len(out)
			out = append(out, record)
		}
	}
	return out
}

func (b *sourceBroker) revoke() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sealed = true
}

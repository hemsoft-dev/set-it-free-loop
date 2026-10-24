package reviewer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Remote is a trusted GitHub transport. The explicit HTTP publisher adapter
// supplies writes/reconciliation; native history and credential acquisition
// require separately qualified composition.
// Scan must exhaust pagination; hashes cover the entire normalized document,
// including inline comments. Caller-provided markers alone are never receipts.
// History is complete post-POST native event history, not merely pending users.
// Annotate must idempotently update the known review, never append new comments.
type Remote interface {
	Current(context.Context, Scope) (RemoteState, error)
	History(context.Context, Scope) (Discovery, error)
	Post(context.Context, WriteIntent) (RemoteReceipt, error)
	Scan(context.Context, Scope, string) ([]RemoteReceipt, bool, error)
	Annotate(context.Context, Scope, int64, string) error
}

// intentScanner avoids expanding unrelated App reviews during reconciliation.
// Completeness still requires scanning every collection page and rejecting
// duplicate IDs and multiple matching intent markers.
type intentScanner interface {
	ScanIntent(context.Context, WriteIntent) ([]RemoteReceipt, bool, error)
}

// publicationRejected is emitted only by a trusted transport after a definitive
// write rejection. Missing/malformed success receipts never qualify.
type publicationRejected struct{ cause error }

func (e *publicationRejected) Error() string { return e.cause.Error() }
func (e *publicationRejected) Unwrap() error { return e.cause }

type RemoteState struct {
	Access, Open bool
	Snapshot     Snapshot
	Request      NativeRequest
}
type Publisher struct {
	Store        *Store
	Remote       Remote
	KnownSecrets []string
	Now          func() time.Time
}

func (p Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Canonical comment order makes the durable document independent of GitHub's
// pagination/creation order while retaining every body, anchor and duplicate.
func canonicalPublicationDocument(doc Document) Document {
	doc.Inline = append([]InlineComment{}, doc.Inline...)
	sort.Slice(doc.Inline, func(i, j int) bool {
		a, b := doc.Inline[i], doc.Inline[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Body < b.Body
	})
	return doc
}

// neutralizedSecretPattern matches value with the reference-neutralization
// artifacts of safeText tolerated: word joiners between characters and a
// fullwidth hyphen-minus for '-'. Neighbouring text decides where safeText puts
// them, so a secret inside model text can render unlike safeText(secret).
func neutralizedSecretPattern(value string) *regexp.Regexp {
	runes := []rune(strings.ReplaceAll(value, commitRunJoiner, ""))
	if len(runes) == 0 {
		return nil
	}
	var b strings.Builder
	for i, r := range runes {
		if i > 0 {
			b.WriteString(regexp.QuoteMeta(commitRunJoiner) + "*")
		}
		if r == '-' || r == '－' {
			b.WriteString("[-－]")
		} else {
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return regexp.MustCompile(b.String())
}
func redactDocument(doc Document, secrets []string) Document {
	doc.Inline = append([]InlineComment{}, doc.Inline...)
	values := append([]string{}, secrets...)
	for _, secret := range secrets {
		if secret != "" {
			segments := strings.Split(secret, "/")
			for i := range segments {
				segments[i] = url.PathEscape(segments[i])
			}
			values = append(values, safeText(secret), url.PathEscape(secret), url.QueryEscape(secret), strings.Join(segments, "/"))
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	patterns := make([]*regexp.Regexp, len(values))
	for i, secret := range values {
		patterns[i] = neutralizedSecretPattern(secret)
	}
	redact := func(s string) string {
		for i, secret := range values {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[redacted]")
			}
			if patterns[i] != nil {
				s = patterns[i].ReplaceAllLiteralString(s, "[redacted]")
			}
		}
		return s
	}
	doc.Summary = redact(doc.Summary)
	for i := range doc.Inline {
		doc.Inline[i].Body = redact(doc.Inline[i].Body)
	}
	return doc
}
func (p Publisher) Prepare(ctx context.Context, out Outcome, diff map[string]map[int]bool) (WriteIntent, error) {
	if snapshotEvidencePacket(out.Job.Packet) {
		return WriteIntent{}, ErrFenced
	}
	return p.prepare(ctx, out, diff)
}
func (p Publisher) prepare(ctx context.Context, out Outcome, diff map[string]map[int]bool) (WriteIntent, error) {
	doc, err := renderOutcome(out, diff)
	if err != nil {
		return WriteIntent{}, err
	}
	doc = redactDocument(doc, p.KnownSecrets)
	var intent WriteIntent
	err = p.Store.update(ctx, func(d *DurableState) error {
		j := d.Jobs[out.Job.ID]
		if j == nil || Digest(j.Job) != Digest(out.Job) || j.LeaseToken != "" || j.Outstanding {
			return ErrDeferred
		}
		pr := d.PRs[j.Scope.key()]
		if pr.Paused || pr.CurrentRequest != j.RequestID || pr.Generation != out.Job.Generation {
			return ErrFenced
		}
		if out.State != "dual_complete" && out.State != "single_complete" {
			return errors.New("substantive publisher requires complete outcome")
		}
		if j.State != out.State || Digest(j.Checkpoint) != Digest(out.Checkpoint()) {
			return errors.New("outcome is not the persisted completion")
		}
		reports := make([]Report, 0, len(j.Checkpoint.Artifacts))
		for _, artifact := range j.Checkpoint.Artifacts {
			if err := validateArtifactProviderEvidence(j, artifact); err != nil {
				return err
			}
			reports = append(reports, artifact.Report)
		}
		if Digest(buildLedger(reports, j.Job.Config.Mode)) != Digest(out.Ledger) {
			return errors.New("publication ledger differs from validated durable reports")
		}
		for _, w := range d.Writes {
			if w.Kind == "review" && w.JobHash == Digest(out.Job) {
				intent = *w
				return nil
			}
		}
		intent = newIntent("review", j.Scope, out.Job.Generation, j.RequestID, out.Job.Snapshot, Digest(out.Job), doc, p.now())
		intent.CheckpointHash = Digest(j.Checkpoint)
		d.Writes[intent.ID] = &intent
		return nil
	})
	return intent, err
}
func validateReceipt(w WriteIntent, receipt RemoteReceipt, bot int64) bool {
	return receipt.ID > 0 && receipt.AuthorID == bot && receipt.Marker == w.Marker && receipt.BodyHash == w.BodyHash && !receipt.CreatedAt.IsZero() && (w.Kind != "review" || receipt.CommitID == w.Snapshot.Head)
}
func (p Publisher) confirm(ctx context.Context, id string, receipt RemoteReceipt) error {
	return p.Store.update(ctx, func(d *DurableState) error {
		w := d.Writes[id]
		if w == nil || !validateReceipt(*w, receipt, d.Enrollment.BotID) {
			return errors.New("untrusted remote receipt")
		}
		if w.State == "confirmed" || w.State == "postcheck_pending" || w.State == "annotation_pending" {
			if w.ReceiptID != receipt.ID {
				return ErrDeferred
			}
			return nil
		}
		if w.State != "dispatched" && w.State != "unknown" {
			return ErrDeferred
		}
		pr := d.PRs[w.Scope.key()]
		if w.Kind == "ack" {
			for _, command := range pr.Commands {
				if command.WriteID == id {
					if receipt.CreatedAt.Before(command.CreatedAt) {
						return ErrDeferred
					}
					if command.Command.Action == "configure" || command.Command.Action == "reset" {
						last := pr.Configs[len(pr.Configs)-1]
						if command.Version != last.Version+1 || receipt.CreatedAt.Before(last.EffectiveAt) {
							return ErrDeferred
						}
						pr.Configs = append(pr.Configs, ConfigRecord{command.Version, command.Config, receipt.CreatedAt})
					}
				}
			}
		}
		w.State = "confirmed"
		if w.Kind == "review" {
			w.State = "postcheck_pending"
		}
		w.ReceiptID = receipt.ID
		w.ReceiptTime = receipt.CreatedAt
		return nil
	})
}
func (p Publisher) Reconcile(ctx context.Context, id string) error {
	d, err := p.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	w := d.Writes[id]
	if w == nil || !d.Enrollment.accepts(w.Scope) {
		return ErrIgnored
	}
	if w.State == "postcheck_pending" || w.State == "annotation_pending" {
		return p.postcheck(ctx, *w, w.ReceiptID)
	}
	if w.State != "unknown" && w.State != "dispatched" {
		return ErrDeferred
	}
	var receipts []RemoteReceipt
	var complete bool
	if scanner, ok := p.Remote.(intentScanner); ok {
		receipts, complete, err = scanner.ScanIntent(ctx, *w)
	} else {
		receipts, complete, err = p.Remote.Scan(ctx, w.Scope, w.Kind)
	}
	if err != nil {
		return err
	}
	if !complete {
		return ErrDeferred
	}
	var matches []RemoteReceipt
	for _, r := range receipts {
		if validateReceipt(*w, r, d.Enrollment.BotID) {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		return ErrDeferred
	} // even a complete empty scan may precede delayed visibility
	if err = p.confirm(ctx, id, matches[0]); err != nil {
		return err
	}
	if w.Kind == "review" {
		return p.postcheck(ctx, *w, matches[0].ID)
	}
	return nil
}
func matchingRemote(w WriteIntent, r RemoteState) bool {
	return r.Access && r.Open && r.Snapshot == w.Snapshot && r.Request.ID == w.RequestID && !r.Request.Removed && r.Request.RequesterID == OwnerID
}
func (p Publisher) Dispatch(ctx context.Context, id string) error {
	d, err := p.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	w := d.Writes[id]
	if w == nil || !d.Enrollment.accepts(w.Scope) {
		return ErrIgnored
	}
	if w.State != "prepared" {
		return ErrDeferred
	}
	current, err := p.Remote.Current(ctx, w.Scope)
	if err != nil {
		return err
	}
	if !current.Access {
		return ErrDeferred
	}
	if !current.Open && w.Kind == "review" {
		return p.markStale(ctx, *w, current, "PR closed before dispatch")
	}
	if !current.Open {
		return ErrDeferred
	}
	if w.Kind == "review" && (!matchingRemote(*w, current) || current.Request.BotID != d.Enrollment.BotID) {
		return p.markStale(ctx, *w, current, "stale before dispatch")
	}
	locallyFenced := false
	err = p.Store.update(ctx, func(d *DurableState) error {
		target := d.Writes[id]
		if target == nil || target.State != "prepared" {
			return ErrDeferred
		}
		pr := d.PRs[target.Scope.key()]
		if target.Kind == "review" && (pr.Paused || pr.Generation != target.Generation || pr.CurrentRequest != target.RequestID) {
			target.State = "stale"
			for _, job := range d.Jobs {
				if Digest(job.Job) == target.JobHash && job.State != "cancelled" {
					job.State = "stale"
				}
			}
			locallyFenced = true
			retireFencedJobs(d, pr)
			return nil
		}
		if target.Kind == "ack" {
			for _, command := range pr.Commands {
				if command.WriteID == target.ID {
					break
				}
				if d.Writes[command.WriteID].State != "confirmed" {
					return ErrDeferred
				}
			}
		}
		if target.Kind == "review" {
			var completed *DurableJob
			for _, job := range d.Jobs {
				if Digest(job.Job) == target.JobHash {
					completed = job
					break
				}
			}
			if completed == nil || completed.Outstanding || completed.LeaseToken != "" ||
				(completed.State != "dual_complete" && completed.State != "single_complete") ||
				len(completed.Checkpoint.Artifacts) != completed.Job.Config.MaxSessions() ||
				completed.Checkpoint.ConsumedSessions != completed.Job.Config.MaxSessions() ||
				target.CheckpointHash != Digest(completed.Checkpoint) {
				return ErrDeferred
			}
			for _, artifact := range completed.Checkpoint.Artifacts {
				if err := validateArtifactProviderEvidence(completed, artifact); err != nil {
					return err
				}
			}
		}
		target.State = "dispatched"
		return nil
	})
	if err != nil {
		return err
	}
	if locallyFenced {
		return ErrFenced
	}
	receipt, postErr := p.Remote.Post(ctx, *w)
	if postErr != nil || !validateReceipt(*w, receipt, d.Enrollment.BotID) {
		var rejected *publicationRejected
		definitive := errors.As(postErr, &rejected)
		saveErr := p.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error {
			if target := d.Writes[id]; target != nil && target.State == "dispatched" {
				target.State = "unknown"
				if definitive {
					target.State = "prepared"
				}
			}
			return nil
		})
		return errors.Join(ErrDeferred, postErr, saveErr)
	}
	if err = p.confirm(context.WithoutCancel(ctx), id, receipt); err != nil {
		return err
	}
	if w.Kind == "review" {
		return p.postcheck(ctx, *w, receipt.ID)
	}
	return nil
}
func (p Publisher) markStale(ctx context.Context, w WriteIntent, current RemoteState, reason string) error {
	err := p.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error {
		pr := d.PRs[w.Scope.key()]
		pr.Paused = true
		pr.PauseRequestID = pr.CurrentRequest
		var latest NativeRequest
		for _, request := range pr.Requests {
			if request.CreatedAt.After(latest.CreatedAt) {
				latest = request
			}
		}
		if latest.ID != 0 {
			pr.PauseRequestID = latest.ID
		}
		// Preserve B locally even when publishing A cleared its native pending state.
		// A can never fulfill B. Complete trusted discovery must later reconcile it.
		if current.Request.ID != 0 && current.Request.ID != w.RequestID && current.Request.RequesterID == OwnerID && current.Request.BotID == d.Enrollment.BotID {
			known := false
			for _, r := range pr.Requests {
				if r.ID == current.Request.ID {
					known = true
				}
			}
			if !known {
				pr.Requests = append(pr.Requests, current.Request)
			}
			pr.CurrentRequest = current.Request.ID
			pr.Generation++
		}
		if pr.Triggers != nil && current.Access && current.Open && current.Request.ID == pr.CurrentRequest && current.Request.Snapshot == current.Snapshot && pr.CurrentRequest != 0 && pr.CurrentRequest != w.RequestID {
			// This ledger is independent of native pending state: stale publication
			// A fences A, while known B may resume once A's write/annotation is
			// reconciled. Do not require an unrelated third request to release B.
			pr.PauseRequestID = w.RequestID
		}
		target := d.Writes[w.ID]
		if target.State == "prepared" {
			target.State = "stale"
		} else if target.State == "confirmed" || target.State == "postcheck_pending" || target.State == "annotation_pending" {
			target.State = "superseded"
			if current.Access {
				target.State = "annotation_pending"
			}
		}
		for _, j := range d.Jobs {
			if Digest(j.Job) == w.JobHash {
				j.State = "stale"
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrDeferred, reason)
}
func (p Publisher) postcheck(ctx context.Context, w WriteIntent, receiptID int64) error {
	saved, err := p.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	if target := saved.Writes[w.ID]; target != nil {
		w = *target
	} else {
		return ErrDeferred
	}
	current, err := p.Remote.Current(ctx, w.Scope)
	if err != nil {
		return err
	}
	if current.Access {
		history, historyErr := p.Remote.History(ctx, w.Scope)
		if historyErr != nil {
			return historyErr
		}
		if history.Scope != w.Scope || !history.Complete || history.Through.Before(w.ReceiptTime) {
			return ErrDeferred
		}
		if err = p.Store.Discover(ctx, history); err != nil {
			return err
		}
		current, err = p.Remote.Current(ctx, w.Scope)
		if err != nil {
			return err
		}
	}
	d, err := p.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	pr := d.PRs[w.Scope.key()]
	// Absence is accepted only after complete post-POST request history. B may
	// have appeared and been cleared without ever being in the local poll cache.
	sameRequest := current.Request.ID == 0 || (current.Request.ID == w.RequestID && current.Request.RequesterID == OwnerID && current.Request.BotID == d.Enrollment.BotID)
	valid := current.Access && current.Open && current.Snapshot == w.Snapshot && sameRequest && !pr.Paused && pr.Generation == w.Generation && pr.CurrentRequest == w.RequestID
	if valid {
		return p.Store.update(ctx, func(d *DurableState) error {
			target := d.Writes[w.ID]
			if target.State != "postcheck_pending" {
				return ErrDeferred
			}
			pr := d.PRs[w.Scope.key()]
			if pr.Paused || pr.Generation != w.Generation || pr.CurrentRequest != w.RequestID {
				return ErrFenced
			}
			target.State = "confirmed"
			return nil
		})
	}
	staleErr := p.markStale(ctx, w, current, "snapshot, access or request changed during publication")
	if current.Access {
		if err = p.Remote.Annotate(ctx, w.Scope, receiptID, "Superseded SFL snapshot review. This review does not fulfill a later request. Reconciliation is required."); err != nil {
			return errors.Join(staleErr, err)
		}
		if err = p.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error { d.Writes[w.ID].State = "superseded"; return nil }); err != nil {
			return errors.Join(staleErr, err)
		}
	}
	return staleErr
}

// Prune only terminal jobs and their confirmed/superseded review receipts after
// configured retention. Accepted commands/config versions stay immutable. Unknown
// writes and unresolved requests are never discarded to make a queue look clean.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	return s.update(ctx, func(d *DurableState) error {
		for id, j := range d.Jobs {
			if j.LeaseToken != "" || now.Sub(j.CreatedAt) < time.Duration(j.Job.Config.RetentionDays)*24*time.Hour {
				continue
			}
			if j.State != "dual_complete" && j.State != "single_complete" && j.State != "cancelled" && j.State != "stale" && j.State != "incomplete_expired" && j.State != "invalid_output" {
				continue
			}
			safe := true
			published := false
			for _, w := range d.Writes {
				if w.Kind == "review" && w.JobHash == Digest(j.Job) && (w.State == "confirmed" || w.State == "superseded") {
					published = true
				}
				if w.JobHash == Digest(j.Job) && w.State != "confirmed" && w.State != "superseded" && w.State != "stale" {
					safe = false
				}
			}
			if !safe || ((j.State == "dual_complete" || j.State == "single_complete") && !published) {
				continue
			}
			for key, w := range d.Writes {
				if w.JobHash == Digest(j.Job) {
					delete(d.Writes, key)
				}
			}
			delete(d.Jobs, id)
		}
		return nil
	})
}

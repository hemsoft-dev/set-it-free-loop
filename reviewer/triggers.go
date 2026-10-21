package reviewer

import (
	"fmt"
	"time"
)

// TriggerPolicy is trusted service configuration, never a PR-controlled value.
// EnabledAt is a durable epoch, not activation authority. Changing it against an
// existing cursor requires a separate explicit migration.
type TriggerPolicy struct{ EnabledAt time.Time }

func (p TriggerPolicy) validate() error {
	if p.EnabledAt.IsZero() {
		return ErrIgnored
	}
	return nil
}

// RequestOrigin keeps the actual event author separate from the policy owner.
// For automatic observations ActorID is the PR author, not an inferred pusher.
// RequesterID on the enclosing request/job remains the authorizing policy owner.
type RequestOrigin struct {
	PolicyEnabledAt time.Time `json:"policy_enabled_at"`
	Kind            string    `json:"kind"`
	PolicyOwnerID   int64     `json:"policy_owner_id"`
	PullID          int64     `json:"pull_id"`
	ActorID         int64     `json:"actor_id"`
	ActorRole       string    `json:"actor_role"`
	CommentID       int64     `json:"comment_id,omitempty"`
	BodyHash        string    `json:"body_hash,omitempty"`
	EventAt         time.Time `json:"event_at"`
	ObservedAt      time.Time `json:"observed_at"`
	Sequence        int64     `json:"observation_sequence,omitempty"`
}

func (o RequestOrigin) validate() error {
	if o.PolicyOwnerID != OwnerID || o.PullID <= 0 || o.ActorID <= 0 || o.ObservedAt.IsZero() || o.PolicyEnabledAt.IsZero() || o.ObservedAt.Before(o.PolicyEnabledAt) || o.ObservedAt.Before(o.EventAt) {
		return ErrIgnored
	}
	switch o.Kind {
	case "comment":
		if o.EventAt.IsZero() || o.EventAt.Before(o.PolicyEnabledAt) || o.ActorRole != "commenter" || o.ActorID != OwnerID || o.CommentID <= 0 || o.BodyHash != Digest("@sfl review") || o.Sequence != 0 {
			return ErrIgnored
		}
	case "opened", "synchronize":
		if (o.Kind == "opened" && (o.EventAt.IsZero() || o.EventAt.Before(o.PolicyEnabledAt))) || (o.Kind == "synchronize" && !o.EventAt.IsZero()) || o.ActorRole != "pr_author" || o.CommentID != 0 || o.BodyHash != "" || o.Sequence <= 0 {
			return ErrIgnored
		}
	default:
		return ErrIgnored
	}
	return nil
}

// TriggerCursor is the bounded, persistent observation ledger for one PR. It is
// not a webhook receipt and supplies no missing push actor or push timestamp.
type TriggerCursor struct {
	Policy           TriggerPolicy
	PullID, AuthorID int64
	CreatedAt        time.Time
	Snapshot         Snapshot
	ObservedAt       time.Time
	Sequence         int64
}

type TriggerBatch struct {
	PreviousHash string
	Cursor       TriggerCursor
}

func triggerStateHash(pr *PRRecord) string { return Digest(pr) }
func exactReviewComment(c CommandEvent) bool {
	return c.AuthorID == OwnerID && c.ID > 0 && c.Body == "@sfl review" && !c.CreatedAt.IsZero() && c.CreatedAt.Equal(c.UpdatedAt)
}

// applyTriggers is a transaction participant: a stale read cannot allocate a
// second numeric request or replace a cursor. Numeric IDs are local request
// identities; the real comment ID and PR observation sequence live in Origin.
func applyTriggers(d *DurableState, batch Discovery) error {
	delta := batch.Triggers
	old := d.PRs[batch.Scope.key()]
	if delta == nil || delta.PreviousHash != triggerStateHash(old) {
		return ErrDeferred
	}
	pr, err := prRecord(d, batch.Scope)
	if err != nil {
		return err
	}
	c := delta.Cursor
	if c.Policy.validate() != nil || c.PullID <= 0 || c.AuthorID <= 0 || c.CreatedAt.IsZero() || c.CreatedAt.After(c.ObservedAt) || c.ObservedAt.After(batch.Through) || c.Snapshot.Validate() != nil || c.Snapshot.PR != batch.Scope.PR || c.Snapshot.RepositoryID != batch.Scope.RepositoryID || c.Sequence < 0 || c.Sequence > 64 || c.ObservedAt.Before(c.Policy.EnabledAt) || len(pr.NativeEvents) != 0 || batch.NativeEvents != nil {
		return ErrDeferred
	}
	for _, r := range pr.Requests {
		if r.Origin == nil {
			return ErrLiveBlocked
		}
	}
	if prev := pr.Triggers; prev != nil {
		if !prev.Policy.EnabledAt.Equal(c.Policy.EnabledAt) || prev.PullID != c.PullID || prev.AuthorID != c.AuthorID || !prev.CreatedAt.Equal(c.CreatedAt) || c.ObservedAt.Before(prev.ObservedAt) || c.Sequence < prev.Sequence || c.Sequence > prev.Sequence+1 {
			return ErrDeferred
		}
	}
	ordered := []CommandEvent{}
	for _, command := range batch.Commands {
		alreadyReview := false
		for _, request := range pr.Requests {
			if request.Origin != nil && request.Origin.Kind == "comment" && request.Origin.CommentID == command.ID {
				alreadyReview = true
			}
		}
		if !alreadyReview {
			ordered = append(ordered, command)
		}
	}
	sortCommands(ordered)
	if err = applyCommands(d, batch.Scope, ordered, batch.Through); err != nil {
		return err
	}
	nextID := int64(1)
	for _, r := range pr.Requests {
		if r.ID >= nextID {
			nextID = r.ID + 1
		}
	}
	for _, r := range batch.Requests {
		if r.ID != nextID || r.BotID != d.Enrollment.BotID || r.RequesterID != OwnerID || r.Origin == nil || r.Origin.validate() != nil || r.Origin.PullID != c.PullID || !r.Origin.PolicyEnabledAt.Equal(c.Policy.EnabledAt) || !r.Origin.ObservedAt.Equal(c.ObservedAt) || r.Snapshot != c.Snapshot || (r.Origin.Kind != "comment" && r.Origin.ActorID != c.AuthorID) || r.CreatedAt.After(batch.Through) || r.Removed || r.Admitted {
			return ErrDeferred
		}
		if r.Origin.Kind == "comment" {
			if r.CreatedAt.Before(c.Policy.EnabledAt) || !r.CreatedAt.Equal(r.Origin.EventAt) {
				return ErrDeferred
			}
			for _, old := range pr.Requests {
				if old.Origin != nil && old.Origin.CommentID == r.Origin.CommentID {
					return ErrDeferred
				}
			}
		} else if !r.CreatedAt.Equal(r.Origin.ObservedAt) || r.Origin.Sequence != c.Sequence || r.Snapshot != c.Snapshot {
			return ErrDeferred
		}
		r.BotID = d.Enrollment.BotID
		if err = applyRequest(d, batch.Scope, r); err != nil {
			return err
		}
		nextID++
	}
	pr.Triggers = &c
	// A later request can release a known snapshot/cancellation fence. Unknown
	// publication still needs reconciliation; it can never be cleared by a push.
	if pr.Paused && pr.CurrentRequest != 0 && pr.CurrentRequest != pr.PauseRequestID {
		safe := true
		var latest NativeRequest
		for _, r := range pr.Requests {
			if r.ID == pr.CurrentRequest {
				latest = r
			}
		}
		if latest.ID == 0 || latest.Removed {
			safe = false
		}
		for _, r := range pr.Requests {
			if r.ID != latest.ID && !r.CreatedAt.Before(latest.CreatedAt) {
				safe = false
			}
		}
		for _, w := range d.Writes {
			if w.Scope == batch.Scope && (w.State == "unknown" || w.State == "dispatched" || w.State == "postcheck_pending" || w.State == "annotation_pending") {
				safe = false
			}
		}
		if safe {
			pr.Paused = false
			pr.PauseRequestID = 0
		}
	}
	return nil
}

// buildTriggerBatch is a pure transformation of a complete, stable remote read.
// It never starts inference or writes to GitHub. New comments discovered after a
// previously complete cutoff defer rather than silently invent old provenance.
func buildTriggerBatch(policy TriggerPolicy, scope Scope, saved *PRRecord, pullID, authorID int64, created, through time.Time, snapshot Snapshot, commands, comments []CommandEvent) (Discovery, error) {
	if policy.validate() != nil || pullID <= 0 || authorID <= 0 || created.IsZero() || created.After(through) || through.Before(policy.EnabledAt) || snapshot.Validate() != nil {
		return Discovery{}, ErrDeferred
	}
	cursor := TriggerCursor{Policy: policy, PullID: pullID, AuthorID: authorID, CreatedAt: created, Snapshot: snapshot, ObservedAt: through}
	known := map[int64]bool{}
	nextID := int64(1)
	if saved != nil {
		if len(saved.NativeEvents) != 0 {
			return Discovery{}, ErrLiveBlocked
		}
		for _, cmd := range saved.Commands {
			known[cmd.Command.EventID] = true
		}
		for _, r := range saved.Requests {
			if r.Origin == nil {
				return Discovery{}, ErrLiveBlocked
			}
			if r.ID >= nextID {
				nextID = r.ID + 1
			}
			if r.Origin.Kind == "comment" {
				known[r.Origin.CommentID] = true
			}
		}
		if saved.Triggers != nil {
			prev := saved.Triggers
			if !prev.Policy.EnabledAt.Equal(policy.EnabledAt) || prev.PullID != pullID || prev.AuthorID != authorID || !prev.CreatedAt.Equal(created) || through.Before(prev.ObservedAt) {
				return Discovery{}, ErrDeferred
			}
			cursor.Sequence = prev.Sequence
		}
	}
	batch := Discovery{Scope: scope, Commands: commands, Through: through, Complete: true, Triggers: &TriggerBatch{PreviousHash: triggerStateHash(saved), Cursor: cursor}}
	sortCommands(comments)
	for _, comment := range comments {
		if known[comment.ID] || !exactReviewComment(comment) || comment.CreatedAt.Before(policy.EnabledAt) {
			continue
		}
		if comment.CreatedAt.After(through) || (saved != nil && !saved.HistoryThrough.IsZero() && !comment.CreatedAt.After(saved.HistoryThrough)) {
			return Discovery{}, ErrDeferred
		}
		origin := RequestOrigin{PolicyEnabledAt: policy.EnabledAt, Kind: "comment", PolicyOwnerID: OwnerID, PullID: pullID, ActorID: comment.AuthorID, ActorRole: "commenter", CommentID: comment.ID, BodyHash: Digest(comment.Body), EventAt: comment.CreatedAt, ObservedAt: through}
		batch.Requests = append(batch.Requests, NativeRequest{ID: nextID, RequesterID: OwnerID, BotID: 0, CreatedAt: comment.CreatedAt, Snapshot: snapshot, Origin: &origin})
		known[comment.ID] = true
		nextID++
	}
	kind := ""
	if saved == nil || saved.Triggers == nil {
		if !created.Before(policy.EnabledAt) {
			kind = "opened"
		}
	} else if saved.Triggers.Snapshot.Head != snapshot.Head || saved.Triggers.Snapshot.HeadRepositoryID != snapshot.HeadRepositoryID {
		kind = "synchronize"
	}
	if kind != "" {
		cursor.Sequence++
		// A fresh explicit request already targets this captured head. Coalescing the
		// automatic observation avoids duplicate provider work in the same poll.
		if len(batch.Requests) == 0 {
			origin := RequestOrigin{PolicyEnabledAt: policy.EnabledAt, Kind: kind, PolicyOwnerID: OwnerID, PullID: pullID, ActorID: authorID, ActorRole: "pr_author", EventAt: created, ObservedAt: through, Sequence: cursor.Sequence}
			if kind == "synchronize" {
				origin.EventAt = time.Time{}
			}
			batch.Requests = append(batch.Requests, NativeRequest{ID: nextID, RequesterID: OwnerID, CreatedAt: through, Snapshot: snapshot, Origin: &origin})
		}
	}
	batch.Triggers.Cursor = cursor
	return batch, nil
}

func triggerDescription(o *RequestOrigin) string {
	if o == nil {
		return ""
	}
	if o.Kind == "comment" {
		return fmt.Sprintf("Trigger: owner comment %d, author ID %d.\n\n", o.CommentID, o.ActorID)
	}
	return fmt.Sprintf("Trigger: automatic %s polling observation %d; PR ID %d, PR author ID %d. Authorized by owner policy %d; push actor/time unavailable.\n\n", o.Kind, o.Sequence, o.PullID, o.ActorID, o.PolicyOwnerID)
}

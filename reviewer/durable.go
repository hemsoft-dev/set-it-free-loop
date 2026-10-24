package reviewer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrDeferred = errors.New("durable state requires reconciliation")
var ErrFenced = errors.New("worker lease or request generation is no longer current")
var ErrRefundPending = errors.New("before-work refund requires durable recovery before lease release")
var ErrSourceAuditPending = errors.New("source audit could not be persisted; stopped-worker recovery required before lease release")

type Enrollment struct{ RepositoryID, InstallationID, AppID, BotID, OwnerID int64 }

func (e Enrollment) Validate() error {
	if e.RepositoryID != RepositoryID || e.OwnerID != OwnerID || e.InstallationID <= 0 || e.AppID <= 0 || e.BotID <= 0 {
		return errors.New("exact alpha enrollment required")
	}
	return nil
}

type Scope struct {
	RepositoryID, InstallationID int64
	PR                           int
}

func (e Enrollment) accepts(s Scope) bool {
	return s.RepositoryID == e.RepositoryID && s.InstallationID == e.InstallationID && s.PR > 0
}
func (s Scope) key() string { return fmt.Sprintf("%d/%d/%d", s.RepositoryID, s.InstallationID, s.PR) }

// NativeRequest retains the legacy wire name. Origin distinguishes the new
// comment/automatic requests; an absent Origin is legacy native history only.
type NativeRequest struct {
	Origin                 *RequestOrigin `json:",omitempty"`
	ID, RequesterID, BotID int64
	CreatedAt              time.Time
	Snapshot               Snapshot
	Removed                bool
	Admitted               bool
}

// NativeHistoryEvent persists both request and removal provenance. A removed
// boolean alone cannot detect disappearance or mutation of a removal event.
type NativeHistoryEvent struct {
	ID, ActorID, RequesterID, BotID int64
	Kind                            string
	CreatedAt                       time.Time
}

type ConfigRecord struct {
	Version     int64
	Config      Config
	EffectiveAt time.Time
}
type CommandRecord struct {
	Command   Command
	CreatedAt time.Time
	Config    Config
	Version   int64
	WriteID   string
}
type PRRecord struct {
	Triggers         *TriggerCursor `json:",omitempty"`
	Scope            Scope
	Generation       int64
	Paused           bool
	HistoryThrough   time.Time
	PauseRequestID   int64
	CancelledThrough time.Time
	Configs          []ConfigRecord
	Commands         []CommandRecord
	Requests         []NativeRequest
	NativeEvents     []NativeHistoryEvent `json:",omitempty"`
	CurrentRequest   int64
}
type DurableJob struct {
	Job                             Job
	Scope                           Scope
	RequestID                       int64
	CreatedAt                       time.Time
	State                           string
	Checkpoint                      Checkpoint
	LeaseToken                      string
	LeaseUntil                      time.Time
	Outstanding                     bool
	AttemptProvider                 Provider
	AttemptRound                    int
	AttemptChargeDay                string
	ChargeDays                      []string
	AttemptID                       string
	AttemptInputHash                string
	SourceAttempts                  []SourceAttempt
	ProviderAttempts                []ProviderAttempt
	AuditedProviders                []Provider
	AttemptProviderEvidenceRequired bool
	// ProviderRecoveries are operator recovery receipts for attempts whose
	// worker was proven stopped without a complete result. See
	// releaseRecoveredProviderAttempt.
	ProviderRecoveries []ProviderRecoveryReceipt `json:",omitempty"`
}

// ProviderRecoveryReceipt records one explicit operator recovery of a stopped
// provider attempt. It contains no auth bytes, model output or source text.
// The attempt stays charged. Its source access remains unknown, and its
// recorded provider evidence is never promoted to a review artifact. State is
// the job's final state after release, including a cancellation fence.
type ProviderRecoveryReceipt struct {
	AttemptID, LaunchID string
	Provider            Provider
	Round               int
	EvidenceHash        string
	Auth, State         string
	RecoveredAt         time.Time
}

type SourceAttempt struct {
	ID     string
	Access *SourceAccess
}
type WriteIntent struct {
	ID, Kind              string
	Scope                 Scope
	RequestID, Generation int64
	JobHash               string
	CheckpointHash        string `json:",omitempty"`
	// Status is the durable job state a status write reports. It is empty for
	// acknowledgments and reviews.
	Status                  string `json:",omitempty"`
	Snapshot                Snapshot
	Document                Document
	BodyHash, Marker, State string
	ReceiptID               int64
	CreatedAt               time.Time
	ReceiptTime             time.Time
}
type RemoteReceipt struct {
	ID, AuthorID     int64
	Marker, BodyHash string
	CreatedAt        time.Time
	CommitID         string
}
type DurableState struct {
	Enrollment Enrollment
	Defaults   Config
	PRs        map[string]*PRRecord
	Jobs       map[string]*DurableJob
	Writes     map[string]*WriteIntent
	Polling    *PollSchedule `json:"-"`
}

// Store keeps one bounded state record in SQLite. An IMMEDIATE transaction
// serializes every mutation across handles/processes. Remote calls never hold it.
// A dedicated private service directory is required, not the interactive home.
type Store struct {
	db   *sql.DB
	path string
}

func OpenStore(path string, e Enrollment, defaults Config) (*Store, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if err := defaults.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute state path required")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if resolved != filepath.Clean(filepath.Dir(path)) {
		return nil, errors.New("state directory ancestors must not be symlinks")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return nil, errors.New("state directory must be private")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("unsafe state file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		f.Close()
	} else if !os.IsExist(err) {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL)`)
	if err == nil {
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS polling_state (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL)`)
	}
	if err == nil {
		err = s.update(context.Background(), func(d *DurableState) error {
			if d.PRs == nil {
				d.Enrollment = e
				d.Defaults = defaults
				d.PRs = map[string]*PRRecord{}
				d.Jobs = map[string]*DurableJob{}
				d.Writes = map[string]*WriteIntent{}
				return nil
			}
			if d.Enrollment != e || d.Defaults.Hash() != defaults.Hash() {
				return errors.New("enrollment/default changes require explicit migration")
			}
			return nil
		})
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) update(ctx context.Context, fn func(*DurableState) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT data FROM state WHERE id=1").Scan(&raw)
	d := DurableState{}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	var polling []byte
	if err := tx.QueryRowContext(ctx, "SELECT data FROM polling_state WHERE id=1").Scan(&polling); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(polling) > 0 {
		value, err := DecodeStrict[PollSchedule](polling, 2048)
		if err != nil {
			return err
		}
		d.Polling = &value
	}
	if err = fn(&d); err != nil {
		return err
	}
	if len(d.PRs) > 64 || len(d.Jobs) > 128 || len(d.Writes) > 512 {
		return errors.New("durable record cap reached")
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return errors.New("durable byte cap reached")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO state(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", raw)
	if err != nil {
		return err
	}
	if d.Polling != nil {
		data, err := json.Marshal(d.Polling)
		if err != nil || len(data) > 2048 {
			return ErrDeferred
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO polling_state(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", data); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Snapshot(ctx context.Context) (DurableState, error) {
	var raw, polling []byte
	err := s.db.QueryRowContext(ctx, "SELECT data,(SELECT data FROM polling_state WHERE id=1) FROM state WHERE id=1").Scan(&raw, &polling)
	var d DurableState
	if err == nil {
		err = json.Unmarshal(raw, &d)
	}
	if err == nil && len(polling) > 0 {
		value, parseErr := DecodeStrict[PollSchedule](polling, 2048)
		err = parseErr
		d.Polling = &value
	}
	return d, err
}
func prRecord(d *DurableState, scope Scope) (*PRRecord, error) {
	if !d.Enrollment.accepts(scope) {
		return nil, errors.New("out-of-scope event")
	}
	p := d.PRs[scope.key()]
	if p == nil {
		p = &PRRecord{Scope: scope, Generation: 1, Configs: []ConfigRecord{{Version: 1, Config: d.Defaults}}}
		d.PRs[scope.key()] = p
	}
	return p, nil
}
func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func newIntent(kind string, scope Scope, gen, request int64, snapshot Snapshot, jobHash string, doc Document, now time.Time) WriteIntent {
	id := randomToken()
	marker := "<!-- sfl-alpha:" + id + " -->"
	doc.Summary += "\n\n" + marker
	doc = canonicalPublicationDocument(doc)
	return WriteIntent{ID: id, Kind: kind, Scope: scope, RequestID: request, Generation: gen, Snapshot: snapshot, JobHash: jobHash, Document: doc, BodyHash: Digest(doc), Marker: marker, State: "prepared", CreatedAt: now}
}

// CaptureCommands requires a complete, paginated original-event history through
// a trusted platform cutoff. It sorts discovery order and never rewrites accepted
// command bodies. Unknown ACK writes pause dependent admission.
func (s *Store) CaptureCommands(ctx context.Context, scope Scope, events []CommandEvent, through time.Time, complete bool) error {
	if !complete || through.IsZero() {
		return ErrDeferred
	}
	// Stable original creation order, never caller/pagination order.
	ordered := append([]CommandEvent{}, events...)
	sortCommands(ordered)
	return s.update(ctx, func(d *DurableState) error {
		if pr := d.PRs[scope.key()]; pr != nil && pr.Triggers != nil {
			return ErrLiveBlocked
		}
		return applyCommands(d, scope, ordered, through)
	})
}
func applyCommands(d *DurableState, scope Scope, ordered []CommandEvent, through time.Time) error {
	p, err := prRecord(d, scope)
	if err != nil {
		return err
	}
	if through.Before(p.HistoryThrough) {
		return ErrDeferred
	}
	for _, event := range ordered {
		if event.AuthorID != OwnerID || !strings.HasPrefix(event.Body, "/sfl ") {
			continue
		}
		found := false
		for _, old := range p.Commands {
			if old.Command.EventID == event.ID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if event.CreatedAt.After(through) {
			return ErrDeferred
		}
		command, parseErr := ParseCommand(event)
		if event.ID <= 0 || event.CreatedAt.IsZero() {
			return errors.New("invalid platform command identity")
		}
		if parseErr != nil {
			command = Command{EventID: event.ID, AuthorID: OwnerID, BodyHash: Digest(event.Body), Action: "reject", Values: map[string]string{}}
		}
		if !p.HistoryThrough.IsZero() && !event.CreatedAt.After(p.HistoryThrough) {
			return ErrDeferred
		}
		if len(p.Commands) >= 64 {
			return errors.New("command retention cap reached")
		}
		current := p.Configs[len(p.Configs)-1]
		// Proposals form a FIFO chain. None becomes effective before its own
		// platform acknowledgment; dispatch enforces the same order.
		if len(p.Commands) > 0 {
			last := p.Commands[len(p.Commands)-1]
			current.Config = last.Config
			current.Version = last.Version
		}

		next := current.Config
		if parseErr == nil {
			next, parseErr = ApplyConfig(current.Config, d.Defaults, command)
		}
		if parseErr != nil {
			command.Action = "reject"
			next = current.Config
		}
		version := current.Version
		if command.Action == "configure" || command.Action == "reset" {
			version++
		}
		if command.Action == "cancel" {
			if event.CreatedAt.After(p.CancelledThrough) {
				p.CancelledThrough = event.CreatedAt
			}
			p.Generation++
			p.CurrentRequest = 0
		}
		body := fmt.Sprintf("SFL %s accepted. Configuration version %d, hash %s. Configuration applies to the next authorized comment or automatic head review request after this acknowledgment. No inference was started.", command.Action, version, next.Hash())
		if parseErr != nil {
			body = "SFL command rejected: " + parseErr.Error() + ". Configuration is unchanged. No inference was started."
		}
		intent := newIntent("ack", scope, p.Generation, 0, Snapshot{}, "", Document{Summary: body, Inline: []InlineComment{}}, event.CreatedAt)
		d.Writes[intent.ID] = &intent
		p.Commands = append(p.Commands, CommandRecord{command, event.CreatedAt, next, version, intent.ID})
	}
	p.HistoryThrough = through
	retireFencedJobs(d, p)
	return nil
}

func sortCommands(events []CommandEvent) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt.Equal(events[j].CreatedAt) {
			return events[i].ID < events[j].ID
		}
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
}

// RecordRequest accepts only complete trusted native history, with immutable
// requester/target IDs. Re-observation is idempotent; pushes cannot create jobs.
func (s *Store) RecordRequest(ctx context.Context, scope Scope, r NativeRequest, complete bool) error {
	if !complete {
		return ErrDeferred
	}
	if r.RequesterID != OwnerID {
		return ErrIgnored
	}
	return s.update(ctx, func(d *DurableState) error {
		if pr := d.PRs[scope.key()]; pr != nil && pr.Triggers != nil {
			return ErrLiveBlocked
		}
		return applyRequest(d, scope, r)
	})
}
func applyRequest(d *DurableState, scope Scope, r NativeRequest) error {
	p, err := prRecord(d, scope)
	if err != nil {
		return err
	}
	defer func() { retireFencedJobs(d, p) }()
	if r.Origin != nil && r.Origin.validate() != nil {
		return ErrIgnored
	}
	if r.BotID != d.Enrollment.BotID || r.ID <= 0 || r.CreatedAt.IsZero() || r.Snapshot.PR != scope.PR || r.Snapshot.RepositoryID != scope.RepositoryID {
		return errors.New("untrusted native request")
	}
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	for i, old := range p.Requests {
		if old.ID == r.ID {
			immutableOld, immutableNew := old, r
			immutableOld.Removed = false
			immutableOld.Admitted = false
			immutableNew.Admitted = false
			immutableNew.Removed = false
			if Digest(immutableOld) != Digest(immutableNew) {
				return errors.New("request identity changed")
			}
			if r.Removed && !old.Removed {
				p.Requests[i].Removed = true
				if p.CurrentRequest == r.ID {
					p.Generation++
					p.CurrentRequest = 0
				}
			}
			return nil
		}
	}
	if len(p.Requests) >= 64 {
		return errors.New("request retention cap reached")
	}
	p.Requests = append(p.Requests, r)
	latest := r
	for _, old := range p.Requests {
		// Automatic IDs are allocated by serialized local observations. A new
		// observed head is causally later than the previous observation even
		// when GitHub's Date header has the same second. This is not a claim
		// that numeric remote event IDs order independent manual comments.
		observedLater := requestBefore(old, r)
		if old.CreatedAt.Equal(r.CreatedAt) && old.ID != r.ID && !observedLater {
			p.Paused = true
		}
		if requestBefore(latest, old) {
			latest = old
		}
	}
	if latest.ID == r.ID {
		p.Generation++
		p.CurrentRequest = r.ID
		if !r.CreatedAt.After(p.CancelledThrough) {
			p.CurrentRequest = 0
			if r.CreatedAt.Equal(p.CancelledThrough) {
				p.Paused = true
				p.PauseRequestID = r.ID
			}
		}
		if r.Removed {
			p.CurrentRequest = 0
		}
	}
	return nil
}

// Admit maps platform request time to confirmed ACK time. Equal timestamps and
// incomplete command history defer, even if poll discovery order differs.
func (s *Store) Admit(ctx context.Context, scope Scope, requestID int64, release, packet string, now time.Time) (Job, error) {
	var job Job
	err := s.update(ctx, func(d *DurableState) error {
		p, err := prRecord(d, scope)
		if err != nil {
			return err
		}
		if p.Paused || p.CurrentRequest != requestID {
			return ErrDeferred
		}
		var request NativeRequest
		for _, r := range p.Requests {
			if r.ID == requestID {
				request = r
			}
		}
		if request.ID == 0 || request.Removed || request.CreatedAt.After(p.HistoryThrough) {
			return ErrDeferred
		}
		for _, c := range p.Commands {
			if d.Writes[c.WriteID].State != "confirmed" {
				return ErrDeferred
			}
		}
		selected := p.Configs[0]
		for _, c := range p.Configs[1:] {
			if request.CreatedAt.Equal(c.EffectiveAt) {
				return ErrDeferred
			}
			if request.CreatedAt.After(c.EffectiveAt) && c.Version > selected.Version {
				selected = c
			}
		}
		id := fmt.Sprintf("native-%d-%d", scope.PR, requestID)
		if request.Origin != nil {
			id = fmt.Sprintf("request-%d-%d", scope.PR, requestID)
		}
		if old := d.Jobs[id]; old != nil {
			job = old.Job
			return nil
		}
		if request.Admitted {
			return ErrDeferred
		}
		for _, old := range d.Jobs {
			if old.Scope == scope && old.RequestID == requestID {
				return ErrDeferred
			}
		}
		job = Job{ID: id, RequesterID: OwnerID, Generation: p.Generation, Snapshot: request.Snapshot, Config: selected.Config, ConfigVersion: selected.Version, Release: release, Packet: packet, Origin: request.Origin}
		if err := job.Validate(); err != nil {
			return err
		}
		for i := range p.Requests {
			if p.Requests[i].ID == requestID {
				p.Requests[i].Admitted = true
			}
		}
		d.Jobs[id] = &DurableJob{Job: job, Scope: scope, RequestID: requestID, CreatedAt: now, State: "queued", Checkpoint: Checkpoint{JobHash: Digest(job), Artifacts: []Artifact{}}}
		return nil
	})
	return job, err
}

// Claim does not steal expired leases. A qualified supervisor must prove the old
// process stopped and call ReleaseStopped before another physical worker starts.
func (s *Store) claimJob(ctx context.Context, id string, now time.Time) (*DurableJob, error) {
	var captured *DurableJob
	token := randomToken()
	err := s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil {
			return ErrFenced
		}
		p := d.PRs[j.Scope.key()]
		if p.Paused || p.Generation != j.Job.Generation || p.CurrentRequest != j.RequestID {
			return ErrFenced
		}
		for _, other := range d.Jobs {
			if other.LeaseToken != "" {
				return ErrDeferred
			}
		}
		if now.Sub(j.CreatedAt) > j.Job.Config.QuotaWait {
			j.State = "incomplete_expired"
			return nil
		}
		if j.State != "queued" && j.State != "quota_wait" {
			return ErrDeferred
		}
		daily := 0
		for _, other := range d.Jobs {
			if chargedOn(other, now.UTC().Format("2006-01-02")) {
				daily++
			}
		}
		if j.Checkpoint.ConsumedSessions == 0 && daily >= j.Job.Config.DailyReviews {
			return ErrDeferred
		}
		j.LeaseToken = token
		j.LeaseUntil = now.Add(j.Job.Config.Limits.Timeout)
		j.State = "running"
		copy := *j
		captured = &copy
		return nil
	})
	if err != nil {
		return nil, err
	}
	if captured == nil {
		return nil, ErrDeferred
	}
	return captured, nil
}
func (s *Store) Claim(ctx context.Context, id string, now time.Time) (string, error) {
	captured, err := s.claimJob(ctx, id, now)
	if err != nil {
		return "", err
	}
	return captured.LeaseToken, nil
}

func fenced(d *DurableState, id, token string, now time.Time) (*DurableJob, error) {
	j := d.Jobs[id]
	if j == nil || token == "" || j.LeaseToken != token || !now.Before(j.LeaseUntil) {
		return nil, ErrFenced
	}
	p := d.PRs[j.Scope.key()]
	if p.Paused || p.Generation != j.Job.Generation || p.CurrentRequest != j.RequestID {
		return nil, ErrFenced
	}
	return j, nil
}
func (s *Store) ReleaseStopped(ctx context.Context, id, token string, state string) error {
	return s.releaseStopped(ctx, id, token, state, false)
}

func (s *Store) releaseStopped(ctx context.Context, id, token, state string, retainOutstanding bool) error {
	switch state {
	case "queued", "quota_wait", "cancelled", "provider_unavailable", "auth_failed", "blocked", "dual_complete", "single_complete", "invalid_output", "incomplete_expired":
	default:
		return errors.New("invalid terminal state")
	}
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token {
			return ErrFenced
		}
		if retainOutstanding && j.Outstanding {
			return ErrDeferred
		}
		if j.Outstanding && j.AttemptProviderEvidenceRequired {
			if _, err := recordedStoppedAttempt(j); err != nil {
				return err
			}
		}
		releaseStoppedJob(d, j, state)
		return nil
	})
}

// recordedStoppedAttempt returns the durably recorded stopped provider
// evidence for the job's outstanding attempt.
func recordedStoppedAttempt(j *DurableJob) (*ProviderEvidence, error) {
	limits, err := protocolLimits(j.Job)
	if err != nil {
		return nil, err
	}
	for _, attempt := range j.ProviderAttempts {
		if attempt.ID == j.AttemptID && attempt.Evidence.validateStoppedCaptured(j.AttemptProvider, limits, j.AttemptInputHash) == nil {
			return attempt.Evidence, nil
		}
	}
	return nil, ErrProviderAuditPending
}

func releaseStoppedJob(d *DurableState, j *DurableJob, state string) {
	j.LeaseToken = ""
	j.LeaseUntil = time.Time{}
	j.Outstanding = false
	j.AttemptChargeDay = ""
	j.State = state
	retireFencedJobs(d, d.PRs[j.Scope.key()])
}

// releaseRecoveredProviderAttempt is the store half of the operator's stopped
// provider recovery (GitReviewerService.RecoverStoppedProvider). Its caller
// must first prove the original unit stopped and settle that attempt's auth.
// It releases only the exact outstanding audited attempt whose recorded
// stopped evidence matches evidenceHash, under the unchanged lease nonce. The
// session stays charged, no source access is invented and the job becomes
// auth_failed or provider_unavailable, which only ResumeUnavailable can requeue.
// A job already fenced by a cancellation or replacement stays cancelled or
// stale. It returns the job's final state, which the receipt also records.
func (s *Store) releaseRecoveredProviderAttempt(ctx context.Context, id, token string, receipt ProviderRecoveryReceipt, now time.Time) (string, error) {
	if receipt.State != "auth_failed" && receipt.State != "provider_unavailable" {
		return "", errors.New("invalid recovery state")
	}
	if !validAuthHash(receipt.EvidenceHash) || receipt.AttemptID == "" || !receipt.Provider.Valid() || receipt.Auth == "" {
		return "", ErrFenced
	}
	final := ""
	err := s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token || !j.Outstanding || !j.AttemptProviderEvidenceRequired || j.AttemptID != receipt.AttemptID || j.AttemptProvider != receipt.Provider || j.AttemptRound != receipt.Round || d.PRs[j.Scope.key()] == nil {
			return ErrFenced
		}
		evidence, err := recordedStoppedAttempt(j)
		if err != nil {
			return err
		}
		if Digest(cloneProviderEvidence(evidence)) != receipt.EvidenceHash {
			return ErrFenced
		}
		if len(j.ProviderRecoveries) >= 128 {
			return errors.New("provider recovery retention cap reached")
		}
		receipt.RecoveredAt = now.UTC()
		releaseStoppedJob(d, j, receipt.State)
		receipt.State = j.State
		j.ProviderRecoveries = append(j.ProviderRecoveries, receipt)
		final = j.State
		return nil
	})
	if err != nil {
		return "", err
	}
	return final, nil
}

// RefundStoppedBeforeWork refunds only the exact outstanding attempt. The trusted
// caller must prove that its worker stopped before doing provider work. A lease
// is never stolen on expiry, so its nonce still authorizes this refund after
// timeout or cancellation. After ErrRefundPending, retry this transaction after
// repairing storage, then call ReleaseStopped; do not start another worker.
func (s *Store) RefundStoppedBeforeWork(ctx context.Context, id, token string, provider Provider, round int) error {
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token || !j.Outstanding || j.AttemptProvider != provider || j.AttemptRound != round || j.Checkpoint.ConsumedSessions <= 0 {
			return ErrFenced
		}
		for _, attempt := range j.ProviderAttempts {
			if attempt.ID == j.AttemptID && attempt.Evidence != nil && attempt.Evidence.Access.Requests > 0 {
				return ErrFenced
			}
		}
		j.Checkpoint.ConsumedSessions--
		if j.AttemptChargeDay != "" {
			days := []string{}
			for _, day := range j.ChargeDays {
				if day != j.AttemptChargeDay {
					days = append(days, day)
				}
			}
			j.ChargeDays = days
		}
		j.AttemptChargeDay = ""
		j.Outstanding = false
		return nil
	})
}

// RecordStoppedSourceAccess is a trusted recovery write after a driver has
// stopped. It persists broker evidence for the exact reserved attempt without
// starting provider work. Repeating the identical write is safe; a different
// receipt or attempt is rejected. After ErrSourceAuditPending, repair storage,
// record Outcome.FailedSourceAccess with the retained nonce/attempt ID, then
// release the stopped lease. Unknown provider work remains charged.
// A trusted original BeforeWork failure must also be refunded through
// RefundStoppedBeforeWork before ReleaseStopped.
func (s *Store) RecordStoppedSourceAccess(ctx context.Context, id, token, attemptID string, access *SourceAccess) error {
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token || !j.Outstanding || attemptID == "" || j.AttemptID != attemptID || access == nil {
			return ErrFenced
		}
		limits, err := protocolLimits(j.Job)
		if err != nil {
			return err
		}
		if err = access.validateCaptured(j.Job.Snapshot, limits, j.AttemptInputHash); err != nil {
			return err
		}
		for _, old := range j.SourceAttempts {
			if old.ID == attemptID {
				if Digest(old.Access) != Digest(access) {
					return ErrFenced
				}
				return nil
			}
		}
		if len(j.SourceAttempts) >= 128 {
			return errors.New("source attempt retention cap reached")
		}
		j.SourceAttempts = append(j.SourceAttempts, SourceAttempt{attemptID, cloneSourceAccess(access)})
		return nil
	})
}

// DurableReviewer reserves a session before provider work and persists validated
// evidence before returning it to Engine. Unknown/crashed attempts consume budget.
// It is an offline seam: the supplied underlying Reviewer must be trusted.
type DurableReviewer struct {
	Store  *Store
	Job    Job
	Token  string
	Inner  Reviewer
	Reader func(Snapshot) (*Reader, error)
	Now    func() time.Time
}

func (r DurableReviewer) Review(ctx context.Context, session Session) (Report, error) {
	report, _, err := r.ReviewSource(ctx, session)
	return report, err
}

func (r DurableReviewer) ReviewSource(ctx context.Context, session Session) (Report, *SourceAccess, error) {
	var access *SourceAccess
	report, err := r.review(ctx, session, func(a *SourceAccess, _ *ProviderEvidence, _ bool) { access = cloneSourceAccess(a) })
	return report, access, err
}

func (r DurableReviewer) review(ctx context.Context, session Session, capture func(*SourceAccess, *ProviderEvidence, bool)) (Report, error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	if err := session.Validate(); err != nil {
		return Report{}, err
	}
	expectedLimits, err := protocolLimits(r.Job)
	if err != nil {
		return Report{}, err
	}
	if session.Limits != expectedLimits || session.JobID != r.Job.ID || session.Snapshot != r.Job.Snapshot || session.ConfigHash != r.Job.Config.Hash() || session.PacketHash != Digest(r.Job.Packet) {
		return Report{}, ErrFenced
	}
	_, requiresAudit := r.Inner.(AuditedReviewer)
	if requiresAudit {
		// Selection survives quota rollback and crashes before reservation.
		// Otherwise a retry could replace this provider with a legacy driver.
		if err = r.Store.update(ctx, func(d *DurableState) error {
			j, err := fenced(d, r.Job.ID, r.Token, now())
			if err != nil {
				return err
			}
			if Digest(j.Job) != Digest(r.Job) || j.Outstanding || j.Checkpoint.ConsumedSessions >= j.Job.Config.MaxSessions() {
				return ErrFenced
			}
			for _, provider := range j.AuditedProviders {
				if provider == session.Identity.Provider {
					return nil
				}
			}
			j.AuditedProviders = append(j.AuditedProviders, session.Identity.Provider)
			return nil
		}); err != nil {
			return Report{}, err
		}
	}
	attemptID := randomToken()
	err = r.Store.update(ctx, func(d *DurableState) error {
		j, err := fenced(d, r.Job.ID, r.Token, now())
		if err != nil {
			return err
		}
		if Digest(j.Job) != Digest(r.Job) || j.Outstanding || j.Checkpoint.ConsumedSessions >= j.Job.Config.MaxSessions() {
			return ErrFenced
		}
		knownAudit := false
		for _, provider := range j.AuditedProviders {
			if provider == session.Identity.Provider {
				knownAudit = true
				break
			}
		}
		if knownAudit && !requiresAudit {
			return ErrProviderAuditPending
		}
		day := now().UTC().Format("2006-01-02")
		j.AttemptChargeDay = ""
		if !chargedOn(j, day) {
			count := 0
			for _, other := range d.Jobs {
				if chargedOn(other, day) {
					count++
				}
			}
			if count >= j.Job.Config.DailyReviews {
				return PhaseError{State: "quota_wait", BeforeWork: true}
			}
			j.AttemptChargeDay = day
			j.ChargeDays = append(j.ChargeDays, day)
		}
		j.Checkpoint.ConsumedSessions++
		j.Outstanding = true
		j.AttemptProvider = session.Identity.Provider
		j.AttemptRound = session.Round
		j.AttemptID = attemptID
		j.AttemptInputHash = Digest(session)
		_, j.AttemptProviderEvidenceRequired = r.Inner.(AuditedReviewer)
		j.LeaseUntil = now().Add(j.Job.Config.Limits.Timeout + time.Minute)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrFenced) {
			return Report{}, errors.Join(context.Canceled, ErrFenced, PhaseError{State: "cancelled", BeforeWork: true})
		}
		return Report{}, err
	}
	report, access, providerEvidence, providerRequired, err := reviewPhase(ctx, r.Inner, session, attemptID)
	capture(access, providerEvidence, providerRequired)
	if errors.Is(err, ErrWorkerStopPending) || errors.Is(err, ErrProviderAuditPending) || errors.Is(err, ErrProviderEgressPending) {
		// No stopped-worker write, refund or lease release is safe until the
		// supervisor recovers the physical worker. Return the retained audit.
		return Report{}, err
	}
	if providerEvidence != nil {
		if auditErr := r.Store.RecordStoppedProviderEvidence(context.WithoutCancel(ctx), r.Job.ID, r.Token, attemptID, providerEvidence); auditErr != nil {
			return Report{}, errors.Join(workerLauncherError(err, false), ErrProviderAuditPending, auditErr)
		}
	}
	if access != nil {
		auditErr := r.Store.RecordStoppedSourceAccess(context.WithoutCancel(ctx), r.Job.ID, r.Token, attemptID, access)
		if auditErr != nil {
			return Report{}, errors.Join(err, ErrSourceAuditPending, auditErr)
		}
	}
	if err != nil {
		var pe PhaseError
		if errors.As(err, &pe) && pe.BeforeWork {
			if refundErr := r.Store.RefundStoppedBeforeWork(context.WithoutCancel(ctx), r.Job.ID, r.Token, session.Identity.Provider, session.Round); refundErr != nil {
				return Report{}, errors.Join(err, ErrRefundPending, fmt.Errorf("persist before-work refund: %w", refundErr))
			}
		}
		return Report{}, err
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
	}
	report, err = DecodeStrict[Report](encoded, session.Limits.OutputBytes)
	if err != nil {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
	}
	if err = ValidateReport(report, session); err != nil {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
	}
	var reader *Reader
	if r.Reader != nil {
		reader, err = r.Reader(r.Job.Snapshot)
		if err != nil {
			return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
		}
	}
	if err = (Adapter{Reader: reader}).verifyReport(ctx, session, report); err != nil {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
	}
	report, err = mergeReaderCoverage(report, reader)
	if err != nil {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, err)
	}
	encoded, err = json.Marshal(report)
	if err != nil || len(encoded) > session.Limits.OutputBytes {
		return Report{}, errors.Join(PhaseError{State: "invalid_output"}, errors.New("durable report exceeds output bound"))
	}
	a := Artifact{JobHash: Digest(r.Job), InputHash: Digest(session), Report: report, ValidationReads: []ReadRecord{}, Coverage: []Coverage{}, SourceAccess: cloneSourceAccess(access), ProviderEvidence: cloneProviderEvidence(providerEvidence), ProviderEvidenceRequired: providerRequired}
	if reader != nil {
		reads, coverage := reader.Evidence()
		a.ValidationReads = append([]ReadRecord{}, reads...)
		a.Coverage = append([]Coverage{}, coverage...)
	}
	err = r.Store.update(ctx, func(d *DurableState) error {
		j, e := fenced(d, r.Job.ID, r.Token, now())
		if e != nil {
			return e
		}
		if !j.Outstanding || j.AttemptProvider != session.Identity.Provider || j.AttemptRound != session.Round {
			return ErrFenced
		}
		j.Checkpoint.Artifacts = append(j.Checkpoint.Artifacts, a)
		j.Outstanding = false
		j.AttemptChargeDay = ""
		return nil
	})
	if errors.Is(err, ErrFenced) {
		return Report{}, errors.Join(context.Canceled, ErrFenced)
	}
	return report, err
}

func chargedOn(j *DurableJob, day string) bool {
	for _, old := range j.ChargeDays {
		if old == day {
			return true
		}
	}
	return false
}

// Discovery is normalized complete pagination from the qualified transport,
// including removal/re-request history. Partial pages cannot advance a cutoff.
type Discovery struct {
	Triggers     *TriggerBatch
	Scope        Scope
	Commands     []CommandEvent
	Requests     []NativeRequest
	NativeEvents []NativeHistoryEvent
	Through      time.Time
	Complete     bool
}

// Native history must reproduce every persisted event's immutable fields.
// New events behind a trusted cutoff contradict that cutoff and cannot admit
// work. A request already persisted by the legacy normalized seam can bootstrap
// its exact creation event; that provides no authority to infer old removals.
func retainNativeHistory(e Enrollment, pr *PRRecord, events []NativeHistoryEvent, through time.Time) error {
	if events == nil {
		if len(pr.NativeEvents) != 0 {
			return ErrDeferred
		}
		return nil
	}
	if len(events) > 128 || through.IsZero() || through.Before(pr.HistoryThrough) {
		return ErrDeferred
	}
	known := map[int64]NativeHistoryEvent{}
	for _, event := range pr.NativeEvents {
		known[event.ID] = event
	}
	seen := map[int64]bool{}
	for _, event := range events {
		if event.ID <= 0 || seen[event.ID] || event.BotID != e.BotID || event.ActorID <= 0 || event.CreatedAt.IsZero() || event.CreatedAt.After(through) ||
			(event.Kind != "review_requested" && event.Kind != "review_request_removed") ||
			(event.Kind == "review_requested" && (event.RequesterID <= 0 || event.RequesterID != event.ActorID)) {
			return ErrDeferred
		}
		seen[event.ID] = true
		if old, ok := known[event.ID]; ok {
			a, b := old, event
			a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
			if a != b || !old.CreatedAt.Equal(event.CreatedAt) {
				return ErrDeferred
			}
			delete(known, event.ID)
			continue
		}
		if !pr.HistoryThrough.IsZero() && !event.CreatedAt.After(pr.HistoryThrough) {
			bootstrap := false
			if len(pr.NativeEvents) == 0 && event.Kind == "review_requested" {
				for _, r := range pr.Requests {
					if r.ID == event.ID && r.BotID == event.BotID && r.RequesterID == event.RequesterID && r.CreatedAt.Equal(event.CreatedAt) {
						bootstrap = true
						break
					}
				}
			}
			if !bootstrap {
				return ErrDeferred
			}
		}
	}
	if len(known) != 0 {
		return ErrDeferred
	}
	pr.NativeEvents = append([]NativeHistoryEvent{}, events...)
	return nil
}

func (s *Store) Discover(ctx context.Context, batch Discovery) error {
	if !batch.Complete || batch.Through.IsZero() {
		return ErrDeferred
	}
	ordered := append([]CommandEvent{}, batch.Commands...)
	sortCommands(ordered)
	return s.update(ctx, func(d *DurableState) error {
		if batch.Triggers != nil {
			return applyTriggers(d, batch)
		}
		if saved := d.PRs[batch.Scope.key()]; saved != nil && saved.Triggers != nil {
			return ErrLiveBlocked
		}
		pr, err := prRecord(d, batch.Scope)
		if err != nil {
			return err
		}
		if err = retainNativeHistory(d.Enrollment, pr, batch.NativeEvents, batch.Through); err != nil {
			return err
		}
		wasPaused := pr.Paused
		pausedRequest := pr.PauseRequestID
		if err = applyCommands(d, batch.Scope, ordered, batch.Through); err != nil {
			return err
		}
		for _, r := range batch.Requests {
			if r.CreatedAt.After(batch.Through) {
				return ErrDeferred
			}
			if r.RequesterID != OwnerID {
				continue
			}
			if err = applyRequest(d, batch.Scope, r); err != nil {
				return err
			}
		}
		if wasPaused && pr.CurrentRequest != 0 && pr.CurrentRequest != pausedRequest {
			safe := true
			var latest NativeRequest
			for _, r := range pr.Requests {
				if r.ID == pr.CurrentRequest {
					latest = r
				}
			}
			if latest.Removed || latest.ID == 0 {
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
	})
}

// RunStopped composes durable phase reservations and Engine. Underlying drivers
// must return only after their worker is stopped, including timeout/cancellation.
// This method never starts a service, chooses credentials or publishes a review.
func (s *Store) RunStopped(ctx context.Context, id string, engine Engine, now func() time.Time) (Outcome, error) {
	if now == nil {
		now = time.Now
	}
	j, err := s.claimJob(ctx, id, now())
	if err != nil {
		return Outcome{}, err
	}
	token := j.LeaseToken
	wrapped := Engine{Reader: engine.Reader, Reviewers: map[Provider]Reviewer{}}
	for provider, inner := range engine.Reviewers {
		d := DurableReviewer{Store: s, Job: j.Job, Token: token, Inner: inner, Reader: engine.Reader, Now: now}
		if _, required := inner.(AuditedReviewer); required {
			wrapped.Reviewers[provider] = durableAuditedReviewer{d}
		} else {
			wrapped.Reviewers[provider] = d
		}
	}
	out, runErr := wrapped.Run(ctx, j.Job, j.Checkpoint)
	retainOutstanding := errors.Is(runErr, ErrRefundPending) || errors.Is(runErr, ErrSourceAuditPending) || errors.Is(runErr, ErrProviderAuditPending) || errors.Is(runErr, ErrProviderEgressPending) || errors.Is(runErr, ErrWorkerStopPending)
	// A pre-reservation rejection has no physical attempt to recover. Check
	// outstanding state atomically with release, preserving real recovery leases.
	state := out.State
	if err = s.releaseStopped(context.WithoutCancel(ctx), id, token, state, retainOutstanding); err != nil {
		return out, errors.Join(runErr, err)
	}
	return out, runErr
}

// RecoverCompleted reconstructs a completed outcome after a crash before outbox
// preparation. It supplies no provider workers, so only validated cached phases
// can be used. Source validation is repeated against the immutable snapshot.
func (s *Store) RecoverCompleted(ctx context.Context, id string, source func(Snapshot) (*Reader, error)) (Outcome, error) {
	d, err := s.Snapshot(ctx)
	if err != nil {
		return Outcome{}, err
	}
	j := d.Jobs[id]
	if j == nil || j.LeaseToken != "" || j.Outstanding || (j.State != "dual_complete" && j.State != "single_complete") || len(j.Checkpoint.Artifacts) != j.Job.Config.MaxSessions() {
		return Outcome{}, ErrDeferred
	}
	for _, artifact := range j.Checkpoint.Artifacts {
		if err := validateArtifactProviderEvidence(j, artifact); err != nil {
			return Outcome{}, err
		}
	}
	return (Engine{Reader: source}).Run(ctx, j.Job, j.Checkpoint)
}

// ResumeUnavailable is an explicit trusted-controller recovery action for the
// existing native request. It performs no inference and resets no budgets,
// config, release or artifacts. Call only after the external cause is repaired;
// configuration comments alone never invoke it or create a review request.
func (s *Store) ResumeUnavailable(ctx context.Context, id string, now time.Time) error {
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || j.LeaseToken != "" || j.Outstanding {
			return ErrDeferred
		}
		if j.State != "auth_failed" && j.State != "provider_unavailable" && j.State != "blocked" {
			return ErrDeferred
		}
		pr := d.PRs[j.Scope.key()]
		if pr.Paused || pr.Generation != j.Job.Generation || pr.CurrentRequest != j.RequestID {
			return ErrFenced
		}
		if now.Sub(j.CreatedAt) > j.Job.Config.QuotaWait || j.Checkpoint.ConsumedSessions >= j.Job.Config.MaxSessions() {
			return ErrDeferred
		}
		j.State = "queued"
		return nil
	})
}

func retireFencedJobs(d *DurableState, p *PRRecord) {
	for _, j := range d.Jobs {
		if j.Scope == p.Scope && (j.Job.Generation != p.Generation || j.RequestID != p.CurrentRequest) {
			if p.CurrentRequest == 0 {
				j.State = "cancelled"
			} else {
				j.State = "stale"
			}
		}
	}
}

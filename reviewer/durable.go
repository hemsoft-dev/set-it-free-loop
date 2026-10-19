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

type NativeRequest struct {
	ID, RequesterID, BotID int64
	CreatedAt              time.Time
	Snapshot               Snapshot
	Removed                bool
	Admitted               bool
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
	Scope          Scope
	Generation     int64
	Paused         bool
	HistoryThrough time.Time
	Configs        []ConfigRecord
	Commands       []CommandRecord
	Requests       []NativeRequest
	CurrentRequest int64
}
type DurableJob struct {
	Job             Job
	Scope           Scope
	RequestID       int64
	CreatedAt       time.Time
	State           string
	Checkpoint      Checkpoint
	LeaseToken      string
	LeaseUntil      time.Time
	Outstanding     bool
	AttemptProvider Provider
	AttemptRound    int
	ChargeDays      []string
}
type WriteIntent struct {
	ID, Kind                string
	Scope                   Scope
	RequestID, Generation   int64
	JobHash                 string
	Snapshot                Snapshot
	Document                Document
	BodyHash, Marker, State string
	ReceiptID               int64
	CreatedAt               time.Time
}
type RemoteReceipt struct {
	ID, AuthorID     int64
	Marker, BodyHash string
	CreatedAt        time.Time
}
type DurableState struct {
	Enrollment Enrollment
	Defaults   Config
	PRs        map[string]*PRRecord
	Jobs       map[string]*DurableJob
	Writes     map[string]*WriteIntent
}

// Store keeps one bounded state record in SQLite. An IMMEDIATE transaction
// serializes every mutation across handles/processes. Remote calls never hold it.
// A dedicated private service directory is required, not the interactive home.
type Store struct{ db *sql.DB }

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
	s := &Store{db}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL)`)
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
	return tx.Commit()
}
func (s *Store) Snapshot(ctx context.Context) (DurableState, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM state WHERE id=1").Scan(&raw)
	var d DurableState
	if err == nil {
		err = json.Unmarshal(raw, &d)
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
			command, err := ParseCommand(event)
			if err != nil {
				return err
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

			next, err := ApplyConfig(current.Config, d.Defaults, command)
			if err != nil {
				return err
			}
			version := current.Version
			if command.Action == "configure" || command.Action == "reset" {
				version++
			}
			if command.Action == "cancel" {
				p.Generation++
				p.CurrentRequest = 0
			}
			body := fmt.Sprintf("SFL %s accepted. Configuration version %d, hash %s. Configuration applies to the next native owner review request after this acknowledgment. No inference was started.", command.Action, version, next.Hash())
			intent := newIntent("ack", scope, p.Generation, 0, Snapshot{}, "", Document{Summary: body, Inline: []InlineComment{}}, event.CreatedAt)
			d.Writes[intent.ID] = &intent
			p.Commands = append(p.Commands, CommandRecord{command, event.CreatedAt, next, version, intent.ID})
		}
		p.HistoryThrough = through
		return nil
	})
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
		p, err := prRecord(d, scope)
		if err != nil {
			return err
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
			if old.CreatedAt.Equal(r.CreatedAt) && old.ID != r.ID {
				p.Paused = true
			}
			if old.CreatedAt.After(latest.CreatedAt) {
				latest = old
			}
		}
		if latest.ID == r.ID && !r.Removed {
			p.Generation++
			p.CurrentRequest = r.ID
		}
		return nil
	})
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
		job = Job{ID: id, RequesterID: OwnerID, Generation: p.Generation, Snapshot: request.Snapshot, Config: selected.Config, ConfigVersion: selected.Version, Release: release, Packet: packet}
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
func (s *Store) Claim(ctx context.Context, id string, now time.Time) (string, error) {
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
		return nil
	})
	if err != nil {
		return "", err
	}
	d, err := s.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	if d.Jobs[id].LeaseToken != token {
		return "", ErrDeferred
	}
	return token, nil
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
	switch state {
	case "queued", "quota_wait", "cancelled", "provider_unavailable", "dual_complete", "single_complete", "invalid_output", "incomplete_expired":
	default:
		return errors.New("invalid terminal state")
	}
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token {
			return ErrFenced
		}
		j.LeaseToken = ""
		j.LeaseUntil = time.Time{}
		j.Outstanding = false
		j.State = state
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
	err = r.Store.update(ctx, func(d *DurableState) error {
		j, err := fenced(d, r.Job.ID, r.Token, now())
		if err != nil {
			return err
		}
		if Digest(j.Job) != Digest(r.Job) || j.Outstanding || j.Checkpoint.ConsumedSessions >= j.Job.Config.MaxSessions() {
			return ErrFenced
		}
		day := now().UTC().Format("2006-01-02")
		if !chargedOn(j, day) {
			count := 0
			for _, other := range d.Jobs {
				if chargedOn(other, day) {
					count++
				}
			}
			if count >= j.Job.Config.DailyReviews {
				return ErrDeferred
			}
			j.ChargeDays = append(j.ChargeDays, day)
		}
		j.Checkpoint.ConsumedSessions++
		j.Outstanding = true
		j.AttemptProvider = session.Identity.Provider
		j.AttemptRound = session.Round
		j.LeaseUntil = now().Add(j.Job.Config.Limits.Timeout + time.Minute)
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	report, err := r.Inner.Review(ctx, session)
	if err != nil {
		var pe PhaseError
		if errors.As(err, &pe) && pe.BeforeWork {
			_ = r.Store.update(context.WithoutCancel(ctx), func(d *DurableState) error {
				j, e := fenced(d, r.Job.ID, r.Token, now())
				if e != nil {
					return e
				}
				j.Checkpoint.ConsumedSessions--
				j.Outstanding = false
				return nil
			})
		}
		return Report{}, err
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return Report{}, err
	}
	report, err = DecodeStrict[Report](encoded, session.Limits.OutputBytes)
	if err != nil {
		return Report{}, err
	}
	if err = ValidateReport(report, session); err != nil {
		return Report{}, err
	}
	var reader *Reader
	if r.Reader != nil {
		reader, err = r.Reader(r.Job.Snapshot)
		if err != nil {
			return Report{}, err
		}
	}
	if err = (Adapter{Reader: reader}).verifyReport(ctx, session, report); err != nil {
		return Report{}, err
	}
	report, err = mergeReaderCoverage(report, reader)
	if err != nil {
		return Report{}, err
	}
	encoded, err = json.Marshal(report)
	if err != nil || len(encoded) > session.Limits.OutputBytes {
		return Report{}, errors.New("durable report exceeds output bound")
	}
	a := Artifact{JobHash: Digest(r.Job), InputHash: Digest(session), Report: report, ValidationReads: []ReadRecord{}, Coverage: []Coverage{}}
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
		return nil
	})
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
	Scope    Scope
	Commands []CommandEvent
	Requests []NativeRequest
	Through  time.Time
	Complete bool
}

func (s *Store) Discover(ctx context.Context, batch Discovery) error {
	if !batch.Complete {
		return ErrDeferred
	}
	if err := s.CaptureCommands(ctx, batch.Scope, batch.Commands, batch.Through, true); err != nil {
		return err
	}
	for _, r := range batch.Requests {
		if r.CreatedAt.After(batch.Through) {
			return ErrDeferred
		}
		if err := s.RecordRequest(ctx, batch.Scope, r, true); err != nil && !errors.Is(err, ErrIgnored) {
			return err
		}
	}
	return nil
}

// RunStopped composes durable phase reservations and Engine. Underlying drivers
// must return only after their worker is stopped, including timeout/cancellation.
// This method never starts a service, chooses credentials or publishes a review.
func (s *Store) RunStopped(ctx context.Context, id string, engine Engine, now func() time.Time) (Outcome, error) {
	if now == nil {
		now = time.Now
	}
	token, err := s.Claim(ctx, id, now())
	if err != nil {
		return Outcome{}, err
	}
	d, err := s.Snapshot(ctx)
	if err != nil {
		return Outcome{}, err
	}
	j := d.Jobs[id]
	wrapped := Engine{Reader: engine.Reader, Reviewers: map[Provider]Reviewer{}}
	for provider, inner := range engine.Reviewers {
		wrapped.Reviewers[provider] = DurableReviewer{Store: s, Job: j.Job, Token: token, Inner: inner, Reader: engine.Reader, Now: now}
	}
	out, runErr := wrapped.Run(ctx, j.Job, j.Checkpoint)
	state := out.State
	if state == "blocked" || state == "auth_failed" {
		state = "provider_unavailable"
	}
	if err = s.ReleaseStopped(context.WithoutCancel(ctx), id, token, state); err != nil {
		return out, errors.Join(runErr, err)
	}
	return out, runErr
}

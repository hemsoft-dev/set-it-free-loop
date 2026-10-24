package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Job struct {
	Origin        *RequestOrigin `json:"request_origin,omitempty"`
	ID            string         `json:"id"`
	RequesterID   int64          `json:"requester_id"`
	Generation    int64          `json:"generation"`
	Snapshot      Snapshot       `json:"snapshot"`
	Config        Config         `json:"config"`
	ConfigVersion int64          `json:"config_version"`
	Release       string         `json:"trusted_release"`
	Packet        string         `json:"packet"`
}

func (j Job) Validate() error {
	if j.Origin != nil && j.Origin.validate() != nil {
		return ErrIgnored
	}
	if j.RequesterID != OwnerID || j.Generation < 1 || j.ConfigVersion < 1 || !objectID.MatchString(j.Release) {
		return errors.New("untrusted job admission or runtime provenance")
	}
	if err := j.Config.Validate(); err != nil {
		return err
	}
	limits, err := protocolLimits(j)
	if err != nil {
		return err
	}
	_, err = NewSession(j.ID, j.Snapshot, j.Config.Hash(), j.Config.Providers()[0], 1, j.Packet, nil, nil, limits)
	return err
}

// Reserve aggregate input space for frozen peer reports, JSON escaping, focus
// IDs and trusted task metadata. A phase cannot admit an output that makes the
// selected later round impossible solely because of serialized input size.
func protocolLimits(j Job) (Limits, error) {
	l := j.Config.Limits
	packet, _ := json.Marshal(j.Packet)
	reserve := 64 << 10
	peers := 1
	if j.Config.Mode == Adversarial {
		peers = 2 * j.Config.PeerRounds
	}
	available := l.InputBytes - len(packet) - reserve
	if available < 4096 {
		return l, errors.New("evidence packet leaves insufficient bounded peer input space")
	}
	if cap := available / peers; l.OutputBytes > cap {
		l.OutputBytes = cap
	}
	return l, nil
}

type Artifact struct {
	JobHash                  string            `json:"job_hash"`
	InputHash                string            `json:"input_hash"`
	Report                   Report            `json:"report"`
	ValidationReads          []ReadRecord      `json:"validation_reads"`
	Coverage                 []Coverage        `json:"coverage"`
	SourceAccess             *SourceAccess     `json:"source_access,omitempty"`
	ProviderEvidence         *ProviderEvidence `json:"provider_evidence,omitempty"`
	ProviderEvidenceRequired bool              `json:"provider_evidence_required,omitempty"`
}

type Assessment struct {
	Provider Provider `json:"provider"`
	Round    int      `json:"round"`
	Judgment Judgment `json:"judgment"`
}

type Entry struct {
	Finding      Finding      `json:"finding"`
	Origin       Provider     `json:"origin"`
	RaisedRound  int          `json:"raised_round"`
	Status       string       `json:"status"`
	History      []Assessment `json:"history"`
	WithdrawnIDs []string     `json:"withdrawn_ids"`
	RelatedIDs   []string     `json:"related_ids"`
	SeverityMin  int          `json:"severity_min"`
	SeverityMax  int          `json:"severity_max"`
}

type Outcome struct {
	Job                    Job               `json:"job"`
	State                  string            `json:"state"`
	Artifacts              []Artifact        `json:"artifacts"`
	Ledger                 []Entry           `json:"ledger"`
	ExecutedSessions       int               `json:"executed_sessions"`
	ConsumedSessions       int               `json:"consumed_sessions"`
	InvalidPhases          int               `json:"invalid_phases"`
	FailedSourceAccess     *SourceAccess     `json:"failed_source_access,omitempty"`
	FailedProviderEvidence *ProviderEvidence `json:"failed_provider_evidence,omitempty"`
}

type Checkpoint struct {
	JobHash          string     `json:"job_hash"`
	Artifacts        []Artifact `json:"artifacts"`
	ConsumedSessions int        `json:"consumed_sessions"`
}

func (o Outcome) Checkpoint() Checkpoint {
	copies := make([]Artifact, len(o.Artifacts))
	for i, a := range o.Artifacts {
		copies[i] = a
		copies[i].Report.Findings = append([]Finding{}, a.Report.Findings...)
		copies[i].Report.Judgments = append([]Judgment{}, a.Report.Judgments...)
		for n, j := range a.Report.Judgments {
			copies[i].Report.Judgments[n].Evidence = append([]Anchor{}, j.Evidence...)
		}
		copies[i].Report.Retractions = append([]string{}, a.Report.Retractions...)
		copies[i].Report.Coverage = append([]Coverage{}, a.Report.Coverage...)
		copies[i].ValidationReads = append([]ReadRecord{}, a.ValidationReads...)
		copies[i].Coverage = append([]Coverage{}, a.Coverage...)
		copies[i].SourceAccess = cloneSourceAccess(a.SourceAccess)
		copies[i].ProviderEvidence = cloneProviderEvidence(a.ProviderEvidence)
	}
	return Checkpoint{Digest(o.Job), copies, o.ConsumedSessions}
}

// BeforeWork is set by a trusted transport only when no reviewer session began.
// A model-generated error message can never refund the durable session budget.
type PhaseError struct {
	State      string
	BeforeWork bool
}

func (e PhaseError) Error() string { return e.State }

type Reviewer interface {
	Review(context.Context, Session) (Report, error)
}
type Engine struct {
	Reviewers map[Provider]Reviewer
	Reader    func(Snapshot) (*Reader, error)
}

func phaseState(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "incomplete_expired"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, ErrLiveBlocked) {
		return "blocked"
	}
	var phase PhaseError
	if errors.As(err, &phase) {
		switch phase.State {
		case "quota_wait", "auth_failed", "provider_unavailable", "incomplete_expired", "invalid_output":
			return phase.State
		}
	}
	return "provider_unavailable"
}

// ErrSessionBudgetExhausted reports a job whose remaining captured session
// budget can no longer run every phase it still needs. It ends the job before
// any session starts, so no provider usage is spent on a certain incomplete.
var ErrSessionBudgetExhausted = errors.New("captured session budget cannot complete the remaining phases")

// sessionBudgetFits reports whether a job's remaining captured session budget
// can run every phase the engine still plans: one session per provider and
// round, less the validated artifacts it resumes. Every charged session
// without an artifact (an interrupted, failed or shutdown-ended phase) leaves
// the schedule one session short, because the budget equals the schedule.
func sessionBudgetFits(job Job, cp Checkpoint) bool {
	phases := len(job.Config.Providers()) * job.Config.Rounds()
	return job.Config.MaxSessions()-cp.ConsumedSessions >= phases-len(cp.Artifacts)
}

// Run executes a bounded sequential protocol. Valid artifacts can resume only
// the exact captured job, input, configuration, release and snapshot. It performs
// no publication, automatic transport retry or change of selected provider mode.
func (e Engine) Run(ctx context.Context, job Job, checkpoint Checkpoint) (Outcome, error) {
	out := Outcome{Job: job, State: "running", Artifacts: []Artifact{}, Ledger: []Entry{}}
	fail := func(state string, err error) (Outcome, error) { out.State = state; out.Ledger = nil; return out, err }
	if err := job.Validate(); err != nil {
		return fail("invalid_admission", err)
	}
	limits, err := protocolLimits(job)
	if err != nil {
		return fail("invalid_admission", err)
	}
	resume := checkpoint.Artifacts
	if (checkpoint.JobHash != "" && checkpoint.JobHash != Digest(job)) || ((len(resume) > 0 || checkpoint.ConsumedSessions > 0) && checkpoint.JobHash != Digest(job)) || checkpoint.ConsumedSessions < len(resume) || checkpoint.ConsumedSessions > job.Config.MaxSessions() {
		return fail("invalid_output", errors.New("invalid durable session accounting"))
	}
	out.ConsumedSessions = checkpoint.ConsumedSessions
	if len(resume) > job.Config.MaxSessions() {
		return fail("invalid_output", errors.New("too many resume artifacts"))
	}
	cache := map[string]Artifact{}
	for _, a := range resume {
		key := fmt.Sprintf("%s:%d", a.Report.Provider, a.Report.Round)
		if _, exists := cache[key]; exists {
			return fail("invalid_output", errors.New("duplicate resume phase"))
		}
		cache[key] = a
	}
	var phaseKeys []string
	for r := 1; r <= job.Config.Rounds(); r++ {
		for _, p := range job.Config.Providers() {
			phaseKeys = append(phaseKeys, fmt.Sprintf("%s:%d", p, r))
		}
	}
	for _, key := range phaseKeys[:len(resume)] {
		if _, ok := cache[key]; !ok {
			return fail("invalid_output", errors.New("resume artifacts are not a valid phase prefix"))
		}
	}
	// Start no phase of a schedule the remaining budget cannot finish.
	if !sessionBudgetFits(job, checkpoint) {
		return fail("incomplete_expired", ErrSessionBudgetExhausted)
	}
	var reports []Report
	for round := 1; round <= job.Config.Rounds(); round++ {
		var peers []Report
		if round > 1 {
			peers = append([]Report(nil), reports...)
		}
		var focus []string
		if round == 3 {
			for _, entry := range buildLedger(reports, job.Config.Mode) {
				if entry.Status == "disputed" || entry.Status == "unverified" {
					focus = append(focus, entry.Finding.ID)
				}
			}
			sort.Strings(focus)
		}
		var current []Report
		for _, provider := range job.Config.Providers() {
			if err := ctx.Err(); err != nil {
				return fail("cancelled", err)
			}
			// A shutdown drain lets only the in-flight phase finish.
			if shutdownDraining(ctx) {
				return fail("cancelled", context.Canceled)
			}
			// Serialization gives each invocation its own frozen peer data.
			peerBytes, _ := json.Marshal(peers)
			var frozen []Report
			if err := json.Unmarshal(peerBytes, &frozen); err != nil {
				return fail("invalid_output", err)
			}
			session, err := NewSession(job.ID, job.Snapshot, job.Config.Hash(), provider, round, job.Packet, frozen, focus, limits)
			if err != nil {
				return fail("invalid_output", err)
			}
			phase, cancel := context.WithTimeout(ctx, job.Config.Limits.Timeout)
			defer cancel()
			key := fmt.Sprintf("%s:%d", provider, round)
			artifact, reused := cache[key]
			if reused {
				if artifact.JobHash != Digest(job) || artifact.InputHash != Digest(session) {
					return fail("invalid_output", errors.New("resume artifact differs from captured job/input"))
				}
				delete(cache, key)
			} else {
				if out.ConsumedSessions >= job.Config.MaxSessions() {
					return fail("incomplete_expired", errors.New("captured session budget exhausted"))
				}
				reviewer := e.Reviewers[provider]
				if reviewer == nil {
					return fail("provider_unavailable", errors.New("required provider unavailable"))
				}
				out.ExecutedSessions++
				out.ConsumedSessions++
				report, access, providerEvidence, providerRequired, callErr := reviewPhase(phase, reviewer, session, "")
				deadlineErr := phase.Err()
				if callErr != nil {
					out.FailedSourceAccess = cloneSourceAccess(access)
					out.FailedProviderEvidence = cloneProviderEvidence(providerEvidence)
					if phaseState(callErr) == "invalid_output" {
						out.InvalidPhases++
					}
					var phaseErr PhaseError
					if errors.As(callErr, &phaseErr) && phaseErr.BeforeWork {
						out.ConsumedSessions--
					}
					return fail(phaseState(callErr), callErr)
				}
				if deadlineErr != nil {
					out.FailedSourceAccess = cloneSourceAccess(access)
					out.FailedProviderEvidence = cloneProviderEvidence(providerEvidence)
					return fail(phaseState(deadlineErr), deadlineErr)
				}
				artifact = Artifact{JobHash: Digest(job), InputHash: Digest(session), Report: report, ValidationReads: []ReadRecord{}, Coverage: []Coverage{}, SourceAccess: access, ProviderEvidence: cloneProviderEvidence(providerEvidence), ProviderEvidenceRequired: providerRequired}
			}
			if _, required := e.Reviewers[provider].(AuditedReviewer); required && artifact.ProviderEvidence == nil {
				out.InvalidPhases++
				return fail("invalid_output", ErrProviderAuditPending)
			}
			if artifact.ProviderEvidenceRequired || artifact.ProviderEvidence != nil {
				if err := artifact.ProviderEvidence.validateCompletedCaptured(provider, session.Limits, Digest(session)); err != nil {
					out.InvalidPhases++
					return fail("invalid_output", err)
				}
			}
			if err := artifact.SourceAccess.validate(session); err != nil {
				out.InvalidPhases++
				return fail("invalid_output", err)
			}
			encoded, err := json.Marshal(artifact.Report)
			if err != nil {
				out.InvalidPhases++
				return fail("invalid_output", err)
			}
			validated, err := DecodeStrict[Report](encoded, session.Limits.OutputBytes)
			if err == nil {
				err = ValidateReport(validated, session)
			}
			if err != nil {
				out.InvalidPhases++
				return fail("invalid_output", err)
			}
			var reader *Reader
			if e.Reader != nil {
				reader, err = e.Reader(job.Snapshot)
				if err != nil {
					return fail("invalid_output", err)
				}
			}
			if err := (Adapter{Reader: reader}).verifyReport(phase, session, validated); err != nil {
				if phase.Err() != nil {
					return fail(phaseState(phase.Err()), phase.Err())
				}
				out.InvalidPhases++
				return fail("invalid_output", err)
			}
			validated, err = mergeReaderCoverage(validated, reader)
			if err != nil {
				return fail("invalid_output", err)
			}
			if artifact.SourceAccess != nil {
				validated, err = mergeCoverageRecords(validated, artifact.SourceAccess.Coverage)
				if err != nil {
					return fail("invalid_output", err)
				}
			}
			encoded, err = json.Marshal(validated)
			if err != nil || len(encoded) > session.Limits.OutputBytes {
				return fail("invalid_output", errors.New("report with authoritative coverage exceeds output bound"))
			}
			if err := phase.Err(); err != nil {
				return fail(phaseState(err), err)
			}
			cancel()
			artifact.Report = validated
			if reader != nil {
				artifact.ValidationReads, _ = reader.Evidence()
				if !reused {
					_, artifact.Coverage = reader.Evidence()
				}
			}
			out.Artifacts = append(out.Artifacts, artifact)
			current = append(current, validated)
		}
		reports = append(reports, current...)
	}
	if len(cache) > 0 {
		return fail("invalid_output", errors.New("resume artifacts outside selected phases"))
	}
	out.Ledger = buildLedger(reports, job.Config.Mode)
	out.State = "dual_complete"
	if len(job.Config.Providers()) == 1 {
		out.State = "single_complete"
	}
	return out, nil
}

func buildLedger(reports []Report, mode Mode) []Entry {
	entries := map[string]*Entry{}
	latest := map[string]Judgment{}
	retracted := map[string]bool{}
	for _, report := range reports {
		for _, finding := range report.Findings {
			f := finding
			entries[f.ID] = &Entry{Finding: f, Origin: report.Provider, RaisedRound: report.Round, Status: "unverified", History: []Assessment{}, RelatedIDs: []string{}, SeverityMin: f.Severity, SeverityMax: f.Severity}
		}
		for _, j := range report.Judgments {
			if entry := entries[j.FindingID]; entry != nil {
				entry.History = append(entry.History, Assessment{report.Provider, report.Round, j})
				if report.Provider != entry.Origin {
					prior := latest[j.FindingID]
					if prior.Verdict != "reject" || j.Verdict == "confirm" || j.Verdict == "reject" {
						latest[j.FindingID] = j
					}
				}
			}
		}
		for _, id := range report.Retractions {
			retracted[id] = true
			if entry := entries[id]; entry != nil {
				entry.History = append(entry.History, Assessment{report.Provider, report.Round, Judgment{FindingID: id, Verdict: "retracted", Reason: "Origin withdrew this allegation", Evidence: []Anchor{}}})
			}
		}
	}
	for id, entry := range entries {
		if retracted[id] {
			entry.Status = "retracted"
			continue
		}
		if mode != Adversarial {
			continue
		}
		if j, ok := latest[id]; ok {
			switch j.Verdict {
			case "confirm":
				entry.Status = "corroborated"
			case "reject":
				entry.Status = "disputed"
			}
		}
	}
	// Link exact duplicate allegations without discarding identities or objections.
	for id, a := range entries {
		for other, b := range entries {
			if id == other || a.Finding.Title != b.Finding.Title || a.Finding.Body != b.Finding.Body || a.Finding.Anchor != b.Finding.Anchor {
				continue
			}
			a.RelatedIDs = append(a.RelatedIDs, other)
			if !retracted[id] && !retracted[other] && b.Finding.Severity < a.SeverityMin {
				a.SeverityMin = b.Finding.Severity
			}
			if !retracted[id] && !retracted[other] && b.Finding.Severity > a.SeverityMax {
				a.SeverityMax = b.Finding.Severity
			}
			if mode == Adversarial && !retracted[id] && !retracted[other] && a.SeverityMin != a.SeverityMax {
				a.Status = "disputed"
			}
		}
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		sort.Strings(entry.RelatedIDs)
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SeverityMin != out[j].SeverityMin {
			return out[i].SeverityMin < out[j].SeverityMin
		}
		return strings.Compare(out[i].Finding.ID, out[j].Finding.ID) < 0
	})
	return out
}

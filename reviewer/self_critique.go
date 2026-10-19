package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ControlSession belongs only to the offline benchmark. It is deliberately
// distinct from production peer rounds and has no publication capability.
type ControlSession struct {
	Job      Job
	Provider Provider
	Step     int
	Limits   Limits
	History  []ControlReport
}
type ControlReport struct {
	InputHash, JobHash string
	Step               int
	Findings           []Finding
	Judgments          []Judgment
	Retractions        []string
	Coverage           []Coverage
	Usage              Usage
}
type ControlReviewer interface {
	Critique(context.Context, ControlSession) (ControlReport, error)
}
type ControlOutcome struct {
	State            string
	Reports          []ControlReport
	Ledger           []Entry
	ConsumedSessions int
}

// RunSelfCritique executes an initial pass plus three self-critiques, using one
// pinned provider, one immutable job and frozen earlier outputs. It never uses
// the peer provider, retries, invokes a judge or converts self-support into peer
// corroboration. Failure returns evidence but no substantive partial ledger.
func RunSelfCritique(ctx context.Context, job Job, provider Provider, worker ControlReviewer, source func(Snapshot) (*Reader, error)) (ControlOutcome, error) {
	out := ControlOutcome{State: "incomplete", Reports: []ControlReport{}, Ledger: []Entry{}}
	fail := func(err error) (ControlOutcome, error) {
		out.State = "invalid_output"
		var pe PhaseError
		if errors.As(err, &pe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLiveBlocked) {
			out.State = phaseState(err)
		}
		out.Ledger = nil
		return out, err
	}
	if err := job.Validate(); err != nil {
		return fail(err)
	}
	if (provider == Codex && job.Config.Mode != CodexOnly) || (provider == Claude && job.Config.Mode != ClaudeOnly) || !provider.Valid() || worker == nil {
		return fail(errors.New("control requires its explicit single-provider job"))
	}
	limits := job.Config.Limits
	packet, _ := json.Marshal(job.Packet)
	available := limits.InputBytes - len(packet) - (64 << 10)
	if available < 4096 {
		return fail(errors.New("control packet leaves no history budget"))
	}
	if len(packet) > EvaluationPacketBytes || limits.InputBytes != DefaultLimits().InputBytes || limits.OutputBytes > EvaluationOutputBytes || limits.OutputBytes > available/3 {
		return fail(errors.New("control must honor the common frozen evaluation byte limits"))
	}
	entries := map[string]*Entry{}
	var order []string
	used := map[string]bool{}
	for step := 1; step <= 4; step++ {
		historyRaw, _ := json.Marshal(out.Reports)
		var history []ControlReport
		if err := json.Unmarshal(historyRaw, &history); err != nil {
			return fail(err)
		}
		input := ControlSession{job, provider, step, limits, history}
		encoded, _ := json.Marshal(input)
		if len(encoded) > limits.InputBytes-1024 {
			return fail(errors.New("control session exceeds aggregate input"))
		}
		phase, cancel := context.WithTimeout(ctx, limits.Timeout)
		out.ConsumedSessions++
		report, err := worker.Critique(phase, input)
		if err != nil {
			cancel()
			var pe PhaseError
			if errors.As(err, &pe) && pe.BeforeWork {
				out.ConsumedSessions--
			}
			return fail(errors.Join(PhaseError{State: phaseState(err)}, err))
		}
		if err = phase.Err(); err != nil {
			cancel()
			return fail(err)
		}
		if step == 4 && len(report.Findings) > 0 {
			cancel()
			return fail(errors.New("final self-critique cannot introduce unexamined allegations"))
		}
		raw, err := json.Marshal(report)
		if err == nil {
			report, err = DecodeStrict[ControlReport](raw, limits.OutputBytes)
		}
		if err != nil {
			cancel()
			return fail(err)
		}
		if report.InputHash != Digest(input) || report.JobHash != Digest(job) || report.Step != step {
			cancel()
			return fail(errors.New("control report provenance mismatch"))
		}
		// Reuse production independent report/source validation for new allegations.
		session, err := NewSession(job.ID, job.Snapshot, job.Config.Hash(), provider, 1, job.Packet, nil, nil, limits)
		if err != nil {
			cancel()
			return fail(err)
		}
		initial := Report{JobID: session.JobID, PolicyVersion: session.PolicyVersion, PacketHash: session.PacketHash, Provider: provider, Round: 1, SnapshotHash: session.SnapshotHash, ConfigHash: session.ConfigHash, Findings: report.Findings, Judgments: []Judgment{}, Retractions: []string{}, Coverage: report.Coverage, Usage: report.Usage}
		if err = ValidateReport(initial, session); err != nil {
			cancel()
			return fail(err)
		}
		var reader *Reader
		if source != nil {
			reader, err = source(job.Snapshot)
			if err != nil {
				cancel()
				return fail(err)
			}
		}
		if err = (Adapter{Reader: reader}).verifyReport(phase, session, initial); err != nil {
			cancel()
			return fail(err)
		}
		dispositions := map[string]bool{}
		var anchors []Anchor
		for _, judgment := range report.Judgments {
			entry := entries[judgment.FindingID]
			if entry == nil || entry.Status == "retracted" || dispositions[judgment.FindingID] || len(judgment.Reason) == 0 || len(judgment.Reason) > 4096 {
				cancel()
				return fail(errors.New("invalid self-critique target"))
			}
			switch judgment.Verdict {
			case "confirm", "reject":
				if len(judgment.Evidence) == 0 {
					cancel()
					return fail(errors.New("self-critique requires source evidence"))
				}
			case "cannot_verify":
			default:
				cancel()
				return fail(errors.New("invalid self-critique disposition"))
			}
			if len(judgment.Evidence) > 8 {
				cancel()
				return fail(errors.New("control evidence cap"))
			}
			for _, a := range judgment.Evidence {
				if !validAnchor(a, job.Snapshot) {
					cancel()
					return fail(errors.New("invalid control source anchor"))
				}
				anchors = append(anchors, a)
			}
			dispositions[judgment.FindingID] = true
		}
		retracted := map[string]bool{}
		for _, id := range report.Retractions {
			entry := entries[id]
			if entry == nil || entry.Status == "retracted" || retracted[id] || dispositions[id] {
				cancel()
				return fail(errors.New("invalid control withdrawal"))
			}
			retracted[id] = true
		}
		for id, entry := range entries {
			if entry.Status != "retracted" && !dispositions[id] && !retracted[id] {
				cancel()
				return fail(fmt.Errorf("missing self-critique for %s", id))
			}
		}
		if len(anchors) > 0 {
			if reader == nil {
				cancel()
				return fail(errors.New("source reader required"))
			}
			if err = reader.VerifyAll(phase, anchors); err != nil {
				cancel()
				return fail(err)
			}
		}
		for _, finding := range report.Findings {
			if used[finding.ID] {
				cancel()
				return fail(errors.New("reused control finding ID"))
			}
			used[finding.ID] = true
		}
		initial, err = mergeReaderCoverage(initial, reader)
		if err != nil {
			cancel()
			return fail(err)
		}
		report.Coverage = initial.Coverage
		raw, _ = json.Marshal(report)
		if len(raw) > limits.OutputBytes {
			cancel()
			return fail(errors.New("control output plus coverage exceeds cap"))
		}
		if err = phase.Err(); err != nil {
			cancel()
			return fail(err)
		}
		cancel()
		for _, finding := range report.Findings {
			entries[finding.ID] = &Entry{Finding: finding, Origin: provider, RaisedRound: step, Status: "unverified", History: []Assessment{}, RelatedIDs: []string{}, SeverityMin: finding.Severity, SeverityMax: finding.Severity}
			order = append(order, finding.ID)
		}
		for _, judgment := range report.Judgments {
			entry := entries[judgment.FindingID]
			entry.History = append(entry.History, Assessment{provider, step, judgment})
			if judgment.Verdict == "reject" {
				entry.Status = "disputed"
			}
		}
		for id := range retracted {
			entries[id].Status = "retracted"
		}
		out.Reports = append(out.Reports, report)
	}
	for _, id := range order {
		out.Ledger = append(out.Ledger, *entries[id])
	}
	out.State = "complete"
	return out, nil
}

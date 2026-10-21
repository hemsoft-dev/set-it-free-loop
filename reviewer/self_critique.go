package reviewer

import (
	"context"
	"encoding/json"
	"errors"
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
	if worker == nil {
		return fail(errors.New("control requires its explicit single-provider job"))
	}
	state := newControlLedger()
	for step := 1; step <= 4; step++ {
		historyRaw, _ := json.Marshal(out.Reports)
		var history []ControlReport
		if err := json.Unmarshal(historyRaw, &history); err != nil {
			return fail(err)
		}
		input := ControlSession{job, provider, step, job.Config.Limits, history}
		if err := ValidateControlSession(input); err != nil {
			return fail(err)
		}
		phase, cancel := context.WithTimeout(ctx, input.Limits.Timeout)
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
		if err = state.check(input, report); err != nil {
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
		if err = verifyControlSource(phase, input, report, reader); err != nil {
			cancel()
			return fail(err)
		}
		report, err = mergeControlCoverage(input, report, nil, reader)
		if err != nil {
			cancel()
			return fail(err)
		}
		if err = phase.Err(); err != nil {
			cancel()
			return fail(err)
		}
		cancel()
		state.apply(input, report)
		out.Reports = append(out.Reports, report)
	}
	for _, id := range state.order {
		out.Ledger = append(out.Ledger, *state.entries[id])
	}
	out.State = "complete"
	return out, nil
}

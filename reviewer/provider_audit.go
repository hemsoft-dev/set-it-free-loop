package reviewer

import (
	"context"
	"errors"
)

var ErrProviderAuditPending = errors.New("provider evidence requires durable stopped-attempt recovery")

// ProviderEvidence is a supervisor-owned receipt and its captured routing
// policy. Report JSON cannot populate this channel. Nil is unknown evidence,
// never a measured zero-request phase. Policies contain no credential values.
type ProviderEvidence struct {
	Policy EgressPolicy   `json:"policy"`
	Access ProviderAccess `json:"access"`
}

type ProviderAttempt struct {
	ID       string
	Evidence *ProviderEvidence
}

// AuditedReviewer supplies private supervisor evidence independently of model
// output. Implementing this interface requires a stopped provider receipt on
// success. Legacy Reviewer/SourceReviewer paths supply unknown provider usage.
type AuditedReviewer interface {
	ReviewAudited(context.Context, Session) (Report, *SourceAccess, *ProviderEvidence, error)
}

// Only audited inner reviewers expose this mandatory evidence interface to the
// outer Engine. The reservation and persistence implementation remains shared.
type durableAuditedReviewer struct{ DurableReviewer }

func (r durableAuditedReviewer) ReviewAudited(ctx context.Context, s Session) (Report, *SourceAccess, *ProviderEvidence, error) {
	var source *SourceAccess
	var evidence *ProviderEvidence
	report, err := r.review(ctx, s, func(a *SourceAccess, p *ProviderEvidence, _ bool) {
		source = cloneSourceAccess(a)
		evidence = cloneProviderEvidence(p)
	})
	return report, source, evidence, err
}

func (e *ProviderEvidence) validateCompletedCaptured(provider Provider, limits Limits, inputHash string) error {
	if e == nil {
		return ErrProviderAuditPending
	}
	return e.Access.validateCompletedCaptured(provider, limits, inputHash, e.Policy)
}

// attemptAuditedReviewer binds the durable attempt ID into its private
// stopped launch receipt. A resumed job's retry can repeat the session hash
// and stopped evidence, so only that ID ties a launch to the current lease.
type attemptAuditedReviewer interface {
	AuditedReviewer
	reviewAttemptAudited(context.Context, Session, string) (Report, *SourceAccess, *ProviderEvidence, error)
}

// reviewPhase passes attemptID, when known, to an attempt-binding reviewer.
func reviewPhase(ctx context.Context, reviewer Reviewer, s Session, attemptID string) (Report, *SourceAccess, *ProviderEvidence, bool, error) {
	worker, required := reviewer.(AuditedReviewer)
	if !required {
		r, source, err := reviewSource(ctx, reviewer, s)
		return r, source, nil, false, err
	}
	var r Report
	var source *SourceAccess
	var evidence *ProviderEvidence
	var err error
	if bound, ok := reviewer.(attemptAuditedReviewer); ok && attemptID != "" {
		r, source, evidence, err = bound.reviewAttemptAudited(ctx, s, attemptID)
	} else {
		r, source, evidence, err = worker.ReviewAudited(ctx, s)
	}
	r, source, err = sourceReviewResult(r, source, err, s)
	evidence = cloneProviderEvidence(evidence)
	var pe PhaseError
	before := errors.As(err, &pe) && pe.BeforeWork
	if evidence == nil {
		if before && !errors.Is(err, ErrWorkerStopPending) && !errors.Is(err, ErrProviderEgressPending) && !errors.Is(err, ErrProviderAuditPending) {
			return Report{}, source, nil, true, err
		}
		return Report{}, source, nil, true, errors.Join(err, ErrProviderAuditPending)
	}
	if before && evidence.Access.Requests > 0 {
		stopPending := errors.Is(err, ErrWorkerStopPending)
		err = errors.Join(workerLauncherError(err, false), ErrProviderEgressFailed)
		if stopPending {
			err = errors.Join(err, ErrWorkerStopPending)
		}
	}
	if auditErr := evidence.validateStoppedCaptured(s.Identity.Provider, s.Limits, Digest(s)); auditErr != nil {
		return Report{}, source, evidence, true, errors.Join(err, ErrProviderAuditPending, auditErr)
	}
	if err == nil {
		if auditErr := evidence.Access.ValidateCompleted(s, evidence.Policy); auditErr != nil {
			return Report{}, source, evidence, true, adapterValidation(auditErr, false)
		}
	}
	return r, source, evidence, true, err
}

func cloneProviderEvidence(e *ProviderEvidence) *ProviderEvidence {
	if e == nil {
		return nil
	}
	copy := *e
	copy.Policy.Routes = append([]EgressRoute{}, e.Policy.Routes...)
	copy.Policy.Headers = append([]string{}, e.Policy.Headers...)
	return &copy
}

func validateArtifactProviderEvidence(j *DurableJob, a Artifact) error {
	auditedProvider := false
	for _, provider := range j.AuditedProviders {
		if provider == a.Report.Provider {
			auditedProvider = true
			break
		}
	}
	for _, attempt := range j.ProviderAttempts {
		if attempt.Evidence != nil && attempt.Evidence.Policy.Provider == a.Report.Provider {
			auditedProvider = true
			break
		}
	}
	if !a.ProviderEvidenceRequired && a.ProviderEvidence == nil && !auditedProvider {
		return nil
	}
	limits, err := protocolLimits(j.Job)
	if err != nil {
		return err
	}
	if err = a.ProviderEvidence.validateCompletedCaptured(a.Report.Provider, limits, a.InputHash); err != nil {
		return err
	}
	for _, attempt := range j.ProviderAttempts {
		if attempt.ID != "" && attempt.Evidence != nil && attempt.Evidence.Access.SessionHash == a.InputHash && Digest(attempt.Evidence) == Digest(a.ProviderEvidence) {
			return nil
		}
	}
	return ErrProviderAuditPending
}

// validateStoppedCaptured accepts stopped failed/empty receipts for recovery.
// ValidateCompleted remains mandatory before accepting a substantive report.
// A bounded overrun byte is retained honestly on failures, not truncated away.
func (e *ProviderEvidence) validateStoppedCaptured(provider Provider, limits Limits, inputHash string) error {
	if e == nil {
		return ErrProviderAuditPending
	}
	if _, err := validateEgressPolicy(provider, limits, e.Policy); err != nil {
		return err
	}
	a, l := e.Access, e.Policy.Limits
	if !a.Stopped {
		return ErrProviderEgressPending
	}
	if len(inputHash) != 64 || !objectID.MatchString(inputHash) || a.Version != 1 || a.SessionHash != inputHash || a.PolicyHash != Digest(e.Policy) ||
		a.Requests < 0 || a.Requests > l.Requests || a.InferenceRequests < 0 || a.InferenceRequests > l.InferenceRequests ||
		a.RefreshRequests < 0 || a.RefreshRequests > l.RefreshRequests || a.MetadataRequests < 0 || a.MetadataRequests > l.MetadataRequests ||
		a.NegotiationRequests < 0 || a.NegotiationRequests > l.NegotiationRequests || a.NegotiationResponses < 0 || a.NegotiationResponses > a.NegotiationRequests ||
		a.Requests != a.InferenceRequests+a.RefreshRequests+a.MetadataRequests+a.NegotiationRequests ||
		a.Forwarded < 0 || a.Forwarded > a.Requests-a.NegotiationRequests || a.Responses < 0 || a.Responses > a.Forwarded ||
		a.RequestBytes < 0 || a.ResponseBytes < 0 || a.RequestBytes > l.TotalBytes+1 || a.ResponseBytes > l.TotalBytes+1-a.RequestBytes ||
		a.RequestBytes > a.Requests*(l.RequestBytes+1) || a.ResponseBytes > a.Forwarded*(l.ResponseBytes+1) || a.Exhausted && !a.Failed ||
		a.RouteDenials < 0 || a.ResponseDenials < 0 || a.ResponseDenials > a.Forwarded || (a.RouteDenials > 0 || a.ResponseDenials > 0) && !a.Failed ||
		// Each request denial is a reserved inference that was never forwarded.
		a.RequestDenials < 0 || a.RequestDenials > a.InferenceRequests || a.RequestDenials > a.Requests-a.NegotiationRequests-a.Forwarded || a.RequestDenials > 0 && (!a.Failed || e.Policy.RequestPolicy == "") {
		return ErrProviderEgressFailed
	}
	return nil
}

// RecordStoppedProviderEvidence is an idempotent recovery write for one exact
// reserved attempt. It starts no work, refunds no usage and releases no lease.
// Failed or zero-inference receipts remain private evidence; they do not prove
// a completed review. An unstopped bridge cannot be retired through this API.
func (s *Store) RecordStoppedProviderEvidence(ctx context.Context, id, token, attemptID string, evidence *ProviderEvidence) error {
	return s.update(ctx, func(d *DurableState) error {
		j := d.Jobs[id]
		if j == nil || token == "" || j.LeaseToken != token || !j.Outstanding || attemptID == "" || j.AttemptID != attemptID || evidence == nil {
			return ErrFenced
		}
		limits, err := protocolLimits(j.Job)
		if err != nil {
			return err
		}
		if err = evidence.validateStoppedCaptured(j.AttemptProvider, limits, j.AttemptInputHash); err != nil {
			return err
		}
		for _, old := range j.ProviderAttempts {
			if old.ID == attemptID {
				if Digest(old.Evidence) != Digest(evidence) {
					return ErrFenced
				}
				return nil
			}
		}
		if len(j.ProviderAttempts) >= 128 {
			return errors.New("provider attempt retention cap reached")
		}
		j.ProviderAttempts = append(j.ProviderAttempts, ProviderAttempt{attemptID, cloneProviderEvidence(evidence)})
		return nil
	})
}

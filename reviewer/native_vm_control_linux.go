//go:build linux

package reviewer

import (
	"context"
	"encoding/json"
	"errors"
)

// NativeVMControlReviewer starts one fresh isolated native phase per Critique.
// It shares the review supervisor, auth return, provider proxy and stop proof.
// Construction and denied eligibility checks never read auth or start a VM.
type NativeVMControlReviewer struct{ reviewer *NativeVMReviewer }

func NewNativeVMControlReviewer(o NativeVMOptions) (*NativeVMControlReviewer, error) {
	reviewer, err := NewNativeVMReviewer(o)
	if err != nil {
		return nil, err
	}
	return &NativeVMControlReviewer{reviewer}, nil
}
func (r *NativeVMControlReviewer) Critique(ctx context.Context, s ControlSession) (ControlReport, error) {
	report, _, _, err := r.CritiqueAudited(ctx, s)
	return report, err
}
func (r *NativeVMControlReviewer) CritiqueAudited(ctx context.Context, s ControlSession) (ControlReport, *SourceAccess, *ProviderEvidence, error) {
	blocked := errors.Join(PhaseError{State: "blocked", BeforeWork: true}, ErrLiveBlocked)
	if r == nil || r.reviewer == nil {
		return ControlReport{}, nil, nil, blocked
	}
	// Copy the full captured input before checking it or invoking trusted gates.
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > s.Limits.InputBytes {
		return ControlReport{}, nil, nil, blocked
	}
	var captured ControlSession
	if json.Unmarshal(raw, &captured) != nil {
		return ControlReport{}, nil, nil, blocked
	}
	capability, err := controlCapability(captured)
	if err != nil {
		return ControlReport{}, nil, nil, blocked
	}
	report, access, evidence, err := r.reviewer.reviewTaskAudited(ctx, capability, &captured)
	if err != nil {
		return ControlReport{}, access, evidence, err
	}
	if report.control == nil {
		return ControlReport{}, access, evidence, adapterValidation(ErrVMBoot, false)
	}
	return *report.control, access, evidence, nil
}

var _ AuditedControlReviewer = (*NativeVMControlReviewer)(nil)

type nativeVMTaskReport struct {
	review  Report
	control *ControlReport
}

func (r nativeVMTaskReport) value() any {
	if r.control != nil {
		return *r.control
	}
	return r.review
}
func nativeControlMatches(control *ControlSession, s Session) bool {
	if control == nil {
		return true
	}
	capability, err := controlCapability(*control)
	return err == nil && Digest(capability) == Digest(s)
}
func decodeNativeVMTaskReport(q nativeVMRequest, raw []byte) (nativeVMTaskReport, error) {
	if err := q.validateReport(raw); err != nil {
		return nativeVMTaskReport{}, err
	}
	if q.Control != nil {
		report, err := DecodeStrict[ControlReport](raw, q.Session.Limits.OutputBytes)
		return nativeVMTaskReport{control: &report}, err
	}
	report, err := DecodeStrict[Report](raw, q.Session.Limits.OutputBytes)
	return nativeVMTaskReport{review: report}, err
}
func validateNativeVMTaskSource(ctx context.Context, q nativeVMRequest, report nativeVMTaskReport, access *SourceAccess, reader *Reader) (nativeVMTaskReport, error) {
	invalid := adapterValidation(ErrSourceService, false)
	if q.Control != nil {
		if report.control == nil || verifyControlSource(ctx, *q.Control, *report.control, reader) != nil {
			return nativeVMTaskReport{}, invalid
		}
		control, err := mergeControlCoverage(*q.Control, *report.control, access.Coverage, reader)
		if err != nil {
			return nativeVMTaskReport{}, invalid
		}
		return nativeVMTaskReport{control: &control}, nil
	}
	if report.control != nil || (Adapter{Reader: reader}).verifyReport(ctx, q.Session, report.review) != nil {
		return nativeVMTaskReport{}, invalid
	}
	merged, err := mergeCoverageRecords(report.review, access.Coverage)
	if err == nil {
		merged, err = mergeReaderCoverage(merged, reader)
	}
	raw, encodeErr := json.Marshal(merged)
	if err != nil || encodeErr != nil || len(raw) > q.Session.Limits.OutputBytes || ValidateReport(merged, q.Session) != nil {
		return nativeVMTaskReport{}, invalid
	}
	return nativeVMTaskReport{review: merged}, nil
}

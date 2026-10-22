package reviewer

import "context"

// Frozen reports were produced in separate bounded phases. Verify each against
// its own same-snapshot allowance, preserving the current phase's reader budget.
func verifyFrozenReports(ctx context.Context, session Session, current *Reader) error {
	for _, report := range session.PeerReports {
		var verifier *Reader
		if current != nil {
			current.mu.Lock()
			store, snapshot := current.store, current.snapshot
			paths := make([]string, 0, len(current.paths))
			for path := range current.paths {
				paths = append(paths, path)
			}
			current.mu.Unlock()
			if snapshot != session.Snapshot {
				return ErrSourceService
			}
			var err error
			verifier, err = NewReader(store, snapshot, paths, session.Limits)
			if err != nil {
				return err
			}
		}
		if err := (Adapter{Reader: verifier}).verifyReport(ctx, session, report); err != nil {
			return err
		}
	}
	return nil
}

//go:build linux || darwin

package reviewer

import (
	"context"
	_ "embed"
	"testing"
)

// Captured from unmodified Claude 2.1.303 against the isolated synthetic
// Messages backend. Only the random event/session identifiers were replaced.
//
//go:embed testdata/claude-2.1.303-rate-limit.json
var capturedClaudeRateLimit []byte

func TestClaudeSourceAcceptsCapturedRateMetadata(t *testing.T) {
	a, l, _ := claudeSourceFixture(t, "source-rate-allowed")
	report, err := a.Review(context.Background(), testSession(Claude))
	if err != nil || report.JobID != testSession(Claude).JobID {
		t.Fatal(report, err)
	}
	assertFixtureStopped(t, &l.fixtureLauncher)
}
func TestClaudeSourceRejectsOverageUnknownRateAndPrematureMetadata(t *testing.T) {
	for _, mode := range []string{"overage", "missing-overage", "wrong-type", "rejected", "missing-info", "before-init"} {
		t.Run(mode, func(t *testing.T) {
			a, l, _ := claudeSourceFixture(t, "source-rate-"+mode)
			report, err := a.Review(context.Background(), testSession(Claude))
			if err == nil || report.JobID != "" {
				t.Fatal("unsafe metadata accepted", report, err)
			}
			assertFixtureStopped(t, &l.fixtureLauncher)
		})
	}
}

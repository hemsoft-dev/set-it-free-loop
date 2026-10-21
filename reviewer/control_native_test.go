package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func controlInputFixture(p Provider) ControlSession {
	job := testJob()
	job.Config.Mode = CodexOnly
	if p == Claude {
		job.Config.Mode = ClaudeOnly
	}
	job.Config.Limits.OutputBytes = EvaluationOutputBytes
	return ControlSession{Job: job, Provider: p, Step: 1, Limits: job.Config.Limits, History: []ControlReport{}}
}
func nextControlInput(s ControlSession) ControlSession {
	s.History = append(s.History, fakeControl(s))
	s.Step++
	return s
}
func TestControlLineageAndCapabilityCannotBeSubstituted(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		s := controlInputFixture(p)
		previous := ""
		for step := 1; step <= 4; step++ {
			capability, err := ControlCapabilitySession(s)
			if err != nil || capability.Packet != s.Job.Packet || capability.Round != 1 || len(capability.PeerReports) != 0 || capability.JobID == previous {
				t.Fatal("lost immutable distinct control capability", step, err)
			}
			previous = capability.JobID
			if err = ValidateControlReport(fakeControl(s), s); err != nil {
				t.Fatal(err)
			}
			if step < 4 {
				s = nextControlInput(s)
			}
		}
		for name, change := range map[string]func(*ControlSession){
			"prior-hash":          func(s *ControlSession) { s.History[0].InputHash = Digest("wrong") },
			"prior-job":           func(s *ControlSession) { s.History[0].JobHash = Digest("wrong") },
			"prior-step":          func(s *ControlSession) { s.History[1].Step = 1 },
			"missing-disposition": func(s *ControlSession) { s.History[1].Judgments = []Judgment{} },
			"changed-limits":      func(s *ControlSession) { s.Limits.OutputBytes-- },
			"missing-history":     func(s *ControlSession) { s.History = s.History[:2] },
			"wrong-mode":          func(s *ControlSession) { s.Job.Config.Mode = Adversarial },
		} {
			t.Run(string(p)+"/"+name, func(t *testing.T) {
				raw, _ := json.Marshal(s)
				var bad ControlSession
				json.Unmarshal(raw, &bad)
				change(&bad)
				if ValidateControlSession(bad) == nil {
					t.Fatal("invalid frozen lineage accepted")
				}
			})
		}
	}
}
func TestControlInvocationAndNativeParserKeepTypedInput(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		s := nextControlInput(controlInputFixture(p))
		inv, err := (Adapter{Provider: p, AuthDirectory: "/synthetic/profile"}).ControlInvocation(s, nil)
		if err != nil {
			t.Fatal(err)
		}
		capability, err := nativeSession(inv)
		expected, _ := ControlCapabilitySession(s)
		if err != nil || Digest(capability) != Digest(expected) || !bytes.Equal(inv.ReadOnlyFiles["sfl-report.schema.json"], ControlReportSchema()) {
			t.Fatal("control task lost", err)
		}
		_, raw, ok := bytes.Cut(inv.Input, []byte{'\n'})
		want, _ := json.Marshal(s)
		if !ok || !bytes.Equal(raw, want) || bytes.Count(inv.Input, want) != 1 || !strings.Contains(string(inv.Input), Digest(s)) {
			t.Fatal("control prompt changed canonical input")
		}
		inv.Control = &ControlSession{}
		if _, err = nativeSession(inv); err == nil {
			t.Fatal("foreign captured input accepted")
		}
	}
	var schema map[string]any
	json.Unmarshal(ControlReportSchema(), &schema)
	properties := schema["properties"].(map[string]any)
	if properties["InputHash"] == nil || properties["Findings"] == nil || properties[""] != nil || properties["job_id"] != nil {
		t.Fatal("control schema differs from preserved JSON names")
	}
}
func controlEnvelope(s ControlSession, r any) []byte {
	raw, _ := json.Marshal(r)
	if s.Provider == Claude {
		out, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 3, "structured_output": json.RawMessage(raw), "usage": map[string]int{"input_tokens": 7, "output_tokens": 9}})
		return out
	}
	event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(raw)}})
	return []byte("{\"type\":\"turn.started\"}\n" + string(event) + "\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":9}}\n")
}
func TestControlTransportKeepsFailureToolAndUsageBarriers(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		s := controlInputFixture(p)
		r := fakeControl(s)
		r.Usage = Usage{Known: true, InputTokens: 999999, Turns: 99}
		raw := controlEnvelope(s, r)
		got, err := NormalizeControlCLIOutput(s, raw)
		if err != nil || got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 9 || got.Usage.Turns == 99 {
			t.Fatal("model supplied usage accepted", got.Usage, err)
		}
		cap, _ := ControlCapabilitySession(s)
		if _, err = NormalizeCLIOutput(cap, raw); err == nil {
			t.Fatal("control became production report")
		}
		review := fakeReport(cap)
		if _, err = NormalizeControlCLIOutput(s, controlEnvelope(s, review)); err == nil {
			t.Fatal("production report became control")
		}
		r.InputHash = Digest("swapped")
		if _, err = NormalizeControlCLIOutput(s, controlEnvelope(s, r)); err == nil {
			t.Fatal("foreign input accepted")
		}
		if p == Codex {
			bad := append([]byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\"}}\n"), raw...)
			if _, err = NormalizeControlCLIOutput(s, bad); err == nil {
				t.Fatal("control permitted shell")
			}
			if _, err = NormalizeControlCLIOutput(s, append(raw, []byte("{}\n")...)); err == nil {
				t.Fatal("control permitted completion trailer")
			}
		} else {
			failed := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`)
			_, err = NormalizeControlCLIOutput(s, failed)
			var failure PhaseError
			if !errors.As(err, &failure) || failure.State != "quota_wait" || failure.BeforeWork {
				t.Fatal("quota failure refunded or lost", err)
			}
		}
	}
}
func TestControlSharedReducerPreservesDisputeAndRetraction(t *testing.T) {
	s := controlInputFixture(Codex)
	engine := testEngine(t, nil)
	out, err := RunSelfCritique(context.Background(), s.Job, s.Provider, controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
		r := fakeControl(s)
		if s.Step == 2 {
			r.Judgments[0].Verdict = "reject"
		}
		if s.Step == 4 {
			r.Retractions = []string{r.Judgments[0].FindingID}
			r.Judgments = []Judgment{}
		}
		return r, nil
	}), engine.Reader)
	if err != nil || out.State != "complete" || out.Ledger[0].Status != "retracted" || len(out.Ledger[0].History) != 2 {
		t.Fatal(out, err)
	}
}

func TestControlMergedCoverageEnforcesEncodedOutputCapAtEveryStep(t *testing.T) {
	for target := 1; target <= 4; target++ {
		t.Run(strconv.Itoa(target), func(t *testing.T) {
			input := controlInputFixture(Codex)
			input.Job.Config.Limits.OutputBytes = 4096
			input.Limits = input.Job.Config.Limits
			workerCalls, readerCalls := 0, 0
			worker := controlFunc(func(_ context.Context, s ControlSession) (ControlReport, error) {
				workerCalls++
				report := fakeControl(s)
				if s.Step == target {
					if s.Step == 1 {
						report.Findings[0].Body = ""
					} else {
						report.Judgments[0].Reason = ""
					}
					raw, _ := json.Marshal(report)
					padding := strings.Repeat("x", s.Limits.OutputBytes-len(raw)-16)
					if s.Step == 1 {
						report.Findings[0].Body = padding
					} else {
						report.Judgments[0].Reason = padding
					}
					raw, _ = json.Marshal(report)
					if len(raw) != s.Limits.OutputBytes-16 || ValidateControlReport(report, s) != nil {
						t.Fatal("invalid near-bound fixture", len(raw))
					}
					// No reader is needed to demonstrate the same check for source-access
					// coverage. Validation already rejects the merged encoded bytes.
					merged, err := mergeControlCoverage(s, report, []Coverage{{Reason: strings.Repeat("o", 64)}}, nil)
					encoded, _ := json.Marshal(merged)
					if err == nil || len(encoded) <= s.Limits.OutputBytes {
						t.Fatal("source-access coverage did not cross/reject bound", len(encoded), err)
					}
				}
				return report, nil
			})
			source := func(snapshot Snapshot) (*Reader, error) {
				readerCalls++
				reader, err := testEngine(t, nil).Reader(snapshot)
				if err == nil && readerCalls == target {
					if _, readErr := reader.Read(context.Background(), snapshot.Head, "outside-allowlist.go"); readErr == nil {
						t.Fatal("omission fixture did not fail")
					}
				}
				return reader, err
			}
			outcome, err := RunSelfCritique(context.Background(), input.Job, input.Provider, worker, source)
			if err == nil || outcome.State != "invalid_output" || workerCalls != target || outcome.ConsumedSessions != target || len(outcome.Reports) != target-1 || len(outcome.Ledger) != 0 {
				t.Fatal("oversized reader coverage escaped or froze into later history", target, outcome.State, workerCalls, len(outcome.Reports), err)
			}
		})
	}
}

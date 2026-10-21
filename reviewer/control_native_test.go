package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

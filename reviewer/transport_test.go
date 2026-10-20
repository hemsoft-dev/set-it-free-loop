package reviewer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNativeFailureMetadataNeverPromotesOrRefunds(t *testing.T) {
	for _, c := range []struct {
		raw   string
		state string
	}{
		{`{"type":"result","subtype":"success","is_error":true,"api_error_status":401}`, "auth_failed"},
		{`{"type":"result","subtype":"success","is_error":true,"api_error_status":429}`, "quota_wait"},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["quota auth secret-canary"]}`, "provider_unavailable"},
		{`{"type":"result","subtype":"error_max_turns","is_error":true}`, "incomplete_expired"},
		{`{"type":"result","subtype":"error_max_budget_usd","is_error":true}`, "incomplete_expired"},
		{`{"type":"result","subtype":"error_max_structured_output_retries","is_error":true}`, "invalid_output"},
	} {
		_, err := NormalizeCLIOutput(testSession(Claude), []byte(c.raw))
		var failure PhaseError
		if !errors.As(err, &failure) || failure.State != c.state || failure.BeforeWork || strings.Contains(err.Error(), "canary") {
			t.Fatal("failure metadata not retained safely", err)
		}
		if _, err := NormalizeCLIOutput(testSession(Claude), []byte(c.raw+" trailing")); err == nil || errors.As(err, &failure) {
			t.Fatal("classified malformed transport as trusted failure", err)
		}
	}
}

func TestNormalizeOfficialClientEnvelopes(t *testing.T) {
	for _, p := range []Provider{Codex, Claude} {
		t.Run(string(p), func(t *testing.T) {
			s := testSession(p)
			r := fakeReport(s)
			r.Usage = Usage{999, 999, true, 999}
			report, _ := json.Marshal(r)
			var raw []byte
			if p == Codex {
				message, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": string(report)}})
				raw = []byte("{\"type\":\"thread.started\"}\n{\"type\":\"turn.started\"}\n" + string(message) + "\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}\n")
			} else {
				raw, _ = json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": r, "num_turns": 1, "usage": map[string]int{"input_tokens": 3, "output_tokens": 4}})
			}
			got, err := NormalizeCLIOutput(s, raw)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Usage.Known || got.Usage.InputTokens != 3 || got.Usage.OutputTokens != 4 || got.Usage.Turns != 1 {
				t.Fatal("accepted model-reported usage", got.Usage)
			}
		})
	}
}

func TestClientTransportNeverPromotesPartialOrForbiddenOutput(t *testing.T) {
	s := testSession(Codex)
	for _, raw := range []string{
		`{"type":"turn.failed"}`,
		"{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"text\":\"read canary\"}}\n",
		"{\"type\":\"turn.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"mcp_tool_call\",\"server\":\"ambient\",\"tool\":\"shell\"}}\n",
		"{\"type\":\"turn.started\"}\n",
		strings.Repeat("x", s.Limits.OutputBytes+1),
	} {
		if _, err := NormalizeCLIOutput(s, []byte(raw)); err == nil {
			t.Fatal("accepted invalid client stream")
		}
	}
	s = testSession(Claude)
	for _, raw := range []string{`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":1,"structured_output":{}}`, `{"type":"result","subtype":"success","num_turns":1}`, `null`} {
		if _, err := NormalizeCLIOutput(s, []byte(raw)); err == nil {
			t.Fatal("accepted incomplete client result")
		}
	}
}

func TestStrictReportRejectsMissingNullAndDuplicateFields(t *testing.T) {
	s := testSession(Codex)
	r := fakeReport(s)
	raw, _ := json.Marshal(r)
	for _, bad := range []string{
		strings.Replace(string(raw), `"findings":[]`, `"findings":null`, 1),
		strings.Replace(string(raw), `"round":1`, `"round":1,"round":2`, 1),
		strings.Replace(string(raw), `"round":1,`, "", 1),
	} {
		if _, err := DecodeStrict[Report]([]byte(bad), s.Limits.OutputBytes); err == nil {
			t.Fatal("accepted ambiguous or schema-invalid report", bad)
		}
	}
	s = testSession(Claude)
	r = fakeReport(s)
	e, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "structured_output": r})
	if _, err := NormalizeCLIOutput(s, append(e, []byte(" invalid trailing bytes")...)); err == nil {
		t.Fatal("accepted malformed transport tail")
	}
}

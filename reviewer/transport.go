package reviewer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

// ReportSchema gives both pinned clients the same strict response contract.
// The isolated driver must mount this generated file read-only for Codex.
func ReportSchema() []byte {
	var schema func(reflect.Type) map[string]any
	schema = func(t reflect.Type) map[string]any {
		switch t.Kind() {
		case reflect.Struct:
			properties := map[string]any{}
			required := []string{}
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				key := strings.Split(f.Tag.Get("json"), ",")[0]
				properties[key] = schema(f.Type)
				required = append(required, key)
			}
			return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
		case reflect.Slice:
			return map[string]any{"type": "array", "items": schema(t.Elem())}
		case reflect.Bool:
			return map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int64:
			return map[string]any{"type": "integer"}
		default:
			return map[string]any{"type": "string"}
		}
	}
	root := schema(reflect.TypeOf(Report{}))
	data, _ := json.Marshal(root)
	return data
}

type transportUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// NormalizeCLIOutput accepts the pinned clients' documented JSON envelopes,
// discards progress text, and rejects incomplete/error/forbidden-tool results.
// It never publishes raw transport output. The actual isolated driver still
// must enforce resource/tool boundaries before execution, not after this check.
func NormalizeCLIOutput(s Session, raw []byte) (Report, error) {
	if err := s.Validate(); err != nil {
		return Report{}, err
	}
	if len(raw) > s.Limits.OutputBytes {
		return Report{}, errors.New("client transport output exceeds byte bound")
	}
	var result []byte
	var usage Usage
	switch s.Identity.Provider {
	case Codex:
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 1024), s.Limits.OutputBytes)
		completed := false
		turns := 0
		for scanner.Scan() {
			var event struct {
				Type string `json:"type"`
				Item struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Server string `json:"server"`
					Tool   string `json:"tool"`
				} `json:"item"`
				Usage *transportUsage `json:"usage"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return Report{}, errors.New("invalid Codex JSONL event")
			}
			if completed {
				return Report{}, errors.New("unexpected client data after completion")
			}
			switch event.Type {
			case "thread.started":
			case "turn.started":
				turns++
				if turns > 1 {
					return Report{}, errors.New("unexpected additional CLI turn")
				}
			case "item.started", "item.updated", "item.completed":
				switch event.Item.Type {
				case "reasoning":
				case "agent_message":
					if event.Type == "item.completed" {
						result = []byte(event.Item.Text)
					}
				case "mcp_tool_call":
					if event.Item.Server != "sfl-source" || (event.Item.Tool != "read" && event.Item.Tool != "search") {
						return Report{}, errors.New("unapproved MCP tool in client output")
					}
				default:
					return Report{}, errors.New("forbidden or unknown client tool/item")
				}
			case "turn.completed":
				if turns != 1 {
					return Report{}, errors.New("missing CLI turn start")
				}
				completed = true
				usage.Turns = turns
				if event.Usage != nil {
					usage.Known = true
					usage.InputTokens = event.Usage.InputTokens
					usage.OutputTokens = event.Usage.OutputTokens
				}
			case "turn.failed", "error":
				// This pin exposes only a free-text message, not an auth/quota
				// code. Preserve an unknown provider failure without echoing it.
				return Report{}, PhaseError{State: "provider_unavailable"}
			default:
				return Report{}, errors.New("client error or unsupported event")
			}
		}
		if scanner.Err() != nil || !completed || len(result) == 0 {
			return Report{}, errors.New("incomplete Codex transport")
		}
	case Claude:
		var envelope struct {
			Type       string          `json:"type"`
			Subtype    string          `json:"subtype"`
			IsError    *bool           `json:"is_error"`
			Result     string          `json:"result"`
			Structured json.RawMessage `json:"structured_output"`
			Usage      *transportUsage `json:"usage"`
			Turns      int             `json:"num_turns"`
			APIStatus  *int            `json:"api_error_status"`
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		if d.Decode(&envelope) != nil || envelope.Type != "result" || envelope.IsError == nil {
			return Report{}, errors.New("incomplete or failed Claude transport")
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return Report{}, errors.New("trailing Claude transport value")
		}
		if *envelope.IsError {
			state := "provider_unavailable"
			switch envelope.Subtype {
			case "success", "error_during_execution":
				if envelope.APIStatus != nil {
					switch *envelope.APIStatus {
					case 401:
						state = "auth_failed"
					case 429:
						state = "quota_wait"
					}
				}
			case "error_max_turns", "error_max_budget_usd":
				state = "incomplete_expired"
			case "error_max_structured_output_retries":
				state = "invalid_output"
			default:
				return Report{}, errors.New("unknown Claude failure envelope")
			}
			return Report{}, PhaseError{State: state}
		}
		if envelope.Subtype != "success" || envelope.Turns < 1 || (envelope.APIStatus != nil && *envelope.APIStatus >= 400) {
			return Report{}, errors.New("inconsistent Claude completion")
		}
		result = envelope.Structured
		if len(result) == 0 || string(result) == "null" {
			result = []byte(envelope.Result)
		}
		usage.Turns = envelope.Turns
		if envelope.Usage != nil {
			usage.Known = true
			usage.InputTokens = envelope.Usage.InputTokens
			usage.OutputTokens = envelope.Usage.OutputTokens
		}
	default:
		return Report{}, errors.New("unsupported provider")
	}
	r, err := DecodeStrict[Report](result, s.Limits.OutputBytes)
	if err != nil {
		return Report{}, err
	}
	// Usage comes from the official transport, never a model's self-report.
	r.Usage = usage
	if err := ValidateReport(r, s); err != nil {
		return Report{}, err
	}
	return r, nil
}

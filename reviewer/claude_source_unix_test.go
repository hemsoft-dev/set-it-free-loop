//go:build linux || darwin

package reviewer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fake native control client: the audit records every received stdin byte,
// including an accidental prompt before readiness. No model/client is run.
func TestClaudeSourceExecutable(t *testing.T) {
	mode := os.Getenv("SFL_NATIVE_FIXTURE")
	if !strings.HasPrefix(mode, "source-") {
		return
	}
	code := func() int {
		file, err := os.OpenFile(os.Getenv("SFL_NATIVE_FIXTURE_AUDIT"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return 30
		}
		defer file.Close()
		dec := json.NewDecoder(io.TeeReader(os.Stdin, file))
		enc := json.NewEncoder(os.Stdout)
		var config claudeSourceConfig
		if json.Unmarshal([]byte(os.Getenv("SFL_NATIVE_FIXTURE_MCP")), &config) != nil {
			return 34
		}
		statuses := 0
		for {
			var frame struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			}
			if dec.Decode(&frame) != nil {
				return 0
			}
			if frame.Type == "user" {
				if statuses == 0 || frame.Message.Role != "user" {
					return 31
				}
				_, data, ok := strings.Cut(frame.Message.Content, "\n")
				var s Session
				if !ok || json.Unmarshal([]byte(data), &s) != nil {
					return 32
				}
				tools := []string{"mcp__sfl-source__read", "mcp__sfl-source__search", "StructuredOutput", "EndConversation"}
				model, version := s.Identity.Model, s.Identity.ClientVersion
				if mode == "source-builtin-inventory" {
					tools = append(tools, "Bash")
				}
				if mode == "source-wrong-model" {
					model = "other-model"
				}
				if mode == "source-wrong-version" {
					version = "other-client"
				}
				if mode != "source-missing-init" {
					_ = enc.Encode(map[string]any{"type": "system", "subtype": "init", "model": model, "claude_code_version": version, "tools": tools, "mcp_servers": []any{map[string]string{"name": "sfl-source", "status": "connected"}}})
				}
				if mode == "source-forbidden-tool" {
					_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "tool_use", "name": "Bash"}}}})
				}
				if mode == "source-large" {
					_ = enc.Encode(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]string{"type": "tool_result", "content": strings.Repeat("s", s.Limits.OutputBytes+1)}}}})
				}
				if mode == "source-duplicate-result" {
					_ = enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": fakeReport(s), "num_turns": 1})
				}
				_ = enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": fakeReport(s), "num_turns": 1})
				// Keep the control client alive until the driver closes stdin.
				continue
			}
			if frame.Type != "control_request" {
				return 33
			}
			id := frame.RequestID
			if mode == "source-wrong-id" {
				id = "wrong-id"
			}
			result := map[string]any{}
			if frame.Request.Subtype == "mcp_status" {
				statuses++
				name, status := "sfl-source", "connected"
				tools := []any{map[string]string{"name": "read"}, map[string]string{"name": "search"}}
				switch mode {
				case "source-failed":
					status = "failed"
				case "source-needs-auth":
					status = "needs-auth"
				case "source-disabled":
					status = "disabled"
				case "source-pending":
					status = "pending"
				case "source-pending-ready":
					if statuses == 1 {
						status = "pending"
					}
				case "source-foreign":
					name = "ambient"
				case "source-missing-tool":
					tools = tools[:1]
				case "source-extra-tool":
					tools = append(tools, map[string]string{"name": "shell"})
				case "source-duplicate-tool":
					tools[1] = tools[0]
				}
				if mode == "source-post-disconnect" && statuses > 1 {
					status = "failed"
				}
				actual := config
				if mode == "source-wrong-command" {
					actual.Command = "/untrusted/helper"
				}
				if mode == "source-wrong-args" {
					actual.Args = []string{"other-session"}
				}
				server := map[string]any{"name": name, "status": status, "tools": tools, "source": "dynamic", "config": actual}
				if mode == "source-redacted" {
					delete(server, "config")
					delete(server, "tools")
				}
				if mode == "source-wrong-scope" {
					server["source"] = "project"
				}
				servers := []any{server}
				if mode == "source-absent" {
					servers = []any{}
				}
				result["mcpServers"] = servers
			}
			_ = enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": result}})
		}
	}()
	os.Exit(code)
}

func claudeSourceFixture(t *testing.T, mode string) (Adapter, *nativeFixtureLauncher, string) {
	t.Helper()
	a, l := nativeFixtureAdapter(t, Claude, mode)
	l.fixtureTest = "TestClaudeSourceExecutable"
	l.auditPath = filepath.Join(t.TempDir(), "stdin-audit")
	a.SourceBridge = &SourceBridge{Program: "/opt/sfl/sourceproxy", Directory: "/run/sfl", ReaderUID: 1201}
	return a, l, l.auditPath
}

func TestClaudeSourceFailuresSendZeroUserPrompts(t *testing.T) {
	for _, mode := range []string{"failed", "needs-auth", "disabled", "pending", "absent", "foreign", "missing-tool", "extra-tool", "duplicate-tool", "wrong-id", "wrong-command", "wrong-args", "redacted", "wrong-scope"} {
		t.Run(mode, func(t *testing.T) {
			a, l, audit := claudeSourceFixture(t, "source-"+mode)
			s := testSession(Claude)
			s.Limits.Timeout = 150 * time.Millisecond
			if _, err := a.Review(context.Background(), s); err == nil {
				t.Fatal("source-free review accepted")
			}
			assertFixtureStopped(t, &l.fixtureLauncher)
			data, err := os.ReadFile(audit)
			if err != nil || strings.Contains(string(data), `"type":"user"`) || strings.Contains(string(data), s.JobID) || strings.Contains(string(data), s.Packet) {
				t.Fatal("review content sent before source readiness", string(data), err)
			}
		})
	}
}

func TestClaudeSourcePromptFollowsConnectedToolStatus(t *testing.T) {
	for _, mode := range []string{"source-success", "source-pending-ready", "source-large"} {
		t.Run(mode, func(t *testing.T) {
			a, l, audit := claudeSourceFixture(t, mode)
			s := testSession(Claude)
			if _, err := a.Review(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			assertFixtureStopped(t, &l.fixtureLauncher)
			data, err := os.ReadFile(audit)
			text := string(data)
			if err != nil || !strings.Contains(text, s.JobID) || strings.Count(text, `"type":"user"`) != 1 || strings.Index(text, `"subtype":"mcp_status"`) > strings.Index(text, `"type":"user"`) {
				t.Fatal("missing or reordered readiness gate", text, err)
			}
		})
	}
}

func TestClaudeSourceRejectsForbiddenToolsAndDuplicateCompletion(t *testing.T) {
	for _, mode := range []string{"source-forbidden-tool", "source-duplicate-result", "source-builtin-inventory", "source-wrong-model", "source-wrong-version", "source-missing-init", "source-post-disconnect"} {
		a, l, _ := claudeSourceFixture(t, mode)
		if _, err := a.Review(context.Background(), testSession(Claude)); err == nil {
			t.Fatal("unsafe source stream accepted", mode)
		}
		assertFixtureStopped(t, &l.fixtureLauncher)
	}
}

func TestClaudeSourceRejectsDriverWithoutInitializationGate(t *testing.T) {
	a, l, _ := claudeSourceFixture(t, "source-success")
	a.Driver = WorkerDriver{Launcher: l}
	if _, err := a.Review(context.Background(), testSession(Claude)); err == nil || len(l.workers) != 0 || l.inspectCalls != 0 {
		t.Fatal("ungated driver started source-enabled Claude", err)
	}
}

type overriddenSourceDriver struct {
	NativeDriver
	inspected bool
	executed  bool
}

func (d *overriddenSourceDriver) Inspect(context.Context, Invocation) (Identity, error) {
	d.inspected = true
	return PinnedIdentity(Claude)
}

func (d *overriddenSourceDriver) Execute(context.Context, Invocation) ([]byte, Identity, error) {
	d.executed = true
	return nil, Identity{}, nil
}

func TestClaudeSourceRejectsPromotedNativeMarkerAndNilDriver(t *testing.T) {
	a, l, _ := claudeSourceFixture(t, "source-success")
	wrapped := &overriddenSourceDriver{NativeDriver: NativeDriver{Launcher: l}}
	a.Driver = wrapped
	if _, err := a.Review(context.Background(), testSession(Claude)); err == nil || wrapped.inspected || wrapped.executed || len(l.workers) != 0 {
		t.Fatal("embedding bypassed concrete source execution guard", err)
	}
	var missing *NativeDriver
	a.Driver = missing
	if _, err := a.Review(context.Background(), testSession(Claude)); err == nil {
		t.Fatal("typed nil native driver accepted")
	}
	// The concrete pointer form runs the same guarded protocol as a value.
	a.Driver = &NativeDriver{Launcher: l}
	if _, err := a.Review(context.Background(), testSession(Claude)); err != nil {
		t.Fatal("concrete native pointer rejected", err)
	}
	assertFixtureStopped(t, &l.fixtureLauncher)
}

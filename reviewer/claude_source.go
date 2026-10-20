package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"
)

type claudeSourceConfig struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func capturedClaudeSource(inv Invocation) (claudeSourceConfig, error) {
	for i, arg := range inv.Args {
		if arg == "--mcp-config" && i+1 < len(inv.Args) {
			var config struct {
				Servers map[string]claudeSourceConfig `json:"mcpServers"`
			}
			if json.Unmarshal([]byte(inv.Args[i+1]), &config) == nil && len(config.Servers) == 1 && config.Servers["sfl-source"].Type == "stdio" {
				return config.Servers["sfl-source"], nil
			}
		}
	}
	return claudeSourceConfig{}, workerInvalid()
}

func claudeSourceStatus(raw []byte, expected claudeSourceConfig) (bool, error) {
	var result struct {
		Servers []struct {
			Name   string             `json:"name"`
			Status string             `json:"status"`
			Source string             `json:"source"`
			Config claudeSourceConfig `json:"config"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Servers) != 1 {
		return false, workerInvalid()
	}
	server := result.Servers[0]
	if server.Name != "sfl-source" || server.Source != "dynamic" || !reflect.DeepEqual(server.Config, expected) {
		return false, workerInvalid()
	}
	if server.Status == "pending" {
		return false, nil
	}
	if server.Status != "connected" || len(server.Tools) != 2 || !((server.Tools[0].Name == "read" && server.Tools[1].Name == "search") || (server.Tools[0].Name == "search" && server.Tools[1].Name == "read")) {
		return false, workerInvalid()
	}
	return true, nil
}

func claudeSourceInit(raw []byte, s Session) bool {
	var init struct {
		Model   string   `json:"model"`
		Version string   `json:"claude_code_version"`
		Tools   []string `json:"tools"`
		Servers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"mcp_servers"`
	}
	if json.Unmarshal(raw, &init) != nil || init.Model != s.Identity.Model || init.Version != s.Identity.ClientVersion || len(init.Servers) != 1 || init.Servers[0].Name != "sfl-source" || init.Servers[0].Status != "connected" {
		return false
	}
	seen := map[string]bool{}
	for _, name := range init.Tools {
		if seen[name] || !claudeSourceTool(name) {
			return false
		}
		seen[name] = true
	}
	return seen["mcp__sfl-source__read"] && seen["mcp__sfl-source__search"]
}

// claudeSourceExchange withholds all user content until this same client's
// control protocol reports the exact connected source server and tool inventory.
// Startup wait flags and system/init metadata are not readiness evidence.
func claudeSourceExchange(ctx context.Context, worker Worker, inv Invocation, s Session) ([]byte, error) {
	if inv.SourceEndpoint.SessionHash != Digest(s) {
		return nil, workerInvalid()
	}
	expected, err := capturedClaudeSource(inv)
	if err != nil {
		return nil, err
	}
	transportLimit := NativeTransportLimit(s)
	scan := bufio.NewScanner(io.LimitReader(worker.Output(), int64(transportLimit)+1))
	scan.Buffer(make([]byte, 4096), transportLimit+1)
	used, frames, sent := 0, 0, 0
	read := func() ([]byte, map[string]json.RawMessage, error) {
		if !scan.Scan() {
			return nil, nil, workerInvalid()
		}
		line := append([]byte(nil), scan.Bytes()...)
		used += len(line) + 1
		frames++
		if used > transportLimit || frames > 4096 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(line)), 0) != nil {
			return nil, nil, workerInvalid()
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(line, &event) != nil || event == nil {
			return nil, nil, workerInvalid()
		}
		return line, event, nil
	}
	write := func(v any) error {
		data, err := json.Marshal(v)
		sent += len(data) + 1
		if err != nil || sent > 6*inv.Limits.InputBytes+(16<<10) {
			return workerInvalid()
		}
		data = append(data, '\n')
		if n, err := worker.Input().Write(data); err != nil || n != len(data) {
			return workerInvalid()
		}
		return nil
	}
	control := func(id, subtype string) (json.RawMessage, error) {
		if err := write(map[string]any{"type": "control_request", "request_id": id, "request": map[string]string{"subtype": subtype}}); err != nil {
			return nil, err
		}
		for {
			_, event, err := read()
			if err != nil {
				return nil, err
			}
			var kind string
			_ = json.Unmarshal(event["type"], &kind)
			if kind == "system" {
				continue // bounded metadata, never the readiness barrier
			}
			var response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			}
			if kind != "control_response" || json.Unmarshal(event["response"], &response) != nil || response.Subtype != "success" || response.RequestID != id || len(response.Response) == 0 || bytes.Equal(response.Response, []byte("null")) {
				return nil, workerInvalid()
			}
			var object map[string]json.RawMessage
			if json.Unmarshal(response.Response, &object) != nil || object == nil {
				return nil, workerInvalid()
			}
			return response.Response, nil
		}
	}
	if _, err := control("sfl-init", "initialize"); err != nil {
		return nil, err
	}
	ready := false
	for attempt := 0; attempt < 32; attempt++ {
		status, err := control(fmt.Sprintf("sfl-status-%d", attempt), "mcp_status")
		if err != nil {
			return nil, err
		}
		ready, err = claudeSourceStatus(status, expected)
		if err != nil {
			return nil, err
		}
		if ready {
			break
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if !ready {
		return nil, workerInvalid()
	}
	if err := write(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": string(inv.Input)}, "parent_tool_use_id": nil, "session_id": "default"}); err != nil {
		return nil, err
	}
	var terminal []byte
	initialized := false
	for scan.Scan() {
		line := append([]byte(nil), scan.Bytes()...)
		used += len(line) + 1
		frames++
		if len(terminal) != 0 || used > transportLimit || frames > 4096 || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(line)), 0) != nil {
			return nil, workerInvalid()
		}
		var event struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(line, &event) != nil {
			return nil, workerInvalid()
		}
		switch event.Type {
		case "system":
			if event.Subtype == "init" {
				if initialized || !claudeSourceInit(line, s) {
					return nil, workerInvalid()
				}
				initialized = true
			}
		case "user":
		case "assistant", "stream_event":
			var value any
			if json.Unmarshal(line, &value) != nil || !claudeSourceTools(value) {
				return nil, workerInvalid()
			}
		case "tool_progress":
			if !claudeSourceTool(event.ToolName) {
				return nil, workerInvalid()
			}
		case "result":
			if !initialized {
				return nil, workerInvalid()
			}
			terminal = line
			status, err := control("sfl-post-status", "mcp_status")
			if err != nil {
				return nil, err
			}
			if ready, err := claudeSourceStatus(status, expected); err != nil || !ready {
				return nil, workerInvalid()
			}
			_ = worker.Input().Close()
		default:
			return nil, workerInvalid()
		}
	}
	if scan.Err() != nil || len(terminal) == 0 {
		return nil, workerInvalid()
	}
	return normalizeNativeResult(s, terminal, worker.Wait())
}

func claudeSourceTool(name string) bool {
	return name == "mcp__sfl-source__read" || name == "mcp__sfl-source__search" || name == "StructuredOutput" || name == "EndConversation"
}

func claudeSourceTools(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if kind, _ := v["type"].(string); kind == "tool_use" || kind == "server_tool_use" {
			name, _ := v["name"].(string)
			if kind != "tool_use" || !claudeSourceTool(name) {
				return false
			}
		}
		for _, nested := range v {
			if !claudeSourceTools(nested) {
				return false
			}
		}
	case []any:
		for _, nested := range v {
			if !claudeSourceTools(nested) {
				return false
			}
		}
	}
	return true
}

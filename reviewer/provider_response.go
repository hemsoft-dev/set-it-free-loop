package reviewer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Response policy is trusted launch configuration, not a per-PR tool list.
// Empty means legacy/unknown enforcement; it cannot qualify a native launcher.
const (
	CodexSourceResponses = "codex-source-responses-v1"
	ClaudeSourceMessages = "claude-source-messages-v1"
)

var ErrProviderToolResponse = errors.New("provider response tool protocol denied")

func validResponsePolicy(provider Provider, policy string) bool {
	return policy == "" || provider == Codex && policy == CodexSourceResponses || provider == Claude && policy == ClaudeSourceMessages
}

type responseTools struct {
	provider Provider
	calls    map[string]string
}

func (g *responseTools) walk(value any, depth int) error {
	if depth > 32 {
		return ErrProviderToolResponse
	}
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if err := g.walk(child, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		kind, _ := v["type"].(string)
		switch kind {
		case "custom_tool_call":
			name, _ := v["name"].(string)
			namespace, _ := v["namespace"].(string)
			id, _ := v["call_id"].(string)
			if g.provider != Codex || namespace != "functions" || name != "exec" || id == "" || len(id) > 1024 {
				return ErrProviderToolResponse
			}
			if _, ok := v["input"].(string); !ok {
				return ErrProviderToolResponse
			}
			if err := g.remember(id, kind+"/"+namespace+"/"+name); err != nil {
				return err
			}
		case "tool_use":
			name, _ := v["name"].(string)
			id, _ := v["id"].(string)
			if g.provider != Claude || !claudeSourceTool(name) || id == "" || len(id) > 1024 {
				return ErrProviderToolResponse
			}
			if _, ok := v["input"].(map[string]any); !ok {
				return ErrProviderToolResponse
			}
			if err := g.remember(id, kind+"/"+name); err != nil {
				return err
			}
		default:
			// Output item/block validators below also reject unknown variants. Catch
			// executable records in every nested response/output container before bytes
			// reach a CLI, including records hidden in a terminal response object.
			if strings.HasSuffix(kind, "_call") || strings.Contains(kind, "tool_use") || kind == "function_call" || kind == "tool_result" || kind == "custom_tool_call_output" || kind == "function_call_output" {
				return ErrProviderToolResponse
			}
		}
		for _, child := range v {
			if err := g.walk(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
func (g *responseTools) remember(id, signature string) error {
	if old, ok := g.calls[id]; ok {
		if old != signature {
			return ErrProviderToolResponse
		}
		return nil
	}
	if len(g.calls) >= 128 {
		return ErrProviderToolResponse
	}
	g.calls[id] = signature
	return nil
}
func responseJSON(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || !utf8.Valid(raw) || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return nil, ErrProviderToolResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrProviderToolResponse
	}
	return value, nil
}
func responseIndex(value any) (int, error) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, ErrProviderToolResponse
	}
	i, err := strconv.Atoi(string(n))
	if err != nil || i < 0 || i > 127 {
		return 0, ErrProviderToolResponse
	}
	return i, nil
}
func responseArray(value any) ([]any, error) {
	v, ok := value.([]any)
	if !ok {
		return nil, ErrProviderToolResponse
	}
	return v, nil
}
func codexResponseItem(item any) error {
	v, ok := item.(map[string]any)
	if !ok {
		return ErrProviderToolResponse
	}
	switch v["type"] {
	case "custom_tool_call":
		return nil // Name/input checked recursively.
	case "message":
		if v["role"] != "assistant" {
			return ErrProviderToolResponse
		}
		content, err := responseArray(v["content"])
		if err != nil {
			return err
		}
		for _, part := range content {
			p, ok := part.(map[string]any)
			if !ok {
				return ErrProviderToolResponse
			}
			switch p["type"] {
			case "output_text":
				if _, ok := p["text"].(string); !ok {
					return ErrProviderToolResponse
				}
			case "refusal":
				if _, ok := p["refusal"].(string); !ok {
					return ErrProviderToolResponse
				}
			default:
				return ErrProviderToolResponse
			}
		}
	case "reasoning": // Opaque reasoning is data, never an executable item.
	default:
		return ErrProviderToolResponse
	}
	return nil
}
func codexResponseObject(value any) error {
	v, ok := value.(map[string]any)
	if !ok {
		return ErrProviderToolResponse
	}
	if output, exists := v["output"]; exists {
		items, err := responseArray(output)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err = codexResponseItem(item); err != nil {
				return err
			}
		}
	}
	return nil
}
func claudeResponseBlock(value any) error {
	v, ok := value.(map[string]any)
	if !ok {
		return ErrProviderToolResponse
	}
	switch v["type"] {
	case "tool_use":
		return nil
	case "text":
		if _, ok := v["text"].(string); !ok {
			return ErrProviderToolResponse
		}
	case "thinking":
		if _, ok := v["thinking"].(string); !ok {
			return ErrProviderToolResponse
		}
	case "redacted_thinking":
		if _, ok := v["data"].(string); !ok {
			return ErrProviderToolResponse
		}
	default:
		return ErrProviderToolResponse
	}
	return nil
}
func claudeResponseMessage(value any) error {
	v, ok := value.(map[string]any)
	if !ok || v["type"] != "message" || v["role"] != "assistant" {
		return ErrProviderToolResponse
	}
	items, err := responseArray(v["content"])
	if err != nil {
		return err
	}
	for _, item := range items {
		if err = claudeResponseBlock(item); err != nil {
			return err
		}
	}
	return nil
}

type claudeResponseStream struct {
	started, done bool
	blocks        map[int]*claudeResponseInput
}
type claudeResponseInput struct {
	kind    string
	initial int
	data    bytes.Buffer
	closed  bool
}

func (c *claudeResponseStream) event(kind string, v map[string]any, tools *responseTools) error {
	if c.done {
		return ErrProviderToolResponse
	}
	switch kind {
	case "error":
		// A provider can terminate before message_start or midway through a
		// block. It is data for native error/retry handling, not a tool event.
		for key, value := range v {
			switch key {
			case "type", "error":
			case "request_id":
				id, ok := value.(string)
				if !ok || id == "" || len(id) > 1024 {
					return ErrProviderToolResponse
				}
			default:
				return ErrProviderToolResponse
			}
		}
		detail, ok := v["error"].(map[string]any)
		if !ok || len(detail) != 2 {
			return ErrProviderToolResponse
		}
		code, codeOK := detail["type"].(string)
		message, messageOK := detail["message"].(string)
		if !codeOK || code == "" || len(code) > 128 || !messageOK || len(message) > 8192 {
			return ErrProviderToolResponse
		}
		c.done = true
		return nil
	case "ping":
		return nil
	case "message_start":
		if c.started {
			return ErrProviderToolResponse
		}
		if err := claudeResponseMessage(v["message"]); err != nil {
			return err
		}
		c.started = true
		return nil
	}
	if !c.started {
		return ErrProviderToolResponse
	}
	switch kind {
	case "content_block_start":
		index, err := responseIndex(v["index"])
		if err != nil || c.blocks[index] != nil {
			return ErrProviderToolResponse
		}
		if err = claudeResponseBlock(v["content_block"]); err != nil {
			return err
		}
		block := v["content_block"].(map[string]any)
		kind := block["type"].(string)
		input := &claudeResponseInput{kind: kind}
		if kind == "tool_use" {
			input.initial = len(block["input"].(map[string]any))
		}
		c.blocks[index] = input
	case "content_block_delta":
		index, err := responseIndex(v["index"])
		if err != nil {
			return err
		}
		block := c.blocks[index]
		delta, ok := v["delta"].(map[string]any)
		if block == nil || block.closed || !ok {
			return ErrProviderToolResponse
		}
		switch block.kind {
		case "tool_use":
			text, ok := delta["partial_json"].(string)
			if delta["type"] != "input_json_delta" || !ok || block.initial != 0 || block.data.Len()+len(text) > 4<<20 {
				return ErrProviderToolResponse
			}
			block.data.WriteString(text)
		case "text":
			if delta["type"] != "text_delta" {
				return ErrProviderToolResponse
			}
			if _, ok := delta["text"].(string); !ok {
				return ErrProviderToolResponse
			}
		case "thinking":
			if delta["type"] != "thinking_delta" && delta["type"] != "signature_delta" {
				return ErrProviderToolResponse
			}
		default:
			return ErrProviderToolResponse
		}
	case "content_block_stop":
		index, err := responseIndex(v["index"])
		if err != nil {
			return err
		}
		block := c.blocks[index]
		if block == nil || block.closed {
			return ErrProviderToolResponse
		}
		if block.data.Len() > 0 {
			input, err := responseJSON(block.data.Bytes())
			if err != nil {
				return err
			}
			if err = tools.walk(input, 0); err != nil {
				return err
			}
		}
		block.closed = true
	case "message_delta":
		if _, ok := v["delta"].(map[string]any); !ok {
			return ErrProviderToolResponse
		}
	case "message_stop":
		for _, block := range c.blocks {
			if !block.closed {
				return ErrProviderToolResponse
			}
		}
		c.done = true
	default:
		return ErrProviderToolResponse
	}
	return nil
}

func validateProviderResponse(provider Provider, policy, contentType string, status int, body []byte) error {
	if policy == "" {
		return nil
	}
	if !validResponsePolicy(provider, policy) || len(body) == 0 || len(body) > 16<<20 || !utf8.Valid(body) {
		return ErrProviderToolResponse
	}
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ErrProviderToolResponse
	}
	tools := &responseTools{provider: provider, calls: map[string]string{}}
	if media == "application/json" {
		value, err := responseJSON(body)
		if err != nil {
			return err
		}
		if err = tools.walk(value, 0); err != nil {
			return err
		}
		if status >= 400 {
			if _, ok := value["error"].(map[string]any); !ok {
				return ErrProviderToolResponse
			}
			if _, ok := value["content"]; ok {
				return ErrProviderToolResponse
			}
			if _, ok := value["output"]; ok {
				return ErrProviderToolResponse
			}
			return nil
		}
		if provider == Codex {
			if value["object"] != "response" {
				return ErrProviderToolResponse
			}
			return codexResponseObject(value)
		}
		return claudeResponseMessage(value)
	}
	// EventSource treats bare CR as a line break. Scanner's ScanLines does not;
	// accepting it could hide a data event behind a comment in this validator.
	for i, b := range body {
		if b == 0 || b == '\r' && (i+1 == len(body) || body[i+1] != '\n') {
			return ErrProviderToolResponse
		}
	}
	if media != "text/event-stream" || status < 200 || status >= 300 {
		return ErrProviderToolResponse
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var data bytes.Buffer
	event := ""
	hasEvent, hasData := false, false
	frames := 0
	codexDone := false
	sentinel := false
	claude := &claudeResponseStream{blocks: map[int]*claudeResponseInput{}}
	dispatch := func() error {
		if !hasData {
			return ErrProviderToolResponse
		}
		frames++
		if frames > 131072 {
			return ErrProviderToolResponse
		}
		raw := bytes.TrimSuffix(data.Bytes(), []byte{'\n'})
		if bytes.Equal(raw, []byte("[DONE]")) {
			if provider != Codex || !codexDone || sentinel {
				return ErrProviderToolResponse
			}
			sentinel = true
			return nil
		}
		if codexDone {
			return ErrProviderToolResponse
		}
		value, err := responseJSON(raw)
		if err != nil {
			return err
		}
		kind, ok := value["type"].(string)
		if !ok || hasEvent && event != kind {
			return ErrProviderToolResponse
		}
		if err = tools.walk(value, 0); err != nil {
			return err
		}
		if provider == Claude {
			return claude.event(kind, value, tools)
		}
		switch kind {
		case "response.created", "response.in_progress":
			return codexResponseObject(value["response"])
		case "response.output_item.added", "response.output_item.done":
			return codexResponseItem(value["item"])
		case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
			return nil
		case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
			// Incremental input cannot introduce a tool name. The complete buffered
			// stream is still recursively checked, including final output items.
			if _, ok := value["item_id"].(string); !ok {
				return ErrProviderToolResponse
			}
			return nil
		case "response.completed", "response.failed", "response.incomplete":
			if err = codexResponseObject(value["response"]); err != nil {
				return err
			}
			codexDone = true
			return nil
		default:
			return ErrProviderToolResponse
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if hasData || hasEvent {
				if err = dispatch(); err != nil {
					return err
				}
			}
			data.Reset()
			event = ""
			hasData = false
			hasEvent = false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return ErrProviderToolResponse
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			if hasEvent || value == "" {
				return ErrProviderToolResponse
			}
			event = value
			hasEvent = true
		case "data":
			if data.Len()+len(value)+1 > 4<<20 {
				return ErrProviderToolResponse
			}
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		default:
			return ErrProviderToolResponse
		}
	}
	if scanner.Err() != nil || hasData || hasEvent || frames == 0 || provider == Codex && !codexDone || provider == Claude && !claude.done {
		return ErrProviderToolResponse
	}
	return nil
}

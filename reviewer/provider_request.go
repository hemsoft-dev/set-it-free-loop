package reviewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Request policy is trusted launch configuration, like the response policy.
// It checks the tool inventory an inference request advertises before the
// host forwards it. Empty means no request enforcement; it cannot qualify a
// native Codex launch.
const CodexSourceRequests = "codex-source-requests-v1"

var ErrProviderToolRequest = errors.New("provider request tool inventory denied")

// The request body is untrusted guest output. The egress already bounds it by
// the captured request limit; this bound holds even for a looser caller.
const maxProviderRequestBytes = 8 << 20

func validRequestPolicy(provider Provider, policy string) bool {
	return policy == "" || provider == Codex && policy == CodexSourceRequests
}

// codexRequestKeys is the top-level key set every measured pinned Codex 0.163.0
// code-mode request used. An unknown key could carry or select tools, such as
// a top-level tools list or a stored prompt, so it is an unknown inventory.
var codexRequestKeys = map[string]bool{"client_metadata": true, "include": true, "input": true, "model": true, "parallel_tool_calls": true, "prompt_cache_key": true, "reasoning": true, "store": true, "stream": true, "text": true, "tool_choice": true}

// codexWaitParameters is the measured wait schema, compared after canonical
// re-encoding with sorted keys.
const codexWaitParameters = `{"additionalProperties":false,"properties":{"cell_id":{"description":"Identifier of the running exec cell.","type":"string"},"max_tokens":{"description":"Output token budget for this wait call. Defaults to 10000 tokens.","type":"number"},"terminate":{"description":"True stops the running exec cell; false or omitted waits for output.","type":"boolean"},"yield_time_ms":{"description":"Wait before yielding more output. Defaults to 10000 ms.","type":"number"}},"required":["cell_id"],"type":"object"}`

// codexNestedTools are the only nested tools exec may declare to the model.
var codexNestedTools = []string{"mcp__sfl_source__read", "mcp__sfl_source__search"}

// validateProviderRequest checks one complete inference request body before
// it is forwarded. It never changes the body.
func validateProviderRequest(provider Provider, policy string, body []byte) error {
	if policy == "" {
		return nil
	}
	if !validRequestPolicy(provider, policy) || len(body) == 0 || len(body) > maxProviderRequestBytes || !utf8.Valid(body) {
		return ErrProviderToolRequest
	}
	// Duplicate keys and nesting deeper than 32 are refused before decoding.
	if uniqueJSONKeys(json.NewDecoder(bytes.NewReader(body)), 0) != nil {
		return ErrProviderToolRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil || decoder.Decode(new(any)) != io.EOF {
		return ErrProviderToolRequest
	}
	return codexRequestInventory(value)
}

// codexRequestInventory accepts exactly the measured code-mode inventory: one
// additional_tools item first in input, holding the functions namespace with
// exec and wait, and exec declaring only the two source tools. No other tool
// list or namespace may appear anywhere in the request.
func codexRequestInventory(value map[string]any) error {
	for key := range value {
		if !codexRequestKeys[key] {
			return ErrProviderToolRequest
		}
	}
	if model, ok := value["model"].(string); !ok || model == "" {
		return ErrProviderToolRequest
	}
	if choice, ok := value["tool_choice"]; ok && choice != "auto" {
		return ErrProviderToolRequest
	}
	input, ok := value["input"].([]any)
	if !ok || len(input) == 0 {
		return ErrProviderToolRequest
	}
	if err := codexAdditionalTools(input[0]); err != nil {
		return err
	}
	for key, child := range value {
		if key == "input" {
			continue
		}
		if err := codexNoInventory(child); err != nil {
			return err
		}
	}
	for _, item := range input[1:] {
		if err := codexNoInventory(item); err != nil {
			return err
		}
	}
	return nil
}

func exactKeys(v map[string]any, keys ...string) bool {
	if len(v) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := v[key]; !ok {
			return false
		}
	}
	return true
}

func boundedString(value any, max int) (string, bool) {
	s, ok := value.(string)
	return s, ok && len(s) <= max
}

func codexAdditionalTools(value any) error {
	item, ok := value.(map[string]any)
	if !ok || !exactKeys(item, "id", "role", "tools", "type") || item["type"] != "additional_tools" || item["role"] != "developer" {
		return ErrProviderToolRequest
	}
	if id, ok := boundedString(item["id"], 256); !ok || id == "" {
		return ErrProviderToolRequest
	}
	namespaces, ok := item["tools"].([]any)
	if !ok || len(namespaces) != 1 {
		return ErrProviderToolRequest
	}
	namespace, ok := namespaces[0].(map[string]any)
	if !ok || !exactKeys(namespace, "description", "name", "tools", "type") || namespace["type"] != "namespace" || namespace["name"] != "functions" {
		return ErrProviderToolRequest
	}
	if _, ok := boundedString(namespace["description"], 4096); !ok {
		return ErrProviderToolRequest
	}
	tools, ok := namespace["tools"].([]any)
	if !ok || len(tools) != 2 {
		return ErrProviderToolRequest
	}
	seen := map[string]bool{}
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			return ErrProviderToolRequest
		}
		name, _ := tool["name"].(string)
		if seen[name] {
			return ErrProviderToolRequest
		}
		seen[name] = true
		var err error
		switch name {
		case "exec":
			err = codexExecTool(tool)
		case "wait":
			err = codexWaitTool(tool)
		default:
			err = ErrProviderToolRequest
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func codexExecTool(tool map[string]any) error {
	if !exactKeys(tool, "description", "format", "name", "type") || tool["type"] != "custom" {
		return ErrProviderToolRequest
	}
	format, ok := tool["format"].(map[string]any)
	if !ok || !exactKeys(format, "definition", "syntax", "type") || format["type"] != "grammar" || format["syntax"] != "lark" {
		return ErrProviderToolRequest
	}
	if _, ok := boundedString(format["definition"], 4096); !ok {
		return ErrProviderToolRequest
	}
	description, ok := boundedString(tool["description"], 1<<20)
	if !ok {
		return ErrProviderToolRequest
	}
	// Code mode lists each nested tool it exposes inside exec under its own
	// "### `name`" heading. The measured description has exactly the two
	// source tools; a widened code-mode inventory adds headings such as
	// apply_patch or read_mcp_resource. The client may omit deferred nested
	// tools from this text, so this check does not prove the runtime set.
	declared := []string{}
	for _, line := range strings.Split(description, "\n") {
		if strings.HasPrefix(line, "###") {
			declared = append(declared, line)
		}
	}
	if len(declared) != len(codexNestedTools) {
		return ErrProviderToolRequest
	}
	for i, name := range codexNestedTools {
		if declared[i] != "### `"+name+"`" {
			return ErrProviderToolRequest
		}
	}
	return nil
}

func codexWaitTool(tool map[string]any) error {
	if !exactKeys(tool, "description", "name", "parameters", "strict", "type") || tool["type"] != "function" || tool["strict"] != false {
		return ErrProviderToolRequest
	}
	if _, ok := boundedString(tool["description"], 8192); !ok {
		return ErrProviderToolRequest
	}
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok {
		return ErrProviderToolRequest
	}
	canonical, err := json.Marshal(parameters)
	if err != nil || string(canonical) != codexWaitParameters {
		return ErrProviderToolRequest
	}
	return nil
}

// codexNoInventory refuses any tool list or tool container outside the one
// measured additional_tools item: a top-level or nested tools key, another
// additional_tools item or a namespace. Depth is already bounded by the
// duplicate-key scan.
func codexNoInventory(value any) error {
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if err := codexNoInventory(child); err != nil {
				return err
			}
		}
	case map[string]any:
		if _, ok := v["tools"]; ok {
			return ErrProviderToolRequest
		}
		if kind, _ := v["type"].(string); kind == "additional_tools" || kind == "namespace" {
			return ErrProviderToolRequest
		}
		for _, child := range v {
			if err := codexNoInventory(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// nativeSourcePolicies is the policy pair every native source launch, release
// and stopped-attempt recovery requires. Codex needs both its response and
// request policies. Claude has no request policy: no measured Claude case
// widened its inventory and its captured policy has no metadata route.
func nativeSourcePolicies(provider Provider, p EgressPolicy) bool {
	switch provider {
	case Codex:
		return p.ResponsePolicy == CodexSourceResponses && p.RequestPolicy == CodexSourceRequests
	case Claude:
		return p.ResponsePolicy == ClaudeSourceMessages && p.RequestPolicy == ""
	}
	return false
}

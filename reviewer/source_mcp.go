package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// SourceMCPVersion pins the supported stateful stdio lifecycle. Clients offering
// another version receive this version and must disconnect if unsupported.
const SourceMCPVersion = "2025-11-25"

var ErrSourceMCPProtocol = errors.New("source MCP connection rejected or exhausted")

type sourceRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func rpcObject(data []byte, target any) error {
	if !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return ErrSourceMCPProtocol
	}
	keys := json.NewDecoder(bytes.NewReader(data))
	if uniqueJSONKeys(keys, 0) != nil || keys.Decode(new(any)) != io.EOF {
		return ErrSourceMCPProtocol
	}
	// encoding/json matches struct fields case-insensitively. Require the
	// exact protocol/schema keys so aliases cannot override a canonical field.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ErrSourceMCPProtocol
	}
	allowed := map[string]bool{}
	shape := reflect.TypeOf(target).Elem()
	for i := 0; i < shape.NumField(); i++ {
		name := strings.Split(shape.Field(i).Tag.Get("json"), ",")[0]
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return ErrSourceMCPProtocol
		}
	}
	if meta, present := fields["_meta"]; present {
		var object map[string]json.RawMessage
		if json.Unmarshal(meta, &object) != nil || object == nil {
			return ErrSourceMCPProtocol
		}
	}
	decode := json.NewDecoder(bytes.NewReader(data))
	decode.DisallowUnknownFields()
	if decode.Decode(target) != nil || decode.Decode(new(any)) != io.EOF {
		return ErrSourceMCPProtocol
	}
	return nil
}
func rpcIDKey(id json.RawMessage) string {
	if len(id) == 0 || len(id) > 128 {
		return ""
	}
	var text string
	if json.Unmarshal(id, &text) == nil {
		if len(text) == 0 || len(text) > 80 {
			return ""
		}
		return "s:" + text
	}
	number, err := strconv.ParseInt(string(id), 10, 64)
	if err != nil {
		return ""
	}
	return "n:" + strconv.FormatInt(number, 10)
}
func rpcID(id json.RawMessage) bool { return rpcIDKey(id) != "" }
func emptyRPCParams(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var params struct {
		Meta json.RawMessage `json:"_meta,omitempty"`
	}
	return rpcObject(raw, &params) == nil
}

// ServeSourceMCP serves only Read/Search on an already-established private stdio
// or local IPC connection. It creates no listener, filesystem root or auth
// store. The trusted caller supplies precisely this invocation's broker, a
// disposable connection and captured limits. The supervisor must place the
// actual reader under a separate identity without provider credentials.
// It is a protocol implementation, not official-client/isolation qualification.
func ServeSourceMCP(ctx context.Context, inv Invocation, connection io.ReadWriteCloser) error {
	if connection == nil {
		return ErrSourceMCPProtocol
	}
	defer connection.Close()
	if inv.Source == nil || inv.Limits.Validate() != nil {
		return ErrSourceMCPProtocol
	}
	ctx, deadline := context.WithTimeout(ctx, inv.Limits.Timeout)
	defer deadline()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	closeOnCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer closeOnCancel()
	var output sync.Mutex
	var active sync.Mutex
	var callID string
	var callCancel context.CancelFunc
	var calls sync.WaitGroup
	sent := 0
	respond := func(id json.RawMessage, result any, code int, message string) error {
		output.Lock()
		defer output.Unlock()
		response := map[string]any{"jsonrpc": "2.0", "id": id}
		if code == 0 {
			response["result"] = result
		} else {
			response["error"] = map[string]any{"code": code, "message": message}
		}
		raw, err := json.Marshal(response)
		sent += len(raw) + 1
		if err != nil || sent > 12*inv.Limits.ReaderBytes+inv.Limits.ReaderCalls*8192+(128<<10) {
			cancel(ErrSourceMCPProtocol)
			return ErrSourceMCPProtocol
		}
		if _, err = connection.Write(append(raw, '\n')); err != nil {
			cancel(ErrSourceMCPProtocol)
			return ErrSourceMCPProtocol
		}
		return nil
	}
	// Cancelling and joining is required before a broker receipt can be sealed.
	defer func() { cancel(context.Canceled); calls.Wait() }()
	requests, received, toolCalls, state := 0, 0, 0, 0
	seen := map[string]bool{}
	scan := bufio.NewScanner(io.LimitReader(connection, int64(inv.Limits.InputBytes+inv.Limits.ReaderCalls*2048+1)))
	scan.Buffer(make([]byte, 4096), (16<<10)+1)
	for scan.Scan() {
		if ctx.Err() != nil {
			break
		}
		received += len(scan.Bytes()) + 1
		requests++
		if requests > inv.Limits.ReaderCalls+32 || received > inv.Limits.InputBytes+inv.Limits.ReaderCalls*2048 || len(scan.Bytes()) > 16<<10 {
			return ErrSourceMCPProtocol
		}
		var request sourceRPC
		if rpcObject(scan.Bytes(), &request) != nil || request.JSONRPC != "2.0" || request.Method == "" {
			return ErrSourceMCPProtocol
		}
		notification := len(request.ID) == 0
		if !notification {
			if !rpcID(request.ID) {
				return ErrSourceMCPProtocol
			}
			// Normalize string escapes and numeric spellings such as -0/0.
			key := rpcIDKey(request.ID)
			if seen[key] {
				return ErrSourceMCPProtocol
			}
			seen[key] = true
		}
		if notification {
			switch request.Method {
			case "notifications/initialized":
				if state != 1 || !emptyRPCParams(request.Params) {
					return ErrSourceMCPProtocol
				}
				state = 2
			case "notifications/cancelled":
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
					Reason    string          `json:"reason,omitempty"`
					Meta      json.RawMessage `json:"_meta,omitempty"`
				}
				if rpcObject(request.Params, &p) != nil || !rpcID(p.RequestID) {
					return ErrSourceMCPProtocol
				}
				active.Lock()
				if callCancel != nil && sameRPCID(callID, p.RequestID) {
					callCancel()
				}
				active.Unlock()
			default:
				// Notifications never execute a tool or create a response, including an
				// invalid tools/call notification. Unknown notifications are inert.
			}
			continue
		}
		if request.Method == "ping" {
			if !emptyRPCParams(request.Params) {
				_ = respond(request.ID, nil, -32602, "Invalid params")
			} else {
				_ = respond(request.ID, struct{}{}, 0, "")
			}
			continue
		}
		if request.Method == "initialize" {
			if state != 0 {
				_ = respond(request.ID, nil, -32600, "Already initialized")
				continue
			}
			var p struct {
				ProtocolVersion string                     `json:"protocolVersion"`
				Capabilities    map[string]json.RawMessage `json:"capabilities"`
				ClientInfo      map[string]json.RawMessage `json:"clientInfo"`
				Meta            json.RawMessage            `json:"_meta,omitempty"`
			}
			if rpcObject(request.Params, &p) != nil || p.ProtocolVersion == "" || p.Capabilities == nil || p.ClientInfo == nil {
				_ = respond(request.ID, nil, -32602, "Invalid params")
				continue
			}
			var name, version string
			if json.Unmarshal(p.ClientInfo["name"], &name) != nil || json.Unmarshal(p.ClientInfo["version"], &version) != nil || name == "" || version == "" {
				_ = respond(request.ID, nil, -32602, "Invalid params")
				continue
			}
			if respond(request.ID, map[string]any{"protocolVersion": SourceMCPVersion, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}}, "serverInfo": map[string]string{"name": "sfl-source", "version": "1"}}, 0, "") != nil {
				break
			}
			state = 1
			continue
		}
		if state != 2 {
			_ = respond(request.ID, nil, -32002, "Not initialized")
			continue
		}
		switch request.Method {
		case "tools/list":
			if !emptyRPCParams(request.Params) {
				_ = respond(request.ID, nil, -32602, "Invalid params")
				continue
			}
			_ = respond(request.ID, map[string]any{"tools": sourceMCPTools()}, 0, "")
		case "tools/call":
			toolCalls++
			if toolCalls > inv.Limits.ReaderCalls {
				return ErrSourceMCPProtocol
			}
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      json.RawMessage `json:"_meta,omitempty"`
			}
			if rpcObject(request.Params, &p) != nil {
				_ = respond(request.ID, nil, -32602, "Invalid params")
				continue
			}
			var revision, path, needle string
			maxResults := 0
			switch p.Name {
			case "read":
				var args struct {
					Revision string `json:"revision"`
					Path     string `json:"path"`
				}
				if rpcObject(p.Arguments, &args) != nil || args.Revision == "" || args.Path == "" {
					_ = respond(request.ID, nil, -32602, "Invalid params")
					continue
				}
				revision, path = args.Revision, args.Path
			case "search":
				var args struct {
					Revision   string `json:"revision"`
					Needle     string `json:"needle"`
					MaxResults int    `json:"max_results"`
				}
				if rpcObject(p.Arguments, &args) != nil || args.Revision == "" || args.Needle == "" || args.MaxResults < 1 || args.MaxResults > 100 {
					_ = respond(request.ID, nil, -32602, "Invalid params")
					continue
				}
				revision, needle, maxResults = args.Revision, args.Needle, args.MaxResults
			default:
				_ = respond(request.ID, nil, -32602, "Unknown source tool")
				continue
			}
			active.Lock()
			if callCancel != nil {
				active.Unlock()
				_ = respond(request.ID, nil, -32000, "Source request active")
				continue
			}
			callCtx, end := context.WithCancel(ctx)
			callID = string(request.ID)
			callCancel = end
			active.Unlock()
			calls.Add(1)
			go func(id json.RawMessage, method, revision, path, needle string, maxResults int) {
				defer calls.Done()
				defer end()
				var text string
				var err error
				if method == "read" {
					text, err = inv.Source.Read(callCtx, revision, path)
				} else {
					var matches []Anchor
					matches, err = inv.Source.Search(callCtx, revision, needle, maxResults)
					if err == nil {
						var raw []byte
						raw, err = json.Marshal(matches)
						text = string(raw)
					}
				}
				denied := err != nil
				if denied {
					text = "Source request denied"
				}
				// Clear the pending operation before emitting its response, so a client
				// that receives it can immediately issue the next request.
				active.Lock()
				callCancel = nil
				callID = ""
				active.Unlock()
				if callCtx.Err() != nil {
					return
				}
				_ = respond(id, map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": denied}, 0, "")
			}(append(json.RawMessage{}, request.ID...), p.Name, revision, path, needle, maxResults)
		default:
			_ = respond(request.ID, nil, -32601, "Method not found")
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if scan.Err() != nil {
		return ErrSourceMCPProtocol
	}
	return nil
}
func sameRPCID(current string, id json.RawMessage) bool {
	key := rpcIDKey([]byte(current))
	return key != "" && key == rpcIDKey(id)
}

func sourceMCPTools() []map[string]any {
	tool := func(name, description string, properties map[string]any, required []string) map[string]any {
		return map[string]any{
			"name": name, "description": description,
			"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false},
			"annotations": map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		}
	}
	revision := map[string]any{"type": "string", "pattern": "^(?:[0-9a-f]{40}|[0-9a-f]{64})$"}
	return []map[string]any{
		tool("read", "Read an allowed path at one captured immutable revision.", map[string]any{"revision": revision, "path": map[string]any{"type": "string", "maxLength": 512}}, []string{"revision", "path"}),
		tool("search", "Search allowed immutable source with a bounded literal needle.", map[string]any{"revision": revision, "needle": map[string]any{"type": "string", "maxLength": 256}, "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, []string{"revision", "needle", "max_results"}),
	}
}

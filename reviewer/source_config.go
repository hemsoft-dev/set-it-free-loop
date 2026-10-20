package reviewer

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SourceBridge is trusted release configuration, never a per-PR/model option.
// Program is the installed sourceproxy executable. The supervisor must create
// the exact per-session endpoint under the separately protected reader identity.
type SourceBridge struct {
	Program   string
	Directory string
	ReaderUID int
}

// SourceEndpoint is a supervisor requirement, not evidence it was enforced.
// Inspect must qualify the isolated-source-profile-1 boundary before inference:
// pristine disposable client config, inert candidate objects, no ambient managed
// hooks/plugins/settings, separate reader identity, bounded tools and egress.
// A generated manifest or matching UID alone cannot qualify that boundary.
type SourceEndpoint struct {
	Socket          string
	ReaderUID       int
	SessionHash     string
	RequiredProfile string
}

func sourcePath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && len(p) <= 512 && utf8.ValidString(p) && !strings.ContainsAny(p, "\x00\r\n\t")
}

func (a Adapter) sourceInvocation(s Session, inv Invocation) (Invocation, error) {
	bridge := a.SourceBridge
	if a.Reader == nil || a.Reader.snapshot != s.Snapshot || !sourcePath(bridge.Program) || !sourcePath(bridge.Directory) || bridge.ReaderUID <= 0 || uint64(bridge.ReaderUID) > uint64(^uint32(0)) {
		return Invocation{}, errors.New("invalid trusted source bridge")
	}
	endpoint := SourceEndpoint{Socket: filepath.Join(bridge.Directory, Digest(s), "source.sock"), ReaderUID: bridge.ReaderUID, SessionHash: Digest(s), RequiredProfile: "isolated-source-profile-1"}
	// Linux sockaddr_un has 108 bytes including its terminating NUL.
	if len(endpoint.Socket) > 107 {
		return Invocation{}, errors.New("source endpoint exceeds Unix socket path bound")
	}
	limits, _ := json.Marshal(s.Limits)
	// env -i is part of the trusted command, so the proxy does not inherit the
	// provider's auth environment. It receives only its fixed typed IPC options.
	arguments := []string{"-i", "LANG=C.UTF-8", bridge.Program, "--socket", endpoint.Socket, "--reader-uid", strconv.Itoa(bridge.ReaderUID), "--limits", string(limits)}
	switch a.Provider {
	case Codex:
		quote := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
		extra := []string{
			"-c", "mcp_servers.sfl-source.command=" + quote("/usr/bin/env"),
			"-c", "mcp_servers.sfl-source.args=" + quote(arguments),
			"-c", "mcp_servers.sfl-source.env_vars=[]",
			"-c", "mcp_servers.sfl-source.enabled_tools=[\"read\",\"search\"]",
			"-c", "mcp_servers.sfl-source.required=true",
			"-c", "mcp_servers.sfl-source.default_tools_approval_mode=\"auto\"",
		}
		// Preserve the positional stdin prompt marker after all added options.
		inv.Args = append(append(inv.Args[:len(inv.Args)-1:len(inv.Args)-1], extra...), "-")
	case Claude:
		// Safe mode disables MCP. The source-enabled profile instead requires a
		// pristine externally confined client filesystem plus explicit controls.
		args := make([]string, 0, len(inv.Args)+16)
		for i := 0; i < len(inv.Args); i++ {
			arg := inv.Args[i]
			if arg != "--safe-mode" {
				args = append(args, arg)
			}
			if arg == "--output-format" {
				args = append(args, "stream-json")
				i++
			}
		}
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"sfl-source": map[string]any{"type": "stdio", "command": "/usr/bin/env", "args": arguments, "alwaysLoad": true}}})
		inv.Environment = append(inv.Environment, "ENABLE_TOOL_SEARCH=false")
		args = append(args, "--input-format", "stream-json", "--verbose", "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--mcp-config", string(config), "--allowedTools", "mcp__sfl-source__read,mcp__sfl-source__search", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--max-turns", strconv.Itoa(s.Limits.MaxTurns))
		inv.Args = args
	default:
		return Invocation{}, errors.New("unsupported source provider")
	}
	inv.SourceEndpoint = &endpoint
	return inv, nil
}

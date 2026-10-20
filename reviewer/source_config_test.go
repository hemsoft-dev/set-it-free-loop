package reviewer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func sourceProxyCommand(t *testing.T, inv Invocation) (string, []string) {
	t.Helper()
	if inv.Provider == Codex {
		values := map[string]string{}
		for i := 0; i < len(inv.Args)-1; i++ {
			if inv.Args[i] == "-c" {
				parts := strings.SplitN(inv.Args[i+1], "=", 2)
				if len(parts) == 2 {
					values[parts[0]] = parts[1]
				}
			}
		}
		var command string
		var arguments []string
		if json.Unmarshal([]byte(values["mcp_servers.sfl-source.command"]), &command) != nil || json.Unmarshal([]byte(values["mcp_servers.sfl-source.args"]), &arguments) != nil {
			t.Fatal("invalid stdio source command", values)
		}
		if values["mcp_servers.sfl-source.required"] != "true" || values["mcp_servers.sfl-source.enabled_tools"] != `["read","search"]` || values["mcp_servers.sfl-source.env_vars"] != "[]" || values["mcp_servers.sfl-source.default_tools_approval_mode"] != `"auto"` {
			t.Fatal("unbounded/optional source configuration", values)
		}
		return command, arguments
	}
	for i, arg := range inv.Args {
		if arg != "--mcp-config" {
			continue
		}
		var config struct {
			Servers map[string]struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if i+1 == len(inv.Args) || json.Unmarshal([]byte(inv.Args[i+1]), &config) != nil || len(config.Servers) != 1 || config.Servers["sfl-source"].Type != "stdio" {
			t.Fatal("invalid Claude stdio server config")
		}
		server := config.Servers["sfl-source"]
		return server.Command, server.Args
	}
	t.Fatal("no source configuration")
	return "", nil
}
func argumentValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
func TestSourceClientManifestsBindExactlyOneSession(t *testing.T) {
	for _, provider := range []Provider{Codex, Claude} {
		t.Run(string(provider), func(t *testing.T) {
			session := testSession(provider)
			reader, _ := brokerReader(session.Snapshot)
			bridge := &SourceBridge{Program: `/opt/sfl/source"proxy`, Directory: "/run/sfl", ReaderUID: 1201}
			a := Adapter{Provider: provider, AuthDirectory: t.TempDir(), Reader: reader, SourceBridge: bridge}
			inv, err := a.Invocation(session, nil)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := inv.SourceEndpoint
			if endpoint == nil || endpoint.SessionHash != Digest(session) || endpoint.ReaderUID != 1201 || endpoint.RequiredProfile != "isolated-source-profile-1" || endpoint.Socket != filepath.Join(bridge.Directory, Digest(session), "source.sock") {
				t.Fatal("unbound source endpoint", endpoint)
			}
			command, args := sourceProxyCommand(t, inv)
			if command != "/usr/bin/env" || len(args) != 9 || args[0] != "-i" || args[1] != "LANG=C.UTF-8" || args[2] != bridge.Program || args[3] != "--socket" || args[4] != endpoint.Socket || args[5] != "--reader-uid" || args[6] != "1201" || args[7] != "--limits" {
				t.Fatal("source helper inherited ambient environment or wrong endpoint", command, args)
			}
			var limits Limits
			if json.Unmarshal([]byte(args[8]), &limits) != nil || limits != session.Limits {
				t.Fatal("proxy budgets differ from session")
			}
			if provider == Codex && inv.Args[len(inv.Args)-1] != "-" {
				t.Fatal("source options consumed stdin marker")
			}
			if provider == Claude {
				for _, arg := range inv.Args {
					if arg == "--safe-mode" || arg == "--bare" {
						t.Fatal("source path has incompatible mode", arg)
					}
				}
				for flag, expected := range map[string]string{"--tools": "", "--setting-sources": "", "--settings": `{"disableAllHooks":true}`, "--permission-mode": "dontAsk", "--permission-prompts": "none", "--max-turns": "24", "--allowedTools": "mcp__sfl-source__read,mcp__sfl-source__search"} {
					if got, ok := argumentValue(inv.Args, flag); !ok || got != expected {
						t.Fatal("missing source control", flag, got)
					}
				}
			}
			second := session
			second.Packet = "different captured evidence"
			second.PacketHash = Digest(second.Packet)
			other, err := a.Invocation(second, nil)
			if err != nil || other.SourceEndpoint.Socket == endpoint.Socket {
				t.Fatal("endpoint reused for different session", err)
			}
		})
	}
}
func TestSourceClientConfigurationRejectsInvalidTrustedInputs(t *testing.T) {
	s := testSession(Codex)
	reader, _ := brokerReader(s.Snapshot)
	for _, bridge := range []SourceBridge{
		{Program: "relative", Directory: "/run/sfl", ReaderUID: 1001},
		{Program: "/opt/source", Directory: "/run/../sfl", ReaderUID: 1001},
		{Program: "/opt/source", Directory: "/" + strings.Repeat("a", 60), ReaderUID: 1001},
		{Program: "/opt/source\n--bad", Directory: "/run/sfl", ReaderUID: 1001},
		{Program: "/opt/source", Directory: "/run/sfl", ReaderUID: 0},
	} {
		a := Adapter{Provider: Codex, AuthDirectory: t.TempDir(), Reader: reader, SourceBridge: &bridge}
		if _, err := a.Invocation(s, nil); err == nil {
			t.Fatal("invalid source bridge accepted", bridge)
		}
	}
	bridge := SourceBridge{Program: "/opt/source", Directory: "/run/sfl", ReaderUID: 1001}
	if _, err := (Adapter{Provider: Codex, AuthDirectory: t.TempDir(), SourceBridge: &bridge}).Invocation(s, nil); err == nil {
		t.Fatal("source configuration without captured reader accepted")
	}
}

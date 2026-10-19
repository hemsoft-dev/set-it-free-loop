package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

var ErrLiveBlocked = errors.New("live adapter blocked: provider support, billing, isolated launcher and owner activation evidence required")

type Invocation struct {
	Provider    Provider
	Args        []string
	Environment []string
	Input       []byte
	OutputLimit int
}

// Driver is a trusted transport seam for a separately qualified isolated worker.
// No production driver is supplied. Fake drivers never run a provider client.
type Driver interface {
	Inspect(context.Context, Invocation) (Identity, error)
	Execute(context.Context, Invocation) ([]byte, Identity, error)
}

type DisabledLiveDriver struct{}

func (DisabledLiveDriver) Inspect(context.Context, Invocation) (Identity, error) {
	return Identity{}, ErrLiveBlocked
}
func (DisabledLiveDriver) Execute(context.Context, Invocation) ([]byte, Identity, error) {
	return nil, Identity{}, ErrLiveBlocked
}

type Adapter struct {
	Provider      Provider
	Driver        Driver
	AuthDirectory string
}

func (a Adapter) Invocation(s Session, environment map[string]string) (Invocation, error) {
	if err := s.Validate(); err != nil {
		return Invocation{}, err
	}
	if a.Provider != s.Identity.Provider || !filepath.IsAbs(a.AuthDirectory) {
		return Invocation{}, errors.New("invalid adapter identity or auth directory")
	}
	input, err := json.Marshal(s)
	if err != nil || len(input) > s.Limits.InputBytes {
		return Invocation{}, errors.New("session exceeds input bound")
	}
	allowed := map[string]bool{"LANG": true, "LC_ALL": true, "TZ": true}
	for k := range environment {
		if !allowed[k] {
			return Invocation{}, errors.New("unexpected environment field")
		}
	}
	env := map[string]string{"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC"}
	for k, v := range environment {
		if strings.ContainsRune(v, 0) {
			return Invocation{}, errors.New("invalid environment value")
		}
		env[k] = v
	}
	var args []string
	switch a.Provider {
	case Codex:
		env["CODEX_HOME"] = a.AuthDirectory
		args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--json", "--model", s.Identity.Model, "-c", "features.shell_tool=false", "-c", "features.unified_exec=false", "-c", "features.hooks=false", "-c", "features.plugins=false", "-c", "features.apps=false", "-c", "features.multi_agent=false", "-c", "web_search=\"disabled\"", "-"}
	case Claude:
		env["CLAUDE_CONFIG_DIR"] = a.AuthDirectory
		args = []string{"--print", "--safe-mode", "--restricted", "--tools", "", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--output-format", "json", "--model", s.Identity.Model}
	default:
		return Invocation{}, errors.New("unsupported provider")
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var values []string
	for _, k := range keys {
		values = append(values, k+"="+env[k])
	}
	return Invocation{a.Provider, args, values, input, s.Limits.OutputBytes}, nil
}

func (a Adapter) Review(ctx context.Context, s Session) (Report, error) {
	inv, err := a.Invocation(s, nil)
	if err != nil {
		return Report{}, err
	}
	if a.Driver == nil {
		return Report{}, ErrLiveBlocked
	}
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	defer cancel()
	identity, err := a.Driver.Inspect(ctx, inv)
	if err != nil {
		return Report{}, err
	}
	if identity != s.Identity {
		return Report{}, errors.New("subscription auth/client/model precheck failed")
	}
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	data, identity, err := a.Driver.Execute(ctx, inv)
	if err != nil {
		return Report{}, err
	}
	if ctx.Err() != nil {
		return Report{}, ctx.Err()
	}
	if identity != s.Identity {
		return Report{}, errors.New("actual client/model/auth identity differs from captured job")
	}
	r, err := DecodeStrict[Report](data, s.Limits.OutputBytes)
	if err != nil {
		return Report{}, err
	}
	if err := ValidateReport(r, s); err != nil {
		return Report{}, err
	}
	return r, nil
}

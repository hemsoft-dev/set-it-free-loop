package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

// This executable fixture is test-only. It is neither an official-client
// launcher nor evidence of OS/egress confinement for credential-bearing clients.
type sourceProcessDriver struct {
	identity Identity
	binary   string
}

func (d sourceProcessDriver) Inspect(context.Context, Invocation) (Identity, error) {
	return d.identity, nil
}

type sourceProcessMessage struct {
	Kind, Revision, Path string
	Report               json.RawMessage
}

func (d sourceProcessDriver) Execute(ctx context.Context, inv Invocation) ([]byte, Identity, error) {
	cmd := exec.CommandContext(ctx, d.binary, "-test.run=^TestFakeSourceWorkerExecutable$")
	cmd.Env = []string{"LANG=C.UTF-8", "SFL_FAKE_SOURCE_WORKER=1"}
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, d.identity, err
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, d.identity, err
	}
	if err = cmd.Start(); err != nil {
		return nil, d.identity, PhaseError{State: "provider_unavailable", BeforeWork: true}
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if err = json.NewEncoder(input).Encode(inv); err != nil {
		return nil, d.identity, err
	}
	scan := bufio.NewScanner(output)
	scan.Buffer(make([]byte, 4096), inv.OutputLimit+4096)
	for count := 0; count < 8 && scan.Scan(); count++ {
		var message sourceProcessMessage
		decode := json.NewDecoder(bytes.NewReader(scan.Bytes()))
		decode.DisallowUnknownFields()
		if err := decode.Decode(&message); err != nil {
			return nil, d.identity, err
		}
		if err := decode.Decode(&struct{}{}); err != io.EOF {
			return nil, d.identity, errors.New("extra fake worker message content")
		}
		switch message.Kind {
		case "read":
			if inv.Source == nil {
				return nil, d.identity, errors.New("fixture has no source capability")
			}
			text, err := inv.Source.Read(ctx, message.Revision, message.Path)
			if err != nil {
				return nil, d.identity, err
			}
			if err = json.NewEncoder(input).Encode(text); err != nil {
				return nil, d.identity, err
			}
		case "report":
			err := cmd.Wait()
			waited = true
			return message.Report, d.identity, err
		default:
			return nil, d.identity, errors.New("unexpected fake worker message")
		}
	}
	return nil, d.identity, fmt.Errorf("fake worker did not finish a bounded report: %v", scan.Err())
}

func TestFakeSourceWorkerExecutable(t *testing.T) {
	if os.Getenv("SFL_FAKE_SOURCE_WORKER") != "1" {
		return
	}
	code := func() int {
		for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK", "HOME"} {
			if os.Getenv(key) != "" {
				return 3
			}
		}
		decode := json.NewDecoder(os.Stdin)
		var inv Invocation
		if err := decode.Decode(&inv); err != nil {
			return 4
		}
		var session Session
		if err := json.Unmarshal(inv.Input[bytes.IndexByte(inv.Input, '\n')+1:], &session); err != nil {
			return 5
		}
		encode := json.NewEncoder(os.Stdout)
		if err := encode.Encode(sourceProcessMessage{Kind: "read", Revision: session.Snapshot.Head, Path: "worker.go", Report: json.RawMessage("{}")}); err != nil {
			return 6
		}
		var text string
		if err := decode.Decode(&text); err != nil || text != "worker-only evidence" {
			return 7
		}
		raw, _ := json.Marshal(fakeReport(session))
		if err := encode.Encode(sourceProcessMessage{Kind: "report", Report: raw}); err != nil {
			return 8
		}
		return 0
	}()
	os.Exit(code)
}

func TestBothFakeExecutableWorkersUseAuditedSourceTools(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SSH_AUTH_SOCK"} {
		t.Setenv(key, "source-process-secret-canary")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	job.Config.Mode = IndependentPair
	e := Engine{Reader: brokerReader, Reviewers: map[Provider]Reviewer{}}
	for _, provider := range []Provider{Codex, Claude} {
		identity, _ := PinnedIdentity(provider)
		r, _ := brokerReader(job.Snapshot)
		e.Reviewers[provider] = Adapter{Provider: provider, Driver: sourceProcessDriver{identity, binary}, AuthDirectory: t.TempDir(), Reader: r}
	}
	out, err := e.Run(context.Background(), job, Checkpoint{})
	if err != nil || out.State != "dual_complete" || len(out.Artifacts) != 2 {
		t.Fatal("actual fake executable failed source contract", out, err)
	}
	for _, a := range out.Artifacts {
		if a.SourceAccess == nil || a.SourceAccess.ToolCalls != 1 || len(a.SourceAccess.Reads) != 1 || a.SourceAccess.Reads[0].Path != "worker.go" || len(a.ValidationReads) != 0 {
			t.Fatal("fake executable access was replaced by verifier reads", a)
		}
	}
}

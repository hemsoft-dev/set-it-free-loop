//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This helper serves only a small trusted synthetic repo through the existing
// fake TLS installation transport and Unix broker. Admission is intentionally
// synthetic, not a production helper/unit qualification policy.
func gitAcquisitionBackend(t *testing.T, fixture snapshotGitFixture) (GitReadEndpoint, func() gitBrokerTestResult) {
	t.Helper()
	fixture.run(t, "update-ref", "refs/heads/base", fixture.snapshot.Base)
	command := exec.Command(fixture.git, "--exec-path")
	location, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(location)), "git-http-backend")
	root := t.TempDir()
	repo := filepath.Join(root, "hemsoft-dev", "set-it-free-loop.git")
	if os.MkdirAll(filepath.Dir(repo), 0700) != nil {
		t.Fatal("backend directory")
	}
	clone := exec.Command(fixture.git, "clone", "--bare", "--no-hardlinks", fixture.dir, repo)
	clone.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	if raw, err := clone.CombinedOutput(); err != nil {
		t.Fatal("synthetic backend", err, string(raw))
	}
	installation, _ := gitReadFixture(t, GitHubReader, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		env := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C", "GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_PROTOCOL=" + q.Header.Get("Git-Protocol")}
		handler := &cgi.Handler{Path: backend, Env: env, Stderr: io.Discard}
		handler.ServeHTTP(w, q)
	}))
	source, err := NewGitHubGitReader(installation)
	if err != nil {
		t.Fatal(err)
	}
	return startGitBrokerTest(t, source, func(context.Context, GitReadPeer) error { return nil })
}

func TestGitAcquisitionActualFetchVerifyAndExport(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			fixture := snapshotGitFixtureFormat(t, format)
			endpoint, finish := gitAcquisitionBackend(t, fixture)
			workspace := t.TempDir()
			if os.Chmod(workspace, 0700) != nil {
				t.Fatal("workspace")
			}
			binding := gitMaterializationBinding{strings.Repeat("1", 32), strings.Repeat("2", 64), strings.Repeat("3", 64), fixture.snapshot}
			var exported bytes.Buffer
			if err := acquireGitMaterialization(context.Background(), &exported, fixture.git, workspace, endpoint, binding, DefaultGitAcquisitionBounds()); err != nil {
				t.Fatal("actual synthetic fetch and materialization", err)
			}
			// Successful acquisition remains spent, including a completed export.
			var duplicate bytes.Buffer
			if acquireGitMaterialization(context.Background(), &duplicate, fixture.git, workspace, endpoint, binding, DefaultGitAcquisitionBounds()) == nil || duplicate.Len() != 0 {
				t.Fatal("completed workspace replayed")
			}
			result := finish()
			audit := result.audit
			if !audit.Stopped || audit.Failed || audit.Requests < 2 || audit.Rejected != 0 || audit.ResponseBytes == 0 {
				t.Fatal("broker did not serve exact acquisition", audit)
			}
			// Remove the original and fetched packs before restoring plain data.
			// No remaining host Git parser participates in ObjectStore reads.
			if os.RemoveAll(workspace) != nil || os.RemoveAll(fixture.dir) != nil {
				t.Fatal("remove trusted synthetic repos")
			}
			store, err := receiveGitMaterialization(context.Background(), io.NopCloser(bytes.NewReader(exported.Bytes())), binding)
			if err != nil {
				t.Fatal("plain materialization restore", err)
			}
			defer store.Close()
			job := testJob()
			job.Snapshot, job.Packet = binding.Snapshot, store.packet
			bound, err := restoreGitMaterialization(context.Background(), store, job)
			if err != nil {
				t.Fatal("restore", err)
			}
			evidence := bound.evidence
			if evidence.snapshot != binding.Snapshot || len(evidence.Paths()) == 0 {
				t.Fatal("wrong immutable snapshot")
			}
			read := false
			for _, path := range evidence.Paths() {
				object, err := store.Read(context.Background(), binding.Snapshot.Head, path, 2<<20)
				if err == nil && len(object.Data) > 0 {
					read = true
				}
			}
			if !read {
				t.Fatal("no restored source")
			}
		})
	}
}

func TestGitAcquisitionFailedFetchAndVerificationAreSpentWithoutExport(t *testing.T) {
	// A merge base that is a complete, different Git answer, and an object,
	// commit or byte overrun, are permanent refusal records instead
	// (TestGitAcquisitionRefusalCrossesWorkerExport). The filesystem entry
	// bound stays opaque.
	for _, kind := range []string{"missing-tip", "non-commit-merge-base", "file-limit"} {
		t.Run(kind, func(t *testing.T) {
			fixture := snapshotGitFixtureFormat(t, "sha1")
			endpoint, finish := gitAcquisitionBackend(t, fixture)
			binding := gitMaterializationBinding{strings.Repeat("1", 32), strings.Repeat("2", 64), strings.Repeat("3", 64), fixture.snapshot}
			bounds := DefaultGitAcquisitionBounds()
			switch kind {
			case "missing-tip":
				binding.Snapshot.Head = strings.Repeat("a", 40)
			case "non-commit-merge-base":
				binding.Snapshot.MergeBase = fixture.run(t, "rev-parse", binding.Snapshot.Head+"^{tree}")
			case "file-limit":
				bounds.Files = 1
			}
			workspace := t.TempDir()
			os.Chmod(workspace, 0700)
			var output bytes.Buffer
			if acquireGitMaterialization(context.Background(), &output, fixture.git, workspace, endpoint, binding, bounds) == nil || output.Len() != 0 {
				t.Fatal("failed acquisition exported source")
			}
			marker, err := os.ReadFile(filepath.Join(workspace, "acquisition.used"))
			if err != nil || string(marker) != Digest(binding)+"\n" {
				t.Fatal("missing spent intent", err)
			}
			if acquireGitMaterialization(context.Background(), &output, fixture.git, workspace, endpoint, binding, DefaultGitAcquisitionBounds()) == nil || output.Len() != 0 {
				t.Fatal("failed acquisition replayed")
			}
			if result := finish(); !result.audit.Stopped || result.audit.Requests == 0 {
				t.Fatal("missing actual fetch evidence", result)
			}
		})
	}
}

func TestGitAcquisitionRejectsDestinationsAndWorkspaceBeforeFetch(t *testing.T) {
	fixture := snapshotGitFixtureFormat(t, "sha1")
	binding := gitMaterializationBinding{strings.Repeat("1", 32), strings.Repeat("2", 64), strings.Repeat("3", 64), fixture.snapshot}
	for _, kind := range []string{"remote-url", "proxy-query", "proxy-fragment", "proxy-relative", "proxy-other-name", "invalid-binding", "invalid-bounds", "cancelled", "nonempty", "symlink", "nil-output"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			os.Chmod(workspace, 0700)
			endpoint := GitReadEndpoint{"/broker/git.sock", gitReadLocalURL}
			b, bounds, ctx := binding, DefaultGitAcquisitionBounds(), context.Background()
			switch kind {
			case "remote-url":
				endpoint.URL = "https://evil.invalid/repo"
			case "proxy-query":
				endpoint.Socket += "?x"
			case "proxy-fragment":
				endpoint.Socket += "#x"
			case "proxy-relative":
				endpoint.Socket = "git.sock"
			case "proxy-other-name":
				endpoint.Socket = "/broker/other.sock"
			case "invalid-binding":
				b.Attempt = "wrong"
			case "invalid-bounds":
				bounds.Objects = 0
			case "cancelled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "nonempty":
				os.WriteFile(filepath.Join(workspace, "candidate"), []byte("inert"), 0600)
			case "symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				os.Symlink(workspace, alias)
				workspace = alias
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if kind == "nil-output" {
				writer = nil
			}
			if acquireGitMaterialization(ctx, writer, fixture.git, workspace, endpoint, b, bounds) == nil || output.Len() != 0 {
				t.Fatal("invalid ingress accepted")
			}
			if _, err := os.Stat(filepath.Join(workspace, "acquisition.used")); !os.IsNotExist(err) {
				t.Fatal("invalid ingress reached acquisition intent", err)
			}
		})
	}
}

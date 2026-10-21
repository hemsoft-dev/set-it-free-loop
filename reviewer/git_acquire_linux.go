//go:build linux

package reviewer

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// acquireGitQuarantine is private worker-side plumbing. Its caller MUST already
// be inside the qualified acquisition unit and hold its one-use worker grant.
// Fetch, index-pack, fsck, graph checks, diff and blob export all stay in that
// same boundary. This function cannot qualify a unit or publish source.
// Direct tests use only small trusted synthetic repositories and fake backends.
func acquireGitQuarantine(ctx context.Context, git, workspace string, endpoint GitReadEndpoint, binding gitMaterializationBinding, bounds GitAcquisitionBounds) (GitStore, error) {
	zero := GitStore{}
	if ctx == nil || ctx.Err() != nil || !binding.valid() || !bounds.valid() ||
		!filepath.IsAbs(git) || filepath.Clean(git) != git ||
		!filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || privateDirectory(workspace) != nil ||
		endpoint.URL != gitReadLocalURL || !filepath.IsAbs(endpoint.Socket) || filepath.Clean(endpoint.Socket) != endpoint.Socket ||
		filepath.Base(endpoint.Socket) != "git.sock" || len(endpoint.Socket) > 107 || strings.ContainsAny(endpoint.Socket, "\x00\r\n?#%") {
		return zero, ErrGitQuarantine
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil || resolved != workspace {
		return zero, ErrGitQuarantine
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return zero, ErrGitQuarantine
	}
	entries, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if readErr != io.EOF || closeErr != nil || len(entries) != 0 {
		return zero, ErrGitQuarantine
	}
	// No retry after any command, lost reply or partial pack. Preserve a spent
	// marker before making a repository, and retain partial bytes for stop.
	marker, err := root.OpenFile("acquisition.used", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return zero, ErrGitQuarantine
	}
	_, writeErr := marker.WriteString(Digest(binding) + "\n")
	syncErr := marker.Sync()
	closeErr = marker.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || root.Mkdir("quarantine", 0700) != nil {
		return zero, ErrGitQuarantine
	}
	// Construct exactly the bare metadata we will later verify. Do not invoke
	// init templates, copy local objects, checkout files or read candidate config.
	for _, name := range []string{"quarantine/objects", "quarantine/objects/pack", "quarantine/objects/info", "quarantine/refs", "quarantine/refs/sfl"} {
		if root.Mkdir(name, 0700) != nil {
			return zero, ErrGitQuarantine
		}
	}
	for _, item := range []struct{ name, body string }{
		{"config", gitAcquisitionConfig(binding.Snapshot)},
		{"HEAD", binding.Snapshot.Head + "\n"},
	} {
		file, err := root.OpenFile("quarantine/"+item.name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return zero, ErrGitQuarantine
		}
		_, err = file.WriteString(item.body)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return zero, ErrGitQuarantine
		}
	}
	store := GitStore{git, filepath.Join(workspace, "quarantine")}
	// Only the fixed broker URL is reachable. Request both immutable tips,
	// including complete history; no shallow/filter/promisor path is accepted.
	// The broker authenticates the actual measured HTTP helper independently.
	err = gitAcquisitionFetch(ctx, store,
		"-c", "protocol.allow=never", "-c", "protocol.http.allow=always", "-c", "protocol.version=2",
		"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "fetch.unpackLimit=1",
		"-c", "http.proxy=socks5h://localhost"+endpoint.Socket,
		"-c", "http.followRedirects=false", "-c", "http.postBuffer=1048576",
		"fetch", "--no-auto-maintenance", "--no-write-fetch-head", "--no-tags", "--no-recurse-submodules",
		endpoint.URL, binding.Snapshot.Head+":refs/sfl/head", binding.Snapshot.Base+":refs/sfl/base")
	if err != nil || ctx.Err() != nil {
		return zero, ErrGitQuarantine
	}
	if _, err := verifyGitQuarantine(ctx, store, binding.Snapshot, bounds); err != nil {
		return zero, ErrGitQuarantine
	}
	return store, nil
}

// Fetch is deliberately separate from the read-only GitStore command path.
// Its environment suppresses every inherited credential/askpass/proxy setting
// and terminal prompting. Transport destination and refspecs are fixed above.
func gitAcquisitionFetch(ctx context.Context, store GitStore, args ...string) error {
	args = append([]string{"--no-replace-objects", "--no-lazy-fetch", "--git-dir=" + store.GitDir}, args...)
	command := exec.CommandContext(ctx, store.Git, args...)
	command.Dir = store.GitDir
	command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C"}
	command.Stdout = &cappedOutput{max: 2048}
	command.Stderr = &cappedOutput{max: 2048}
	if command.Run() != nil {
		return ErrGitQuarantine
	}
	return nil
}

// acquireGitMaterialization never returns an ObjectStore or a host path. Its
// plain export is untrusted staged data until the supervisor authenticates the
// producer, revokes/drains the broker, proves whole-unit stop and publishes it.
func acquireGitMaterialization(ctx context.Context, output io.Writer, git, workspace string, endpoint GitReadEndpoint, binding gitMaterializationBinding, bounds GitAcquisitionBounds) error {
	if output == nil {
		return ErrGitQuarantine
	}
	store, err := acquireGitQuarantine(ctx, git, workspace, endpoint, binding, bounds)
	if err != nil {
		return err
	}
	return writeGitMaterialization(ctx, output, store, binding, bounds)
}

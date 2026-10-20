package reviewer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrSourceService = errors.New("private source service failed")

// SourceReaderConfig is controller input on a private pipe, never a model tool.
// The guest's immutable object repository is fixed at /source, with a trusted
// git at /usr/bin/git. No arbitrary executable, GitHub or provider auth is read.
type SourceReaderConfig struct {
	LaunchID  string   `json:"launch_id"`
	Session   Session  `json:"session"`
	Paths     []string `json:"paths"`
	Root      string   `json:"root"`
	ReaderUID int      `json:"reader_uid"`
	WorkerUID int      `json:"worker_uid"`
	WorkerGID int      `json:"worker_gid"`
}

// SourceReaderEvent travels only to the trusted supervisor's audit pipe.
// Ready is not inference authorization; stopped contains actual broker evidence.
// No raw source, native transcript, credential or backend diagnostic is emitted.
type SourceReaderEvent struct {
	Version      int           `json:"version"`
	Kind         string        `json:"kind"`
	SessionHash  string        `json:"session_hash"`
	ManifestHash string        `json:"manifest_hash"`
	LaunchID     string        `json:"launch_id"`
	Socket       string        `json:"socket,omitempty"`
	ReaderUID    int           `json:"reader_uid,omitempty"`
	Failed       bool          `json:"failed"`
	Access       *SourceAccess `json:"access,omitempty"`
}

func (c SourceReaderConfig) validate() error {
	_, tokenErr := hex.DecodeString(c.LaunchID)
	if len(c.LaunchID) != 32 || strings.ToLower(c.LaunchID) != c.LaunchID || tokenErr != nil || c.Session.Validate() != nil || !sourcePath(c.Root) || c.ReaderUID <= 0 || c.WorkerUID <= 0 || c.WorkerUID == c.ReaderUID || c.WorkerGID <= 0 || uint64(c.ReaderUID) > uint64(^uint32(0)) || uint64(c.WorkerUID) > uint64(^uint32(0)) || uint64(c.WorkerGID) > uint64(^uint32(0)) || len(c.Paths) > 4096 {
		return ErrSourceService
	}
	seen := map[string]bool{}
	for _, path := range c.Paths {
		if !validPath(path) || seen[path] {
			return ErrSourceService
		}
		seen[path] = true
	}
	if len(filepath.Join(c.Root, Digest(c.Session), "source.sock")) > 107 {
		return ErrSourceService
	}
	raw, err := json.Marshal(c.Session)
	if err != nil || len(raw) > c.Session.Limits.InputBytes {
		return ErrSourceService
	}
	return nil
}

// SourceReaderMain supplies the separately launched reader program. The
// supervisor must already have set its UID/GID, mounts and private environment.
// It creates no accounts, namespaces, credentials, App or persistent daemon.
func SourceReaderMain(ctx context.Context, environment []string, input io.ReadCloser, output io.WriteCloser) int {
	return SourceReaderMainWithStore(ctx, environment, input, output, GitStore{Git: "/usr/bin/git", GitDir: "/source"})
}

// SourceReaderMainWithStore supports the native guest's private object channel.
// The store is supplied by trusted launch code, never its input manifest. The
// same environment, manifest, identity, socket and audit checks still apply.
func SourceReaderMainWithStore(ctx context.Context, environment []string, input io.ReadCloser, output io.WriteCloser, store ObjectStore) int {
	if input == nil || output == nil || store == nil {
		return 2
	}
	var inputClosed, outputClosed sync.Once
	closeInput := func() { inputClosed.Do(func() { _ = input.Close() }) }
	closeOutput := func() { outputClosed.Do(func() { _ = output.Close() }) }
	defer closeInput()
	defer closeOutput()
	for _, value := range environment {
		key, data, ok := strings.Cut(value, "=")
		if !ok || (key != "LANG" && key != "LC_ALL" && key != "TZ") || len(data) > 64 || strings.ContainsRune(data, 0) {
			return 2
		}
	}
	// Intake is separately bounded before the captured phase deadline is known.
	intake, stopIntake := context.WithTimeout(ctx, 5*time.Second)
	stopInput := context.AfterFunc(intake, closeInput)
	raw, err := io.ReadAll(io.LimitReader(input, (16<<20)+1))
	intakeErr := intake.Err()
	stopInput()
	stopIntake()
	if err != nil || intakeErr != nil || len(raw) > 16<<20 || ctx.Err() != nil {
		return 2
	}
	var config SourceReaderConfig
	if json.Unmarshal(raw, &config) != nil {
		return 2
	}
	canonical, err := json.Marshal(config)
	if err != nil || string(canonical) != string(raw) || config.validate() != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, config.Session.Limits.Timeout)
	defer cancel()
	stopOutput := context.AfterFunc(ctx, closeOutput)
	defer stopOutput()
	reader, err := NewReader(store, config.Session.Snapshot, config.Paths, config.Session.Limits)
	if err != nil {
		return 2
	}
	auditBytes := 0
	emit := func(event SourceReaderEvent) error {
		raw, err := json.Marshal(event)
		auditBytes += len(raw) + 1
		if err != nil || auditBytes > 2<<20 {
			return ErrSourceService
		}
		raw = append(raw, '\n')
		if n, err := output.Write(raw); err != nil || n != len(raw) {
			return ErrSourceService
		}
		return nil
	}
	access, err := ServeSourceSocket(ctx, config, reader, func(endpoint SourceEndpoint) error {
		return emit(SourceReaderEvent{Version: 1, Kind: "ready", SessionHash: endpoint.SessionHash, ManifestHash: Digest(config), LaunchID: config.LaunchID, Socket: endpoint.Socket, ReaderUID: endpoint.ReaderUID})
	})
	if access == nil || emit(SourceReaderEvent{Version: 1, Kind: "stopped", SessionHash: Digest(config.Session), ManifestHash: Digest(config), LaunchID: config.LaunchID, Failed: err != nil, Access: access}) != nil || err != nil {
		return 1
	}
	return 0
}

package reviewer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"sync"
)

// SourceReaderProcess is the private reader role, never a provider transport.
// Stop must confirm/reap its entire containment and honor ctx. It must allow a
// normal stopped audit to drain before forcing shutdown, and leave Audit open
// for the caller to consume EOF. Wait has one owner and returns the reader exit
// status. The supervisor implementation must support concurrent Wait and Stop.
type SourceReaderProcess interface {
	Input() io.WriteCloser
	Audit() io.ReadCloser
	Wait() error
	Stop(context.Context) error
}

// SourceReaderLauncher must persist the launch/config/process recovery binding
// before creating a process. It supplies stripped reader identity/environment,
// protected mounts/socket ownership and a private audit pipe. Return a process
// whenever one may exist, even on error; nil attests no process started. No OS
// launcher, actual identity provisioning or credential access is selected here.
type SourceReaderLauncher interface {
	StartReader(context.Context, SourceReaderConfig) (SourceReaderProcess, error)
}

// ManagedSourceReviewer connects a separate reader's private audit to the
// existing NativeDriver and durable SourceReviewer contract. Each call owns a
// fresh supervisor and launch nonce. Reader remains the independent citation
// verifier; it never supplies the worker access receipt in this path.
type ManagedSourceReviewer struct {
	Adapter   Adapter
	Readers   SourceReaderLauncher
	WorkerUID int
	WorkerGID int
}

func (r ManagedSourceReviewer) Review(ctx context.Context, s Session) (Report, error) {
	report, _, err := r.ReviewSource(ctx, s)
	return report, err
}

func (r ManagedSourceReviewer) ReviewSource(ctx context.Context, s Session) (Report, *SourceAccess, error) {
	blocked := func() (Report, *SourceAccess, error) {
		return Report{}, nil, errors.Join(PhaseError{State: "blocked", BeforeWork: true}, ErrLiveBlocked)
	}
	if s.Validate() != nil || r.Readers == nil || r.Adapter.Reader == nil || r.Adapter.SourceBridge == nil {
		return blocked()
	}
	var client WorkerLauncher
	switch d := r.Adapter.Driver.(type) {
	case NativeDriver:
		client = d.Launcher
	case *NativeDriver:
		if d != nil {
			client = d.Launcher
		}
	}
	if client == nil {
		return blocked()
	}
	inv, err := r.Adapter.Invocation(s, nil)
	if err != nil {
		return Report{}, nil, adapterValidation(err, true)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return blocked()
	}
	paths := make([]string, 0, len(r.Adapter.Reader.paths))
	for path := range r.Adapter.Reader.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	config := SourceReaderConfig{LaunchID: hex.EncodeToString(nonce[:]), Session: s, Paths: paths,
		Root: r.Adapter.SourceBridge.Directory, ReaderUID: r.Adapter.SourceBridge.ReaderUID,
		WorkerUID: r.WorkerUID, WorkerGID: r.WorkerGID}
	if config.validate() != nil || !readerInvocationMatches(config, inv) {
		return blocked()
	}
	ctx, cancel := context.WithTimeout(ctx, s.Limits.Timeout)
	defer cancel()
	supervisor := &sourceSupervisor{clients: client, readers: r.Readers, config: config}
	a := r.Adapter
	a.Driver = NativeDriver{Launcher: supervisor}
	report, err := a.review(ctx, s, nil)
	access, auditErr := supervisor.access()
	if auditErr != nil {
		return Report{}, access, errors.Join(err, auditErr)
	}
	if err != nil {
		return Report{}, access, err
	}
	if access == nil {
		return Report{}, nil, ErrSourceAuditPending
	}
	report, err = mergeCoverageRecords(report, access.Coverage)
	if err == nil {
		err = ValidateReport(report, s)
	}
	if data, encodeErr := json.Marshal(report); err == nil && (encodeErr != nil || len(data) > s.Limits.OutputBytes) {
		err = ErrSourceService
	}
	if err != nil {
		return Report{}, access, adapterValidation(ErrSourceService, false)
	}
	return report, access, nil
}

func readerInvocationMatches(c SourceReaderConfig, inv Invocation) bool {
	s, err := nativeSession(inv)
	return err == nil && Digest(s) == Digest(c.Session) && inv.Source == nil && inv.SourceEndpoint != nil &&
		inv.SourceEndpoint.SessionHash == Digest(c.Session) && inv.SourceEndpoint.ReaderUID == c.ReaderUID &&
		inv.SourceEndpoint.RequiredProfile == "isolated-source-profile-1" &&
		inv.SourceEndpoint.Socket == filepath.Join(c.Root, Digest(c.Session), "source.sock")
}

type sourceSupervisor struct {
	clients WorkerLauncher
	readers SourceReaderLauncher
	config  SourceReaderConfig
	worker  *managedSourceWorker
}

func (s *sourceSupervisor) Inspect(ctx context.Context, inv Invocation) (Identity, error) {
	if !readerInvocationMatches(s.config, inv) {
		return Identity{}, ErrSourceService
	}
	return s.clients.Inspect(ctx, inv)
}

func (s *sourceSupervisor) Start(ctx context.Context, inv Invocation) (Worker, error) {
	if s.worker != nil || !readerInvocationMatches(s.config, inv) || ctx.Err() != nil {
		return nil, ErrSourceService
	}
	process, err := s.readers.StartReader(ctx, s.config)
	if process == nil {
		return nil, ErrSourceService
	}
	w := &managedSourceWorker{reader: process, config: s.config}
	s.worker = w
	w.waitDone = make(chan error, 1)
	go func() { w.waitDone <- process.Wait() }()
	if process.Input() == nil || process.Audit() == nil {
		return w, ErrSourceService
	}
	w.auditDone = make(chan readerAuditResult, 1)
	ready := make(chan bool, 1)
	go readManagedAudit(process.Audit(), s.config, ready, w.auditDone)
	if err != nil {
		return w, ErrSourceService
	}
	manifest, err := json.Marshal(s.config)
	if err != nil || len(manifest) > 16<<20 {
		return w, ErrSourceService
	}
	written := make(chan bool, 1)
	go func() {
		n, err := process.Input().Write(manifest)
		closed := w.closeReaderInput()
		written <- err == nil && n == len(manifest) && closed == nil
	}()
	select {
	case ok := <-written:
		if !ok {
			return w, ErrSourceService
		}
	case <-ctx.Done():
		return w, ctx.Err()
	}
	select {
	case ok := <-ready:
		if !ok {
			return w, ErrSourceService
		}
	case <-ctx.Done():
		return w, ctx.Err()
	}
	w.client, err = s.clients.Start(ctx, inv)
	if err != nil || w.client == nil {
		return w, ErrSourceService
	}
	return w, nil
}

func (s *sourceSupervisor) access() (*SourceAccess, error) {
	if s.worker == nil {
		return nil, nil // validation/identity failure before any role started
	}
	w := s.worker
	w.mu.Lock()
	defer w.mu.Unlock()
	access := cloneSourceAccess(w.audit.Access)
	if !w.stopped {
		return access, ErrWorkerStopPending
	}
	if w.auditErr != nil || access == nil {
		return nil, ErrSourceAuditPending
	}
	if w.audit.Failed {
		return access, errors.Join(PhaseError{State: "provider_unavailable"}, ErrSourceService)
	}
	return access, nil
}

type readerAuditResult struct {
	raw []byte
	err error
}

func readManagedAudit(input io.Reader, c SourceReaderConfig, ready chan<- bool, done chan<- readerAuditResult) {
	bounded := bufio.NewReader(io.LimitReader(input, (2<<20)+1))
	first, firstErr := bounded.ReadBytes('\n')
	expected, _ := json.Marshal(SourceReaderEvent{Version: 1, Kind: "ready", SessionHash: Digest(c.Session), ManifestHash: Digest(c), LaunchID: c.LaunchID,
		Socket: filepath.Join(c.Root, Digest(c.Session), "source.sock"), ReaderUID: c.ReaderUID})
	ready <- firstErr == nil && bytes.Equal(first, append(expected, '\n'))
	rest, err := io.ReadAll(bounded)
	raw := append(first, rest...)
	if firstErr != nil || len(raw) > 2<<20 {
		err = ErrSourceService
	}
	done <- readerAuditResult{raw, err}
}

type managedSourceWorker struct {
	reader    SourceReaderProcess
	client    Worker
	config    SourceReaderConfig
	auditDone chan readerAuditResult
	waitDone  chan error
	stopOnce  sync.Once
	inputOnce sync.Once
	inputErr  error
	mu        sync.Mutex
	stopped   bool
	audit     SourceReaderAudit
	auditErr  error
	stopErr   error
}

type absentSourceInput struct{}

func (absentSourceInput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (absentSourceInput) Close() error              { return nil }

func (w *managedSourceWorker) Input() io.WriteCloser {
	if w.client == nil {
		return absentSourceInput{}
	}
	return w.client.Input()
}
func (w *managedSourceWorker) Output() io.ReadCloser {
	if w.client == nil {
		return io.NopCloser(bytes.NewReader(nil))
	}
	return w.client.Output()
}
func (w *managedSourceWorker) Wait() error {
	if w.client == nil {
		return ErrSourceService
	}
	return w.client.Wait()
}

func (w *managedSourceWorker) closeReaderInput() error {
	w.inputOnce.Do(func() {
		if w.reader.Input() != nil {
			w.inputErr = w.reader.Input().Close()
		}
	})
	return w.inputErr
}

func (w *managedSourceWorker) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		_ = w.closeReaderInput()
		// Both stops run even if one containment cannot confirm termination.
		// Reader.Stop must allow the final audit to drain while Client.Stop
		// closes/reaps the provider and its source-proxy descendants.
		stops := make(chan error, 2)
		go func() { stops <- w.reader.Stop(ctx) }()
		go func() {
			if w.client == nil {
				stops <- nil
				return
			}
			stops <- w.client.Stop(ctx)
		}()
		physical := true
		for range 2 {
			select {
			case err := <-stops:
				physical = physical && err == nil
			case <-ctx.Done():
				physical = false
			}
		}
		audit, auditErr := SourceReaderAudit{}, ErrSourceService
		if w.auditDone != nil {
			select {
			case result := <-w.auditDone:
				if result.err == nil {
					audit, auditErr = DecodeSourceReaderAudit(w.config, result.raw)
				}
			case <-ctx.Done():
			}
		}
		if w.reader.Audit() != nil {
			_ = w.reader.Audit().Close()
		}
		if w.waitDone != nil {
			select {
			case err := <-w.waitDone:
				if err != nil && !audit.Failed {
					auditErr = ErrSourceService
				}
			case <-ctx.Done():
				physical = false
			}
		} else {
			auditErr = ErrSourceService
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.stopped, w.audit, w.auditErr = physical, audit, auditErr
		if !physical {
			w.stopErr = ErrWorkerStopPending
		}
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopErr
}

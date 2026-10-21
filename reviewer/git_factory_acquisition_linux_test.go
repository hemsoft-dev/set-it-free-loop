//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// Explicit fake lifecycle. Wire validation, archive fsync/sealed blobs and the
// credential owner are real; this never claims physical containment admission.
type fakeGitFactoryAcquisition struct {
	archive                *gitExportArchive
	binding                gitMaterializationBinding
	wire                   []byte
	materialized           *gitMaterialization
	stopEntered, allowStop chan struct{}
	stopError              error
	closeError             error
	closed                 bool
	acquireError           error
	generation             *githubReaderGeneration
}

func (f *fakeGitFactoryAcquisition) retainGeneration(g *githubReaderGeneration) { f.generation = g }

func (f *fakeGitFactoryAcquisition) acquire(ctx context.Context, source *gitGenerationReader) (*gitMaterialization, error) {
	f.generation = source.generation
	m, err := f.archive.stage(ctx, f.binding, io.NopCloser(bytes.NewReader(f.wire)))
	f.materialized = m
	if err != nil {
		return nil, err
	}
	return m, f.acquireError
}
func (f *fakeGitFactoryAcquisition) stop(ctx context.Context) error {
	if f.stopEntered != nil {
		close(f.stopEntered)
		f.stopEntered = nil
	}
	if f.allowStop != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.allowStop:
		}
	}
	return f.stopError
}
func (f *fakeGitFactoryAcquisition) close() error { f.closed = true; return f.closeError }

func TestGitFactoryStagedExportCannotPublishBeforeStopAndLeaseRelease(t *testing.T) {
	wire, binding, _ := gitMaterializationFixture(t, "sha1")
	archive, _ := exportArchiveFixture(t)
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake pair qualification")
	}
	entered, allow := make(chan struct{}), make(chan struct{})
	f := &fakeGitFactoryAcquisition{archive: archive, binding: binding, wire: wire, stopEntered: entered, allowStop: allow}
	type result struct {
		m   *gitMaterialization
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := runGitFactoryAcquisition(context.Background(), owner, binding, f)
		done <- result{m, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("staged export never reached stop")
	}
	select {
	case result := <-done:
		if result.m != nil {
			result.m.Close()
		}
		t.Fatal("staged output escaped before physical stop", result.err)
	default:
	}
	owner.state.mu.Lock()
	owned, active := len(owner.state.acquisitions), len(owner.state.active)
	owner.state.mu.Unlock()
	if owned != 1 || active != 0 {
		t.Fatal("EOF retired whole generation before physical stop", owned, active)
	}
	if f.materialized == nil {
		t.Fatal("test never staged a complete valid export")
	}
	close(allow)
	resultValue := <-done
	if resultValue.err != nil || resultValue.m == nil || !f.closed {
		t.Fatal("proven stop did not publish", resultValue.err)
	}
	defer resultValue.m.Close()
	owner.state.mu.Lock()
	owned = len(owner.state.acquisitions)
	owner.state.mu.Unlock()
	if owned != 0 {
		t.Fatal("published source still owns acquisition lease")
	}
	if resultValue.m.manifest.Binding != binding || resultValue.m.packet == "" {
		t.Fatal("published different or empty source")
	}
}

func TestGitFactoryUnknownStopRetainsLeaseAndClosesStagedObjects(t *testing.T) {
	wire, binding, _ := gitMaterializationFixture(t, "sha256")
	archive, _ := exportArchiveFixture(t)
	owner, _, _, _, _ := credentialOwnerFixture(t)
	if owner.Refresh(context.Background()) != nil {
		t.Fatal("fake pair qualification")
	}
	f := &fakeGitFactoryAcquisition{archive: archive, binding: binding, wire: wire, stopError: errors.New("fake live broker/unit")}
	if m, err := runGitFactoryAcquisition(context.Background(), owner, binding, f); err == nil || m != nil {
		t.Fatal("unknown stop published source")
	}
	owner.state.mu.Lock()
	owned := len(owner.state.acquisitions)
	owner.state.mu.Unlock()
	if owned != 1 || f.closed || f.materialized == nil || !f.materialized.closed || len(f.materialized.files) != 0 {
		t.Fatal("unknown stop lost lease or staged descriptors", owned, f.closed)
	}
	f.stopError = nil
	if f.generation.stop(context.Background()) != nil {
		t.Fatal("explicit fake recovery failed")
	}
	if f.close() != nil {
		t.Fatal("recovered fixture did not close")
	}
}

func TestGitFactoryCleanupOrAcquisitionFailureNeverPublishes(t *testing.T) {
	for _, mode := range []string{"close", "acquire", "binding"} {
		t.Run(mode, func(t *testing.T) {
			wire, binding, _ := gitMaterializationFixture(t, "sha1")
			archive, _ := exportArchiveFixture(t)
			owner, _, _, _, _ := credentialOwnerFixture(t)
			if owner.Refresh(context.Background()) != nil {
				t.Fatal("fake pair qualification")
			}
			f := &fakeGitFactoryAcquisition{archive: archive, binding: binding, wire: wire}
			expected := binding
			switch mode {
			case "close":
				f.closeError = errors.New("fake cleanup failure")
			case "acquire":
				f.acquireError = errors.New("fake bad final worker status")
			case "binding":
				expected.Attempt = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			if m, err := runGitFactoryAcquisition(context.Background(), owner, expected, f); err == nil || m != nil {
				t.Fatal("failed acquisition published source")
			}
			if !f.closed || f.materialized == nil || !f.materialized.closed {
				t.Fatal("failed publication leaked staged handles")
			}
			owner.state.mu.Lock()
			owned := len(owner.state.acquisitions)
			owner.state.mu.Unlock()
			if owned != 0 {
				t.Fatal("proven stopped failure retained generation")
			}
		})
	}
}

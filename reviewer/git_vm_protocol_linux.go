//go:build linux

package reviewer

import (
	"context"
	"errors"
	"io"
)

var errGitVMProtocol = errors.New("source VM protocol refused")

const gitVMSourcePurpose = "isolated-git-source-vm"
const gitVMRecordBytes = 16 << 10

// gitVMManifest is trusted release input, separate from NativeVMManifest and
// gitGrantManifest. It has no auth lease, provider, URL, host path or argv.
// Its hash occupies the materialization binding's RuntimeHash only for this
// acquisition mechanism. Existing host records keep their original meaning.
// Valid metadata is not launch authority or a physical-stop observation.
type gitVMManifest struct {
	Version                                                  int
	Purpose                                                  string
	QEMU, Kernel, Initrd, BIOS, QEMURuntime, NamespaceHelper VMAssetPin
	BootstrapLibraries                                       []nativeRuntimeFile
	Runtime                                                  GitRuntimePins
	Worker, Filter                                           VMAssetPin
}

func (m gitVMManifest) valid() bool {
	if m.Version != 1 || m.Purpose != gitVMSourcePurpose || !nativeBootstrapPinsValid(m.BootstrapLibraries) {
		return false
	}
	for _, asset := range []struct {
		pin   VMAssetPin
		limit int64
	}{
		{m.QEMU, 64 << 20}, {m.Kernel, 64 << 20}, {m.Initrd, 512 << 20},
		{m.BIOS, 64 << 20}, {m.QEMURuntime, nativeRuntimeArtifactLimit},
		{m.NamespaceHelper, 64 << 20}, {m.Worker, 64 << 20},
		{m.Runtime.Git, 64 << 20}, {m.Runtime.HTTP, 64 << 20},
		{m.Runtime.Artifact, gitRuntimeDataBytes + gitRuntimeManifestBytes + int64(len(gitRuntimeMagic)) + 4},
	} {
		if !gitRuntimePin(asset.pin, asset.limit) {
			return false
		}
	}
	filter, err := gitFixedFilterPin()
	return err == nil && m.Filter == filter
}

type gitVMRequest struct {
	Version  int
	Purpose  string
	Binding  gitMaterializationBinding
	Manifest gitVMManifest
}

func (q gitVMRequest) valid() bool {
	return q.Version == 1 && q.Purpose == gitVMSourcePurpose && q.Binding.valid() &&
		q.Manifest.valid() && q.Binding.RuntimeHash == Digest(q.Manifest)
}

// This header binds a plain export to the exact request, including the release,
// guest/runtime pins and snapshot. It is neither a stop receipt nor proof of a
// measured producer. Only the future source supervisor may authenticate that
// producer and make the export eligible after broker drain and whole-VM stop.
type gitVMExportHeader struct {
	Version              int
	Purpose, RequestHash string
	Binding              gitMaterializationBinding
}

func (h gitVMExportHeader) valid(q gitVMRequest) bool {
	return q.valid() && h.Version == 1 && h.Purpose == gitVMSourcePurpose &&
		h.RequestHash == Digest(q) && h.Binding == q.Binding
}

// writeGitVMExport runs inside the protected source guest after acquisition.
// All Git parsing stays in writeGitMaterialization. The fixed guest entry and
// qualified launcher are separate work; this function launches nothing.
func writeGitVMExport(ctx context.Context, output io.Writer, store GitStore, q gitVMRequest) error {
	if ctx == nil || ctx.Err() != nil || output == nil || !q.valid() {
		return errGitVMProtocol
	}
	header := gitVMExportHeader{1, gitVMSourcePurpose, Digest(q), q.Binding}
	if writeVMRecord(output, header, gitVMRecordBytes) != nil {
		return errGitVMProtocol
	}
	return writeGitMaterialization(ctx, output, store, q.Binding, DefaultGitAcquisitionBounds())
}

// receiveGitVMExport returns private staging only. It owns input on every
// path, bounds the header before allocation, and requires exact EOF after the
// existing materialization frame. No host Git parser or source publication.
func receiveGitVMExport(ctx context.Context, input io.ReadCloser, q gitVMRequest) (*gitMaterialization, error) {
	if input == nil {
		return nil, errGitVMProtocol
	}
	defer input.Close()
	if ctx == nil || ctx.Err() != nil || !q.valid() {
		return nil, errGitVMProtocol
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); input.Close() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	header, err := readVMRecord[gitVMExportHeader](input, gitVMRecordBytes)
	if err != nil || !header.valid(q) || ctx.Err() != nil {
		return nil, errGitVMProtocol
	}
	return receiveGitMaterialization(ctx, input, q.Binding)
}

//go:build linux

package reviewer

import "strings"

const gitVMGuestUID = 1203
const gitVMGuestGID = 1203
const gitVMGuestPurpose = "source-vm-guest"
const gitVMGuestWorkerPath = "/source-init"
const gitVMGuestRuntimePath = "/source-runtime.bin"
const gitVMGuestHelperPath = "/source-bwrap"
const gitVMGuestBrokerPath = "/run/source-broker"
const gitVMGuestControlPath = gitVMGuestBrokerPath + "/worker.sock"

// Trusted image metadata, not a launch command. Boot/kernel/firmware and the
// initrd itself are separately pinned by the host manifest. Keeping those
// outer pins out of the initrd avoids a self-referential initrd hash.
type gitVMGuestRelease struct {
	Version                 int
	Purpose, SourceHead     string
	Worker, NamespaceHelper VMAssetPin
	Runtime                 GitRuntimePins
	BootstrapLibraries      []nativeRuntimeFile
	Modules                 []nativeGuestModule
}

func (r gitVMGuestRelease) valid() bool {
	if r.Version != 1 || r.Purpose != gitVMGuestPurpose || !objectID.MatchString(r.SourceHead) ||
		!gitRuntimePin(r.Worker, 64<<20) || !gitRuntimePin(r.NamespaceHelper, 64<<20) ||
		!gitRuntimePin(r.Runtime.Git, 64<<20) || !gitRuntimePin(r.Runtime.HTTP, 64<<20) ||
		!gitRuntimePin(r.Runtime.Artifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) ||
		!nativeBootstrapPinsValid(r.BootstrapLibraries) || len(r.Modules) > 8 {
		return false
	}
	previous := ""
	for _, m := range r.Modules {
		if !sourcePath(m.Path) || !strings.HasPrefix(m.Path, "/runtime/modules/") || !strings.HasSuffix(m.Path, ".ko") ||
			m.Path <= previous || !gitRuntimePin(VMAssetPin{m.SHA256, m.Bytes}, 32<<20) {
			return false
		}
		previous = m.Path
	}
	return true
}

type gitVMGuestRequest struct {
	Version int
	Purpose string
	Source  gitVMRequest
	Read    gitVMReadBinding
}

func (q gitVMGuestRequest) valid() bool {
	return q.Version == 1 && q.Purpose == gitVMGuestPurpose && q.Source.valid() && q.Read.valid() && q.Read.Source == q.Source.Binding
}

func (r gitVMGuestRelease) matches(q gitVMGuestRequest) bool {
	if !r.valid() || !q.valid() || r.Worker != q.Source.Manifest.Worker || r.NamespaceHelper != q.Source.Manifest.NamespaceHelper ||
		r.Runtime != q.Source.Manifest.Runtime || len(r.BootstrapLibraries) != len(q.Source.Manifest.BootstrapLibraries) {
		return false
	}
	for i, pin := range r.BootstrapLibraries {
		if pin != q.Source.Manifest.BootstrapLibraries[i] {
			return false
		}
	}
	return true
}

// Phase records are carried only by private original-instance channels. They
// bind messages; their booleans/metadata never prove host admission or VM stop.
type gitVMGuestPhase struct {
	Version                     int
	Purpose, RequestHash, Phase string
}

func (p gitVMGuestPhase) valid(q gitVMGuestRequest, phase string) bool {
	return q.valid() && p.Version == 1 && p.Purpose == gitVMGuestPurpose && p.RequestHash == Digest(q) && p.Phase == phase
}

type gitVMGuestFinished struct {
	Version              int
	Purpose, RequestHash string
	ExportBytes          int64
	ExportHash           string
	Broker               GitReadBrokerAudit
	WorkerStopped        bool
}

func (f gitVMGuestFinished) valid(q gitVMGuestRequest) bool {
	return q.valid() && f.Version == 1 && f.Purpose == gitVMGuestPurpose && f.RequestHash == Digest(q) &&
		f.ExportBytes > 0 && f.ExportBytes <= gitExportWireBytes+gitVMRecordBytes+4 && validAuthHash(f.ExportHash) &&
		f.Broker.Version == 1 && f.Broker.Stopped && !f.Broker.Failed && f.WorkerStopped &&
		f.Broker.Connections > 0 && f.Broker.Connections <= 64 && f.Broker.Requests > 0 && f.Broker.Requests <= gitVMReadRequests && f.Broker.Rejected == 0 &&
		f.Broker.RequestBytes >= 0 && f.Broker.RequestBytes <= gitVMReadInputBytes && f.Broker.ResponseBytes >= 0 && f.Broker.ResponseBytes <= gitVMReadOutputBytes
}

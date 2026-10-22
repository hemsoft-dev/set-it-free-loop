//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const gitBootstrapGrant = "bootstrap"
const gitWorkerGrant = "worker"
const gitGrantManifestBytes = 4096

// All selectors are pinned release assets, never candidate argv or paths.
// Its canonical hash is the binding's RuntimeHash, independently of a grant.
type gitGrantManifest struct {
	Version                         int
	Purpose                         string
	Runtime                         GitRuntimePins
	NamespaceHelper, Worker, Filter VMAssetPin
}

func gitFixedFilterPin() (VMAssetPin, error) {
	filter, err := gitPayloadSeccomp()
	if err != nil {
		return VMAssetPin{}, err
	}
	var raw bytes.Buffer
	if binary.Write(&raw, binary.LittleEndian, filter) != nil {
		return VMAssetPin{}, errGitUnit
	}
	return VMAssetPin{gitRawHash(raw.Bytes()), int64(raw.Len())}, nil
}
func (m gitGrantManifest) validAssets() bool {
	filter, err := gitFixedFilterPin()
	return err == nil && m.Version == 2 && m.Purpose == "isolated-git-acquisition" &&
		gitRuntimePin(m.Runtime.Git, 64<<20) && gitRuntimePin(m.Runtime.HTTP, 64<<20) && gitRuntimePin(m.Runtime.Artifact, gitRuntimeDataBytes+gitRuntimeManifestBytes+int64(len(gitRuntimeMagic))+4) &&
		gitRuntimePin(m.NamespaceHelper, 64<<20) && gitRuntimePin(m.Worker, 64<<20) && m.Filter == filter
}

func (m gitGrantManifest) valid(binding gitMaterializationBinding) bool {
	return binding.valid() && Digest(m) == binding.RuntimeHash && m.validAssets()
}

type gitUnitGrantPacket struct {
	Version      int
	Stage        string
	Binding      gitMaterializationBinding
	Recipient    GateIdentity
	ManifestHash string
}

func (p gitUnitGrantPacket) valid(binding gitMaterializationBinding, stage string) bool {
	return p.Version == 1 && (stage == gitBootstrapGrant || stage == gitWorkerGrant) && p.Stage == stage && p.Binding == binding && binding.valid() && p.ManifestHash == binding.RuntimeHash && p.Recipient.PID > 0 && p.Recipient.StartTicks > 0 && p.Recipient.BootID != "" && gitQuarantineHex(p.Recipient.InvocationID, 32)
}
func readGitUnitGrant(ctx context.Context, conn *net.UnixConn, binding gitMaterializationBinding, stage string) (packet gitUnitGrantPacket, files []*os.File, result error) {
	if !binding.valid() || (stage != gitBootstrapGrant && stage != gitWorkerGrant) {
		return packet, nil, errGitUnit
	}
	expected := 0
	if stage == gitBootstrapGrant {
		expected = 8
	}
	raw, files, err := readGitControlPacket(ctx, conn, expected)
	if err != nil {
		return packet, nil, err
	}
	defer func() {
		if result != nil {
			closeVMFiles(files)
			files = nil
		}
	}()
	packet, err = DecodeStrict[gitUnitGrantPacket](raw, gitControlPacketBytes)
	canonical, marshalErr := json.Marshal(packet)
	if err != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || !packet.valid(binding, stage) {
		return gitUnitGrantPacket{}, files, errGitUnit
	}
	return packet, files, nil
}

// Manifest v2 bootstrap roles: manifest, verified runtime directory, original namespace
// helper, sealed worker, sealed filter, broker directory, export write pipe,
// original supervisor pidfd. None is a provider or GitHub credential.
func verifyGitBootstrapGrant(ctx context.Context, binding gitMaterializationBinding, files []*os.File, stubHash string, supervisor GateIdentity) (gitGrantManifest, error) {
	zero := gitGrantManifest{}
	if ctx == nil || ctx.Err() != nil || len(files) != 8 {
		return zero, errGitUnit
	}
	for _, file := range files {
		if file == nil {
			return zero, errGitUnit
		}
	}
	info, err := files[0].Stat()
	seals, sealErr := unix.FcntlInt(files[0].Fd(), unix.F_GET_SEALS, 0)
	if err != nil || sealErr != nil || seals&requiredStubSeals != requiredStubSeals || seals&unix.F_SEAL_EXEC == 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() <= 0 || info.Size() > gitGrantManifestBytes {
		return zero, errGitUnit
	}
	raw, err := io.ReadAll(io.NewSectionReader(files[0], 0, gitGrantManifestBytes+1))
	manifest, decodeErr := DecodeStrict[gitGrantManifest](raw, gitGrantManifestBytes)
	canonical, marshalErr := json.Marshal(manifest)
	if err != nil || decodeErr != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || !manifest.valid(binding) || manifest.Worker.SHA256 != stubHash {
		return zero, errGitUnit
	}
	for _, item := range []struct {
		index int
		pin   VMAssetPin
	}{{0, VMAssetPin{binding.RuntimeHash, int64(len(raw))}}, {4, manifest.Filter}} {
		file := files[item.index]
		if verifyGrantedVMAsset(ctx, file, item.pin, 1) != nil {
			return zero, errGitUnit
		}
		seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
		if err != nil || seals&unix.F_SEAL_EXEC == 0 {
			return zero, errGitUnit
		}
	}
	if _, err := verifyGitRuntimeDirectory(ctx, files[1], manifest.Runtime); err != nil {
		return zero, errGitUnit
	}
	if verifyNativeBootstrap(ctx, files[2], manifest.NamespaceHelper) != nil || verifyGrantedVMAsset(ctx, files[3], manifest.Worker, 0) != nil {
		return zero, errGitUnit
	}
	if verifyGitBrokerDirectory(files[5]) != nil {
		return zero, errGitUnit
	}
	var pipe unix.Stat_t
	pipeFD, err := gitControlFileFD(files[6])
	if err != nil {
		return zero, errGitUnit
	}
	flags, err := unix.FcntlInt(uintptr(pipeFD), unix.F_GETFL, 0)
	if err != nil || unix.Fstat(pipeFD, &pipe) != nil || pipe.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_WRONLY {
		return zero, errGitUnit
	}
	if gitCheckProcessPIDFD(supervisor, int(files[7].Fd())) != nil || ctx.Err() != nil {
		return zero, errGitUnit
	}
	if _, err := gitLivePIDFDIdentity(files[7]); err != nil {
		return zero, errGitUnit
	}
	return manifest, nil
}
func verifyGitBrokerDirectory(file *os.File) error {
	return verifyGitBrokerDirectoryIDs(file, uint32(os.Geteuid()), uint32(os.Geteuid()), 0700)
}

func verifyGitBrokerDirectoryIDs(file *os.File, owner, worker uint32, mode os.FileMode) error {
	info, err := file.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != mode {
		return errGitUnit
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner {
		return errGitUnit
	}
	// Open a fresh directory description rather than consuming a caller's
	// shared readdir offset; the anchored FD still selects the exact inode.
	fd, err := unix.Openat(int(file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errGitUnit
	}
	directory := os.NewFile(uintptr(fd), "sfl-git-broker-directory")
	defer directory.Close()
	entries, err := directory.ReadDir(4)
	if err != nil && err != io.EOF || len(entries) != 3 {
		return errGitUnit
	}
	wanted := map[string]bool{"git.sock": true, "worker.sock": true, "git-read.used": true}
	for _, entry := range entries {
		if !wanted[entry.Name()] {
			return errGitUnit
		}
		delete(wanted, entry.Name())
		var value unix.Stat_t
		wantOwner := worker
		if entry.Name() == "git-read.used" {
			wantOwner = owner
		}
		if unix.Fstatat(fd, entry.Name(), &value, unix.AT_SYMLINK_NOFOLLOW) != nil || value.Uid != wantOwner || value.Mode&0777 != 0600 {
			return errGitUnit
		}
		if entry.Name() == "git-read.used" {
			if value.Mode&unix.S_IFMT != unix.S_IFREG || value.Nlink != 1 || value.Size != int64(len("sfl-git-read-broker-1\n")) {
				return errGitUnit
			}
			markerFD, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				return errGitUnit
			}
			marker := os.NewFile(uintptr(markerFD), "sfl-git-broker-marker")
			var opened unix.Stat_t
			statErr := unix.Fstat(markerFD, &opened)
			raw, readErr := io.ReadAll(io.LimitReader(marker, 64))
			marker.Close()
			if statErr != nil || opened.Dev != value.Dev || opened.Ino != value.Ino || readErr != nil || string(raw) != "sfl-git-read-broker-1\n" {
				return errGitUnit
			}
		} else if value.Mode&unix.S_IFMT != unix.S_IFSOCK {
			return errGitUnit
		}
	}
	return nil
}

// grant records one-use authority before the sole atomic send. A lost send,
// crash or reopened owner never replays it. This private unit-level step is
// composed only after factory runtime/namespace/resource/peer qualification.
func (o *gitUnitOwner) grant(ctx context.Context, binding gitMaterializationBinding, stage string, conn *net.UnixConn, files []*os.File) error {
	if ctx == nil || !o.enter(ctx) {
		return errGitUnit
	}
	defer o.leave()
	if o.closed || ctx.Err() != nil || !binding.valid() || (stage != gitBootstrapGrant && stage != gitWorkerGrant) || gitControlPacketSocket(conn) != nil || o.grantFailed[binding.Attempt] || !o.grantEligible[binding.Attempt] {
		return errGitUnit
	}
	r, err := o.read(binding.Attempt)
	if err != nil || r.Binding != binding || !r.Captured || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return errGitUnit
	}
	rootFD, held := o.rootPIDFDs[binding.Attempt]
	if !held || gitCheckProcessPIDFD(r.Identity, rootFD) != nil {
		return errGitUnit
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return errGitUnit
	}
	if stage == gitBootstrapGrant && r.BootstrapGrantHash != "" || stage == gitWorkerGrant && (r.BootstrapGrantHash == "" || r.WorkerGrantHash != "") {
		return errGitUnit
	}
	expected := 0
	if stage == gitBootstrapGrant {
		expected = 8
	}
	if len(files) != expected {
		return errGitUnit
	}
	duplicates := make([]*os.File, 0, len(files))
	defer func() { closeVMFiles(duplicates) }()
	for _, file := range files {
		duplicate, err := duplicateGitControlFile(file)
		if err != nil {
			return err
		}
		duplicates = append(duplicates, duplicate)
	}
	if stage == gitBootstrapGrant {
		supervisor, err := processGateIdentity(os.Getpid(), r.Identity.InvocationID)
		if err != nil {
			return errGitUnit
		}
		if _, err := verifyGitBootstrapGrant(ctx, binding, duplicates, r.StubHash, supervisor); err != nil {
			return err
		}
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		return errGitUnit
	}
	defer peer.kernel.Close()
	if peer.UID != uint32(os.Geteuid()) || peer.GID != uint32(os.Getegid()) {
		return errGitUnit
	}
	identity, pidfd, err := retainGitBrokerPeer(peer, r.Identity.InvocationID)
	if err != nil {
		return errGitUnit
	}
	defer unix.Close(pidfd)
	if stage == gitBootstrapGrant && identity != r.Identity || stage == gitWorkerGrant && identity.PID == r.Identity.PID {
		return errGitUnit
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	unit, invocation, err := o.manager.UnitForPIDFD(bounded, pidfd)
	if err != nil || unit != r.Unit || invocation != r.Identity.InvocationID || o.kernel.Executable(identity.PID, r.StubHash) != nil {
		return errGitUnit
	}
	current, err := o.manager.Observe(bounded, r.Unit)
	if err != nil || !expectedGitUnit(r, current) || current.InvocationID != r.Identity.InvocationID || current.MainPID != uint32(r.Identity.PID) {
		return errGitUnit
	}
	group, err := o.kernel.Capture(current.ControlGroup, r.Unit)
	if err != nil || group != r.Cgroup || gitCheckProcessPIDFD(identity, pidfd) != nil || ctx.Err() != nil {
		return errGitUnit
	}
	packet := gitUnitGrantPacket{1, stage, binding, identity, binding.RuntimeHash}
	raw, err := json.Marshal(packet)
	if err != nil || !packet.valid(binding, stage) {
		return errGitUnit
	}
	if stage == gitBootstrapGrant {
		r.BootstrapGrantHash = gitRawHash(raw)
	} else {
		r.WorkerGrantHash = gitRawHash(raw)
		r.WorkerIdentity = identity
	}
	if o.write(r) != nil {
		return errGitUnit
	}
	o.grantFailed[binding.Attempt] = true
	if writeGitControlPacket(ctx, conn, raw, duplicates) != nil {
		r.Revoked = true
		o.write(r)
		return errGitUnit
	}
	o.grantFailed[binding.Attempt] = false
	return nil
}

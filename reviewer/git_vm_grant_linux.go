//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// grant authenticates the captured original stub on its connection and sends
// the boot inputs and channel endpoints exactly once. The grant hash is durable
// before the send; a lost send, crash or reopened owner never replays it.
func (o *gitVMUnitOwner) grant(ctx context.Context, attempt string, conn *net.UnixConn, boot, channels []*os.File) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	if gitControlPacketSocket(conn) != nil || len(boot) != gitVMStubBootFDs || len(channels) != gitVMStubChannelFDs || !o.grantEligible[attempt] {
		return errGitVMOwner
	}
	r, err := o.read(attempt)
	if err != nil || !r.Captured || r.Revoked || r.Stopped || r.GrantHash != "" || r.ManagerOwner != o.manager.Owner() {
		return errGitVMOwner
	}
	rootFD, held := o.rootPIDFDs[attempt]
	if !held || gitCheckProcessPIDFD(r.Identity, rootFD) != nil {
		return errGitVMOwner
	}
	boot0, err := o.kernel.BootID()
	if err != nil || boot0 != r.BootID {
		return errGitVMOwner
	}
	duplicates := make([]*os.File, 0, len(boot)+len(channels))
	defer func() { closeVMFiles(duplicates) }()
	for _, file := range append(append([]*os.File{}, boot...), channels...) {
		duplicate, err := duplicateGitControlFile(file)
		if err != nil {
			return errGitVMOwner
		}
		duplicates = append(duplicates, duplicate)
	}
	bootFiles, channelFiles := duplicates[:gitVMStubBootFDs], duplicates[gitVMStubBootFDs:]
	if verifyGitVMBootFiles(ctx, r.Request.Source.Manifest, bootFiles) != nil || nativeVMStreams(channelFiles) != nil {
		return errGitVMOwner
	}
	raw, files, err := readGitControlPacket(ctx, conn, 0)
	closeVMFiles(files)
	if err != nil {
		return errGitVMOwner
	}
	hello, err := DecodeStrict[gitVMStubHello](raw, gitControlPacketBytes)
	canonical, marshalErr := json.Marshal(hello)
	if err != nil || marshalErr != nil || !bytes.Equal(raw, canonical) || hello.Version != gitVMStubProtocol || hello.RequestHash != r.RequestHash || hello.Identity != r.Identity {
		return errGitVMOwner
	}
	peer, err := gitBrokerPeer(conn)
	if err != nil {
		return errGitVMOwner
	}
	defer peer.kernel.Close()
	if peer.UID != uint32(os.Geteuid()) || peer.GID != uint32(os.Getegid()) {
		return errGitVMOwner
	}
	identity, pidfd, err := retainGitBrokerPeer(peer, r.Identity.InvocationID)
	if err != nil {
		return errGitVMOwner
	}
	defer unix.Close(pidfd)
	if identity != r.Identity {
		return errGitVMOwner
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	unit, invocation, err := o.manager.UnitForPIDFD(bounded, pidfd)
	if err != nil || unit != r.Unit || invocation != r.Identity.InvocationID || o.kernel.Executable(identity.PID, r.StubHash) != nil {
		return errGitVMOwner
	}
	current, err := o.manager.Observe(bounded, r.Unit)
	if err != nil || !expectedGitVMUnit(r, current) || current.InvocationID != r.Identity.InvocationID || current.MainPID != uint32(r.Identity.PID) {
		return errGitVMOwner
	}
	group, err := o.kernel.Capture(current.ControlGroup, r.Unit)
	if err != nil || group != r.Cgroup || gitCheckProcessPIDFD(identity, pidfd) != nil || ctx.Err() != nil {
		return errGitVMOwner
	}
	packet := gitVMStubGrant{gitVMStubProtocol, gitVMStubPurpose, r.RequestHash, identity}
	grantRaw, err := json.Marshal(packet)
	if err != nil || !packet.valid(r.RequestHash) {
		return errGitVMOwner
	}
	channelRaw, err := json.Marshal(gitVMStubChannels{gitVMStubProtocol, gitRawHash(grantRaw)})
	if err != nil {
		return errGitVMOwner
	}
	r.GrantHash = gitRawHash(grantRaw)
	if o.write(r) != nil {
		return errGitVMOwner
	}
	delete(o.grantEligible, attempt)
	if writeGitControlPacket(ctx, conn, grantRaw, bootFiles) != nil || writeGitControlPacket(ctx, conn, channelRaw, channelFiles) != nil {
		r.Revoked = true
		o.write(r)
		return errGitVMOwner
	}
	return nil
}

// live rechecks the granted original instance for each source read admission.
// It requires the unrevoked durable grant, the retained root pidfd and the same
// invocation, main PID and captured cgroup. It never captures a replacement.
func (o *gitVMUnitOwner) live(ctx context.Context, attempt string) error {
	if !o.enter(ctx) {
		return errGitVMOwner
	}
	defer o.leave()
	r, err := o.read(attempt)
	if err != nil || !r.Captured || r.GrantHash == "" || r.Revoked || r.Stopped || r.ManagerOwner != o.manager.Owner() {
		return errGitVMOwner
	}
	rootFD, held := o.rootPIDFDs[attempt]
	if !held || gitCheckProcessPIDFD(r.Identity, rootFD) != nil {
		return errGitVMOwner
	}
	boot, err := o.kernel.BootID()
	if err != nil || boot != r.BootID {
		return errGitVMOwner
	}
	bounded, cancel := boundedManagerContext(ctx)
	defer cancel()
	current, err := o.manager.Observe(bounded, r.Unit)
	if err != nil || !expectedGitVMUnit(r, current) || current.InvocationID != r.Identity.InvocationID || current.MainPID != uint32(r.Identity.PID) {
		return errGitVMOwner
	}
	group, err := o.kernel.Capture(current.ControlGroup, r.Unit)
	if err != nil || group != r.Cgroup || ctx.Err() != nil {
		return errGitVMOwner
	}
	return nil
}

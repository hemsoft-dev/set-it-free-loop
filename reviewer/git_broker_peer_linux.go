//go:build linux

package reviewer

import (
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Only a live broker connection creates this capability from SO_PEERPIDFD.
// Public metadata cannot manufacture it. Keep the original fd until handler
// shutdown; revocation under the same lock prevents fd-number reuse afterward.
type gitBrokerPeerCapability struct {
	mu       sync.Mutex
	original GateIdentity
	uid, gid uint32
	fd       int
	closed   bool
}

func newGitBrokerPeerCapability(peer GitReadPeer, fd int) (*gitBrokerPeerCapability, error) {
	identity, err := processGateIdentity(peer.PID, strings.Repeat("0", 32))
	if err != nil || gitCheckProcessPIDFD(identity, fd) != nil {
		return nil, ErrGitReadBroker
	}
	return &gitBrokerPeerCapability{original: identity, uid: peer.UID, gid: peer.GID, fd: fd}, nil
}
func (c *gitBrokerPeerCapability) alive(peer GitReadPeer) error {
	if c == nil {
		return ErrGitReadBroker
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || peer.kernel != c || peer.PID != c.original.PID || peer.UID != c.uid || peer.GID != c.gid || gitCheckProcessPIDFD(c.original, c.fd) != nil {
		return ErrGitReadBroker
	}
	return nil
}

// The composing factory supplies a separately manager-qualified invocation.
// Retention preserves the original kernel peer; it does not prove that supplied
// invocation or authorize source access. Caller owns the returned CLOEXEC fd.
func retainGitBrokerPeer(peer GitReadPeer, invocation string) (GateIdentity, int, error) {
	c := peer.kernel
	if c == nil {
		return GateIdentity{}, -1, ErrGitReadBroker
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || peer.PID != c.original.PID || peer.UID != c.uid || peer.GID != c.gid {
		return GateIdentity{}, -1, ErrGitReadBroker
	}
	identity := c.original
	identity.InvocationID = invocation
	fd, err := gitRetainProcessPIDFD(identity, c.fd)
	if err != nil {
		return GateIdentity{}, -1, ErrGitReadBroker
	}
	return identity, fd, nil
}
func (c *gitBrokerPeerCapability) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	unix.Close(c.fd)
}

//go:build linux

package reviewer

import (
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeGuestPortWaitsForConnectedWritableCharacterDevice(t *testing.T) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "private-readiness-fixture")
	defer master.Close()
	if unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0) != nil {
		t.Fatal("PTY unlock failed")
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	path := "/dev/pts/" + strconv.Itoa(number)
	openPeer := func() *os.File {
		peer, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		return peer
	}
	peer := openPeer()
	peer.Close()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	if _, err := unix.Poll(poll, 0); err != nil || poll[0].Revents&unix.POLLHUP == 0 {
		t.Fatal("disconnected character fixture lacks HUP", poll, err)
	}
	done := make(chan error, 1)
	go func() { done <- nativeGuestPortConnected(master, time.Now().Add(2*time.Second)) }()
	var early bool
	select {
	case <-done:
		early = true
	case <-time.After(50 * time.Millisecond):
	}
	peer = openPeer()
	defer peer.Close()
	if early {
		t.Fatal("open character device accepted before host connection")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("connected character device rejected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection readiness did not resume")
	}
}

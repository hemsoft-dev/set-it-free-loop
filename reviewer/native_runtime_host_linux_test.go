//go:build linux

package reviewer

import (
	"context"
	"strings"
	"testing"
)

func TestNativeRuntimeBootstrapPinsRejectUnlistedHostInputs(t *testing.T) {
	good := []VMBootAsset{{Path: "/lib64/ld-linux-x86-64.so.2", SHA256: strings.Repeat("a", 64), Bytes: 1}, {Path: "/usr/lib/x86_64-linux-gnu/libc.so.6", SHA256: strings.Repeat("b", 64), Bytes: 1}}
	pins, err := nativeBootstrapLibraryPins(good)
	if err != nil || !nativeBootstrapPinsValid(pins) {
		t.Fatal(err)
	}
	for _, edit := range []func([]VMBootAsset){
		func(a []VMBootAsset) { a[0].Path = "/outside/credential" },
		func(a []VMBootAsset) { a[0].Path = "/usr/lib/../private" },
		func(a []VMBootAsset) { a[0].Path = "/firmware/linuxboot.bin" },
		func(a []VMBootAsset) { a[1].Path = a[0].Path },
		func(a []VMBootAsset) { a[1].SHA256 = "invalid" },
		func(a []VMBootAsset) { a[1].Bytes = 0 },
	} {
		bad := append([]VMBootAsset{}, good...)
		edit(bad)
		if _, err := nativeBootstrapLibraryPins(bad); err == nil {
			t.Fatal("invalid host input accepted", bad)
		}
	}
	if _, err := nativeBootstrapLibraryPins(good[:1]); err == nil {
		t.Fatal("missing required libc accepted")
	}
	good[0].Path = "/changed"
	if pins[0].Path != "lib64/ld-linux-x86-64.so.2" {
		t.Fatal("caller mutated captured pins")
	}
	if verifyNativeBootstrapLibraries(context.Background(), nil) == nil {
		t.Fatal("empty host qualification accepted")
	}
}

//go:build linux

package main

import (
	"errors"
	"testing"
)

func TestDeferredCleanupInvalidatesTentativePass(t *testing.T) {
	for _, prior := range []string{"", "original exercise failure"} {
		receipt := result{Passed: prior == "", Error: prior}
		fallback, closed := 0, 0
		finish(&receipt, func() error { return errors.New("stop failed") }, func() error { fallback++; return nil }, func() error { closed++; return nil })
		if receipt.Passed || !receipt.FallbackCleanup || receipt.Error == "" || fallback != 1 || closed != 1 {
			t.Fatal("cleanup manufactured success", receipt, fallback, closed)
		}
		if prior != "" && receipt.Error != prior {
			t.Fatal("cleanup replaced primary failure", receipt.Error)
		}
	}
}
func TestDeferredCloseFailureInvalidatesPass(t *testing.T) {
	receipt := result{Passed: true}
	finish(&receipt, func() error { return nil }, func() error { t.Fatal("unneeded fallback"); return nil }, func() error { return errors.New("close failed") })
	if receipt.Passed || receipt.FallbackCleanup || receipt.Error == "" {
		t.Fatal(receipt)
	}
}
func TestDeferredCleanStopPreservesPass(t *testing.T) {
	receipt := result{Passed: true}
	finish(&receipt, func() error { return nil }, func() error { t.Fatal("unneeded fallback"); return nil }, func() error { return nil })
	if !receipt.Passed || receipt.FallbackCleanup || receipt.Error != "" {
		t.Fatal(receipt)
	}
}

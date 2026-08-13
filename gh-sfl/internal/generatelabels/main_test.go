package main

import "testing"

func TestNormalizeLineEndings(t *testing.T) {
	input := []byte("first\r\nsecond\rthird\nfourth")
	want := "first\nsecond\nthird\nfourth"
	if got := string(normalizeLineEndings(input)); got != want {
		t.Fatalf("normalizeLineEndings() = %q, want %q", got, want)
	}
}

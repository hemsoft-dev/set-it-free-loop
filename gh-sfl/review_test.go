package main

import (
	"io"
	"strings"
	"testing"
)

func TestDetectDispatchInputs(t *testing.T) {
	lockV639 := `
  workflow_dispatch:
    inputs:
      aw_context:
        required: true
        type: string
      base_sha:
        required: true
        type: string
      head_sha:
        required: true
        type: string
      item_number:
        required: true
        type: string
      retry_count:
        default: "0"
        required: false
        type: string
`
	lockV638 := `
  workflow_dispatch:
    inputs:
      aw_context:
        required: true
        type: string
      head_sha:
        required: true
        type: string
      item_number:
        required: true
        type: string
      retry_count:
        default: "0"
        required: false
        type: string
`
	lockLegacy := `
  workflow_dispatch:
    inputs:
      aw_context:
        default: ""
        type: string
      item_number:
        default: ""
        type: string
`

	for _, tc := range []struct {
		name       string
		lock       string
		headSHA    bool
		baseSHA    bool
		retryCount bool
	}{
		{"v6.3.9 declares all provenance inputs", lockV639, true, true, true},
		{"v6.3.8 lacks base_sha only", lockV638, true, false, true},
		{"legacy lock declares neither", lockLegacy, false, false, false},
		{"commented mention is not a declaration", "#       head_sha: see docs\n", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectDispatchInputs(tc.lock)
			want := dispatchCapabilities{headSHA: tc.headSHA, baseSHA: tc.baseSHA, retryCount: tc.retryCount}
			if got != want {
				t.Errorf("detectDispatchInputs() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseReviewOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantPR  int
		wantErr string
	}{
		{name: "pr number", args: []string{"94"}, wantPR: 94},
		{name: "hash prefix", args: []string{"#94"}, wantPR: 94},
		{name: "repo flag", args: []string{"--repo", "owner/repo", "94"}, wantPR: 94},
		{name: "short repo flag", args: []string{"-R", "owner/repo", "94"}, wantPR: 94},
		{name: "missing number", args: []string{}, wantErr: "exactly one pull request number"},
		{name: "two numbers", args: []string{"1", "2"}, wantErr: "exactly one pull request number"},
		{name: "not a number", args: []string{"abc"}, wantErr: "invalid pull request number"},
		{name: "zero", args: []string{"0"}, wantErr: "invalid pull request number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseReviewOptions(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseReviewOptions(%v) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReviewOptions(%v) unexpected error: %v", tc.args, err)
			}
			if opts.pr != tc.wantPR {
				t.Errorf("parseReviewOptions(%v) pr = %d, want %d", tc.args, opts.pr, tc.wantPR)
			}
			if strings.Contains(tc.name, "repo flag") && opts.repo != "owner/repo" {
				t.Errorf("parseReviewOptions(%v) repo = %q, want owner/repo", tc.args, opts.repo)
			}
		})
	}
}

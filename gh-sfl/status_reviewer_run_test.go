package main

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

type reviewerRunREST struct {
	gets []string
}

func (f *reviewerRunREST) Get(path string, response interface{}) error {
	f.gets = append(f.gets, path)
	switch {
	case strings.Contains(path, "/workflows/sfl-pr-review-auto.yml/runs?"):
		return decodeTestResponse(response, map[string]any{"workflow_runs": []map[string]any{
			{"id": 2, "status": "completed", "conclusion": "success", "html_url": "https://github.test/runs/2"},
			{"id": 1, "status": "completed", "conclusion": "failure", "html_url": "https://github.test/runs/1"},
		}})
	case strings.Contains(path, "/actions/runs/2/jobs?"):
		return decodeTestResponse(response, map[string]any{"jobs": []map[string]any{
			{"name": "Observe authenticated Codex review", "status": "completed", "conclusion": "skipped"},
		}})
	case strings.Contains(path, "/actions/runs/1/jobs?"):
		return decodeTestResponse(response, map[string]any{"jobs": []map[string]any{
			{"name": "Observe authenticated Codex review", "status": "completed", "conclusion": "failure"},
		}})
	default:
		return fmt.Errorf("unexpected GET %s", path)
	}
}

func (f *reviewerRunREST) GetWithETag(string, interface{}) (string, error) {
	return "", fmt.Errorf("unexpected GetWithETag")
}

func (f *reviewerRunREST) Post(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected POST")
}

func (f *reviewerRunREST) Put(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PUT")
}

func (f *reviewerRunREST) Patch(string, io.Reader, interface{}) error {
	return fmt.Errorf("unexpected PATCH")
}

func (f *reviewerRunREST) Delete(string, interface{}) error {
	return fmt.Errorf("unexpected DELETE")
}

func TestLatestReviewerRunSkipsRunsWithoutExecutedObserver(t *testing.T) {
	rest := &reviewerRunREST{}
	run, err := latestReviewerRunWithClient(rest, "HemSoft", "consumer")
	if err != nil {
		t.Fatalf("latestReviewerRunWithClient() unexpected error: %v", err)
	}
	if run.ID != 1 || run.Conclusion != "failure" {
		t.Fatalf("latest actual observer run = %+v, want run 1", run)
	}
	if len(rest.gets) != 3 || !strings.Contains(rest.gets[0], "actor=chatgpt-codex-connector%5Bbot%5D") {
		t.Fatalf("reviewer run queries = %v", rest.gets)
	}
}

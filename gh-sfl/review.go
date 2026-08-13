package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/cli/go-gh/v2"
)

type reviewOptions struct {
	repo string
	pr   int
}

type pullRequestShas struct {
	BaseSHA string
	HeadSHA string
	State   string
}

func runReview(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseReviewOptions(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpDisplayed) {
			return nil
		}
		return err
	}

	owner, repo, err := parseMutationTarget(opts.repo)
	if err != nil {
		return err
	}

	defaultBranch, err := getDefaultBranch(owner, repo)
	if err != nil {
		return err
	}

	lockContent, err := fetchFileRaw(owner, repo, ".github/workflows/sfl-pr-review.lock.yml", "")
	if err != nil {
		if isNotFoundError(err) {
			return fmt.Errorf("SFL is not installed in %s/%s — run 'gh sfl init --repo %s/%s' first", owner, repo, owner, repo)
		}
		return fmt.Errorf("reading SFL review workflow from %s/%s: %w", owner, repo, err)
	}

	pr, err := fetchPullRequestShas(owner, repo, opts.pr)
	if err != nil {
		return err
	}
	if pr.State != "open" {
		return fmt.Errorf("pull request #%d in %s/%s is %s — reviews only run on open pull requests", opts.pr, owner, repo, pr.State)
	}

	awContext := fmt.Sprintf(
		`{"item_type":"pull_request","item_number":%d,"base_sha":"%s","head_sha":"%s"}`,
		opts.pr, pr.BaseSHA, pr.HeadSHA,
	)

	dispatchArgs := []string{
		"api", "--method", "POST",
		fmt.Sprintf("repos/%s/%s/actions/workflows/sfl-pr-review.lock.yml/dispatches", owner, repo),
		"-f", "ref=" + defaultBranch,
		"-f", fmt.Sprintf("inputs[item_number]=%d", opts.pr),
		"-f", "inputs[aw_context]=" + awContext,
	}
	// Send exactly the inputs the deployed workflow declares — unknown inputs
	// are rejected with HTTP 422 and missing required inputs fail the same
	// way. Detection mirrors the grep checks in sfl-pr-review-auto.yml.
	capabilities := detectDispatchInputs(lockContent)
	if !capabilities.baseSHA {
		return fmt.Errorf(
			"%s/%s runs an SFL review workflow that cannot seal the base revision;\n"+
				"a base-branch advance during the review would not be detected.\n"+
				"  Run 'gh sfl sync --repo %s/%s' to upgrade to v6.3.9+, then retry.",
			owner, repo, owner, repo)
	}
	if capabilities.headSHA {
		dispatchArgs = append(dispatchArgs, "-f", "inputs[head_sha]="+pr.HeadSHA)
	}
	dispatchArgs = append(dispatchArgs, "-f", "inputs[base_sha]="+pr.BaseSHA)
	if capabilities.retryCount {
		dispatchArgs = append(dispatchArgs, "-f", "inputs[retry_count]=0")
	}

	fmt.Fprintf(stdout, "Dispatching SFL review for %s/%s#%d (head %.7s)...\n",
		owner, repo, opts.pr, pr.HeadSHA)

	// Captured before the dispatch call: GitHub creates the run before the
	// POST returns, so a slow response must not push the window past the
	// run's creation time.
	dispatchedAt := time.Now()
	_, stderrBuf, err := gh.Exec(dispatchArgs...)
	if err != nil {
		return fmt.Errorf("dispatching review: %s: %w", stderrBuf.String(), err)
	}

	if runURL := waitForReviewRun(owner, repo, opts.pr, pr.BaseSHA, pr.HeadSHA, dispatchedAt); runURL != "" {
		fmt.Fprintf(stdout, "  ✓ Review started: %s\n", runURL)
	} else {
		fmt.Fprintf(stdout, "  ✓ Dispatched. Watch progress: gh sfl list --repo %s/%s\n", owner, repo)
	}
	return nil
}

func parseReviewOptions(args []string, stderr io.Writer) (reviewOptions, error) {
	var opts reviewOptions

	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeReviewUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	if flags.NArg() != 1 {
		return opts, errors.New("exactly one pull request number is required")
	}
	number, err := strconv.Atoi(strings.TrimPrefix(flags.Arg(0), "#"))
	if err != nil || number < 1 {
		return opts, fmt.Errorf("invalid pull request number %q", flags.Arg(0))
	}
	opts.pr = number
	return opts, nil
}

func fetchPullRequestShas(owner, repo string, number int) (pullRequestShas, error) {
	var result pullRequestShas
	stdoutBuf, stderrBuf, err := gh.Exec(
		"api", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number),
		"--jq", `{base: .base.sha, head: .head.sha, state: .state}`,
	)
	if err != nil {
		if isNotFoundMessage(stderrBuf.String()) {
			return result, fmt.Errorf("pull request #%d not found in %s/%s", number, owner, repo)
		}
		return result, fmt.Errorf("reading pull request #%d in %s/%s: %s",
			number, owner, repo, strings.TrimSpace(stderrBuf.String()))
	}

	var parsed struct {
		Base  string `json:"base"`
		Head  string `json:"head"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(stdoutBuf.Bytes(), &parsed); err != nil {
		return result, fmt.Errorf("parsing pull request #%d: %w", number, err)
	}
	result.BaseSHA = parsed.Base
	result.HeadSHA = parsed.Head
	result.State = parsed.State
	return result, nil
}

// dispatchCapabilities models which workflow_dispatch inputs the deployed
// review workflow declares.
type dispatchCapabilities struct {
	headSHA    bool
	baseSHA    bool
	retryCount bool
}

// detectDispatchInputs inspects the compiled review workflow and reports
// which provenance inputs it declares, using the same six-space indentation
// pattern as sfl-pr-review-auto.yml's grep checks. Declared inputs must be
// sent (head_sha is required in newer locks); undeclared inputs must not be
// sent at all, because GitHub rejects unknown dispatch inputs with HTTP 422.
func detectDispatchInputs(lockContent string) dispatchCapabilities {
	declared := func(name string) bool {
		return regexp.MustCompile(`(?m)^      ` + name + `:\s*$`).MatchString(lockContent)
	}
	return dispatchCapabilities{
		headSHA:    declared("head_sha"),
		baseSHA:    declared("base_sha"),
		retryCount: declared("retry_count"),
	}
}

// isNotFoundError reports whether an API read failed because the file does
// not exist, as opposed to an authentication, rate-limit, or network failure.
func isNotFoundError(err error) bool {
	return isNotFoundMessage(err.Error())
}

func isNotFoundMessage(msg string) bool {
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "Not Found")
}

// waitForReviewRun polls briefly for the dispatched review run and returns its
// URL, matched exactly on the "#<pr> <base>:<head>" sealed run title. Returns
// empty string when no exact match appears — legacy runs carry no identifying
// title, so an ambiguous guess would risk printing another PR's run.
func waitForReviewRun(owner, repo string, prNumber int, baseSHA, headSHA string, dispatchedAt time.Time) string {
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(3 * time.Second)
		runs, err := fetchWorkflowRuns(owner, repo, 20)
		if err != nil {
			continue
		}
		marker := fmt.Sprintf("#%d ", prNumber)
		for _, r := range runs {
			if strings.Contains(strings.ToLower(r.WorkflowName), "sfl pr review") &&
				r.CreatedAt.After(dispatchedAt.Add(-5*time.Second)) &&
				strings.Contains(r.DisplayTitle, marker) &&
				strings.Contains(r.DisplayTitle, baseSHA) &&
				strings.Contains(r.DisplayTitle, headSHA) {
				return r.URL
			}
		}
	}
	return ""
}

func writeReviewUsage(w io.Writer) {
	fmt.Fprint(w, reviewUsage)
}

const reviewUsage = `Trigger a full-spectrum SFL review of a pull request.

Resolves the pull request's base and head revisions and dispatches the SFL
PR Review workflow at the repository's default branch, sealing the review to
the exact current revisions.

Usage:
  gh sfl review [flags] <pr-number>

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.

Examples:
  gh sfl review 94                     # Review PR #94 in the current repo
  gh sfl review --repo owner/repo 94   # Review PR #94 in another repo
`

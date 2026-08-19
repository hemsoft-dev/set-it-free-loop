package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

const codexReviewCommand = "@codex review"

type reviewOptions struct {
	repo  string
	pr    int
	retry bool
}

type pullRequestShas struct {
	BaseSHA  string
	BaseRef  string
	HeadSHA  string
	BaseRepo string
	HeadRepo string
	State    string
}

type reviewTriggerComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type reviewerCheckRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	ExternalID string `json:"external_id"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
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

	client, err := newRESTClient()
	if err != nil {
		return fmt.Errorf("creating GitHub REST client: %w", err)
	}
	if err := requireCodexReviewObserver(client, owner, repo); err != nil {
		return err
	}
	if err := requireReviewerEnabled(client, owner, repo); err != nil {
		return err
	}

	pr, err := fetchPullRequestShasWithClient(client, owner, repo, opts.pr)
	if err != nil {
		return err
	}
	if pr.State != "open" {
		return fmt.Errorf("pull request #%d in %s/%s is %s — reviews only run on open pull requests", opts.pr, owner, repo, pr.State)
	}
	if pr.HeadRepo == "" {
		return fmt.Errorf("pull request #%d in %s/%s has no available head repository — subscription-backed SFL reviews cannot run after the source repository is deleted", opts.pr, owner, repo)
	}
	if !strings.EqualFold(pr.HeadRepo, owner+"/"+repo) {
		return fmt.Errorf("pull request #%d in %s/%s comes from fork %s — subscription-backed SFL reviews support same-repository branches only", opts.pr, owner, repo, pr.HeadRepo)
	}
	defaultBranch, err := fetchRepositoryDefaultBranchWithClient(client, owner, repo)
	if err != nil {
		return err
	}
	if !strings.EqualFold(pr.BaseRef, defaultBranch) {
		return fmt.Errorf("pull request #%d in %s/%s targets %s — subscription-backed SFL reviews support only the default branch %s", opts.pr, owner, repo, pr.BaseRef, defaultBranch)
	}

	contextToken, err := fetchReviewContextToken(client, owner, repo, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("reading the current SFL review context: %w", err)
	}
	marker := codexReviewMarker(pr.HeadSHA, pr.BaseSHA, contextToken)
	conflictingURL, conflictErr := findConflictingCodexBaseRequest(client, owner, repo, opts.pr, pr.HeadSHA, codexReviewBaseMarker(pr.HeadSHA, pr.BaseSHA))
	if conflictErr != nil {
		return fmt.Errorf("checking prior Codex review bases: %w", conflictErr)
	}
	if conflictingURL != "" {
		return fmt.Errorf("pull request #%d head %.10s was already requested against another base at %s — update the pull request branch to a new head before requesting review for the current base", opts.pr, pr.HeadSHA, conflictingURL)
	}
	existing, findErr := findCodexReviewTrigger(client, owner, repo, opts.pr, marker)
	if findErr != nil {
		return fmt.Errorf("checking existing Codex review requests: %w", findErr)
	}
	if existing.HTMLURL != "" {
		if !opts.retry {
			fmt.Fprintf(stdout, "Codex review already requested for %s/%s#%d at %.10s: %s\nUse --retry if its result event was skipped.\n",
				owner, repo, opts.pr, pr.HeadSHA, existing.HTMLURL)
			return nil
		}
		completed, completedErr := hasTerminalGateForRequest(client, owner, repo, opts.pr, pr.HeadSHA, pr.BaseSHA, existing)
		if completedErr != nil {
			return fmt.Errorf("checking the prior Codex review result: %w", completedErr)
		}
		if !completed {
			return fmt.Errorf("the latest Codex review request for %s/%s#%d is still outstanding — wait for its result before using --retry", owner, repo, opts.pr)
		}
	}

	body := codexReviewCommand + "\n\n" + marker
	payload, err := jsonBody(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("encoding Codex review request: %w", err)
	}
	var created reviewTriggerComment
	if err := client.Post(
		fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, repo, opts.pr),
		payload,
		&created,
	); err != nil {
		return fmt.Errorf("requesting Codex review: %w", err)
	}
	if strings.TrimSpace(created.HTMLURL) == "" {
		return errors.New("GitHub created the Codex review request without returning its URL")
	}

	fmt.Fprintf(stdout, "Requested subscription-backed Codex review for %s/%s#%d at %.10s: %s\n",
		owner, repo, opts.pr, pr.HeadSHA, created.HTMLURL)
	return nil
}

func requireReviewerEnabled(client restAPI, owner, repo string) error {
	var variable struct {
		Value string `json:"value"`
	}
	err := client.Get(fmt.Sprintf("repos/%s/%s/actions/variables/SFL_ENABLED", owner, repo), &variable)
	if err == nil {
		if strings.EqualFold(strings.TrimSpace(variable.Value), "false") {
			return fmt.Errorf("SFL is stopped in %s/%s — run 'gh sfl start --repo %s/%s' before requesting a review", owner, repo, owner, repo)
		}
		return nil
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("checking SFL maintenance mode in %s/%s: %w", owner, repo, err)
}

func parseReviewOptions(args []string, stderr io.Writer) (reviewOptions, error) {
	var opts reviewOptions

	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeReviewUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.IntVar(&opts.pr, "pr", 0, "Pull request number")
	flags.BoolVar(&opts.retry, "retry", false, "Post another request for the current head")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	if flags.NArg() > 1 || (flags.NArg() == 1 && opts.pr != 0) {
		return opts, errors.New("specify the pull request number exactly once")
	}
	if flags.NArg() == 1 {
		number, err := strconv.Atoi(strings.TrimPrefix(flags.Arg(0), "#"))
		if err != nil || number < 1 {
			return opts, fmt.Errorf("invalid pull request number %q", flags.Arg(0))
		}
		opts.pr = number
	}
	if opts.pr < 1 {
		return opts, errors.New("a pull request number is required")
	}
	return opts, nil
}

func requireCodexReviewObserver(client restAPI, owner, repo string) error {
	var workflow struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	err := client.Get(
		fmt.Sprintf("repos/%s/%s/contents/.github/workflows/sfl-pr-review-auto.yml", owner, repo),
		&workflow,
	)
	if err == nil {
		if workflow.Encoding != "base64" {
			return fmt.Errorf("checking the SFL Codex observer in %s/%s: unsupported content encoding %q", owner, repo, workflow.Encoding)
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(workflow.Content, "\n", ""))
		if decodeErr != nil {
			return fmt.Errorf("checking the SFL Codex observer in %s/%s: %w", owner, repo, decodeErr)
		}
		content := string(decoded)
		if strings.Contains(content, "name: SFL Codex Review Observer") &&
			strings.Contains(content, "github.event.sender.id == 199175422") {
			var workflow struct {
				State string `json:"state"`
			}
			if stateErr := client.Get(
				fmt.Sprintf("repos/%s/%s/actions/workflows/sfl-pr-review-auto.yml", owner, repo),
				&workflow,
			); stateErr != nil {
				return fmt.Errorf("checking the SFL Codex observer state in %s/%s: %w", owner, repo, stateErr)
			}
			if workflow.State != "active" {
				return fmt.Errorf("the subscription-backed SFL reviewer is %s in %s/%s — enable sfl-pr-review-auto.yml before requesting a Codex review", workflow.State, owner, repo)
			}
			return nil
		}
		return fmt.Errorf("%s/%s still has the retired SFL reviewer — run 'gh sfl sync --repo %s/%s' before requesting a Codex review", owner, repo, owner, repo)
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("the subscription-backed SFL reviewer is not installed in %s/%s — run 'gh sfl sync --repo %s/%s' first", owner, repo, owner, repo)
	}
	return fmt.Errorf("checking the SFL Codex observer in %s/%s: %w", owner, repo, err)
}

func fetchPullRequestShasWithClient(client restAPI, owner, repo string, number int) (pullRequestShas, error) {
	var response struct {
		Base struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		State string `json:"state"`
	}
	err := client.Get(fmt.Sprintf("repos/%s/%s/pulls/%d", owner, repo, number), &response)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return pullRequestShas{}, fmt.Errorf("pull request #%d not found in %s/%s", number, owner, repo)
		}
		return pullRequestShas{}, fmt.Errorf("reading pull request #%d in %s/%s: %w", number, owner, repo, err)
	}
	return pullRequestShas{
		BaseSHA:  response.Base.SHA,
		BaseRef:  response.Base.Ref,
		HeadSHA:  response.Head.SHA,
		BaseRepo: response.Base.Repo.FullName,
		HeadRepo: response.Head.Repo.FullName,
		State:    response.State,
	}, nil
}

func fetchRepositoryDefaultBranchWithClient(client restAPI, owner, repo string) (string, error) {
	var response struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.Get(fmt.Sprintf("repos/%s/%s", owner, repo), &response); err != nil {
		return "", fmt.Errorf("reading the default branch for %s/%s: %w", owner, repo, err)
	}
	if strings.TrimSpace(response.DefaultBranch) == "" {
		return "", fmt.Errorf("repository %s/%s has no default branch", owner, repo)
	}
	return response.DefaultBranch, nil
}

func codexReviewMarker(headSHA, baseSHA, contextToken string) string {
	return "<!-- sfl-codex-review:head=" + strings.ToLower(headSHA) +
		";base=" + strings.ToLower(baseSHA) + ";context=" + contextToken + " -->"
}

func codexReviewHeadMarker(headSHA string) string {
	return "<!-- sfl-codex-review:head=" + strings.ToLower(headSHA) + ";base="
}

func codexReviewBaseMarker(headSHA, baseSHA string) string {
	return codexReviewHeadMarker(headSHA) + strings.ToLower(baseSHA) + ";"
}

func findConflictingCodexBaseRequest(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA, currentBaseMarker string,
) (string, error) {
	headMarker := codexReviewHeadMarker(headSHA)
	for page := 1; ; page++ {
		var comments []reviewTriggerComment
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", owner, repo, prNumber, page),
			&comments,
		); err != nil {
			return "", err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.User.Login, owner) &&
				strings.Contains(comment.Body, headMarker) && !strings.Contains(comment.Body, currentBaseMarker) {
				return comment.HTMLURL, nil
			}
		}
		if len(comments) < 100 {
			return "", nil
		}
	}
}

func findCodexReviewTrigger(
	client restAPI,
	owner, repo string,
	prNumber int,
	marker string,
) (reviewTriggerComment, error) {
	var latest reviewTriggerComment
	for page := 1; ; page++ {
		var comments []reviewTriggerComment
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", owner, repo, prNumber, page),
			&comments,
		); err != nil {
			return reviewTriggerComment{}, err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.User.Login, owner) && strings.Contains(comment.Body, marker) {
				if latest.HTMLURL == "" || reviewCommentTime(comment).After(reviewCommentTime(latest)) {
					latest = comment
				}
			}
		}
		if len(comments) < 100 {
			return latest, nil
		}
	}
}

func reviewCommentTime(comment reviewTriggerComment) time.Time {
	parsed, _ := time.Parse(time.RFC3339, comment.CreatedAt)
	return parsed
}

func fetchReviewContextToken(client restAPI, owner, repo, headSHA string) (string, error) {
	var response struct {
		CheckRuns []reviewerCheckRun `json:"check_runs"`
	}
	if err := client.Get(
		fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?check_name=SFL%%20Reviewer%%20Gate%%20Runner&filter=all&per_page=100", owner, repo, headSHA),
		&response,
	); err != nil {
		return "", err
	}
	latest := reviewerCheckRun{}
	for _, check := range response.CheckRuns {
		if check.App.ID == 15368 &&
			(strings.HasPrefix(check.ExternalID, "sfl-codex-review:pull-context:") ||
				strings.HasPrefix(check.ExternalID, "sfl-codex-review:base-advance:")) &&
			check.ID > latest.ID {
			latest = check
		}
	}
	if latest.ExternalID != "" {
		return latest.ExternalID, nil
	}
	return "none", nil
}

func reviewContextTokenFromBody(body string) (string, bool) {
	const prefix = ";context="
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", false
	}
	start += len(prefix)
	end := strings.Index(body[start:], " -->")
	if end < 1 {
		return "", false
	}
	return body[start : start+end], true
}

func hasTerminalGateForRequest(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA, baseSHA string,
	request reviewTriggerComment,
) (bool, error) {
	requestTime := reviewCommentTime(request)
	if requestTime.IsZero() {
		return false, errors.New("latest Codex review request has no valid timestamp")
	}
	contextToken, ok := reviewContextTokenFromBody(request.Body)
	if !ok {
		return false, errors.New("latest Codex review request has no valid context token")
	}
	expected := fmt.Sprintf(
		"sfl-codex-review:pull:%d:base:%s:context:%s:request:%d:at:%d",
		prNumber,
		strings.ToLower(baseSHA),
		url.QueryEscape(contextToken),
		request.ID,
		requestTime.UnixMilli(),
	)
	for page := 1; ; page++ {
		var response struct {
			CheckRuns []reviewerCheckRun `json:"check_runs"`
		}
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?check_name=SFL%%20Reviewer%%20Gate%%20Runner&filter=all&per_page=100&page=%d", owner, repo, headSHA, page),
			&response,
		); err != nil {
			return false, err
		}
		for _, check := range response.CheckRuns {
			if check.App.ID == 15368 && check.Status == "completed" &&
				(check.ExternalID == expected || strings.HasPrefix(check.ExternalID, expected+":artifact:")) {
				return true, nil
			}
		}
		if len(response.CheckRuns) < 100 {
			return false, nil
		}
	}
}

func writeReviewUsage(w io.Writer) {
	fmt.Fprint(w, reviewUsage)
}

const reviewUsage = `Request a subscription-backed Codex review of a pull request.

Posts one authenticated @codex review request for the exact current head. The
deployed SFL observer validates the Codex result and publishes the required
SFL Reviewer Gate Runner check on that head.

Usage:
  gh sfl review [flags] <pr-number>
  gh sfl review [flags] --pr <pr-number>

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
      --pr int         Pull request number.
      --retry          Post another request when a prior result event was skipped.

Examples:
  gh sfl review 94
  gh sfl review --repo HemSoft/hs-buddy --pr 94
  gh sfl review --repo HemSoft/hs-buddy --pr 94 --retry
`

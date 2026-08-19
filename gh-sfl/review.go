package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

const (
	codexReviewCommand                = "@codex review"
	codexReviewRequestRegistryContext = "SFL Codex Review Request Registry"
)

var waitForRetryOrdering = func() {
	time.Sleep(1100 * time.Millisecond)
}

var waitForCodexReactionPoll = func() {
	time.Sleep(2 * time.Second)
}

var waitForReviewInvalidationPoll = func() {
	time.Sleep(5 * time.Second)
}

type reviewOptions struct {
	repo  string
	pr    int
	retry bool
}

type pullRequestShas struct {
	BaseSHA   string
	BaseRef   string
	HeadSHA   string
	BaseRepo  string
	HeadRepo  string
	State     string
	UpdatedAt string `json:"updated_at"`
}

type reviewWorkflowRun struct {
	ID           int64  `json:"id"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	CreatedAt    string `json:"created_at"`
	PullRequests []struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
	} `json:"pull_requests"`
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

type reviewCommentReaction struct {
	Content string `json:"content"`
	User    struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

type reviewRequestStatus struct {
	Context   string `json:"context"`
	TargetURL string `json:"target_url"`
	Creator   struct {
		Login string `json:"login"`
	} `json:"creator"`
}

type reviewerCheckRun struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
	ExternalID  string `json:"external_id"`
	App         struct {
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

	owner, repo, err := parseReviewTarget(opts.repo)
	if err != nil {
		return err
	}

	client, err := newRESTClient()
	if err != nil {
		return fmt.Errorf("creating GitHub REST client: %w", err)
	}
	defaultBranch, err := fetchRepositoryDefaultBranchWithClient(client, owner, repo)
	if err != nil {
		return err
	}
	if err := requireCodexReviewObserver(client, owner, repo, defaultBranch); err != nil {
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
	if !strings.EqualFold(pr.BaseRef, defaultBranch) {
		return fmt.Errorf("pull request #%d in %s/%s targets %s — subscription-backed SFL reviews support only the default branch %s", opts.pr, owner, repo, pr.BaseRef, defaultBranch)
	}
	if err := waitForReviewInvalidations(client, owner, repo, opts.pr, defaultBranch, pr); err != nil {
		return fmt.Errorf("waiting for SFL review invalidation: %w", err)
	}
	confirmedDefaultBranch, err := fetchRepositoryDefaultBranchWithClient(client, owner, repo)
	if err != nil {
		return err
	}
	confirmedPR, err := fetchPullRequestShasWithClient(client, owner, repo, opts.pr)
	if err != nil {
		return err
	}
	if confirmedDefaultBranch != defaultBranch || confirmedPR.State != "open" ||
		confirmedPR.HeadSHA != pr.HeadSHA || confirmedPR.BaseSHA != pr.BaseSHA ||
		confirmedPR.BaseRef != pr.BaseRef ||
		!strings.EqualFold(confirmedPR.HeadRepo, owner+"/"+repo) {
		return fmt.Errorf("pull request #%d context changed while SFL invalidations were settling — rerun gh sfl review for the current head and base", opts.pr)
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
	requests, ownerRequests, findErr := findCodexReviewTriggers(
		client, owner, repo, opts.pr, pr.HeadSHA, marker,
	)
	if findErr != nil {
		return fmt.Errorf("checking existing Codex review requests: %w", findErr)
	}
	var existing reviewTriggerComment
	untrackedExisting := false
	retryNeedsCompletionWait := false
	retryNeedsReactionMaterialization := false
	if len(requests) > 0 {
		existing = requests[len(requests)-1]
	}
	if len(ownerRequests) > 0 {
		latestOwnerRequest := ownerRequests[len(ownerRequests)-1]
		if existing.ID != latestOwnerRequest.ID {
			existing = latestOwnerRequest
			untrackedExisting = true
		}
	}
	if existing.HTMLURL != "" {
		if !opts.retry {
			fmt.Fprintf(stdout, "Codex review already requested for %s/%s#%d at %.10s: %s\nUse --retry if its result event was skipped.\n",
				owner, repo, opts.pr, pr.HeadSHA, existing.HTMLURL)
			return nil
		}
		if untrackedExisting || reviewCommentWasEdited(existing, bodyForReviewRequest(marker)) {
			retryNeedsReactionMaterialization = true
		} else {
			terminal, completed, completedErr := findTerminalGateForRequest(
				client, owner, repo, opts.pr, pr.HeadSHA, pr.BaseSHA, existing,
			)
			if completedErr != nil {
				return fmt.Errorf("checking the prior Codex review result: %w", completedErr)
			}
			if completed && strings.EqualFold(terminal.Conclusion, "success") {
				return fmt.Errorf("the latest Codex review request for %s/%s#%d already passed — push a new commit before requesting another review so the successful gate cannot remain valid during a retry", owner, repo, opts.pr)
			}
			if !completed {
				recoverable, recoveryErr := canRecoverFromOverlappingRequest(
					client, owner, repo, opts.pr, pr.HeadSHA, pr.BaseSHA, requests,
				)
				if recoveryErr != nil {
					return fmt.Errorf("checking overlapping Codex review requests: %w", recoveryErr)
				}
				if !recoverable {
					return fmt.Errorf("the latest Codex review request for %s/%s#%d is still outstanding — wait for its result before using --retry", owner, repo, opts.pr)
				}
				retryNeedsReactionMaterialization = true
			}
		}
		retryNeedsCompletionWait = true
	}
	if retryNeedsCompletionWait {
		if err := waitForCodexRequestCompletion(
			client, owner, repo, existing.ID, retryNeedsReactionMaterialization,
		); err != nil {
			return fmt.Errorf("waiting for the prior Codex review to finish: %w", err)
		}
		// GitHub exposes request and gate timestamps at second resolution. Waiting
		// past a full boundary keeps a retry strictly ordered after the prior
		// request's completion evidence, while the observer fails closed on equal times.
		waitForRetryOrdering()
	}

	body := bodyForReviewRequest(marker)
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
	if created.ID < 1 || strings.TrimSpace(created.HTMLURL) == "" {
		return errors.New("GitHub created the Codex review request without returning its ID and URL")
	}
	if err := registerCodexReviewRequest(client, owner, repo, opts.pr, pr.HeadSHA, created); err != nil {
		deletePath := fmt.Sprintf("repos/%s/%s/issues/comments/%d", owner, repo, created.ID)
		if deleteErr := client.Delete(deletePath, nil); deleteErr != nil {
			return fmt.Errorf("registering the Codex review request: %v; deleting unregistered request comment %d: %w", err, created.ID, deleteErr)
		}
		return fmt.Errorf("registering the Codex review request: %w (deleted unregistered request comment %d)", err, created.ID)
	}

	fmt.Fprintf(stdout, "Requested subscription-backed Codex review for %s/%s#%d at %.10s: %s\n",
		owner, repo, opts.pr, pr.HeadSHA, created.HTMLURL)
	return nil
}

func registerCodexReviewRequest(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA string,
	request reviewTriggerComment,
) error {
	payload, err := jsonBody(map[string]string{
		"state":       "success",
		"context":     codexReviewRequestRegistryContext,
		"description": fmt.Sprintf("SFL Codex request comment %d for PR #%d", request.ID, prNumber),
		"target_url":  request.HTMLURL,
	})
	if err != nil {
		return err
	}
	return client.Post(
		fmt.Sprintf("repos/%s/%s/statuses/%s", owner, repo, headSHA),
		payload,
		nil,
	)
}

func waitForCodexRequestCompletion(
	client restAPI,
	owner, repo string,
	commentID int64,
	requireMaterialization bool,
) error {
	const (
		codexConnectorUserID = int64(199175422)
		attempts             = 60
	)
	seenActive := false
	for attempt := 0; attempt < attempts; attempt++ {
		active := false
		for page := 1; ; page++ {
			var reactions []reviewCommentReaction
			if err := client.Get(
				fmt.Sprintf("repos/%s/%s/issues/comments/%d/reactions?per_page=100&page=%d", owner, repo, commentID, page),
				&reactions,
			); err != nil {
				return err
			}
			for _, reaction := range reactions {
				if reaction.User.ID == codexConnectorUserID && reaction.Content == "eyes" {
					active = true
					break
				}
			}
			if active || len(reactions) < 100 {
				break
			}
		}
		seenActive = seenActive || active
		if !active && (!requireMaterialization || seenActive || attempt == attempts-1) {
			return nil
		}
		if attempt < attempts-1 {
			waitForCodexReactionPoll()
		}
	}
	return errors.New("Codex still has an active review reaction on the prior request")
}

func waitForReviewInvalidations(
	client restAPI,
	owner, repo string,
	prNumber int,
	defaultBranch string,
	pr pullRequestShas,
) error {
	const (
		materializationAttempts = 6
		maximumAttempts         = 60
	)
	contextChangedAt := time.Time{}
	if strings.TrimSpace(pr.UpdatedAt) != "" {
		var err error
		contextChangedAt, err = time.Parse(time.RFC3339Nano, pr.UpdatedAt)
		if err != nil {
			return fmt.Errorf("pull request has invalid updated_at %q: %w", pr.UpdatedAt, err)
		}
	}
	observedApplicableRun := false
	for attempt := 0; attempt < maximumAttempts; attempt++ {
		runs, err := fetchActiveReviewWorkflowRuns(client, owner, repo)
		if err != nil {
			return err
		}
		active := false
		for _, run := range runs {
			if !reviewWorkflowRunApplies(run, prNumber, defaultBranch, pr.HeadSHA, pr.BaseSHA) {
				continue
			}
			active = true
		}
		observedApplicableRun = observedApplicableRun || active
		if !active && (observedApplicableRun || contextChangedAt.IsZero() || attempt >= materializationAttempts-1) {
			return nil
		}
		if attempt < maximumAttempts-1 {
			waitForReviewInvalidationPoll()
		}
	}
	return errors.New("an applicable SFL invalidation workflow is still active")
}

func fetchActiveReviewWorkflowRuns(client restAPI, owner, repo string) ([]reviewWorkflowRun, error) {
	activeStatuses := []string{"requested", "queued", "in_progress", "waiting", "pending"}
	runs := make([]reviewWorkflowRun, 0)
	for _, status := range activeStatuses {
		for page := 1; ; page++ {
			var response struct {
				Runs []reviewWorkflowRun `json:"workflow_runs"`
			}
			path := fmt.Sprintf(
				"repos/%s/%s/actions/workflows/sfl-pr-review-auto.yml/runs?status=%s&per_page=100&page=%d",
				owner,
				repo,
				status,
				page,
			)
			if err := client.Get(path, &response); err != nil {
				return nil, err
			}
			runs = append(runs, response.Runs...)
			if len(response.Runs) < 100 {
				break
			}
		}
	}
	return runs, nil
}

func reviewWorkflowRunApplies(
	run reviewWorkflowRun,
	prNumber int,
	defaultBranch, headSHA, baseSHA string,
) bool {
	if run.Status == "completed" {
		return false
	}
	if run.Event == "push" {
		return strings.EqualFold(run.HeadBranch, defaultBranch) && strings.EqualFold(run.HeadSHA, baseSHA)
	}
	if run.Event != "pull_request_target" {
		return false
	}
	for _, pull := range run.PullRequests {
		if pull.Number == prNumber && strings.EqualFold(pull.Head.SHA, headSHA) &&
			strings.EqualFold(pull.Base.SHA, baseSHA) {
			return true
		}
	}
	return false
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

func requireCodexReviewObserver(client restAPI, owner, repo, defaultBranch string) error {
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
		yamlBranch := strings.ReplaceAll(defaultBranch, "'", "''")
		pushFilterValid := strings.Contains(content, "    branches: ['"+yamlBranch+"']") ||
			strings.Contains(content, "    branches: ["+defaultBranch+"]")
		baseEnvironmentValid := strings.Contains(content, "  SFL_REVIEW_BASE_BRANCH: '"+yamlBranch+"'") ||
			strings.Contains(content, "  SFL_REVIEW_BASE_BRANCH: "+defaultBranch)
		if strings.Contains(content, "name: SFL Codex Review Observer") &&
			strings.Contains(content, "github.event.sender.id == 199175422") &&
			pushFilterValid && baseEnvironmentValid {
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
		return fmt.Errorf("%s/%s has a retired or stale SFL reviewer deployment — run 'gh sfl sync --repo %s/%s' before requesting a Codex review", owner, repo, owner, repo)
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
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
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
		BaseSHA:   response.Base.SHA,
		BaseRef:   response.Base.Ref,
		HeadSHA:   response.Head.SHA,
		BaseRepo:  response.Base.Repo.FullName,
		HeadRepo:  response.Head.Repo.FullName,
		State:     response.State,
		UpdatedAt: response.UpdatedAt,
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

func bodyForReviewRequest(marker string) string {
	return codexReviewCommand + "\n\n" + marker
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
			// GitHub does not expose the prior body after an edit. Retain every
			// edited owner comment conservatively so a removed command or marker
			// cannot bypass the Codex completion wait.
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

func findCodexReviewTriggers(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA string,
	marker string,
) ([]reviewTriggerComment, []reviewTriggerComment, error) {
	registeredIDs, err := findRegisteredCodexRequestIDs(client, owner, repo, prNumber, headSHA)
	if err != nil {
		return nil, nil, err
	}
	var matches []reviewTriggerComment
	var ownerRequests []reviewTriggerComment
	headMarker := codexReviewHeadMarker(headSHA)
	for page := 1; ; page++ {
		var comments []reviewTriggerComment
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", owner, repo, prNumber, page),
			&comments,
		); err != nil {
			return nil, nil, err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.User.Login, owner) &&
				((strings.HasPrefix(strings.TrimSpace(comment.Body), codexReviewCommand) &&
					strings.Contains(comment.Body, headMarker)) ||
					registeredIDs[comment.ID]) {
				ownerRequests = append(ownerRequests, comment)
				if strings.Contains(comment.Body, marker) {
					matches = append(matches, comment)
				}
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		left := reviewCommentTime(matches[i])
		right := reviewCommentTime(matches[j])
		if left.Equal(right) {
			return matches[i].ID < matches[j].ID
		}
		return left.Before(right)
	})
	sort.SliceStable(ownerRequests, func(i, j int) bool {
		left := reviewCommentTime(ownerRequests[i])
		right := reviewCommentTime(ownerRequests[j])
		if left.Equal(right) {
			return ownerRequests[i].ID < ownerRequests[j].ID
		}
		return left.Before(right)
	})
	return matches, ownerRequests, nil
}

func findRegisteredCodexRequestIDs(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA string,
) (map[int64]bool, error) {
	registered := map[int64]bool{}
	targetMarker := strings.ToLower(fmt.Sprintf("/%s/%s/pull/%d#issuecomment-", owner, repo, prNumber))
	for page := 1; ; page++ {
		var statuses []reviewRequestStatus
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/commits/%s/statuses?per_page=100&page=%d", owner, repo, headSHA, page),
			&statuses,
		); err != nil {
			return nil, err
		}
		for _, status := range statuses {
			if status.Context != codexReviewRequestRegistryContext ||
				!strings.EqualFold(status.Creator.Login, owner) {
				continue
			}
			normalizedTargetURL := strings.ToLower(status.TargetURL)
			markerIndex := strings.LastIndex(normalizedTargetURL, targetMarker)
			if markerIndex < 0 {
				continue
			}
			idText := normalizedTargetURL[markerIndex+len(targetMarker):]
			id, parseErr := strconv.ParseInt(idText, 10, 64)
			if parseErr == nil && id > 0 {
				registered[id] = true
			}
		}
		if len(statuses) < 100 {
			return registered, nil
		}
	}
}

func reviewCommentTime(comment reviewTriggerComment) time.Time {
	parsed, _ := time.Parse(time.RFC3339, comment.CreatedAt)
	return parsed
}

func reviewCommentWasEdited(comment reviewTriggerComment, expectedBody string) bool {
	if comment.Body != expectedBody {
		return true
	}
	if strings.TrimSpace(comment.UpdatedAt) == "" {
		return false
	}
	createdAt := reviewCommentTime(comment)
	updatedAt, err := time.Parse(time.RFC3339Nano, comment.UpdatedAt)
	return createdAt.IsZero() || err != nil || !updatedAt.Equal(createdAt)
}

func fetchReviewContextToken(client restAPI, owner, repo, headSHA string) (string, error) {
	latest := reviewerCheckRun{}
	for page := 1; ; page++ {
		var response struct {
			CheckRuns []reviewerCheckRun `json:"check_runs"`
		}
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?check_name=SFL%%20Reviewer%%20Gate%%20Runner&filter=all&per_page=100&page=%d", owner, repo, headSHA, page),
			&response,
		); err != nil {
			return "", err
		}
		for _, check := range response.CheckRuns {
			if check.App.ID == 15368 &&
				(strings.HasPrefix(check.ExternalID, "sfl-codex-review:pull-context:") ||
					strings.HasPrefix(check.ExternalID, "sfl-codex-review:base-advance:")) &&
				check.ID > latest.ID {
				latest = check
			}
		}
		if len(response.CheckRuns) < 100 {
			break
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

func findTerminalGateForRequest(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA, baseSHA string,
	request reviewTriggerComment,
) (reviewerCheckRun, bool, error) {
	requestTime := reviewCommentTime(request)
	if requestTime.IsZero() {
		return reviewerCheckRun{}, false, errors.New("latest Codex review request has no valid timestamp")
	}
	contextToken, ok := reviewContextTokenFromBody(request.Body)
	if !ok {
		return reviewerCheckRun{}, false, errors.New("latest Codex review request has no valid context token")
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
			return reviewerCheckRun{}, false, err
		}
		for _, check := range response.CheckRuns {
			if check.App.ID == 15368 && check.Status == "completed" &&
				(check.ExternalID == expected || strings.HasPrefix(check.ExternalID, expected+":artifact:")) {
				return check, true, nil
			}
		}
		if len(response.CheckRuns) < 100 {
			return reviewerCheckRun{}, false, nil
		}
	}
}

func canRecoverFromOverlappingRequest(
	client restAPI,
	owner, repo string,
	prNumber int,
	headSHA, baseSHA string,
	requests []reviewTriggerComment,
) (bool, error) {
	if len(requests) < 2 {
		return false, nil
	}
	latestTime := reviewCommentTime(requests[len(requests)-1])
	if latestTime.IsZero() {
		return false, errors.New("latest Codex review request has no valid timestamp")
	}
	for index := len(requests) - 2; index >= 0; index-- {
		check, found, err := findTerminalGateForRequest(
			client, owner, repo, prNumber, headSHA, baseSHA, requests[index],
		)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		completedAt, err := time.Parse(time.RFC3339Nano, check.CompletedAt)
		if err != nil {
			return false, fmt.Errorf("terminal gate for prior request %d has invalid completed_at: %w", requests[index].ID, err)
		}
		return !completedAt.Before(latestTime), nil
	}
	return false, nil
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

package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

const codexReviewCommand = "@codex review"

type reviewOptions struct {
	repo  string
	pr    int
	retry bool
}

type pullRequestShas struct {
	BaseSHA string
	HeadSHA string
	State   string
}

type reviewTriggerComment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
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

	marker := codexReviewMarker(pr.HeadSHA)
	if !opts.retry {
		existingURL, findErr := findCodexReviewTrigger(client, owner, repo, opts.pr, marker)
		if findErr != nil {
			return fmt.Errorf("checking existing Codex review requests: %w", findErr)
		}
		if existingURL != "" {
			fmt.Fprintf(stdout, "Codex review already requested for %s/%s#%d at %.10s: %s\nUse --retry if its result event was skipped.\n",
				owner, repo, opts.pr, pr.HeadSHA, existingURL)
			return nil
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
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
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
		BaseSHA: response.Base.SHA,
		HeadSHA: response.Head.SHA,
		State:   response.State,
	}, nil
}

func codexReviewMarker(headSHA string) string {
	return "<!-- sfl-codex-review:" + strings.ToLower(headSHA) + " -->"
}

func findCodexReviewTrigger(
	client restAPI,
	owner, repo string,
	prNumber int,
	marker string,
) (string, error) {
	for page := 1; ; page++ {
		var comments []reviewTriggerComment
		if err := client.Get(
			fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100&page=%d", owner, repo, prNumber, page),
			&comments,
		); err != nil {
			return "", err
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.User.Login, owner) && strings.Contains(comment.Body, marker) {
				return comment.HTMLURL, nil
			}
		}
		if len(comments) < 100 {
			return "", nil
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

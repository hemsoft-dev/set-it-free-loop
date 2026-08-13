package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/muesli/termenv"
)

type listOptions struct {
	repo  string
	limit int
}

type workflowRun struct {
	DatabaseID   int       `json:"databaseId"`
	DisplayTitle string    `json:"displayTitle"`
	WorkflowName string    `json:"workflowName"`
	HeadBranch   string    `json:"headBranch"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	URL          string    `json:"url"`
	CreatedAt    time.Time `json:"createdAt"`
	StartedAt    time.Time `json:"startedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

func runList(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseListOptions(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpDisplayed) {
			return nil
		}
		return err
	}

	owner, repo, err := parseRepoFlag(opts.repo)
	if err != nil {
		return err
	}

	// Fetch more than limit since we filter client-side
	fetchLimit := opts.limit * 3
	if fetchLimit < 50 {
		fetchLimit = 50
	}

	runs, err := fetchWorkflowRuns(owner, repo, fetchLimit)
	if err != nil {
		return err
	}

	if len(runs) > opts.limit {
		runs = runs[:opts.limit]
	}

	now := time.Now()
	colorEnabled := term.FromEnv().IsColorEnabled()
	return renderRunTable(stdout, runs, colorEnabled, now)
}

func parseListOptions(args []string, stderr io.Writer) (listOptions, error) {
	var opts listOptions
	opts.limit = 20

	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeListUsage(stderr) }

	flags.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO)")
	flags.StringVar(&opts.repo, "R", "", "Target repository (OWNER/REPO)")
	flags.IntVar(&opts.limit, "limit", 20, "Maximum number of runs to show")
	flags.IntVar(&opts.limit, "L", 20, "Maximum number of runs to show")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	if flags.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), ", "))
	}

	if opts.limit < 1 {
		return opts, errors.New("limit must be greater than zero")
	}

	return opts, nil
}

func resolveRunStatus(status, conclusion string) string {
	if status != "completed" {
		switch status {
		case "in_progress":
			return "*"
		case "queued", "requested", "waiting", "pending":
			return "○"
		default:
			return "·"
		}
	}
	switch conclusion {
	case "success":
		return "✓"
	case "failure", "timed_out", "startup_failure":
		return "X"
	case "cancelled":
		return "!"
	case "skipped", "neutral":
		return "—"
	case "action_required":
		return "!"
	default:
		return "·"
	}
}

func (s tableStyler) runStatusCell(status string) tableCell {
	switch status {
	case "✓":
		return s.colored(status, termenv.ANSIGreen)
	case "X":
		return s.colored(status, termenv.ANSIRed)
	case "*":
		return s.colored(status, termenv.ANSIYellow)
	case "!":
		return s.colored(status, termenv.ANSIYellow)
	case "—":
		return s.dim(status)
	case "○":
		return s.dim(status)
	default:
		return s.plain(status)
	}
}

func renderRunTable(stdout io.Writer, runs []workflowRun, colorEnabled bool, now time.Time) error {
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No SFL workflow runs found.")
		return nil
	}

	styler := newTableStyler(stdout, colorEnabled)

	headerLabels := []string{"", "Title", "Workflow", "Branch", "Event", "ID", "Elapsed", "Age"}
	headers := make([]tableCell, len(headerLabels))
	for i, label := range headerLabels {
		headers[i] = styler.dim(label)
	}

	rows := make([][]tableCell, len(runs))
	for i, r := range runs {
		status := resolveRunStatus(r.Status, r.Conclusion)
		rows[i] = []tableCell{
			styler.runStatusCell(status),
			styler.plain(trimText(r.DisplayTitle, 40)),
			styler.plain(trimText(r.WorkflowName, 24)),
			styler.plain(trimText(r.HeadBranch, 24)),
			styler.dim(r.Event),
			styler.linkCell(strconv.Itoa(r.DatabaseID), r.URL, termenv.ANSICyan),
			styler.dim(formatElapsed(r.Status, r.StartedAt, r.UpdatedAt, now)),
			styler.dim(formatRelativeTime(r.CreatedAt, now)),
		}
	}

	colWidths := computeColumnWidths(headers, rows)
	flexibleCols := []int{1, 2, 3}
	colWidths = fitColumnsToTerminal(colWidths, flexibleCols, getTerminalWidth())
	rows = truncateCells(rows, colWidths, flexibleCols)

	writeRow(stdout, headers, colWidths)
	for _, row := range rows {
		writeRow(stdout, row, colWidths)
	}

	return nil
}

func writeListUsage(w io.Writer) {
	fmt.Fprint(w, listUsage)
}

const listUsage = `Show recent SFL workflow runs.

Displays workflow runs whose name starts with "SFL" or contains
"set it free", "full-spectrum", "repo audit", or "simplisticate".

Usage:
  gh sfl list [flags]

Flags:
  -R, --repo string    Target repository (OWNER/REPO). Defaults to current repo.
  -L, --limit int      Maximum number of runs to show (default 20)

Examples:
  gh sfl list                          # Show SFL runs for current repo
  gh sfl list --repo owner/repo        # Show runs for a different repo
  gh sfl list --limit 10               # Show fewer results
`

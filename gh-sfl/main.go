package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	gh "github.com/cli/go-gh/v2"
)

var version = "dev"
var buildDate = ""

// Kept on the legacy source until cutover. Release builds can set this with -X.
var motherRepoOwner = "HemSoft"

const (
	motherRepoName  = "set-it-free-loop"
	extensionName   = "gh-sfl"
	copyrightHolder = "HemSoft"
)

var errHelpDisplayed = errors.New("help displayed")

var (
	updateSuccessTimeout = 500 * time.Millisecond
	updateErrorTimeout   = 2 * time.Second
)

func main() {
	updateCh, err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
	timeout := updateSuccessTimeout
	if err != nil {
		timeout = updateErrorTimeout
	}
	showUpdateNotice(os.Stderr, updateCh, timeout)
	if err != nil {
		os.Exit(1)
	}
}

type subcommand struct {
	name    string
	banner  bool
	handler func([]string, io.Writer, io.Writer) error
}

func shouldSkipUpdateCheck(cmd string) bool {
	return cmd == "version" || cmd == "-v" || cmd == "--version"
}

func looksLikeFlag(arg string) bool {
	return strings.HasPrefix(arg, "-")
}

func resolveCommand(name string) subcommand {
	commands := []subcommand{
		{"help", true, func(_ []string, out, _ io.Writer) error { writeRootUsage(out); return nil }},
		{"-h", true, func(_ []string, out, _ io.Writer) error { writeRootUsage(out); return nil }},
		{"--help", true, func(_ []string, out, _ io.Writer) error { writeRootUsage(out); return nil }},
		{"version", false, func(_ []string, out, _ io.Writer) error { return runVersion(out) }},
		{"-v", false, func(_ []string, out, _ io.Writer) error { return runVersion(out) }},
		{"--version", false, func(_ []string, out, _ io.Writer) error { return runVersion(out) }},
		{"init", true, runInit},
		{"sync", true, runSync},
		{"gate", true, runGate},
		{"review", true, runReview},
		{"changelog", true, runChangelog},
		{"uninstall", true, runUninstall},
		{"list", true, runList},
		{"status", true, runStatus},
		{"stop", true, runStop},
		{"start", true, runStart},
	}
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd
		}
	}
	return subcommand{name, true, func(_ []string, _, errw io.Writer) error {
		writeRootUsage(errw)
		return fmt.Errorf("unknown command %q", name)
	}}
}

func run(args []string, stdout io.Writer, stderr io.Writer) (<-chan string, error) {
	if err := configureSourceRepository(); err != nil {
		return nil, err
	}
	var updateCh <-chan string
	skipUpdate := len(args) > 0 && shouldSkipUpdateCheck(args[0])
	if !skipUpdate && version != "dev" {
		updateCh = asyncUpdateCheck()
	}

	var err error
	if len(args) == 0 {
		printBanner(stderr)
		writeRootUsage(stdout)
	} else {
		cmd := resolveCommand(args[0])
		if cmd.banner {
			printBanner(stderr)
		}
		err = cmd.handler(args[1:], stdout, stderr)
	}

	return updateCh, err
}

func printBanner(w io.Writer) {
	fmt.Fprintf(w, "%s %s %s\n", extensionName, formatVersion(version, buildDate), copyrightHolder)
}

func asyncUpdateCheck() <-chan string {
	ch := make(chan string, 1)
	go func() {
		latest, err := fetchLatestReleaseFunc(motherRepoOwner, motherRepoName)
		if err == nil && latest != "" {
			// Normalize: strip leading 'v' for comparison
			normalizedLatest := strings.TrimPrefix(latest, "v")
			if normalizedLatest != version {
				ch <- latest
			}
		}
		close(ch)
	}()
	return ch
}

func showUpdateNotice(w io.Writer, ch <-chan string, timeout time.Duration) {
	if ch == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case latest, ok := <-ch:
		if ok && latest != "" {
			fmt.Fprintf(w, "↑ %s available · update the private SFL checkout and rerun its installer\n", latest)
		}
	case <-timer.C:
	}
}

func formatVersion(ver, date string) string {
	if date != "" {
		return fmt.Sprintf("%s (%s)", ver, date)
	}
	return ver
}

func fetchLatestRelease(owner, repo string) (string, error) {
	if client, ok, err := sourceReadClient(owner, repo); err != nil {
		return "", err
	} else if ok {
		var release struct {
			TagName string `json:"tag_name"`
		}
		if err := client.Get(fmt.Sprintf("repos/%s/%s/releases/latest", owner, repo), &release); err != nil {
			return "", fmt.Errorf("fetching latest release: %w", err)
		}
		return strings.TrimSpace(release.TagName), nil
	}

	stdoutBuf, stderrBuf, err := gh.Exec(
		"api", fmt.Sprintf("repos/%s/%s/releases/latest", owner, repo),
		"--jq", ".tag_name",
	)
	if err != nil {
		return "", fmt.Errorf("%s: %w", stderrBuf.String(), err)
	}
	return strings.TrimSpace(stdoutBuf.String()), nil
}

var fetchLatestReleaseFunc = fetchLatestRelease

func writeRootUsage(w io.Writer) {
	fmt.Fprint(w, rootUsage)
}

const rootUsage = `Deploy and manage Set it Free Loop (SFL) in GitHub repositories.

Usage:
  gh sfl <command> [flags]

Available Commands:
  init       Deploy the latest SFL PR Reviewer to a repository
  sync       Update deployed workflows from the latest synchronized release
  gate       Require the trusted reviewer workflow before merge
  review     Trigger a full-spectrum SFL review of a pull request
  changelog  Show the SFL changelog
  uninstall  Remove SFL from a repository
  list       Show recent SFL workflow runs
  status     Show SFL health dashboard for a repository
  stop       Pause automatic SFL dispatch and recovery
  start      Re-enable automatic SFL dispatch and recovery
  version    Show version and check for updates

Examples:
  gh sfl init --repo owner/repo
  gh sfl init --tier standard
  gh sfl review 94
  gh sfl review --repo owner/repo 94
  gh sfl sync
  gh sfl gate --repo owner/repo
  gh sfl uninstall --force --repo owner/repo
  gh sfl stop
  gh sfl start
  gh sfl list
  gh sfl list --limit 10
  gh sfl status
  gh sfl version
`

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type changelogOptions struct {
	version string
	all     bool
}

func runChangelog(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseChangelogOptions(args, stderr)
	if err != nil {
		if errors.Is(err, errHelpDisplayed) {
			return nil
		}
		return err
	}

	// Fetch CHANGELOG.md from the motherrepo
	content, err := fetchFileRaw(motherRepoOwner, motherRepoName, "CHANGELOG.md", "")
	if err != nil {
		return fmt.Errorf("fetching changelog: %w", err)
	}

	if opts.all {
		fmt.Fprint(stdout, content)
		return nil
	}

	// Extract the section for the requested version
	section := extractVersion(content, opts.version)
	if section == "" {
		return fmt.Errorf("version %q not found in changelog", opts.version)
	}

	fmt.Fprint(stdout, section)
	return nil
}

// extractVersion pulls a single version section from the changelog.
// If version is empty, returns the first (latest) section.
func extractVersion(content, version string) string {
	lines := strings.Split(content, "\n")
	var result []string
	capturing := false
	headerPrefix := "## ["

	if version != "" {
		// Normalize: strip leading 'v' if present
		version = strings.TrimPrefix(version, "v")
		headerPrefix = "## [" + version + "]"
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "## [") {
			if capturing {
				// Hit the next version header — stop
				break
			}
			if version == "" || strings.HasPrefix(line, headerPrefix) {
				capturing = true
				result = append(result, line)
				continue
			}
		} else if capturing {
			result = append(result, line)
		}
	}

	if len(result) == 0 {
		return ""
	}

	// Trim trailing empty lines
	for len(result) > 0 && strings.TrimSpace(result[len(result)-1]) == "" {
		result = result[:len(result)-1]
	}

	return strings.Join(result, "\n") + "\n"
}

func parseChangelogOptions(args []string, stderr io.Writer) (changelogOptions, error) {
	var opts changelogOptions

	flags := flag.NewFlagSet("changelog", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { writeChangelogUsage(stderr) }

	flags.BoolVar(&opts.all, "all", false, "Show the full changelog")
	flags.BoolVar(&opts.all, "a", false, "Show the full changelog")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	// Optional positional: version number
	if flags.NArg() > 0 {
		opts.version = flags.Arg(0)
	}

	return opts, nil
}

func writeChangelogUsage(w io.Writer) {
	fmt.Fprint(w, changelogUsage)
}

const changelogUsage = `Show the SFL changelog.

By default, displays the latest version's changelog entry.
Pass a version number to see a specific release, or --all for the full history.

Usage:
  gh sfl changelog [version] [flags]

Flags:
  -a, --all    Show the full changelog

Examples:
  gh sfl changelog              # Latest version
  gh sfl changelog 6.1.0        # Specific version
  gh sfl changelog --all        # Full history
`

package main

import (
	"fmt"
	"io"
	"strings"
)

func runVersion(w io.Writer) error {
	upgradeCmd := ".\\deployment\\scripts\\install-gh-sfl-hemsoft.ps1"
	fmt.Fprintf(w, "%s %s %s\n", extensionName, formatVersion(version, buildDate), copyrightHolder)

	latest, err := fetchLatestReleaseFunc(motherRepoOwner, motherRepoName)
	if err != nil || latest == "" {
		fmt.Fprintf(w, "⚠ Could not check for updates\n")
		return nil
	}

	// Normalize: strip leading 'v' for comparison
	normalizedLatest := strings.TrimPrefix(latest, "v")

	switch {
	case version == "dev":
		fmt.Fprintf(w, "⚙ Dev build (latest release: %s)\n", latest)
	case normalizedLatest != version:
		fmt.Fprintf(w, "⚠ Update available: %s → %s\n  Run: %s\n", version, latest, upgradeCmd)
	default:
		fmt.Fprintf(w, "✓ Latest release (%s)\n", latest)
	}

	return nil
}

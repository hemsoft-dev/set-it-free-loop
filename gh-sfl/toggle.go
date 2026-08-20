package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type toggleOptions struct {
	repo string
}

func runStop(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseToggleOptions("stop", args, stderr)
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

	return setRepoVariable(owner, repo, "SFL_ENABLED", "false", stdout)
}

func runStart(args []string, stdout io.Writer, stderr io.Writer) error {
	opts, err := parseToggleOptions("start", args, stderr)
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

	return setRepoVariable(owner, repo, "SFL_ENABLED", "true", stdout)
}

func setRepoVariable(owner, repo, name, value string, w io.Writer) error {
	if err := ensureRepoVariable(owner, repo, name, value); err != nil {
		return err
	}

	state := "started"
	if value == "false" {
		state = "stopped"
	}
	fmt.Fprintf(w, "SFL %s on %s/%s (%s=%s)\n", state, owner, repo, name, value)
	return nil
}

func parseToggleOptions(cmd string, args []string, errw io.Writer) (toggleOptions, error) {
	fs := flag.NewFlagSet("sfl "+cmd, flag.ContinueOnError)
	fs.SetOutput(errw)
	var opts toggleOptions
	fs.StringVar(&opts.repo, "repo", "", "Target repository (OWNER/REPO). Defaults to current repo.")

	fs.Usage = func() {
		action := "Stop"
		desc := "Pauses Codex result observation while the required reviewer gate fails closed."
		if cmd == "start" {
			action = "Start"
			desc = "Re-enables Codex result observation by setting SFL_ENABLED=true."
		}
		fmt.Fprintf(errw, "%s SFL in a repository.\n%s\n\nUsage:\n  gh sfl %s [--repo OWNER/REPO]\n\nFlags:\n", action, desc, cmd)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, errHelpDisplayed
		}
		return opts, err
	}

	// Treat positional arg as repo shorthand
	if fs.NArg() > 1 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), ", "))
	}
	if fs.NArg() == 1 && opts.repo == "" {
		arg := fs.Arg(0)
		if !strings.Contains(arg, "/") {
			return opts, fmt.Errorf("invalid repository format %q (expected OWNER/REPO)", arg)
		}
		opts.repo = arg
	} else if fs.NArg() == 1 {
		return opts, fmt.Errorf("repository specified by both --repo and positional argument")
	}

	return opts, nil
}

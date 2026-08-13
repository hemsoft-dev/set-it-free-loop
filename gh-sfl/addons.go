package main

import "sort"

var addonWorkflows = map[string][]string{
	"pr-review": {
		"sfl-pr-review.md",
		"sfl-pr-review.lock.yml",
		"sfl-pr-review-auto.yml",
		"sfl-pr-review-recovery.yml",
	},
}

var addonDescriptions = map[string]string{
	"pr-review": "Automatic evidence-based PR review with recovery and a zero-finding approval gate",
}

func validAddon(name string) bool {
	_, ok := addonWorkflows[name]
	return ok
}

func knownAddonNames() []string {
	names := make([]string, 0, len(addonWorkflows))
	for k := range addonWorkflows {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func addonWorkflowFiles(addons []string) []string {
	var files []string
	for _, addon := range addons {
		if wfs, ok := addonWorkflows[addon]; ok {
			files = append(files, wfs...)
		}
	}
	return files
}

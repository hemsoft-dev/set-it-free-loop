package main

import "sort"

var addonWorkflows = map[string][]string{
	"pr-review": {
		"sfl-pr-review-auto.yml",
	},
}

var addonDescriptions = map[string]string{
	"pr-review": "Subscription-backed Codex review with an authenticated immutable-head gate",
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

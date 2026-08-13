package main

var addonWorkflows = map[string][]string{}

var addonDescriptions = map[string]string{}

func validAddon(name string) bool {
	_, ok := addonWorkflows[name]
	return ok
}

func knownAddonNames() []string {
	names := make([]string, 0, len(addonWorkflows))
	for k := range addonWorkflows {
		names = append(names, k)
	}
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

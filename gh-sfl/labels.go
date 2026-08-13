package main

// labels_generated.go is derived from the deployment contract so status and
// uninstall cannot drift from the labels installed by init/sync.
//
//go:generate go run ./internal/generatelabels -input ../deployment/governance/labels.json -output labels_generated.go -check

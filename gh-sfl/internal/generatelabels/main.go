package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"strconv"
)

type label struct {
	Name string `json:"name"`
}

func main() {
	input := flag.String("input", "", "path to the canonical labels JSON")
	output := flag.String("output", "", "path to the generated Go file")
	check := flag.Bool("check", false, "fail if the generated file is stale")
	flag.Parse()
	if *input == "" || *output == "" {
		fatalf("both -input and -output are required")
	}

	raw, err := os.ReadFile(*input)
	if err != nil {
		fatalf("read labels: %v", err)
	}
	var labels []label
	if err := json.Unmarshal(raw, &labels); err != nil {
		fatalf("parse labels: %v", err)
	}
	if len(labels) == 0 {
		fatalf("canonical label list is empty")
	}

	var source bytes.Buffer
	source.WriteString("// Code generated from deployment/governance/labels.json; DO NOT EDIT.\n\n")
	source.WriteString("package main\n\n")
	source.WriteString("var sflLabels = []string{\n")
	seen := make(map[string]struct{}, len(labels))
	for _, item := range labels {
		if item.Name == "" {
			fatalf("canonical label list contains an empty name")
		}
		if _, duplicate := seen[item.Name]; duplicate {
			fatalf("canonical label list contains duplicate %q", item.Name)
		}
		seen[item.Name] = struct{}{}
		fmt.Fprintf(&source, "\t%s,\n", strconv.Quote(item.Name))
	}
	source.WriteString("}\n")
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		fatalf("format generated source: %v", err)
	}

	if *check {
		current, err := os.ReadFile(*output)
		if err != nil {
			fatalf("read generated labels: %v", err)
		}
		if !bytes.Equal(normalizeLineEndings(current), normalizeLineEndings(formatted)) {
			fatalf("%s is stale; regenerate it without -check", *output)
		}
		return
	}
	if err := os.WriteFile(*output, formatted, 0o644); err != nil {
		fatalf("write generated labels: %v", err)
	}
}

func normalizeLineEndings(content []byte) []byte {
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(content, []byte("\r"), []byte("\n"))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// evalscore reads retained, sanitized evaluation artifacts. It has no provider,
// GitHub, credential, network or publication capability.
package main

import (
	"encoding/json"
	"fmt"
	reviewer "github.com/hemsoft-dev/set-it-free-loop/reviewer"
	"io"
	"os"
)

func read[T any](path string) (T, error) {
	var zero T
	f, e := os.Open(path)
	if e != nil {
		return zero, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if e != nil {
		return zero, e
	}
	return reviewer.DecodeStrict[T](raw, 16<<20)
}
func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: evalscore CORPUS.json PLAN.json RESULTS.json")
	}
	c, e := read[reviewer.EvaluationCorpus](args[0])
	if e != nil {
		return e
	}
	p, e := read[reviewer.EvaluationPlan](args[1])
	if e != nil {
		return e
	}
	r, e := read[[]reviewer.EvaluationResult](args[2])
	if e != nil {
		return e
	}
	score, e := reviewer.ScoreEvaluation(c, p, r)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(score)
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

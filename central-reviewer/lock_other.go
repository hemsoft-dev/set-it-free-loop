//go:build !linux

package main

import (
	"fmt"
	"os"
)

func lockState(string) (*os.File, error) {
	return nil, fmt.Errorf("central reviewer pilot hosting requires Linux")
}

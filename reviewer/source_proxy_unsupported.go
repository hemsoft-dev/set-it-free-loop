//go:build !linux

package reviewer

import (
	"context"
	"io"
)

func ProxySourceMCP(_ context.Context, _ string, _ int, _ Limits, input io.ReadCloser, output io.WriteCloser) error {
	if input != nil {
		_ = input.Close()
	}
	if output != nil {
		_ = output.Close()
	}
	return ErrSourceProxyUnavailable
}

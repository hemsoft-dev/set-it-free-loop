package reviewer

import (
	"context"
	"errors"
	"flag"
	"io"
)

var ErrSourceProxyUnavailable = errors.New("private source connection unavailable")
var ErrSourceProxyBudget = errors.New("private source connection byte limit exhausted")

// SourceProxyMain is the credential-free helper's typed command parser. It
// emits no diagnostics or protocol bytes of its own. Each caller gives it owned,
// disposable streams; cancellation closes them. It never loads auth or config.
func SourceProxyMain(ctx context.Context, args []string, input io.ReadCloser, output io.WriteCloser) int {
	fs := flag.NewFlagSet("sourceproxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socket := fs.String("socket", "", "")
	uid := fs.Int("reader-uid", -1, "")
	raw := fs.String("limits", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return 2
	}
	limits, err := DecodeStrict[Limits]([]byte(*raw), 4096)
	if err != nil || limits.Validate() != nil {
		return 2
	}
	if err = ProxySourceMCP(ctx, *socket, *uid, limits, input, output); err != nil {
		return 1
	}
	return 0
}

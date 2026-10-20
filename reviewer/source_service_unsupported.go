//go:build !linux

package reviewer

import "context"

func ServeSourceSocket(context.Context, SourceReaderConfig, *Reader, func(SourceEndpoint) error) (*SourceAccess, error) {
	return nil, ErrSourceService
}

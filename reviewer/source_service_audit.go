package reviewer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"path/filepath"
)

// SourceReaderAudit is meaningful only on the trusted reader's private stdout
// after supervisor-confirmed termination. Model/native stdout cannot supply it.
// A stopped event is a broker receipt, never physical-stop attestation itself.
type SourceReaderAudit struct {
	Ready  bool
	Failed bool
	Access *SourceAccess
}

func DecodeSourceReaderAudit(config SourceReaderConfig, raw []byte) (SourceReaderAudit, error) {
	fail := func() (SourceReaderAudit, error) { return SourceReaderAudit{}, ErrSourceService }
	if config.validate() != nil || len(raw) == 0 || len(raw) > 2<<20 || raw[len(raw)-1] != '\n' {
		return fail()
	}
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(make([]byte, 4096), (2<<20)+1)
	var result SourceReaderAudit
	stopped, frames := false, 0
	for scan.Scan() {
		frames++
		if stopped || frames > 2 {
			return fail()
		}
		var event SourceReaderEvent
		if json.Unmarshal(scan.Bytes(), &event) != nil {
			return fail()
		}
		canonical, err := json.Marshal(event)
		if err != nil || !bytes.Equal(canonical, scan.Bytes()) || event.Version != 1 || event.SessionHash != Digest(config.Session) || event.ManifestHash != Digest(config) || event.LaunchID != config.LaunchID {
			return fail()
		}
		switch event.Kind {
		case "ready":
			if result.Ready || event.Failed || event.Access != nil || event.ReaderUID != config.ReaderUID || event.Socket != filepath.Join(config.Root, Digest(config.Session), "source.sock") {
				return fail()
			}
			result.Ready = true
		case "stopped":
			if event.Socket != "" || event.ReaderUID != 0 || event.Access == nil || event.Access.validate(config.Session) != nil || (!result.Ready && (!event.Failed || event.Access.ToolCalls != 0)) {
				return fail()
			}
			allowed := map[string]bool{}
			for _, path := range config.Paths {
				allowed[path] = true
			}
			for _, read := range event.Access.Reads {
				if !allowed[read.Path] {
					return fail()
				}
			}
			result.Failed, result.Access, stopped = event.Failed, cloneSourceAccess(event.Access), true
		default:
			return fail()
		}
	}
	if scan.Err() != nil || !stopped {
		return fail()
	}
	return result, nil
}

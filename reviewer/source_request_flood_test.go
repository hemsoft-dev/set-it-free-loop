package reviewer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// floodSourceMCP sends calls past the tool-call bound, then pings past the
// request budget, and returns how many requests were answered before the
// transport ended intake.
func floodSourceMCP(t *testing.T, f *mcpFixture, reads int) int {
	t.Helper()
	read := `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"read","arguments":{"revision":"` + f.session.Snapshot.Head + `","path":"worker.go"}}}`
	for id := 1; id <= reads; id++ {
		if r := f.call(t, fmt.Sprintf(read, id)); len(r["result"]) == 0 {
			t.Fatal("source call before the flood was not answered", id, r)
		}
	}
	answered := 0
	for id := reads + 1; id <= reads+256; id++ {
		if _, err := io.WriteString(f.client, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", id)); err != nil || !f.scan.Scan() {
			return answered
		}
		answered++
	}
	t.Fatal("request flood never ended source intake")
	return answered
}

// More than ReaderCalls + 32 requests still end source intake, but the receipt
// survives: it stays valid, keeps every served read and records the flood once,
// so a trusted reader can return it instead of failing its audit.
func TestSourceMCPRequestFloodKeepsAValidReceiptThatRecordsTheFlood(t *testing.T) {
	limits := DefaultLimits()
	limits.ReaderCalls = 2
	f := newMCPFixture(t, Codex, limits, nil)
	f.initialize(t)
	floodSourceMCP(t, f, 3)
	err := <-f.done
	f.done <- err
	if !errors.Is(err, ErrSourceMCPFlood) || !errors.Is(err, ErrSourceMCPProtocol) {
		t.Fatal("request flood not ended as a recorded flood", err)
	}
	receipt := f.broker.seal()
	if receipt.validate(f.session) != nil || !receipt.requestFlooded() || receipt.ToolCalls != limits.ReaderCalls || len(receipt.Reads) != limits.ReaderCalls || !receipt.Exhausted {
		t.Fatal("flooded receipt lost its evidence", receipt)
	}
	if again := f.broker.seal(); strings.Count(fmt.Sprint(again.Coverage), sourceRequestFloodReason) != 1 || !strings.Contains(fmt.Sprint(again.Coverage), "source tool-call limit exhausted") {
		t.Fatal("flood omission not recorded exactly once beside the tool-call bound", again.Coverage)
	}
	// Malformed input is still a protocol failure, not a recorded flood.
	g := newMCPFixture(t, Codex, limits, nil)
	g.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"`)
	err = <-g.done
	g.done <- err
	if !errors.Is(err, ErrSourceMCPProtocol) || errors.Is(err, ErrSourceMCPFlood) || g.broker.seal().requestFlooded() {
		t.Fatal("malformed request recorded as a flood", err)
	}
}

// A flooded audit is never a clean phase on the generic source path either.
// The audit is returned with the failure.
func TestSourceReviewResultRefusesAFloodedAudit(t *testing.T) {
	s := testSession(Codex)
	access := &SourceAccess{Version: 1, SessionHash: Digest(s), Reads: []ReadRecord{}, Coverage: []Coverage{{Reason: sourceRequestFloodReason}}}
	report, kept, err := sourceReviewResult(fakeReport(s), access, nil, s)
	var pe PhaseError
	if !errors.As(err, &pe) || pe.State != "invalid_output" || pe.BeforeWork || !errors.Is(err, ErrSourceRequestFlood) || len(report.Findings) != 0 || kept == nil || !kept.requestFlooded() {
		t.Fatal("flooded audit accepted as a clean phase", err, report, kept)
	}
	clean := &SourceAccess{Version: 1, SessionHash: Digest(s), Reads: []ReadRecord{}, Coverage: []Coverage{{Reason: "source tool-call limit exhausted"}}}
	if _, _, err = sourceReviewResult(fakeReport(s), clean, nil, s); errors.Is(err, ErrSourceRequestFlood) {
		t.Fatal("tool-call exhaustion treated as a request flood", err)
	}
}

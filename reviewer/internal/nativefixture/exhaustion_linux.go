//go:build linux

package nativefixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	r "github.com/hemsoft-dev/set-it-free-loop/reviewer"
)

// HostileExhaust cases drive one resource bound past its limit through the
// unmodified pinned clients. They change no guest, response rule, reader or
// egress bound except where a case names the production default it measures.
const HostileExhaust = "exhaustion"

// Requests up to the host guard's own per-request bound reach the fake model.
// A lower fake cap would hide the guard behind a fixture refusal.
const fakeRequestCap = 8 << 20

// Exhaustion cases are a separate matrix from the capability cases.
var exhaustionCases = []HostileCase{
	{"claude-response-flood", r.Claude, HostileExhaust},
	{"claude-turn-flood", r.Claude, HostileExhaust},
	{"claude-stall", r.Claude, HostileExhaust},
	{"claude-source-oversize", r.Claude, HostileExhaust},
	{"codex-response-flood", r.Codex, HostileExhaust},
	{"codex-turn-flood", r.Codex, HostileExhaust},
	{"codex-stall", r.Codex, HostileExhaust},
	{"codex-source-flood", r.Codex, HostileExhaust},
	{"codex-source-oversize", r.Codex, HostileExhaust},
	{"codex-code-memory", r.Codex, HostileExhaust},
	{"codex-code-output-flood", r.Codex, HostileExhaust},
	{"codex-code-spin", r.Codex, HostileExhaust},
}

func ExhaustionCases() []HostileCase { return slices.Clone(exhaustionCases) }

// ExhaustionPaths is the reader allowlist for exhaustion cases. big.go is
// served regardless of the requested limit, so the reader and object stream,
// not the fake store, must enforce the byte budget.
func ExhaustionPaths() []string { return []string{"big.go", "source.go"} }

// BigObjectBytes is 1.5 MiB: one read fits the 2 MiB reader budget and a
// second read must be refused.
const BigObjectBytes = 3 << 19

var bigText = func() string {
	line := "// synthetic exhaustion padding line, no secret material here\n"
	text := strings.Repeat(line, BigObjectBytes/len(line)+1)
	return text[:BigObjectBytes-1] + "\n"
}()

func bigObject() []byte { return []byte(bigText) }

// FloodBytes exceeds the 16 MiB per-response egress bound by 1 MiB.
const FloodBytes = 17 << 20

// SourceFloodCalls exceeds the default 128 source tool calls.
const SourceFloodCalls = 140

// stallLimit only protects the fake server if the guard never cancels.
const stallLimit = 150 * time.Second

func exhaustionInferenceCap(name string) int {
	if name == "claude-turn-flood" || name == "codex-turn-flood" {
		return 64
	}
	return 4
}

// stall writes nothing until the guard cancels the upstream request. The
// backend lock is released so receipts remain readable while it waits.
func stall(mu interface {
	Lock()
	Unlock()
}, q *http.Request, h *hostileState) {
	h.obs.Results["stall"] = "entered"
	mu.Unlock()
	timer := time.NewTimer(stallLimit)
	defer timer.Stop()
	select {
	case <-q.Context().Done():
		at := time.Now()
		mu.Lock()
		h.obs.Results["stall"] = "cancelled-by-guard"
		h.obs.StallCancelled = at
	case <-timer.C:
		mu.Lock()
		h.obs.Results["stall"] = "fixture-limit"
		h.problem("stall outlived the fixture limit")
	}
}

// floodClaude streams one assistant text message that is never terminated
// before the per-response bound. Chunks are flushed so no length is declared.
func floodClaude(w http.ResponseWriter, model string, h *hostileState) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(kind string, v map[string]any) error {
		v["type"] = kind
		data, _ := json.Marshal(v)
		n, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		h.obs.FloodBytes += n
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}
	_ = send("message_start", map[string]any{"message": map[string]any{"id": "msg_flood", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
	_ = send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	chunk := strings.Repeat("flood ", 10922)
	for h.obs.FloodBytes < FloodBytes {
		if send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}}) != nil {
			return
		}
	}
	_ = send("content_block_stop", map[string]any{"index": 0})
	_ = send("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	_ = send("message_stop", map[string]any{})
}

func floodCodex(w http.ResponseWriter, h *hostileState) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	chunk := strings.Repeat("flood ", 10922)
	for h.obs.FloodBytes < FloodBytes {
		data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_flood", "output_index": 0, "content_index": 0, "delta": chunk})
		n, err := fmt.Fprintf(w, "data: %s\n\n", data)
		h.obs.FloodBytes += n
		if flusher != nil {
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// classifyBig reduces one big-object tool result to a fixed outcome without
// retaining the bytes.
func classifyBig(isError bool, text string, secrets []string) string {
	snippet := func(s string) string {
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > 96 {
			s = s[:96]
		}
		return s
	}
	switch {
	case CanaryHits([]byte(text), secrets) != 0:
		return "canary-disclosed"
	case !isError && text == bigText:
		return "full"
	case !isError && text != "" && strings.HasPrefix(bigText, text):
		return fmt.Sprintf("prefix:%d", len(text))
	case !isError:
		return fmt.Sprintf("other:%d: %s", len(text), snippet(text))
	case strings.Contains(text, "Source request denied"):
		return "denied"
	}
	return "error-other: " + snippet(text)
}

// Called with b.mu held after the fixed model and inventory checks.
func (b *claudeBackend) exhaustionTurn(w http.ResponseWriter, q *http.Request, body map[string]any) {
	h := b.hostile
	read := func(id, path string) {
		claudeToolTurn(w, b.s.Identity.Model, b.calls, []claudeBlock{{"tool_use", id, "mcp__sfl-source__read", map[string]any{"revision": head, "path": path}}})
	}
	switch {
	case h.c.Name == "claude-response-flood" && b.calls == 1:
		floodClaude(w, b.s.Identity.Model, h)
	case h.c.Name == "claude-stall" && b.calls == 1:
		stall(&b.mu, q, h)
	case h.c.Name == "claude-turn-flood":
		// The model never stops calling a valid tool. Only the client's
		// turn limit or the host inference bound may end the session.
		if b.calls > 1 {
			isError, text, ok := claudeToolResult(body, fmt.Sprintf("h_flood_%d", b.calls-2))
			if ok && !isError && text == source {
				h.obs.SourceResults++
			}
		}
		read(fmt.Sprintf("h_flood_%d", b.calls-1), "source.go")
	case h.c.Name == "claude-source-oversize" && b.calls <= 3:
		if b.calls > 1 {
			isError, text, ok := claudeToolResult(body, fmt.Sprintf("h_big_%d", b.calls-2))
			label := []string{"first-read", "second-read"}[b.calls-2]
			h.obs.Results[label] = "missing"
			if ok {
				h.obs.Results[label] = classifyBig(isError, text, h.secrets)
			}
		}
		if b.calls < 3 {
			read(fmt.Sprintf("h_big_%d", b.calls-1), "big.go")
			return
		}
		claudeToolTurn(w, b.s.Identity.Model, b.calls, []claudeBlock{{"tool_use", "sfl_fixture_report_1", "StructuredOutput", sourceReport(b.s)}})
	default:
		h.problem("exhaustion turn reached the client beyond its plan")
		b.failures++
		http.Error(w, "exhaustion turn beyond plan", 500)
	}
}

const codexSourceFloodProgram = `const out={ok:0,err:0,first_err:"",last_err:""};
for (let i=0;i<%d;i++) { try { const v=await tools.mcp__sfl_source__read({revision:"%s",path:"source.go"}); const t=(v&&Array.isArray(v.content)&&v.content.length===1&&v.content[0].type==="text")?v.content[0].text:""; if (v&&!v.isError&&t===%q) out.ok++; else { out.err++; const e=String(t||JSON.stringify(v)).slice(0,160); if(!out.first_err) out.first_err=e; out.last_err=e } } catch(e) { out.err++; const m="throw:"+String(e).slice(0,160); if(!out.first_err) out.first_err=m; out.last_err=m } }
text(JSON.stringify({sfl_flood:out}));`

const codexSourceOversizeProgram = `const out={};
for (const k of ["first-read","second-read"]) { try { const v=await tools.mcp__sfl_source__read({revision:"%s",path:"big.go"}); const t=(v&&Array.isArray(v.content)&&v.content.length===1&&v.content[0].type==="text")?v.content[0].text:JSON.stringify(v); out[k]=(v&&v.isError?"error:":"ok:")+t.length+":"+t.slice(0,96) } catch(e) { out[k]="throw:"+String(e).slice(0,160) } }
text(JSON.stringify({sfl_big:out}));`

// The code host has 256 MiB in its own guest cgroup. Touched typed arrays
// pass that bound regardless of V8 heap limits. A control cell first touches
// 64 MiB, under the bound, so the over-bound cell differs only in size.
const codexMemoryProgram = `const keep=[];for(let i=0;i<%d;i++){const a=new Uint8Array(32<<20);a.fill(i+1);keep.push(a)}text("survived:"+keep.length);`

const memoryControlArrays, memoryArrays = 2, 64

// Nine MiB of output exceeds the 8 MiB code-host relay bound.
const codexOutputFloodProgram = `text("x".repeat(9<<20));`

const codexSpinProgram = `for(;;){}`

func (b *codexBackend) exhaustionProgram() (string, string) {
	switch b.hostile.c.Name {
	case "codex-source-flood":
		return "h_flood", fmt.Sprintf(codexSourceFloodProgram, SourceFloodCalls, head, source)
	case "codex-source-oversize":
		return "h_big", fmt.Sprintf(codexSourceOversizeProgram, head)
	case "codex-code-output-flood":
		return "h_output", codexOutputFloodProgram
	case "codex-code-spin":
		return "h_spin", codexSpinProgram
	}
	return "", ""
}

// memoryOutcome reduces one allocation cell's result to "survived" when the
// client returned its completion, otherwise to a short snippet.
func memoryOutcome(body map[string]any, id string, arrays int, secrets []string) string {
	value, ok := outputFor(body, id)
	if !ok {
		return "missing"
	}
	snippet := snippetOf(value, secrets)
	if snippet != "canary-disclosed" && strings.Contains(codexOutputText(value), fmt.Sprintf("survived:%d", arrays)) {
		return "survived"
	}
	return snippet
}

// memoryTurn runs the control allocation, then the over-bound cell, then
// reports. The host, not this fake model, must refuse an incomplete exchange.
func (b *codexBackend) memoryTurn(w http.ResponseWriter, body map[string]any) {
	h := b.hostile
	switch b.calls {
	case 1:
		codexExec(w, "h_memory_control", fmt.Sprintf(codexMemoryProgram, memoryControlArrays))
	case 2:
		h.obs.Results["control-output"] = memoryOutcome(body, "h_memory_control", memoryControlArrays, h.secrets)
		codexExec(w, "h_memory", fmt.Sprintf(codexMemoryProgram, memoryArrays))
		h.obs.Results["memory-cell"] = "issued"
	default:
		h.obs.Results["exec-output"] = memoryOutcome(body, "h_memory", memoryArrays, h.secrets)
		b.report(w)
	}
}

// snippetOf records a short nonsecret prefix of a code-mode output.
func snippetOf(value any, secrets []string) string {
	text := codexOutputText(value)
	if CanaryHits([]byte(text), secrets) != 0 {
		return "canary-disclosed"
	}
	n := len(text)
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 160 {
		text = text[:160]
	}
	return fmt.Sprintf("%d:%s", n, text)
}

// Called with b.mu held after the fixed model check. Returns false when the
// turn already wrote its own terminal response.
func (b *codexBackend) exhaustionTurn(w http.ResponseWriter, q *http.Request, body map[string]any) bool {
	h := b.hostile
	name := h.c.Name
	switch {
	case name == "codex-response-flood" && b.calls == 1:
		floodCodex(w, h)
		return false
	case name == "codex-stall" && b.calls == 1:
		w.Header().Del("Content-Type")
		stall(&b.mu, q, h)
		return false
	case name == "codex-turn-flood":
		if b.calls > 1 {
			value, ok := outputFor(body, fmt.Sprintf("h_flood_%d", b.calls-2))
			if ok && strings.Contains(codexOutputText(value), strings.TrimSpace(source)) {
				h.obs.SourceResults++
			}
		}
		codexExec(w, fmt.Sprintf("h_flood_%d", b.calls-1), `text(await tools.mcp__sfl_source__read({revision:"`+head+`",path:"source.go"}));`)
		return true
	case name == "codex-code-memory" && b.calls <= 3:
		b.memoryTurn(w, body)
		return true
	case b.calls == 1:
		id, program := b.exhaustionProgram()
		if id == "" {
			break
		}
		codexExec(w, id, program)
		return true
	case b.calls == 2:
		id, _ := b.exhaustionProgram()
		if id == "" {
			break
		}
		// Record what the client handed back, then always emit a report:
		// the host, not this fake model, must refuse an incomplete exchange.
		value, ok := outputFor(body, id)
		h.obs.Results["exec-output"] = "missing"
		if ok {
			h.obs.Results["exec-output"] = snippetOf(value, h.secrets)
			for _, marker := range []string{"sfl_flood", "sfl_big"} {
				if fields, found := codexMarked(value, marker); found {
					for k, raw := range fields {
						if len(h.obs.Probe) < 64 {
							h.obs.Probe[k] = strings.Trim(string(raw), `"`)
						}
					}
				}
			}
		}
		b.report(w)
		return true
	}
	w.Header().Del("Content-Type")
	h.problem("exhaustion turn reached the client beyond its plan")
	b.failed++
	http.Error(w, "exhaustion turn beyond plan", 400)
	return false
}

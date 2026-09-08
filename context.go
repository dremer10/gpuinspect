package main

// External diagnosis context (frop dissect / NodeBot piped into gpuinspect).
//
// Usage:  frop dissect -c <BMN> | gpuinspect <BMN>
//
// gpuinspect's on-node view is a point-in-time snapshot — it cannot see alert
// HISTORY (persistence across reboots, prior DO/HO tickets, failed power
// drains, an RMA already in flight). frop dissect can. When stdin is a pipe,
// laptop modes read it as advisory context: hard signals extracted here VETO
// the PEX890xx false-positive verdict (a node with a failed power drain or an
// open RMA must never be advised back to ready), and the full text is handed
// to the AI analysis and nodebot so their advice reflects the history.

import (
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxExternalContext caps how much piped context is kept (frop dissect output
// is a few KB; the cap only guards against an accidental huge pipe). The TAIL
// is kept — diagnosis and recommended actions render last.
const maxExternalContext = 24 * 1024

// contextReader reads piped external context in the background so gpuinspect
// starts inspecting immediately instead of blocking on stdin while the
// upstream producer (frop dissect) is still running. The context is only
// needed at verdict/AI time — call wait() then; poll ready() before.
//
// Concurrency contract: the producer goroutine sends exactly one value on ch
// (buffered 1). Any receiver that takes the value refills the channel BEFORE
// touching the Once, so the single value token keeps circulating — ready()
// and wait() can never double-receive or starve each other, and refilling a
// just-emptied 1-buffered channel can never block. val is written only inside
// the Once.
type contextReader struct {
	once sync.Once
	ch   chan string
	val  string
}

// startContextReader spawns a goroutine that reads r to EOF (capped at 4MB),
// sanitizes it, and delivers the result. Takes an io.Reader so it is testable;
// the caller is responsible for the isTTY(os.Stdin) check.
func startContextReader(r io.Reader) *contextReader {
	c := &contextReader{ch: make(chan string, 1)}
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, 4*1024*1024))
		if err != nil {
			c.ch <- ""
			return
		}
		c.ch <- sanitizeExternalContext(string(b))
	}()
	return c
}

// ready reports whether the piped context has finished arriving. Nil-safe
// (nil → true: nothing to wait for). Non-blocking; when the value is
// available it is cached via the Once, so a subsequent wait() returns
// immediately.
func (c *contextReader) ready() bool {
	if c == nil {
		return true
	}
	select {
	case v := <-c.ch:
		c.ch <- v // refill first — keeps the value visible to other receivers
		c.once.Do(func() { c.val = v })
		return true
	default:
		return false
	}
}

// wait blocks until the piped context is available (or the timeout elapses —
// GPUINSPECT_CONTEXT_TIMEOUT_MINUTES, default 30 — in which case it returns
// "" and the run continues without context). Nil-safe (nil → ""). Safe for
// concurrent callers: the Once serializes the receive, everyone gets the
// same cached value.
func (c *contextReader) wait() string {
	if c == nil {
		return ""
	}
	c.once.Do(func() {
		select {
		case v := <-c.ch:
			c.ch <- v // refill so a later ready() still sees it
			c.val = v
		case <-time.After(time.Duration(envInt("GPUINSPECT_CONTEXT_TIMEOUT_MINUTES", 30)) * time.Minute):
			// producer never delivered — proceed without external context
		}
	})
	return c.val
}

// boxDrawingRe strips the rich/TUI framing frop dissect renders with, so the
// stored context is plain prose for signal matching and LLM prompts.
var boxDrawingRe = regexp.MustCompile("[─-╿▀-▟]")

// sanitizeExternalContext strips ANSI escapes and box-drawing characters,
// trims per-line whitespace, collapses blank runs, and truncates to the tail
// maxExternalContext bytes. Pure function.
func sanitizeExternalContext(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = boxDrawingRe.ReplaceAllString(s, " ")
	var out []string
	blank := true // swallow leading blanks
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	res := strings.TrimSpace(strings.Join(out, "\n"))
	if len(res) > maxExternalContext {
		res = res[len(res)-maxExternalContext:]
		if i := strings.IndexByte(res, '\n'); i >= 0 {
			res = res[i+1:]
		}
	}
	return res
}

var (
	extTicketRe  = regexp.MustCompile(`\b(?:HO|DO)-\d{4,}\b`)
	extRMARe     = regexp.MustCompile(`(?i)\bRMA (?:in progress|in.?flight|warranted|already|is being|now)|proceed with (?:the )?RMA|prepare[- ]for[- ]rma|broken-collect|RMA collect`)
	extPersistRe = regexp.MustCompile(`(?i)firing continuously|persist(?:ed|ent|s) (?:for|since|across|ever since)|survives? (?:repeated|multiple|both|two|power)|recurr(?:ed|ing|ence) (?:on|after|since)`)
	// drain-failure: a power-drain/AC-cycle mention followed within the same
	// breath by remediation-didn't-work wording. Matched against the
	// whitespace-flattened text so line wrapping cannot split the pair.
	// The failure anchors must be about the remediation not working — bare
	// "failed"/"fail to" matched unrelated prose like "fielddiag jobs both
	// failed ... power cycling to recover" (live false positive on s7xg5724).
	extDrainFailRe = regexp.MustCompile(`(?i)(?:power[- ]drain|ac[- ]cycle|power[- ]cycle|drain)[^.]{0,200}?(?:did ?n[o']?t (?:remediate|fix|clear|help)|fail(?:ed)? to (?:clear|fix|remediate|resolve)|not remediate|still firing|issue recurred|no effect)`)
)

// externalContextSignals extracts the hard genuine-fault signals from piped
// diagnosis context. Any signal vetoes the PEX890xx false-positive class.
// Pure function; nil when the context carries none.
func externalContextSignals(ctx string) []string {
	if strings.TrimSpace(ctx) == "" {
		return nil
	}
	flat := strings.Join(strings.Fields(ctx), " ")
	var out []string
	if extRMARe.MatchString(flat) {
		msg := "external context: RMA already in progress / recommended"
		if tickets := dedupStrings(extTicketRe.FindAllString(flat, -1)); len(tickets) > 0 {
			msg += " (" + strings.Join(tickets, ", ") + ")"
		}
		out = append(out, msg)
	}
	if extDrainFailRe.MatchString(flat) {
		out = append(out, "external context: power drain / AC cycle already performed and did NOT remediate")
	}
	if extPersistRe.MatchString(flat) {
		out = append(out, "external context: alert persisting across reboots/power events")
	}
	return out
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

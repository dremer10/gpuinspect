package main

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Trimmed-down real frop dissect output for ss892297x4318514 (genuine fault:
// persistent PEX degradation, two failed drains, RMA in flight).
const fropGenuine = `
│ Status: 🔴 Critical — Persistent PCIe Link Speed/Width Degradation on PEX890xx Switch | RMA in Progress (HO-187120 open)                    │
│  1 NodePCISpeedPLXSMC (URGENT, firing continuously since 2026-07-30): ...                                                                   │
│  • A 10–15 minute AC power drain was already performed (DO-177509, closed 2026-07-17) and the node initially recovered — but the issue      │
│    recurred on 2026-07-30 and has persisted ever since.                                                                                     │
│  • An additional full 20-minute power drain was performed (DO-194388, closed 2026-08-03) — the node is still firing all three PCIe alerts,  │
│    meaning the drain did not fix it.                                                                                                        │
│  1 Proceed with RMA — all prerequisites are met: the power drain (twice) did not fix the PCIe degradation ...                               │
`

const fropBenign = `
│ Status: 🟢 Healthy — idle-port width alerts match the known RNO2A false-positive pattern.  │
│ No open tickets. Recommend return-to-ready after golden-state check.                       │
`

func TestSanitizeExternalContextStripsFraming(t *testing.T) {
	got := sanitizeExternalContext("\x1b[31m│ Status: bad │\x1b[0m\n\n\n│ line2 │\n")
	if strings.ContainsAny(got, "│─╔") {
		t.Fatalf("box drawing not stripped: %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ANSI not stripped: %q", got)
	}
	if !strings.Contains(got, "Status: bad") || !strings.Contains(got, "line2") {
		t.Fatalf("content lost: %q", got)
	}
}

func TestExternalContextSignalsGenuine(t *testing.T) {
	sig := externalContextSignals(sanitizeExternalContext(fropGenuine))
	if len(sig) == 0 {
		t.Fatal("expected genuine-fault signals, got none")
	}
	joined := strings.Join(sig, "\n")
	for _, want := range []string{"RMA already in progress", "HO-187120", "did NOT remediate", "persisting"} {
		if !strings.Contains(joined, want) {
			t.Errorf("signals missing %q:\n%s", want, joined)
		}
	}
}

func TestExternalContextSignalsBenign(t *testing.T) {
	if sig := externalContextSignals(sanitizeExternalContext(fropBenign)); len(sig) != 0 {
		t.Fatalf("benign context must not raise signals, got %v", sig)
	}
}

func TestExternalContextSignalsEmpty(t *testing.T) {
	if sig := externalContextSignals(""); sig != nil {
		t.Fatalf("empty context: got %v", sig)
	}
}

func TestSanitizeExternalContextCaps(t *testing.T) {
	big := strings.Repeat("filler line\n", 10000) + "TAIL-MARKER\n"
	got := sanitizeExternalContext(big)
	if len(got) > maxExternalContext {
		t.Fatalf("not capped: %d bytes", len(got))
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Fatal("tail (diagnosis/actions) must be kept")
	}
}

func TestContextReaderAsync(t *testing.T) {
	c := startContextReader(strings.NewReader("\x1b[31m│ hello │\x1b[0m\n\n\nworld\n"))
	got := c.wait()
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("wait() lost content: %q", got)
	}
	if strings.Contains(got, "\x1b") || strings.ContainsAny(got, "│") {
		t.Fatalf("wait() returned unsanitized content: %q", got)
	}
	if !c.ready() {
		t.Fatal("ready() must be true after wait() returned")
	}
}

func TestContextReaderSlowPipe(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(100 * time.Millisecond)
		pw.Write([]byte("late diagnosis context\n"))
		pw.Close()
	}()
	c := startContextReader(pr)
	if c.ready() {
		t.Fatal("ready() must be false while the pipe is still filling")
	}
	got := c.wait()
	if got != "late diagnosis context" {
		t.Fatalf("wait() = %q, want %q", got, "late diagnosis context")
	}
	if !c.ready() {
		t.Fatal("ready() must be true after wait()")
	}
}

func TestContextReaderNil(t *testing.T) {
	var c *contextReader
	if got := c.wait(); got != "" {
		t.Fatalf("nil wait() = %q, want \"\"", got)
	}
	if !c.ready() {
		t.Fatal("nil ready() must be true")
	}
}

func TestContextReaderConcurrentWait(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(50 * time.Millisecond)
		pw.Write([]byte("shared context value\n"))
		pw.Close()
	}()
	c := startContextReader(pr)
	const n = 8
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.wait()
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		if got != "shared context value" {
			t.Fatalf("goroutine %d: wait() = %q, want %q", i, got, "shared context value")
		}
	}
}

func TestDrainFailRegexNotFooledByJobFailures(t *testing.T) {
	// Live false positive (BMN s7xg5724): bare "failed" near a power-cycle
	// mention is about job failures, not a remediation that didn't work.
	txt := "Two AWX fielddiag jobs launched by staff both failed. A power cycle " +
		"was then used to recover the node after the fielddiag jobs failed to complete."
	sig := externalContextSignals(sanitizeExternalContext(txt))
	for _, s := range sig {
		if strings.Contains(s, "power drain / AC cycle") {
			t.Fatalf("job-failure prose must not raise the drain-failure signal, got %v", sig)
		}
	}
	if len(sig) != 0 {
		t.Fatalf("text carries no RMA/persistence wording, expected no signals, got %v", sig)
	}
}

func TestDrainFailRegexStillFires(t *testing.T) {
	txt := "An additional full 20-minute power drain was performed — the node is " +
		"still firing all three PCIe alerts, meaning the drain did not fix it."
	sig := externalContextSignals(sanitizeExternalContext(txt))
	found := false
	for _, s := range sig {
		if strings.Contains(s, "did NOT remediate") {
			found = true
		}
	}
	if !found {
		t.Fatalf("genuine drain-failure wording must raise the signal, got %v", sig)
	}
}

func TestOldestPCIAlertAge(t *testing.T) {
	old := time.Now().Add(-100 * time.Hour).Format(time.RFC3339)
	fresh := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	age, oldest := oldestPCIAlertAge([]bmnAlert{
		{Name: "NodePCILinkSpeedUnexpected", LastTransition: fresh},
		{Name: "NodePCILinkWidthUnexpected", LastTransition: old},
		{Name: "NodeTPMEKCertificateMissing", LastTransition: time.Now().Add(-1000 * time.Hour).Format(time.RFC3339)}, // non-PCI: ignored
	})
	if age < 99*time.Hour || age > 101*time.Hour {
		t.Fatalf("age = %v, want ~100h", age)
	}
	if oldest != old {
		t.Fatalf("oldest = %q, want %q", oldest, old)
	}
	if age > fpMaxAlertAge() == false {
		t.Fatal("100h must exceed the default 72h gate")
	}
}

func TestOldestPCIAlertAgeNoPCI(t *testing.T) {
	age, _ := oldestPCIAlertAge([]bmnAlert{{Name: "NodeTPMEKCertificateMissing", LastTransition: "2020-01-01T00:00:00Z"}})
	if age != 0 {
		t.Fatalf("expected zero age, got %v", age)
	}
}

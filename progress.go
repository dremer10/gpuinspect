package main

// Live progress display: one animated line per BMN (spinner + phase emoji +
// elapsed time), redrawn several times a second while workers run, so long
// inspections never look hung. Completed node sections are printed above the
// progress area, which clears and redraws itself — Claude-CLI style.
//
// Animation only runs on a color TTY; on pipes/--no-color it degrades to
// plain per-phase lines on stderr so logs stay clean.

import (
	"fmt"
	"sync"
	"time"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type progressRow struct {
	bmn     string
	phase   string
	start   time.Time // per-phase clock (reset by set)
	began   time.Time // total clock
	done    bool
	final   string        // rendered final marker (emoji + verdict), set on finish
	elapsed time.Duration // frozen total on finish
}

type progress struct {
	mu      sync.Mutex
	rows    []progressRow
	enabled bool
	c       palette
	stop    chan struct{}
	stopped chan struct{}
	drawn   int // lines currently on screen (for cursor-up erase)
	frame   int
}

func newProgress(bmns []string, c palette, enabled bool) *progress {
	p := &progress{enabled: enabled, c: c, stop: make(chan struct{}), stopped: make(chan struct{})}
	now := time.Now()
	for _, b := range bmns {
		p.rows = append(p.rows, progressRow{bmn: b, phase: "⏳ queued", start: now, began: now})
	}
	if enabled {
		go p.loop()
	} else {
		close(p.stopped)
	}
	return p
}

func (p *progress) loop() {
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			p.mu.Lock()
			p.clearLocked()
			p.mu.Unlock()
			close(p.stopped)
			return
		case <-t.C:
			p.mu.Lock()
			p.frame++
			p.redrawLocked()
			p.mu.Unlock()
		}
	}
}

// set updates a node's phase. The row restarts its own clock so the elapsed
// time shown is per-phase, which reads better than a global timer.
func (p *progress) set(idx int, phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.rows) {
		return
	}
	p.rows[idx].phase = phase
	p.rows[idx].start = time.Now()
	if !p.enabled {
		fmt.Printf("  %s %s\n", p.rows[idx].bmn, phase)
		return
	}
	p.redrawLocked()
}

// finishAndPrint clears the progress area, replays the node's buffered
// section, marks the row done, and redraws — all atomically, so parallel
// sections never interleave.
func (p *progress) finishAndPrint(idx int, res inspectResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	fmt.Print(res.Output)
	if idx >= 0 && idx < len(p.rows) {
		p.rows[idx].done = true
		p.rows[idx].final = verdictEmoji(res.VerdictCode) + " " + verdictWord(res.VerdictCode)
		p.rows[idx].elapsed = time.Since(p.rows[idx].began)
	}
	if !p.enabled {
		fmt.Printf("  %s done — %s\n", res.BMN, verdictWord(res.VerdictCode))
		return
	}
	p.redrawLocked()
}

// close stops the animation and erases the progress area (the summary matrix
// takes over from here).
func (p *progress) close() {
	if !p.enabled {
		return
	}
	close(p.stop)
	<-p.stopped
}

func (p *progress) clearLocked() {
	if p.drawn > 0 {
		fmt.Printf("\x1b[%dA\x1b[J", p.drawn)
		p.drawn = 0
	}
}

func (p *progress) redrawLocked() {
	p.clearLocked()
	c := p.c
	spin := spinnerFrames[p.frame%len(spinnerFrames)]
	for _, r := range p.rows {
		if r.done {
			fmt.Printf("%s  %s%-18s%s %s(%s)%s\n", r.final, c.B, r.bmn, c.X, c.D, fmtDur(r.elapsed), c.X)
		} else {
			fmt.Printf("%s%s%s %s%-18s%s %s  %s(%s)%s\n",
				c.C, spin, c.X, c.B, r.bmn, c.X, r.phase, c.D, fmtDur(time.Since(r.start)), c.X)
		}
		p.drawn++
	}
}

func verdictEmoji(code int) string {
	switch code {
	case 0:
		return "✅"
	case 1:
		return "⚠️ "
	case 2:
		return "🔴"
	}
	return "❌"
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

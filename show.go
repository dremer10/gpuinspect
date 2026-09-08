package main

// Tee'd command execution: every GPU-runbook / nvidia-smi command echoes its
// command line and output through the logger — which writes to BOTH the
// terminal (streamed live to the laptop in remote/orchestrated runs) and the
// plain-text triage log — while the full untruncated output still lands in
// the bundle file for the ticket.

import (
	"os"
	"strings"
)

// showMax returns the per-command terminal/log line cap. The bundle file
// always carries the full output; the cap only keeps the streamed report
// readable when a command is very long (e.g. nvlink -s on 8 GPUs).
func showMax() int { return envInt("GPUINSPECT_SHOW_MAX_LINES", 200) }

// echoCmd prints the command line being run.
func (t *triage) echoCmd(cmdline string) {
	t.log.p("%s$ %s%s", t.c.B, cmdline, t.c.X)
}

// echoOutput prints command output through the logger, capped at showMax
// lines; notes where the full output lives when truncated.
func (t *triage) echoOutput(out, fullAt string) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		t.log.p("%s  <no output>%s", t.c.D, t.c.X)
		return
	}
	max := showMax()
	shown := lines
	if len(lines) > max {
		shown = lines[:max]
	}
	for _, l := range shown {
		t.log.p("  %s", l)
	}
	if len(lines) > len(shown) {
		note := ""
		if fullAt != "" {
			note = " — full output: " + fullAt
		}
		t.log.p("%s  … +%d more line(s)%s%s", t.c.D, len(lines)-len(shown), note, t.c.X)
	}
}

// runShow executes a command, echoes command line + output to terminal/log,
// and writes the full output to path when non-empty. Returns the output.
func (t *triage) runShow(path, name string, args ...string) string {
	t.echoCmd(name + " " + strings.Join(args, " "))
	out := cmdOut(name, args...)
	if path != "" {
		_ = os.WriteFile(path, []byte(out), 0o644)
	}
	t.echoOutput(out, path)
	return out
}

// bashShow is runShow for a shell pipeline.
func (t *triage) bashShow(path, script string) string {
	t.echoCmd(script)
	out := cmdOut("bash", "-c", script)
	if path != "" {
		_ = os.WriteFile(path, []byte(out), 0o644)
	}
	t.echoOutput(out, path)
	return out
}

// runQuiet executes a command whose output is too bulky for the report:
// echoes the command line plus "captured to <file> (N lines)" and writes the
// full output to path.
func (t *triage) runQuiet(path, name string, args ...string) {
	t.echoCmd(name + " " + strings.Join(args, " "))
	out := cmdOut(name, args...)
	_ = os.WriteFile(path, []byte(out), 0o644)
	t.log.p("%s  captured to %s (%d lines)%s", t.c.D, path, len(strings.Split(out, "\n")), t.c.X)
}

// showFileTail echoes the last n lines of a captured file through the logger
// (e.g. the tail of a dmon sampling window or nvbandwidth run).
func (t *triage) showFileTail(path string, n int, fullAt string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		t.log.p("%s  … tail (last %d of %d lines — full output: %s)%s", t.c.D, n, len(lines), fullAt, t.c.X)
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		t.log.p("  %s", l)
	}
}

// bashQuiet is runQuiet for a shell pipeline.
func (t *triage) bashQuiet(path, script string) {
	t.echoCmd(script)
	out := cmdOut("bash", "-c", script)
	_ = os.WriteFile(path, []byte(out), 0o644)
	t.log.p("%s  captured to %s (%d lines)%s", t.c.D, path, len(strings.Split(out, "\n")), t.c.X)
}

package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Colors + logging (colored stdout, plain-text logfile)
// ---------------------------------------------------------------------------

type palette struct{ R, G, Y, C, B, D, X string }

func colors() palette {
	if os.Getenv("FORCE_COLOR") != "" || isTTY(os.Stdout) {
		return palette{
			R: "\x1b[31;1m", G: "\x1b[32;1m", Y: "\x1b[33;1m",
			C: "\x1b[36;1m", B: "\x1b[1m", D: "\x1b[2m", X: "\x1b[0m",
		}
	}
	return palette{}
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type logger struct{ file *os.File }

func (l *logger) p(format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	fmt.Println(s)
	if l.file != nil {
		_, _ = fmt.Fprintln(l.file, ansiRe.ReplaceAllString(s, ""))
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func haveCmd(name string) bool { _, err := exec.LookPath(name); return err == nil }

func readSysfs(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func cmdOut(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

func cmdToFile(path, name string, args ...string) {
	_ = os.WriteFile(path, []byte(cmdOut(name, args...)), 0o644)
}

func bashToFile(path, script string) {
	cmdToFile(path, "bash", "-c", script)
}

var numRe = regexp.MustCompile(`^\s*([0-9]+(\.[0-9]+)?)`)

// gtNum extracts the leading float from strings like "32.0 GT/s PCIe" / "2.5GT/s"
func gtNum(s string) (float64, bool) {
	m := numRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	return f, err == nil
}

func numLT(a, b string) bool {
	x, ok1 := gtNum(a)
	y, ok2 := gtNum(b)
	return ok1 && ok2 && x < y
}

func sanitize(bdf string) string {
	return strings.NewReplacer(":", "_", ".", "_").Replace(bdf)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envInt returns the positive-integer value of an env var, or def.
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func dirExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// osReadDirNames returns the entry names of a directory (empty on error).
func osReadDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func descOnly(lspciLine string) string {
	if i := strings.Index(lspciLine, ": "); i >= 0 {
		return lspciLine[i+2:]
	}
	return lspciLine
}

func orNA(s string) string {
	if s == "" {
		return "<unavailable>"
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func splitCmd(s string) []string { return strings.Fields(s) }

// copyToClipboard pipes text into the system clipboard via the first
// available helper: pbcopy (macOS), then xclip, then xsel.
func copyToClipboard(text string) error {
	candidates := [][]string{
		{"pbcopy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
	}
	for _, cand := range candidates {
		if !haveCmd(cand[0]) {
			continue
		}
		cmd := exec.Command(cand[0], cand[1:]...)
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard command found (need pbcopy, xclip, or xsel)")
}

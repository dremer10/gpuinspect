package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var evalEnvNumRe = regexp.MustCompile(`^[0-9]+$`)

// cacheShaRe validates the sha256 hex digest before it is interpolated into
// the remote command string (we compute it ourselves, but keep the same
// belt-and-braces stance as the other interpolated values in this file).
var cacheShaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ---------------------------------------------------------------------------
// Remote mode — replaces run_triage_remote.sh
// ---------------------------------------------------------------------------

func transport() (sshCmd, scpCmd []string) {
	if v := os.Getenv("TRIAGE_SSH"); v != "" {
		sshCmd = splitCmd(v)
	} else if haveCmd("tsh") {
		sshCmd = []string{"tsh", "ssh"}
	} else {
		sshCmd = []string{"ssh"}
	}
	if v := os.Getenv("TRIAGE_SCP"); v != "" {
		scpCmd = splitCmd(v)
	} else if haveCmd("tsh") {
		scpCmd = []string{"tsh", "scp"}
	} else {
		scpCmd = []string{"scp"}
	}
	return
}

// unameToGoarch maps a node's `uname -m` output to the GOARCH of the binary
// to push. "" means unknown — callers fall back to amd64 (the pre-arm64
// behavior). Pure.
func unameToGoarch(m string) string {
	switch strings.TrimSpace(m) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

// findLinuxBinary locates the linux binary matching the node's architecture
// (GH200/GB200 nodes are aarch64 — pushing amd64 there fails with the shell's
// "Syntax error: word unexpected" as it tries to interpret the foreign ELF as
// a script). --linux-bin overrides everything; the operator owns the arch.
func findLinuxBinary(o *options, goarch string) (string, error) {
	if o.linuxBin != "" {
		if _, err := os.Stat(o.linuxBin); err != nil {
			return "", fmt.Errorf("--linux-bin %s: %v", o.linuxBin, err)
		}
		return o.linuxBin, nil
	}
	name := "gpuinspect-linux-" + goarch
	// If we ARE the right linux binary, push ourselves.
	if runtime.GOOS == "linux" && runtime.GOARCH == goarch {
		if self, err := os.Executable(); err == nil {
			return self, nil
		}
	}
	// Otherwise look for the cross-compiled artifact next to us / in cwd.
	candidates := []string{}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), name))
	}
	candidates = append(candidates, "./"+name, "./bin/"+name)
	for _, cand := range candidates {
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no linux/%s binary found to push (node arch needs %s — run `make build`, or use --linux-bin)", goarch, name)
}

// sha256File returns the lowercase hex sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cacheRemotePath is where a pushed binary is cached on the node. /var/tmp
// survives reboots longer than /tmp and is writable by the login user.
func cacheRemotePath(sha string) string {
	return "/var/tmp/gpuinspect_cache_" + sha
}

// buildRemoteScript assembles the single &&-chained command executed on the
// node. It stays one flat `&&` chain — no subshells or heredocs — because a
// past intermittent "wait: remote command exited without exit status or exit
// signal" bug was suspected to involve the compound remote command. The cache
// store is the one brace group ({ ...; } runs in the current shell, not a
// subshell) so a full /var/tmp can never break the run or swallow an earlier
// failure via `||` precedence.
//
// Layout by mode:
//   - sha == ""        : legacy script, binary streamed over stdin, no caching.
//   - cached == false  : binary streamed over stdin (`cat >`), then stored as
//     the node's single cached version (old cache versions removed first).
//   - cached == true   : binary copied from the node cache; the script reads
//     NOTHING from stdin (no `cat >`), so the caller must not attach the
//     binary to stdin or the run would hang.
//
// The trailing cd + sudo invocation is byte-identical across all modes. All
// interpolated values are locally built, regex-validated identifiers/paths
// (see callers); sha is validated against cacheShaRe.
func buildRemoteScript(remoteDir, fc, rerunCmd, localDir, evalEnv, bmn, passthru string, expectedGPUs int, sha string, cached bool) string {
	var stage string
	if cached && sha != "" {
		stage = "cp " + cacheRemotePath(sha) + " '" + remoteDir + "/gpuinspect'"
	} else {
		stage = "cat > '" + remoteDir + "/gpuinspect'"
	}
	script := "mkdir -p '" + remoteDir + "' && " + stage + " && chmod +x '" + remoteDir + "/gpuinspect'"
	if !cached && sha != "" {
		script += " && { rm -f /var/tmp/gpuinspect_cache_* 2>/dev/null; " +
			"cp '" + remoteDir + "/gpuinspect' " + cacheRemotePath(sha) + " 2>/dev/null || true; }"
	}
	script += fmt.Sprintf(
		" && cd '%s' && sudo FORCE_COLOR=%s RERUN_CMD='%s' LOCAL_HINT='%s' LOGDIR='%s' GPUINSPECT_EXPECTED_GPUS='%d' %s'%s/gpuinspect' --bmn '%s' %s",
		remoteDir, fc, rerunCmd, localDir, remoteDir, expectedGPUs, evalEnv, remoteDir, bmn, passthru)
	return script
}

// remoteProbe runs the one-shot preflight (~1-2s round trip): the node's
// machine architecture (for binary selection) and every executable cached
// binary path (for the push skip), in a single ssh so arm64 support costs no
// extra round trip. Any ssh/exec failure degrades to ("", empty) — callers
// fall open to the amd64 push. The trailing `true` keeps the remote exit 0
// when no cache files exist, so err is reserved for real transport failures.
func remoteProbe(sshCmd []string, target string) (uname string, cached map[string]bool, err error) {
	probe := `echo GPUINSPECT_ARCH=$(uname -m); for f in /var/tmp/gpuinspect_cache_*; do [ -x "$f" ] && echo "$f"; done; true`
	args := append(sshCmd[1:], target, probe)
	out, err := exec.Command(sshCmd[0], args...).Output()
	cached = map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "GPUINSPECT_ARCH="):
			uname = strings.TrimPrefix(line, "GPUINSPECT_ARCH=")
		case strings.HasPrefix(line, "/var/tmp/gpuinspect_cache_"):
			cached[line] = true
		}
	}
	return uname, cached, err
}

// runRemote is the legacy direct mode (--remote <gMAC> --bmn <BMN>). After
// the on-node run it renders the same one-row matrix and summary files as the
// orchestrated mode, sourcing verdict/issues/next-steps from the retrieved
// verdict.json (readVerdictJSON in orchestrate.go) when available.
func runRemote(o *options) int {
	code, localDir := runRemoteTo(os.Stdout, o)

	c := palette{}
	if !o.noColor && isTTY(os.Stdout) {
		c = colors()
	}

	// Same precedence as inspectOneBMN: the on-node verdict.json wins over
	// the ssh exit code when it parsed.
	_, vf := readVerdictJSON(localDir)
	vcode := code
	var issues, nextSteps []string
	fp := false
	if vf != nil {
		if vf.VerdictCode != nil {
			vcode = *vf.VerdictCode
		}
		issues = append(issues, vf.Issues...)
		nextSteps = append(nextSteps, vf.NextSteps...)
		// Direct mode never sees the mgmt alert feed, so the orchestrator's
		// "only fallout reason" gate cannot run here — the on-node flag is
		// taken as-is. Use BMN-first mode for the gated classification.
		fp = vf.Flags["pex_false_positive"]
	}
	// Piped external context (frop dissect) still gates here: genuine-fault
	// history vetoes the false-positive claim even in direct mode. wait() is
	// nil-safe ("" when nothing was piped) and this is post-inspection, the
	// point where blocking on the upstream producer is acceptable.
	ext := o.extCtx.wait()
	if extSignals := externalContextSignals(ext); len(extSignals) > 0 {
		issues = append(issues, extSignals...)
		if fp {
			fp = false
			issues = append(issues, "false-positive claim WITHDRAWN — external context (frop/NodeBot) shows genuine-fault history")
			if vcode == 0 {
				vcode = 1
			}
		}
	}
	if len(nextSteps) == 0 && vcode > 0 {
		nextSteps = []string{"follow the printed runbook steps"}
	}

	results := []inspectResult{{
		BMN:         o.bmn,
		Info:        &bmnInfo{Name: o.bmn, GMAC: o.remote},
		VerdictCode: vcode, FalsePositive: fp,
		Issues: issues, NextSteps: nextSteps, OutDir: localDir,
	}}

	printMatrix(results, c)

	// The orchestrator creates outDir up front; direct mode only creates
	// <outDir>/<bmn>_<ts> after a successful run, so ensure the parent exists
	// before writeSummaries (which does a plain WriteFile).
	if o.outDir != "" {
		_ = os.MkdirAll(o.outDir, 0o755)
	}
	txtPath, mdPath := writeSummaries(o.outDir, results)
	archiveEvidence(os.Stdout, localDir)

	fmt.Println()
	if txtPath != "" {
		fmt.Printf("%sSummary (txt)%s : %s\n", c.B, c.X, txtPath)
	}
	if mdPath != "" {
		fmt.Printf("%sSummary (md)%s  : %s\n", c.B, c.X, mdPath)
	}
	if localDir != "" {
		fmt.Printf("%s  %s → %s%s\n", c.D, o.bmn, localDir, c.X)
	}

	return vcode
}

// runRemoteTo pushes the linux binary to the node, runs it, streams all output
// (stdout and stderr merged into one stream) to w, retrieves the results, and
// returns the verdict code (3 on error) plus the local results directory (""
// when execution failed before anything could be retrieved).
func runRemoteTo(w io.Writer, o *options) (int, string) {
	sshCmd, scpCmd := transport()

	// Running under sudo breaks tsh (root has no ~/.tsh profile)
	if os.Geteuid() == 0 && sshCmd[0] == "tsh" && os.Getenv("TELEPORT_HOME") == "" {
		fmt.Fprintln(w, "ERROR: do not run remote mode with sudo — tsh needs YOUR user's")
		fmt.Fprintln(w, "Teleport profile (~/.tsh). The remote side handles sudo itself.")
		return 3, ""
	}

	target := o.user + "@" + o.remote

	// One preflight ssh: node arch (binary selection) + cached binaries (push
	// skip). Every failure mode falls open to the amd64 push — never fatal.
	unameM, cachedSet, probeErr := remoteProbe(sshCmd, target)
	if probeErr != nil {
		if _, ok := probeErr.(*exec.ExitError); !ok {
			// Non-exit errors mean the preflight ssh itself could not run
			// (transport misconfig etc.) — worth one WARN before the real push
			// surfaces the same problem.
			fmt.Fprintf(w, "WARN: node preflight failed (%v) — assuming amd64, pushing binary\n", probeErr)
		}
	}
	goarch := unameToGoarch(unameM)
	if goarch == "" {
		if unameM != "" {
			fmt.Fprintf(w, "WARN: unrecognized node arch %q — assuming amd64\n", unameM)
		}
		goarch = "amd64"
	}

	bin, err := findLinuxBinary(o, goarch)
	if err != nil {
		fmt.Fprintln(w, "ERROR:", err)
		return 3, ""
	}

	ts := time.Now().Format("20060102_150405")
	remoteDir := "/tmp/pci_triage_" + ts
	localDir := filepath.Join(".", "triage_results", o.bmn+"_"+ts)
	if o.outDir != "" {
		localDir = filepath.Join(o.outDir, o.bmn+"_"+ts)
	}

	var passthru []string
	if o.postDrain {
		passthru = append(passthru, "--post-drain")
	}
	if o.postSwap {
		passthru = append(passthru, "--post-swap")
	}
	if o.postReseat {
		passthru = append(passthru, "--post-reseat")
	}
	if o.triaged {
		passthru = append(passthru, "--triaged")
	}
	if o.allPCI {
		passthru = append(passthru, "--all-pci")
	}
	if o.temps {
		passthru = append(passthru, "--temps")
	}
	if o.eval {
		passthru = append(passthru, "--eval")
	}
	if o.evalDCGM {
		passthru = append(passthru, "--eval-dcgm")
	}
	if o.evalNVBW {
		passthru = append(passthru, "--eval-nvbandwidth")
	}
	if o.evalBugReport {
		passthru = append(passthru, "--eval-bug-report")
	}
	if o.yesIntrusive {
		passthru = append(passthru, "--yes-intrusive")
	}
	if o.eval && o.dcgmLevel != 2 {
		passthru = append(passthru, "--dcgm-level", strconv.Itoa(o.dcgmLevel))
	}
	passthru = append(passthru, o.bdfs...)

	fmt.Fprintf(w, ">>> SSH target  : %s   (via: %s)\n", target, strings.Join(sshCmd, " "))
	fmt.Fprintf(w, ">>> BMN         : %s\n", o.bmn)
	if unameM != "" {
		fmt.Fprintf(w, ">>> Node arch   : %s (linux/%s)\n", unameM, goarch)
	}

	// Binary cache: the ~9MB push costs 5-15s per node per run; the preflight
	// already listed the node's cached binaries, so a hit is a local lookup.
	sha, hashErr := sha256File(bin)
	cacheHit := false
	if hashErr == nil && cacheShaRe.MatchString(sha) {
		cacheHit = cachedSet[cacheRemotePath(sha)]
	} else {
		sha = "" // no usable hash — plain legacy push, no caching
	}

	pushNote := ""
	switch {
	case cacheHit:
		pushNote = " (cached on node — transfer skipped)"
	case sha != "":
		pushNote = " (pushing, will cache as gpuinspect_cache_" + sha[:12] + ")"
	}
	fmt.Fprintf(w, ">>> Pushing bin : %s%s\n", bin, pushNote)
	fmt.Fprintf(w, ">>> Remote dir  : %s\n", remoteDir)
	fmt.Fprintf(w, ">>> Local out   : %s\n", localDir)
	fmt.Fprintf(w, ">>> Triage args : --bmn %s %s\n\n", o.bmn, strings.Join(passthru, " "))

	// The captured buffer is replayed to a terminal by the orchestrator, so
	// color follows --no-color rather than whether w is a TTY. colors() on
	// the node treats ANY non-empty FORCE_COLOR as "on", so disabling means
	// passing an empty value, not "0".
	fc := "1"
	if o.noColor {
		fc = ""
	}
	rerunCmd := fmt.Sprintf("%s --remote %s --bmn %s", os.Args[0], o.remote, o.bmn)

	// Forward eval tuning env vars to the node (numeric-validated so the
	// values are safe to interpolate into the remote command string).
	evalEnv := ""
	for _, k := range []string{
		"GPUINSPECT_DMON_SECONDS", "GPUINSPECT_PMON_SECONDS", "GPUINSPECT_TEMP_SECONDS",
		"GPUINSPECT_DCGM_TIMEOUT", "GPUINSPECT_NVBW_TIMEOUT", "GPUINSPECT_BUGREPORT_TIMEOUT",
	} {
		if v := os.Getenv(k); v != "" && evalEnvNumRe.MatchString(v) {
			evalEnv += k + "='" + v + "' "
		}
	}

	remoteScript := buildRemoteScript(remoteDir, fc, rerunCmd, localDir, evalEnv,
		o.bmn, strings.Join(passthru, " "), o.expectedGPUs, sha, cacheHit)
	// Defensive: a cache-hit script must never read stdin (nothing would feed
	// its `cat >` and the run would hang). Cannot happen by construction, but
	// fall back to the normal push if it ever does.
	if cacheHit && strings.Contains(remoteScript, "cat >") {
		cacheHit = false
		remoteScript = buildRemoteScript(remoteDir, fc, rerunCmd, localDir, evalEnv,
			o.bmn, strings.Join(passthru, " "), o.expectedGPUs, sha, false)
	}

	var captured strings.Builder
	args := append(sshCmd[1:], target, remoteScript)
	cmd := exec.Command(sshCmd[0], args...)
	if !cacheHit {
		binFile, err := os.Open(bin)
		if err != nil {
			fmt.Fprintln(w, "ERROR:", err)
			return 3, ""
		}
		defer func() { _ = binFile.Close() }()
		cmd.Stdin = binFile
	}
	cmd.Stdout = io.MultiWriter(w, &captured)
	cmd.Stderr = io.MultiWriter(w, &captured)
	runErr := cmd.Run()

	verdictCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			verdictCode = ee.ExitCode()
		} else {
			fmt.Fprintln(w, "ERROR: ssh execution failed:", runErr)
			return 3, ""
		}
	}

	// If the remote binary never actually ran, don't misreport ssh's exit
	// code as a triage verdict.
	if !strings.Contains(captured.String(), "VERDICT") {
		fmt.Fprintln(w, "\nERROR: remote execution failed before the triage produced a")
		fmt.Fprintln(w, "verdict (ssh/Teleport/auth problem — see output above).")
		return 3, ""
	}

	fmt.Fprintf(w, "\n>>> Remote run finished with verdict code: %d\n", verdictCode)
	switch verdictCode {
	case 0:
		fmt.Fprintln(w, ">>> HEALTHY")
	case 1:
		fmt.Fprintln(w, ">>> DEGRADED — remediation steps in log")
	case 2:
		fmt.Fprintln(w, ">>> PRESENT FOR RMA")
	default:
		fmt.Fprintln(w, ">>> Script/environment error")
	}

	// Retrieve results; fall back to $HOME if the results root is unwritable.
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		home, _ := os.UserHomeDir()
		fallback := filepath.Join(home, "triage_results", o.bmn+"_"+ts)
		fmt.Fprintf(w, "WARN: %s not writable; using %s\n", localDir, fallback)
		localDir = fallback
		if err := os.MkdirAll(localDir, 0o755); err != nil {
			fmt.Fprintln(w, "ERROR: cannot create local results dir:", err)
		}
	}

	fmt.Fprintln(w, "\n>>> Retrieving results...")
	// Drop the pushed binary before retrieval — evidence only, and no 9MB
	// transfer per node. Local remove is the backstop if the remote rm fails.
	rmBinArgs := append(sshCmd[1:], target, "rm -f '"+remoteDir+"/gpuinspect'")
	_ = exec.Command(sshCmd[0], rmBinArgs...).Run()
	scpArgs := append(scpCmd[1:], "-r", target+":"+remoteDir+"/*", localDir+"/")
	retrieved := exec.Command(scpCmd[0], scpArgs...).Run() == nil
	if retrieved {
		_ = os.Remove(filepath.Join(localDir, "gpuinspect"))
		// Stray on-node bundle tarballs (older binaries still create them)
		// would duplicate the bundle dir inside the evidence archive.
		if dups, err := filepath.Glob(filepath.Join(localDir, "collect_bundle_*.tar.gz")); err == nil {
			for _, d := range dups {
				_ = os.Remove(d)
			}
		}
		fmt.Fprintf(w, ">>> Results saved to %s\n", localDir)
		if entries, err := os.ReadDir(localDir); err == nil {
			for _, e := range entries {
				fmt.Fprintf(w, "    %s\n", e.Name())
			}
		}
		// Remote cleanup ONLY after successful retrieval; /var/tmp snapshots
		// stay on the node for before/after comparisons.
		cleanArgs := append(sshCmd[1:], target, "rm -rf '"+remoteDir+"'")
		_ = exec.Command(sshCmd[0], cleanArgs...).Run()
	} else {
		fmt.Fprintln(w, "WARN: retrieval failed. Remote files were KEPT. Fetch manually:")
		fmt.Fprintf(w, "  %s -r '%s:%s/*' '%s/'\n", strings.Join(scpCmd, " "), target, remoteDir, localDir)
	}

	return verdictCode, localDir
}

// archiveEvidence tars the retrieved per-node evidence dir into a sibling
// <dir>.tar.gz — one dir + one archive per node for HO/DO tickets. Runs
// LAST in the pipeline so late artifacts (analysis.md) are included.
func archiveEvidence(w io.Writer, localDir string) {
	if localDir == "" {
		return
	}
	if _, err := os.Stat(localDir); err != nil {
		return
	}
	if err := exec.Command("tar", "-czf", localDir+".tar.gz",
		"-C", filepath.Dir(localDir), filepath.Base(localDir)).Run(); err == nil {
		fmt.Fprintf(w, ">>> Evidence archive: %s.tar.gz\n", localDir)
	} else {
		fmt.Fprintf(w, "WARN: evidence archiving failed (%v) — tar %s manually\n", err, localDir)
	}
}

// gpuinspect — BMN-first GPU/PCIe inspection and analysis tool
//
// Evolution of gpu-node-helper. Aligned to the FRO runbook
// "NodePCILinkSpeedUnexpected" (Confluence page 649756759, v4) and the METAL
// runbook "Triaging NodePCILinkWidthUnexpected: mapping a PCIe bridge BDF to
// a NIC, GPU or NVMe drive" (Confluence page 1616642163).
//
// gpuinspect INSPECTS AND ADVISES ONLY. It never executes cwctl or any other
// state-changing command — all remediation is emitted as copy-paste advice.
//
// Modes:
//
//	Fleet scan (no args, mgmt-cluster kubectl context):
//	  gpuinspect
//	Inspect one or more BMNs (BDFs and ssh target derived from the BMN):
//	  gpuinspect <BMN> [<BMN>...]
//	Direct remote (legacy; target a gMAC yourself):
//	  gpuinspect --remote <gMAC> --bmn <BMN> [flags] [BDF...]
//	On a node (as root; what the pushed binary runs):
//	  sudo gpuinspect --bmn <BMN> [--triaged] [--post-drain|--post-swap|--post-reseat] [BDF...]
//
// Exit codes: 0=healthy 1=degraded (advice issued) 2=present-for-RMA 3=error
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var version = "dev"

// Parse-time validation. These exist both to catch operator typos early and
// to make the values safe for interpolation into the remote ssh command
// string built in remote.go — anything matching them cannot carry shell
// metacharacters.
var (
	// PCI BDF, with optional 4-hex-digit domain prefix (lspci short form).
	bdfRe = regexp.MustCompile(`^([0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
	// BMN, gMAC (--remote), and --user identifiers.
	identRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// ---------------------------------------------------------------------------
// CLI options
// ---------------------------------------------------------------------------

type options struct {
	// on-node / direct-remote mode
	bmn        string
	remote     string // gMAC; set = direct remote mode
	user       string
	linuxBin   string
	postDrain  bool
	postSwap   bool
	postReseat bool
	triaged    bool
	bdfs       []string

	// full GPU evaluation (--eval; eval.go)
	eval          bool
	evalDCGM      bool
	evalNVBW      bool
	evalBugReport bool
	yesIntrusive  bool
	dcgmLevel     int

	expectedGPUs int // set by orchestrate; passed to the node as GPUINSPECT_EXPECTED_GPUS

	// quick live thermal readout for all GPUs (--temps; temps.go) — skips the
	// PCIe triage, AI and nodebot phases entirely.
	temps bool

	// scope: GPU-focused by default; --all-pci restores the wide PCIe sweep
	// (Broadcom PEX / Mellanox discovery) for the rare non-GPU chase.
	allPCI bool

	// laptop BMN-first mode
	bmns         []string
	kubeContext  string
	outDir       string
	parallel     int
	noAI         bool
	nodebot      bool
	nodebotForce bool // --nodebot: run nodebot even when frop context was piped in
	noColor      bool
	copyMatrix   bool // --copy: markdown matrix → system clipboard
	ignoreRMA    bool // --ignore-rma: fleet scan hides nodes already in an RMA state
	noGolden     bool // --no-golden: skip the GloQL golden-state check (goldenstate.go)

	// extCtx streams piped stdin (frop dissect / NodeBot output) in the
	// background: advisory history the on-node snapshot cannot see. Consumers
	// call extCtx.wait() at gate time (nil-safe → ""). See context.go.
	extCtx *contextReader
}

// defaultOutDir resolves to ~/Downloads/tmp so evidence survives reboots
// (unlike /tmp); falls back to /tmp/gpuinspect when the home dir is unknown.
func defaultOutDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, "Downloads", "tmp")
	}
	return "/tmp/gpuinspect"
}

func usage() {
	fmt.Fprintf(os.Stderr, `gpuinspect %s — GPU/PCIe inspection and analysis (advisory only, never mutates)

Fleet scan:     gpuinspect                      (lists nodes firing PCI link alerts)
Inspect BMNs:   gpuinspect <BMN> [<BMN>...]     (everything derived from the BMN)
Direct remote:  gpuinspect --remote <gMAC> --bmn <BMN> [flags] [BDF...]
On a node:      sudo gpuinspect --bmn <BMN> [--triaged] [--post-drain|--post-swap|--post-reseat] [BDF...]

Flags:
  --context <ctx>    kubectl context for the mgmt cluster (default: current)
  --out-dir <dir>    evidence output dir (default: %s)
  --parallel <n>     concurrent BMN inspections (default: 4)
  --ignore-rma       fleet scan: hide nodes whose FLCC state is RMA-related
                     ('rma', 'prepare-for-rma', ...) — already being serviced
  --no-ai            skip the AI analysis
  --no-golden        skip the golden-state check (GloQL expected_pci_link_* vs
                     live, per alert BDF incl. device identity — catches alerts
                     caused by enumeration/golden-table errors that no drain or
                     RMA can clear; verdict GOLDEN-MIS)
  --no-nodebot       skip the per-BMN 'nodebot diagnose' (on by default)
  --nodebot          force nodebot even when frop context is piped in
                     (piped context normally auto-skips the duplicate diagnose)
  --no-color         disable ANSI colors
  --copy             copy the markdown summary matrix to the clipboard
  --all-pci          widen auto-discovery beyond GPU-related devices (PEX/NIC/NVMe
                     sweep); default is GPU-focused (alert BDFs always checked)
  --remote <gMAC>    direct remote via tsh ssh (target = gMAC, NOT the BMN)
  --bmn <BMN>        node BMN (on-node / direct-remote modes)
  --user <user>      remote ssh user (default: acc)
  --linux-bin <p>    path to linux/amd64 binary to push (default: auto)
  --temps            live thermal readout for ALL GPUs: core+HBM temps sampled at
                     1 Hz (GPUINSPECT_TEMP_SECONDS window, default 10s), driver
                     slowdown/max-operating thresholds, throttle-reason bits and
                     dmesg thermal history. Quick mode — skips PCIe triage, AI
                     and nodebot. Exit 1 on any thermal finding.
  --eval             full GPU evaluation on the node (extended nvidia-smi queries,
                     retired pages, per-GPU PCIe detail, dmon/pmon samples -> bundle eval/)
  --eval-dcgm        + dcgmi diag (implies --eval; requires --yes-intrusive)
  --dcgm-level <n>   DCGM diag level 1-4 (default 2)
  --eval-nvbandwidth + nvbandwidth (implies --eval; requires --yes-intrusive)
  --eval-bug-report  + nvidia-bug-report.sh (implies --eval; slow but safe)
  --yes-intrusive    confirm GPU-stressing diagnostics (DCGM diag / nvbandwidth)
  --post-drain       this run is after the DCT AC power drain
  --post-swap        this run is after an NVMe drive swap
  --post-reseat      this run is after the DCT endpoint reseat (GPU/NIC path)
  --triaged          node already in triage; suppress steps 0-1
  --version          print version

Positional args: BMN names (laptop mode) and/or PCI BDFs (override / on-node).
Piped stdin (laptop modes) is read as external diagnosis context:
     frop dissect -c <BMN> | gpuinspect <BMN>
  The pipe is read in the BACKGROUND — inspection starts immediately while
  frop dissect keeps running; the context is awaited only at verdict/AI time.
  Hard signals in it (open HO/DO ticket, RMA in progress, failed power drain,
  persistent alert) veto the PEX890xx false-positive verdict and are handed to
  the AI analysis. frop dissect IS a nodebot diagnose, so piped context
  auto-skips gpuinspect's own nodebot run (--nodebot forces it anyway).
Env: TRIAGE_SSH / TRIAGE_SCP override transport (default: tsh ssh / tsh scp)
     LOGDIR, STATEDIR, FORCE_COLOR, NVLINK_EXPECTED, ANTHROPIC_API_KEY / op signin
     GPUINSPECT_SHOW_MAX_LINES (per-command report cap, default 200)
     GPUINSPECT_FP_MAX_ALERT_AGE_HOURS (self-cleared FP age gate, default 72)
     GRAFANA_API_TOKEN / GRAFANA_URL, GPUINSPECT_GOLDEN_DS_UID (golden-state check)
`, version, defaultOutDir())
}

func parseArgs(args []string) (*options, error) {
	o := &options{user: "acc", outDir: defaultOutDir(), parallel: 4, nodebot: true, dcgmLevel: 2}
	i := 0
	next := func(flag string) (string, error) {
		i++
		if i >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return args[i], nil
	}
	for ; i < len(args); i++ {
		a := args[i]
		var err error
		switch a {
		case "--remote":
			o.remote, err = next(a)
		case "--bmn":
			o.bmn, err = next(a)
		case "--user":
			o.user, err = next(a)
		case "--linux-bin":
			o.linuxBin, err = next(a)
		case "--context":
			o.kubeContext, err = next(a)
		case "--out-dir":
			o.outDir, err = next(a)
		case "--parallel":
			var v string
			if v, err = next(a); err == nil {
				n, convErr := strconv.Atoi(v)
				if convErr != nil || n < 1 {
					return nil, fmt.Errorf("--parallel: expected positive integer, got %q", v)
				}
				o.parallel = n
			}
		case "--all-pci":
			o.allPCI = true
		case "--temps":
			o.temps = true
		case "--eval":
			o.eval = true
		case "--eval-dcgm":
			o.evalDCGM = true
		case "--eval-nvbandwidth":
			o.evalNVBW = true
		case "--eval-bug-report":
			o.evalBugReport = true
		case "--yes-intrusive":
			o.yesIntrusive = true
		case "--dcgm-level":
			var v string
			if v, err = next(a); err == nil {
				n, convErr := strconv.Atoi(v)
				if convErr != nil || n < 1 || n > 4 {
					return nil, fmt.Errorf("--dcgm-level: expected 1-4, got %q", v)
				}
				o.dcgmLevel = n
			}
		case "--no-ai":
			o.noAI = true
		case "--no-golden":
			o.noGolden = true
		case "--nodebot": // nodebot is on by default; --nodebot FORCES it even
			// when piped frop context would auto-skip the duplicate diagnose
			o.nodebot = true
			o.nodebotForce = true
		case "--no-nodebot":
			o.nodebot = false
		case "--no-color":
			o.noColor = true
		case "--copy":
			o.copyMatrix = true
		case "--ignore-rma":
			o.ignoreRMA = true
		case "--post-drain":
			o.postDrain = true
		case "--post-swap":
			o.postSwap = true
		case "--post-reseat":
			o.postReseat = true
		case "--triaged":
			o.triaged = true
		case "--version":
			fmt.Println("gpuinspect", version)
			os.Exit(0)
		case "-h", "--help":
			usage()
			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") {
				return nil, fmt.Errorf("unknown flag: %s", a)
			}
			if bdfRe.MatchString(a) {
				bdf := strings.ToLower(a)
				if len(bdf) == len("bb:dd.f") { // short form as printed by lspci
					bdf = "0000:" + bdf
				}
				o.bdfs = append(o.bdfs, bdf)
				continue
			}
			if !identRe.MatchString(a) {
				return nil, fmt.Errorf("invalid argument %q: expected a BMN name or PCI BDF", a)
			}
			o.bmns = append(o.bmns, strings.ToLower(a))
		}
		if err != nil {
			return nil, err
		}
	}
	if o.evalDCGM || o.evalNVBW || o.evalBugReport {
		o.eval = true
	}
	if o.temps && o.eval {
		return nil, fmt.Errorf("--temps is a quick thermal-only mode — run --eval separately")
	}
	if o.temps {
		// A temps run answers one question fast; the AI and nodebot phases
		// would multiply its runtime for no thermal signal.
		o.noAI = true
		o.nodebot = false
	}
	if (o.evalDCGM || o.evalNVBW) && !o.yesIntrusive {
		return nil, fmt.Errorf("--eval-dcgm/--eval-nvbandwidth run GPU-stressing diagnostics on the node — confirm with --yes-intrusive")
	}
	if (o.bmn != "" || o.remote != "") && len(o.bmns) > 0 {
		return nil, fmt.Errorf("BMN positional args cannot be combined with --bmn/--remote")
	}
	if o.remote != "" && o.bmn == "" {
		return nil, fmt.Errorf("--remote requires --bmn")
	}
	if o.bmn == "" && o.remote == "" && len(o.bdfs) > 0 && len(o.bmns) == 0 {
		return nil, fmt.Errorf("BDF args need a target: give a BMN, or use --bmn/--remote")
	}
	for _, v := range []struct{ flag, val string }{
		{"--bmn", o.bmn}, {"--remote", o.remote}, {"--user", o.user},
	} {
		if v.val != "" && !identRe.MatchString(v.val) {
			return nil, fmt.Errorf("invalid %s %q: must match [A-Za-z0-9_-]+", v.flag, v.val)
		}
	}
	return o, nil
}

// loadConfigDefaults applies KEY=VALUE lines from ~/.config/gpuinspect/config
// (override path with GPUINSPECT_CONFIG) as environment DEFAULTS — real env
// vars always win. This makes per-user settings like GPUINSPECT_GRAFANA_OP_REF
// work in any shell without profile edits. Store only op:// references or
// plain settings here, never secrets.
func loadConfigDefaults() {
	path := os.Getenv("GPUINSPECT_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		path = filepath.Join(home, ".config", "gpuinspect", "config")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k, v := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		if k != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func main() {
	loadConfigDefaults()
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		usage()
		os.Exit(3)
	}
	// Laptop modes read piped stdin as external diagnosis context (frop
	// dissect / NodeBot) — in the BACKGROUND, so inspection starts while the
	// upstream producer is still running; consumers wait() at gate time. The
	// on-node mode must never touch stdin — the orchestrator streams the
	// binary itself over it during the push.
	if o.bmn == "" && (o.remote != "" || len(o.bmns) > 0) && !isTTY(os.Stdin) {
		o.extCtx = startContextReader(os.Stdin)
		fmt.Fprintln(os.Stderr, "external context: reading piped stdin in the background (frop dissect can keep running)")
	}
	switch {
	case o.remote != "":
		os.Exit(runRemote(o))
	case o.bmn != "":
		os.Exit(runLocal(o))
	case len(o.bmns) > 0:
		os.Exit(orchestrateBMNs(o))
	default:
		os.Exit(runScan(o))
	}
}

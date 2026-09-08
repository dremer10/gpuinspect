package main

// AI advisory layer: 1Password-backed Anthropic API key loading and a
// hand-rolled Messages API client, both ported from frop's internal/s3ai.
// Stdlib only — no SDK. The key lives only in process memory; it is never
// passed as argv to a subcommand or written to disk.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 1Password key loading (ported from frop internal/s3ai)
// ---------------------------------------------------------------------------

func decodeEmbeddedOPRef(b64 string) string {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic("gpuinspect: embedded 1Password ref decode: " + err.Error())
	}
	return string(b)
}

// opAnthropicRef is the default op read target for the Anthropic API key —
// op://eng-fleeteng-frops/anthropic_apikey/password, the same vault/item frop
// uses. Base64-at-rest is the repo idiom to keep op:// paths out of shallow
// scans; the ref itself is not a secret, only the field it points to.
var opAnthropicRef = decodeEmbeddedOPRef("b3A6Ly9lbmctZmxlZXRlbmctZnJvcHMvYW50aHJvcGljX2FwaWtleS9wYXNzd29yZA==")

// normalizeSecretOutput returns the first line of stdout from `op read`,
// trimmed and BOM-stripped — shells sourced via -ilc can prepend banner noise
// on later lines.
func normalizeSecretOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, string(rune(0xFEFF))) // UTF-8 BOM
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// looksLikeOpReadFailureMessage is true when `op read` likely printed an error
// to stdout — it sometimes does so with exit status 0, so exit codes alone
// cannot be trusted.
func looksLikeOpReadFailureMessage(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "[error]") || strings.HasPrefix(strings.TrimSpace(l), "error:") ||
		strings.Contains(l, "could not read") || strings.Contains(l, "not currently signed in") ||
		strings.Contains(l, "you are not signed in") || strings.Contains(l, "authorization failed")
}

// readOnePasswordField resolves an op:// reference. Tries argv-style `op read`
// first (works when the biometric session is live), then $SHELL -c, and
// finally $SHELL -ilc — GUI-launched binaries have no login shell, so op may
// only be on PATH (or signed in) inside an interactive login shell.
func readOnePasswordField(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if opBin, err := exec.LookPath("op"); err == nil {
		cmd := exec.Command(opBin, "read", ref)
		cmd.Env = os.Environ()
		if out, err := cmd.Output(); err == nil {
			if k := normalizeSecretOutput(out); k != "" {
				return k
			}
		}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	quoted := `op read "` + strings.ReplaceAll(ref, `"`, `\"`) + `"`
	for _, args := range [][]string{
		{shell, "-c", quoted},
		{shell, "-ilc", quoted},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = os.Environ()
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if k := normalizeSecretOutput(out); k != "" {
			return k
		}
	}
	return ""
}

// loadAnthropicAPIKey resolves the API key with frop's precedence: a literal
// in ANTHROPIC_API_KEY or anthropic_api_key (the lowercase spelling exists
// because pydantic-based tools export it that way), then an op:// ref in
// either env var, then the embedded default ref. Anything shorter than a
// plausible key or resembling an op error message is rejected.
func loadAnthropicAPIKey() string {
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key"} {
		v := strings.TrimSpace(os.Getenv(k))
		if v != "" && !strings.HasPrefix(v, "op://") {
			return v
		}
	}
	ref := opAnthropicRef
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key"} {
		if v := strings.TrimSpace(os.Getenv(k)); strings.HasPrefix(v, "op://") {
			ref = v
			break
		}
	}
	key := readOnePasswordField(ref)
	if len(key) < 20 || looksLikeOpReadFailureMessage(key) {
		return ""
	}
	return key
}

// ---------------------------------------------------------------------------
// Anthropic Messages API client (hand-rolled net/http, like frop — no SDK)
// ---------------------------------------------------------------------------

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// aiSystemPrompt encodes the triage doctrine the AI must apply. It is a const
// so the whole prompt is reviewable in one place; the runbook references and
// false-positive classes here are the point of the AI layer — without them
// the model would advise generic hardware swaps.
const aiSystemPrompt = `You are a CoreWeave fleet GPU/PCIe triage expert analyzing gpuinspect output for a baremetal node. You ADVISE ONLY — recommend exact commands/steps for the operator; never claim to have executed anything.

You know these runbooks:
- FRO runbook NodePCILinkSpeedUnexpected (Confluence 649756759): PEX890xx switch degraded -> collect bundle -> 10-15 minute DCT AC power drain -> recheck -> still degraded = RMA of PEX/baseboard.
- METAL width runbook (Confluence 1616642163): the alert BDF is a bridge — map it to the endpoint behind it. GPU/NIC/NVMe endpoint -> exactly ONE DCT reseat -> still degraded = RMA (bay/riser/backplane work is out of DCT scope). NVMe path: swap two drives rather than power cycle.

FALSE-POSITIVE CLASSES you must always consider:
1. Unused PEX downstream ports idle at x0 by design — the kernel reports saturated width 63 (METAL-4742). An alert on a bridge with no child device is likely this.
2. fwbundle/golden-state mismatch — node-fwbundle.current unset or != target means expected_pci_link_width rows may be wrong. An OS-verified-healthy link plus a firing alert = data problem: advise checking golden state, NOT hardware action.

GOLDEN-STATE CHECK: the findings may carry a "Golden-state check" section — gpuinspect's laptop-side comparison of the golden table (expected_pci_link_* for the node's SKU+fwbundle) against the live node_pci_cur_* series, per BDF INCLUDING device identity, which the alert rule itself never checks. Treat it as authoritative for the golden-state question:
- IDENTITY MISMATCH or layout sweep hits: the node enumerates a different device at the BDF than the golden row — the alert compares two different devices. Advise fixing enumeration (BIOS settings/NVRAM reset + reapply profile, reboot) or refreshing the golden state (fleetops-common-ansible get_pci_golden_state). A power drain or RMA will NEVER clear this, and return-to-ready will not stick (the alert carries failure_cause and re-fails the node).
- STALE golden values (same device, link at full capability on-node): golden refresh, no hardware action.
- "golden AGREES with the alert": genuine degradation — follow the hardware runbook path.
- "golden now MATCHES live": the table was fixed after the alert fired — advise re-checking the alert before any action.

Missing GPUs (lspci BDF absent or "(rev ff)"): advise collect + Xid check + the power-drain path, RMA if it persists.

EXTERNAL DIAGNOSIS CONTEXT: when the user message carries a "frop dissect / NodeBot" section, it holds alert HISTORY the on-node snapshot cannot see. Persistence across reboots, a power drain already performed that did NOT remediate, or an open DO/HO/RMA ticket OVERRIDES any false-positive classification — a link that re-checks healthy after days of continuous alerts is a flapping-link suspect, not a transient. In that case advise the RMA/hardware path and say explicitly why the false-positive class does not apply. Never advise repeating a remediation the history shows already failed.

XID BIBLE (FMAA Confluence 335773794 "XID Bible" — cite it when you use it). The findings JSON carries the raw dmesg lines in xid_lines; extract every Xid number and apply the fleet action:
- App-level, RESTART_APP — NOT a hardware fault unless recurring across apps/reboots: 8, 11, 13 (graphics engine exception), 25, 31 (GPU memory page fault; triage only when raised by nvvs), 32, 39-41, 60, 68-72, 75-77, 80, 82-86, 88-89, 94 (contained ECC), 96-105, 126-135, 139, 170.
- Reset/reboot class (CWNC production-reboot): 46 (GPU stopped processing), 48 (double-bit ECC; 171=DRAM / 172=SRAM detail), 62 (PMU halt — can follow 48/95), 74 (NVLink error), 95 (uncontained ECC), 136 (ALI link training fail), 140 (unrecovered ECC escape), 155, 156, 158, 160, 167 (PCIe fatal timeout), 169.
- Triage class (node to triage): 54 (GPU aux power not connected — check mechanicals/cabling), 64 (row-remap FAILURE — GPU RMA path), 79 (GPU fallen off the bus — restart BM; RMA path if it recurs), 109 (context-switch timeout — WHEN SOLO this is the known benign class, same as the PEX890xx false positive), 110 (security fault), 119/120 (GSP RPC timeout / GSP error — triage only when solo or paired only with each other), 143 (GPU init error).
- Informational/IGNORE: 43, 44, 45 (preemptive cleanup — points at an earlier root-cause Xid), 63 (row-remap event — reboot to apply remap; often with 92 excessive-SBE), 92, 93, 106-107, 121 (C2C), 137, 141, 152-153, 157, 161-165.
- NVLink5 family 144-150: follow the NVLink5 workflow; only FATAL severity gates action.
Sequencing rules: Xid 45, 62 and 154 usually FOLLOW a root-cause Xid — diagnose the FIRST Xid in the sequence, not the followers. Any app-level or reset-class Xid that recurs after the reset/reboot escalates to the RMA path.

Output format:
"## Summary" (2-3 sentences), then
"## Assessment" (real hardware fault vs probable false positive, with the evidence), then
"## Advised next steps" (numbered, exact copy-paste commands where possible, each marked advisory).`

// aiAnalyze sends the node's findings JSON plus BMN metadata (and, when
// present, piped frop/NodeBot diagnosis context) to the Anthropic Messages
// API and returns the analysis text. Errors carry operator-actionable
// remediation (env var, op signin, or --no-ai) because this runs at the end of
// a long inspection and a bare failure would waste the collected data.
// aiModel resolves the Claude model used for the advisory analysis — shown in
// the report header so it's always clear which model (not nodebot) wrote it.
func aiModel() string { return envOr("ANTHROPIC_MODEL", "claude-opus-4-8") }

func aiAnalyze(findingsJSON string, info *bmnInfo, externalContext string) (string, error) {
	key := anthropicKey()
	if key == "" {
		return "", fmt.Errorf("no Anthropic API key: set ANTHROPIC_API_KEY, or run `op signin` so `op read op://eng-fleeteng-frops/anthropic_apikey/password` works, or re-run with --no-ai")
	}

	model := aiModel()
	timeout := 120 * time.Second
	if d, err := time.ParseDuration(envOr("ANTHROPIC_TIMEOUT", "120s")); err == nil && d > 0 {
		timeout = d
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("marshal bmn info: %w", err)
	}
	user := "BMN metadata:\n" + string(infoJSON) + "\n\nFindings (verdict.json from the node):\n" + findingsJSON
	if strings.TrimSpace(externalContext) != "" {
		user += "\n\nExternal diagnosis context (frop dissect / NodeBot — alert HISTORY the on-node snapshot cannot see; weigh persistence, prior power drains, and open DO/HO tickets against any false-positive classification):\n" + externalContext
	}

	body, err := json.Marshal(anthropicRequest{
		Model:     model,
		MaxTokens: 2000,
		System:    aiSystemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: timeout}
	var resp *http.Response
	// Retry on 5xx with linear backoff; the request must be rebuilt each
	// attempt because the body reader is consumed by the previous send.
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		resp, err = client.Do(req)
		if err != nil {
			return "", err
		}
		if resp.StatusCode < 500 {
			break
		}
		resp.Body.Close()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errMsg := resp.Status
		var out anthropicResponse
		if json.NewDecoder(resp.Body).Decode(&out) == nil && out.Error.Message != "" {
			errMsg += ": " + out.Error.Message
		}
		if resp.StatusCode == http.StatusUnauthorized {
			errMsg += " (run: eval $(op signin), or set ANTHROPIC_API_KEY)"
		}
		return "", fmt.Errorf("anthropic api: %s", errMsg)
	}

	var out anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	var content strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}
	return content.String(), nil
}

// aiFirstLine extracts the first substantive line of an analysis for the
// summary matrix: skips blanks and markdown headings, trims, caps at 100
// chars so the matrix column stays readable.
func aiFirstLine(analysis string) string {
	for _, line := range strings.Split(analysis, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 100 {
			line = line[:100]
		}
		return line
	}
	return ""
}

package main

// nodebot.go — optional per-BMN launch of node-bot (Python CLI, advisory-only).
//
// node-bot runs its own Grafana+LLM diagnosis (`nodebot diagnose <id> --lookback 12`).
// gpuinspect hands it our on-node findings as extra LLM prompt context via the
// FROP_NODEBOT_DIAGNOSE_CONTEXT_APPEND env var — the same contract frop dissect
// uses (node-bot merges that env content into its LLM user prompt).
//
// SECURITY: the API key travels only in cmd.Env — never argv, never disk.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// nodebotContextEnv is read by node-bot diagnose: merged into the LLM user
// prompt, never echoed as the Issue line.
const nodebotContextEnv = "FROP_NODEBOT_DIAGNOSE_CONTEXT_APPEND"

// Fixed Grafana lookback (hours) for nodebot diagnose; not configurable.
const nodebotLookbackHours = 12

// nodebotContextMaxBytes caps the context-append payload handed to the LLM.
const nodebotContextMaxBytes = 8 * 1024

// findNodebot locates the nodebot executable:
//  1. GPUINSPECT_NODEBOT env (explicit path),
//  2. `nodebot` on PATH,
//  3. frop's bootstrapped venv console scripts under the OS user cache
//     (lexically last nodebot-venv-* wins, i.e. the newest bootstrap).
func findNodebot() (string, error) {
	if p := strings.TrimSpace(os.Getenv("GPUINSPECT_NODEBOT")); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("GPUINSPECT_NODEBOT=%q is not an executable file", p)
	}
	if p, err := exec.LookPath("nodebot"); err == nil {
		return p, nil
	}
	if cache, err := os.UserCacheDir(); err == nil {
		for _, pattern := range []string{
			filepath.Join(cache, "gpuinspect", "nodebot-venv-*", "bin", "nodebot"), // our own bootstrap
			filepath.Join(cache, "frop", "nodebot-venv-*", "bin", "nodebot"),       // frop's bootstrap
		} {
			matches, _ := filepath.Glob(pattern)
			sort.Strings(matches)
			for i := len(matches) - 1; i >= 0; i-- {
				if fi, err := os.Stat(matches[i]); err == nil && !fi.IsDir() {
					return matches[i], nil
				}
			}
		}
	}
	return "", fmt.Errorf("nodebot not found: export GPUINSPECT_NODEBOT=<path>, " +
		"install nodebot on PATH, or run `frop dissect` once to let frop bootstrap " +
		"its venv (github.com/coreweave/nodebot)")
}

// runNodebot launches `nodebot diagnose <id> --lookback 12` for one BMN,
// streaming its output to w. It tries the GMAC first (node-bot's node= label is
// usually the reported k8s node id), then the BMN name; the last error is
// returned if every candidate fails.
func runNodebot(gmac, bmn, contextAppend string, w io.Writer) error {
	bin, err := ensureNodebot(w)
	if err != nil {
		return err
	}
	key := anthropicKey()
	if key == "" {
		return fmt.Errorf("no Anthropic API key for nodebot: set ANTHROPIC_API_KEY, " +
			"run `op signin`, or drop --nodebot")
	}
	grafana, err := grafanaPreflight()
	if err != nil {
		return fmt.Errorf("nodebot skipped: %v", err)
	}

	// Filter the inherited environment: empty ANTHROPIC_API_KEY /
	// anthropic_api_key placeholders would stop python-dotenv from filling
	// them, a stale context-append must not leak into this diagnosis, and
	// GRAFANA_* is replaced below (frop's auth-merge pattern) so nodebot
	// talks to Grafana with the preflight-validated Bearer token instead of
	// whatever its local .env happens to contain.
	env := make([]string, 0, len(os.Environ())+6)
	dataDirSet, modelSet := false, false
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		switch name {
		case "ANTHROPIC_API_KEY", "anthropic_api_key", nodebotContextEnv:
			continue
		case "GRAFANA_API_TOKEN", "GRAFANA_USERNAME", "GRAFANA_PASSWORD":
			if grafana != "" {
				continue // superseded by the token below
			}
		case "DATA_DIR":
			dataDirSet = true
		case "MODEL", "model":
			modelSet = true
		}
		env = append(env, kv)
	}
	env = append(env, "ANTHROPIC_API_KEY="+key, "anthropic_api_key="+key) // node-bot's Pydantic Settings field is lowercase
	if grafana != "" {
		env = append(env, "GRAFANA_API_TOKEN="+grafana)
	}
	// Default node-bot to Opus; an operator-set MODEL env var wins (node-bot's
	// Pydantic Settings prefer real env over its .env, so this also overrides
	// a stale .env default).
	if !modelSet {
		env = append(env, "MODEL=claude-opus-4-8")
	}
	if contextAppend != "" {
		env = append(env, nodebotContextEnv+"="+contextAppend)
	}
	// Persistent DATA_DIR keeps MCP OAuth refresh tokens between runs (typical
	// IdP access tokens rotate ~hourly); an existing DATA_DIR wins.
	if !dataDirSet {
		if cache, err := os.UserCacheDir(); err == nil {
			dataDir := filepath.Join(cache, "gpuinspect", "nodebot")
			if err := os.MkdirAll(dataDir, 0o755); err == nil {
				env = append(env, "DATA_DIR="+dataDir)
			}
		}
	}

	var ids []string
	if gmac != "" {
		ids = append(ids, gmac)
	}
	if bmn != "" && bmn != gmac {
		ids = append(ids, bmn)
	}
	if len(ids) == 0 {
		return fmt.Errorf("nodebot: no node id (GMAC or BMN) to diagnose")
	}

	c := colors()
	var lastErr error
	for _, id := range ids {
		fmt.Fprintf(w, "%s>>> nodebot diagnose %s --lookback %d (advisory)%s\n",
			c.D, id, nodebotLookbackHours, c.X)
		cmd := exec.Command(bin, "diagnose", id, "--lookback", strconv.Itoa(nodebotLookbackHours))
		cmd.Env = env
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = fmt.Errorf("nodebot diagnose %s: %w", id, err)
		}
	}
	return lastErr
}

// nodebotContextAppend wraps gpuinspect's findings JSON for node-bot's LLM
// prompt: preamble + findings + answer-format request, capped at ~8KB.
// grafanaPreflight validates the resolved Grafana token with a trivial
// instant query BEFORE spending minutes inside nodebot (frop's preflight
// pattern) — an expired shared token otherwise surfaces as a misleading
// "Node not found in GloQL" deep inside nodebot. The validated token is
// cached in the keychain; a cached-but-expired one is dropped. Skip the
// check with GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1 (e.g. when nodebot's own
// .env carries working credentials).
func grafanaPreflight() (string, error) {
	token := grafanaToken()
	if os.Getenv("GPUINSPECT_SKIP_GRAFANA_PREFLIGHT") != "" {
		return token, nil
	}
	if token == "" {
		return "", fmt.Errorf("no Grafana credentials: nodebot needs GRAFANA_API_TOKEN " +
			"(or a valid token in the 1Password token-viewer item); " +
			"set GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1 to try anyway")
	}
	// GRAFANA_URL follows nodebot's own setting of the same name, so the
	// preflight validates against the instance nodebot will actually query.
	base := strings.TrimRight(envOr("GRAFANA_URL", "https://grafana.int.coreweave.com"), "/")
	req, err := http.NewRequest(http.MethodGet,
		base+"/api/datasources/proxy/uid/benz42hlglhj4a/api/v1/query?query=1", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("grafana preflight: %v (set GPUINSPECT_SKIP_GRAFANA_PREFLIGHT=1 to try anyway)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
		keychainSet(keychainSvcGrafana, token)
		return token, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		keychainDelete(keychainSvcGrafana)
		return "", fmt.Errorf("Grafana token is INVALID/EXPIRED (HTTP %d) — the shared 1Password "+
			"token-viewer item (eng-fleeteng-frops) needs rotation; ask in #fro-internal-help, "+
			"or export a personal GRAFANA_API_TOKEN", resp.StatusCode)
	default:
		return "", fmt.Errorf("grafana preflight: unexpected HTTP %d", resp.StatusCode)
	}
}

func nodebotContextAppend(findingsJSON, externalContext string) string {
	const preamble = "gpuinspect (read-only GPU/PCIe inspector) already collected the " +
		"following on-node findings — factor them in; recommend actions only, " +
		"nothing has been executed:"
	const answerFormat = "End with: short summary, then numbered advisory remediation steps."
	const truncMark = "...[truncated]"
	body := findingsJSON
	if strings.TrimSpace(externalContext) != "" {
		body += "\n\nPrior external diagnosis (frop dissect — weigh its alert history, " +
			"prior power drains and open tickets):\n" + externalContext
	}
	if max := nodebotContextMaxBytes - len(preamble) - len(answerFormat) - len(truncMark) - 4; len(body) > max {
		body = body[:max] + truncMark
	}
	return preamble + "\n\n" + body + "\n\n" + answerFormat
}

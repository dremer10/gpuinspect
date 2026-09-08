package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// anthropicKey is the shared entry point for the AI and nodebot layers. Two
// caching tiers keep 1Password from prompting more than once:
//
//   - per-process (sync.Once): a multi-BMN run resolves the key exactly once,
//     no matter how many parallel workers call aiAnalyze/runNodebot;
//   - macOS keychain, 1h TTL: back-to-back gpuinspect runs inside an hour
//     reuse the cached key without touching `op` at all. The keychain is
//     encrypted at rest, and the value is piped over stdin (never argv, never
//     a plaintext file) per the repo secrets rules.
//
// Env literals still win over the keychain so a freshly exported key is
// picked up immediately.
var (
	anthropicKeyOnce sync.Once
	anthropicKeyVal  string
	grafanaTokenOnce sync.Once
	grafanaTokenVal  string
)

const (
	keychainSvcAnthropic = "gpuinspect-anthropic"
	keychainSvcGrafana   = "gpuinspect-grafana"
	keychainTTL          = time.Hour
)

func anthropicKey() string {
	anthropicKeyOnce.Do(func() {
		for _, n := range []string{"ANTHROPIC_API_KEY", "anthropic_api_key"} {
			if v := strings.TrimSpace(os.Getenv(n)); v != "" && !strings.HasPrefix(v, "op://") {
				anthropicKeyVal = v
				return
			}
		}
		if k := keychainGet(keychainSvcAnthropic); k != "" {
			anthropicKeyVal = k
			return
		}
		if k := loadAnthropicAPIKey(); k != "" {
			anthropicKeyVal = k
			keychainSet(keychainSvcAnthropic, k)
		}
	})
	return anthropicKeyVal
}

// grafanaToken resolves the Grafana Bearer token node-bot needs (frop's
// cw-dashboards-as-code token-viewer item), with the same caching tiers as
// anthropicKey — except the keychain is only WRITTEN after the token passes
// the preflight in nodebot.go, so an expired vault token is never cached.
// Empty result means "leave nodebot's own env/.env alone".
func grafanaToken() string {
	grafanaTokenOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("GRAFANA_API_TOKEN")); v != "" && !strings.HasPrefix(v, "op://") {
			grafanaTokenVal = v
			return
		}
		if k := keychainGet(keychainSvcGrafana); k != "" {
			grafanaTokenVal = k
			return
		}
		// GPUINSPECT_GRAFANA_OP_REF points token resolution at a personal
		// 1Password item (an op:// path) instead of the shared token-viewer
		// item. Note the token must be valid for the Grafana that nodebot
		// queries (GRAFANA_URL, default grafana.int.coreweave.com).
		ref := opGrafanaTokenRef
		if v := strings.TrimSpace(os.Getenv("GPUINSPECT_GRAFANA_OP_REF")); strings.HasPrefix(v, "op://") {
			ref = v
		}
		k := readOnePasswordField(ref)
		if len(k) >= 20 && !looksLikeOpReadFailureMessage(k) {
			grafanaTokenVal = k
		}
	})
	return grafanaTokenVal
}

// keychainDelete drops a cached secret (e.g. after it fails validation).
func keychainDelete(service string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("security", "delete-generic-password",
		"-s", service, "-a", os.Getenv("USER")).Run()
}

// keychainGet returns the cached secret for a service when it is younger
// than keychainTTL. The stored value is "<unix-ts>:<secret>" so the TTL
// rides along with it.
func keychainGet(service string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", service, "-a", os.Getenv("USER"), "-w").Output()
	if err != nil {
		return ""
	}
	ts, key, ok := splitKeychainValue(strings.TrimSpace(string(out)))
	if !ok || time.Since(time.Unix(ts, 0)) > keychainTTL {
		return ""
	}
	return key
}

func splitKeychainValue(s string) (int64, string, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return 0, "", false
	}
	ts, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || len(s[i+1:]) < 20 {
		return 0, "", false
	}
	return ts, s[i+1:], true
}

// keychainSet stores a secret with a timestamp. `security -i` reads its
// command from stdin, keeping the secret out of process argv.
func keychainSet(service, key string) {
	if runtime.GOOS != "darwin" || key == "" {
		return
	}
	val := fmt.Sprintf("%d:%s", time.Now().Unix(), key)
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(
		"add-generic-password -U -s %q -a %q -w %q\n",
		service, os.Getenv("USER"), val))
	_ = cmd.Run()
}

// opGrafanaTokenRef is frop's Grafana token-viewer item (Bearer token in the
// password field). Base64 at rest per the repo idiom for op:// paths.
var opGrafanaTokenRef = decodeEmbeddedOPRef("b3A6Ly9lbmctZmxlZXRlbmctZnJvcHMvY3ctZGFzaGJvYXJkcy1hcy1jb2RlX2dyYWZhbmFfdG9rZW5fdmlld2VyL3Bhc3N3b3Jk")

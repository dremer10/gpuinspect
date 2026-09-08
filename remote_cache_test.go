package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func buildTestScript(t *testing.T, sha string, cached bool) string {
	t.Helper()
	return buildRemoteScript(
		"/tmp/pci_triage_20260804_120000",      // remoteDir
		"1",                                    // fc
		"gpuinspect --remote host --bmn ss1x1", // rerunCmd
		"/tmp/out/ss1x1_20260804_120000",       // localDir
		"GPUINSPECT_DCGM_TIMEOUT='600' ",       // evalEnv
		"ss1x1",                                // bmn
		"--triaged 0000:16:00.0",               // passthru
		8, sha, cached)
}

func TestCacheRemotePath(t *testing.T) {
	got := cacheRemotePath(testSha)
	want := "/var/tmp/gpuinspect_cache_" + testSha
	if got != want {
		t.Fatalf("cacheRemotePath = %q, want %q", got, want)
	}
}

func TestBuildRemoteScriptMissPushesAndStoresCache(t *testing.T) {
	s := buildTestScript(t, testSha, false)
	if !strings.Contains(s, "cat > '/tmp/pci_triage_20260804_120000/gpuinspect'") {
		t.Errorf("miss script must stream the binary via cat >:\n%s", s)
	}
	if !strings.Contains(s, "rm -f /var/tmp/gpuinspect_cache_* 2>/dev/null") {
		t.Errorf("miss script must evict older cached versions:\n%s", s)
	}
	if !strings.Contains(s, "cp '/tmp/pci_triage_20260804_120000/gpuinspect' "+cacheRemotePath(testSha)+" 2>/dev/null || true") {
		t.Errorf("miss script must store the cache non-fatally:\n%s", s)
	}
	if !strings.Contains(s, "chmod +x '/tmp/pci_triage_20260804_120000/gpuinspect' && {") {
		t.Errorf("cache store must come after chmod +x and before cd/sudo:\n%s", s)
	}
}

func TestBuildRemoteScriptCachedCopiesAndNeverReadsStdin(t *testing.T) {
	s := buildTestScript(t, testSha, true)
	if strings.Contains(s, "cat >") {
		t.Errorf("cached script must not contain cat > (would hang with nil stdin):\n%s", s)
	}
	if !strings.Contains(s, "cp "+cacheRemotePath(testSha)+" '/tmp/pci_triage_20260804_120000/gpuinspect'") {
		t.Errorf("cached script must copy the binary from the node cache:\n%s", s)
	}
	if strings.Contains(s, "rm -f /var/tmp/gpuinspect_cache_*") {
		t.Errorf("cached script must not touch the cache store:\n%s", s)
	}
}

func TestBuildRemoteScriptNoShaIsLegacyPush(t *testing.T) {
	s := buildTestScript(t, "", false)
	if !strings.Contains(s, "cat > '/tmp/pci_triage_20260804_120000/gpuinspect'") {
		t.Errorf("no-sha script must stream the binary via cat >:\n%s", s)
	}
	if strings.Contains(s, "gpuinspect_cache_") {
		t.Errorf("no-sha script must not reference the cache at all:\n%s", s)
	}
	// cached=true with an empty sha must degrade to the stdin push, never a
	// cp from a nonsense cache path.
	if s2 := buildTestScript(t, "", true); !strings.Contains(s2, "cat >") || strings.Contains(s2, "gpuinspect_cache_") {
		t.Errorf("cached+empty-sha must degrade to legacy push:\n%s", s2)
	}
}

// The cd + sudo invocation (env, expected GPUs, eval env, --bmn, passthru)
// must be byte-identical in every mode — only the binary-staging prefix may
// differ. A regression here changes on-node behavior depending on cache state.
func TestBuildRemoteScriptSudoTailIdenticalAcrossModes(t *testing.T) {
	tail := func(s string) string {
		i := strings.Index(s, " && cd '")
		if i < 0 {
			t.Fatalf("script has no cd/sudo tail:\n%s", s)
		}
		return s[i:]
	}
	legacy := tail(buildTestScript(t, "", false))
	miss := tail(buildTestScript(t, testSha, false))
	hit := tail(buildTestScript(t, testSha, true))
	if miss != legacy {
		t.Errorf("miss tail differs from legacy tail:\n%s\nvs\n%s", miss, legacy)
	}
	if hit != legacy {
		t.Errorf("hit tail differs from legacy tail:\n%s\nvs\n%s", hit, legacy)
	}
	for _, want := range []string{
		"sudo FORCE_COLOR=1",
		"GPUINSPECT_EXPECTED_GPUS='8'",
		"GPUINSPECT_DCGM_TIMEOUT='600' ",
		"--bmn 'ss1x1' --triaged 0000:16:00.0",
	} {
		if !strings.Contains(legacy, want) {
			t.Errorf("sudo tail missing %q:\n%s", want, legacy)
		}
	}
}

// Every mode must stay one flat && chain (plus the single brace group for the
// cache store) — no subshells, heredocs, or bare `;` outside the brace group.
// The past "wait: remote command exited without exit status" bug was suspected
// to involve the compound remote command shape.
func TestBuildRemoteScriptStaysFlatChain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"legacy", buildTestScript(t, "", false)},
		{"miss", buildTestScript(t, testSha, false)},
		{"hit", buildTestScript(t, testSha, true)},
	} {
		if strings.Contains(tc.script, "$(") || strings.Contains(tc.script, "<<") {
			t.Errorf("%s: script contains subshell/heredoc:\n%s", tc.name, tc.script)
		}
		braceless := tc.script
		if i, j := strings.Index(braceless, "{"), strings.Index(braceless, "}"); i >= 0 && j > i {
			braceless = braceless[:i] + braceless[j+1:]
		}
		if strings.Contains(braceless, "{") || strings.Contains(braceless, ";") {
			t.Errorf("%s: script is not a flat && chain outside the cache-store group:\n%s", tc.name, tc.script)
		}
	}
}

func TestUnameToGoarch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64", " aarch64\n": "arm64",
		"riscv64": "", "": "",
	} {
		if got := unameToGoarch(in); got != want {
			t.Errorf("unameToGoarch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindLinuxBinaryArch(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"gpuinspect-linux-amd64", "gpuinspect-linux-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	for _, goarch := range []string{"amd64", "arm64"} {
		got, err := findLinuxBinary(&options{}, goarch)
		if err != nil {
			t.Fatalf("findLinuxBinary(%s): %v", goarch, err)
		}
		if !strings.HasSuffix(got, "gpuinspect-linux-"+goarch) {
			t.Fatalf("findLinuxBinary(%s) = %q, want the %s artifact", goarch, got, goarch)
		}
	}
	// missing arch artifact → actionable error naming the arch
	if err := os.Remove(filepath.Join(dir, "gpuinspect-linux-arm64")); err != nil {
		t.Fatal(err)
	}
	if _, err := findLinuxBinary(&options{}, "arm64"); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("want linux/arm64 error, got %v", err)
	}
	// --linux-bin overrides arch selection entirely
	explicit := filepath.Join(dir, "gpuinspect-linux-amd64")
	if got, err := findLinuxBinary(&options{linuxBin: explicit}, "arm64"); err != nil || got != explicit {
		t.Fatalf("--linux-bin must win: got %q, %v", got, err)
	}
}

func TestSha256File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(p, []byte("hello gpuinspect"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sha256File(p)
	if err != nil {
		t.Fatal(err)
	}
	// printf 'hello gpuinspect' | shasum -a 256
	const want = "be2dd6cce27aaaf98f9774e2c6609ff539f80d766a7d57375547039cdb477697"
	if got != want {
		t.Fatalf("sha256File = %s, want %s", got, want)
	}
	if !cacheShaRe.MatchString(got) {
		t.Fatalf("digest %q does not match cacheShaRe", got)
	}
	if _, err := sha256File(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

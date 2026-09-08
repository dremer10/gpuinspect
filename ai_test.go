package main

import (
	"strings"
	"testing"
)

// No network calls in this file: aiAnalyze is not exercised, and the
// loadAnthropicAPIKey cases either short-circuit on env literals or point the
// op fallbacks at a no-op shell.

func TestNormalizeSecretOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "sk-ant-abc123\n", "sk-ant-abc123"},
		{"multi-line keeps first", "sk-ant-abc123\nsome shell banner\n", "sk-ant-abc123"},
		{"crlf", "sk-ant-abc123\r\nnext", "sk-ant-abc123"},
		{"bom stripped", string(rune(0xFEFF)) + "sk-ant-abc123\n", "sk-ant-abc123"},
		{"surrounding space", "  sk-ant-abc123  \n", "sk-ant-abc123"},
		{"empty", "", ""},
		{"whitespace only", " \n\t\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeSecretOutput([]byte(tc.in)); got != tc.want {
				t.Errorf("normalizeSecretOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLooksLikeOpReadFailureMessage(t *testing.T) {
	positives := []string{
		"[ERROR] 2026/07/25 something broke",
		"error: invalid reference",
		"could not read secret op://vault/item/field",
		"you are not currently signed in",
		"You are not signed in to 1Password",
		"authorization failed",
	}
	for _, s := range positives {
		if !looksLikeOpReadFailureMessage(s) {
			t.Errorf("looksLikeOpReadFailureMessage(%q) = false, want true", s)
		}
	}
	negatives := []string{
		"sk-ant-api03-abcdefghijklmnop",
		"",
		// "error" only counts as a prefix or bracketed tag — a key that
		// happens to contain the substring must not be rejected.
		"sk-ant-errorless-abcdefghijk",
	}
	for _, s := range negatives {
		if looksLikeOpReadFailureMessage(s) {
			t.Errorf("looksLikeOpReadFailureMessage(%q) = true, want false", s)
		}
	}
}

func TestAIFirstLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"skips headings and blanks", "## Summary\n\nGPU 3 fell off the bus.\nMore detail.", "GPU 3 fell off the bus."},
		{"plain first line", "Probable false positive.\n## Assessment", "Probable false positive."},
		{"trims", "   padded line   \n", "padded line"},
		{"empty", "", ""},
		{"only headings", "## Summary\n### Assessment\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aiFirstLine(tc.in); got != tc.want {
				t.Errorf("aiFirstLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	long := strings.Repeat("x", 150)
	if got := aiFirstLine(long); len(got) != 100 {
		t.Errorf("aiFirstLine long line: len = %d, want 100", len(got))
	}
}

func TestLoadAnthropicAPIKeyEnvLiteral(t *testing.T) {
	t.Setenv("anthropic_api_key", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-literal-key")
	if got := loadAnthropicAPIKey(); got != "sk-ant-test-literal-key" {
		t.Errorf("loadAnthropicAPIKey() = %q, want env literal", got)
	}
}

func TestLoadAnthropicAPIKeyOpRefNotLiteral(t *testing.T) {
	// An op:// value must be resolved via `op read`, never returned verbatim.
	// Point SHELL at a no-op so the fallback tiers fail fast without a real
	// 1Password session; we only assert the ref itself is not leaked back.
	t.Setenv("anthropic_api_key", "")
	t.Setenv("ANTHROPIC_API_KEY", "op://x")
	t.Setenv("SHELL", "/usr/bin/false")
	if got := loadAnthropicAPIKey(); got == "op://x" {
		t.Errorf("loadAnthropicAPIKey() returned the op:// ref verbatim")
	}
}

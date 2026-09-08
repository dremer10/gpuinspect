package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Bootstrap of the nodebot CLI, ported from frop's internal/nodebotsetup:
// when nodebot is not already runnable, discover its Python source, create a
// persistent venv under the user cache, and editable-install it (plus the
// [mcp] extra). Bump nodebotBootstrapVersion after recipe changes — the venv
// path embeds it, so a new version forces a clean rebuild.

const nodebotBootstrapVersion = "1"

// ensureNodebot returns a runnable nodebot executable, bootstrapping a
// gpuinspect-managed venv when discovery fails. Progress streams to w — the
// first bootstrap downloads packages and takes a minute or two.
func ensureNodebot(w io.Writer) (string, error) {
	if bin, err := findNodebot(); err == nil {
		return bin, nil
	}
	src, err := findNodebotSource()
	if err != nil {
		return "", err
	}
	c := colors()
	fmt.Fprintf(w, "%s>>> nodebot not found — bootstrapping venv from %s (one-time)%s\n", c.D, src, c.X)
	py, err := ensureNodebotVenv(src, w)
	if err != nil {
		return "", err
	}
	bin := filepath.Join(filepath.Dir(py), "nodebot")
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return "", fmt.Errorf("bootstrap succeeded but %s is missing", bin)
	}
	return bin, nil
}

// findNodebotSource locates a nodebot source checkout (pyproject.toml):
//  1. GPUINSPECT_NODEBOT_DIR env,
//  2. the frop submodule path in any engineer's workspace
//     (~/coreweave/*/team/*/gox/frop/nodebot),
//  3. a previously cloned copy under the user cache,
//  4. `git clone` of github.com/coreweave/nodebot into the user cache.
func findNodebotSource() (string, error) {
	hasSrc := func(d string) bool {
		_, err := os.Stat(filepath.Join(d, "pyproject.toml"))
		return err == nil
	}
	if d := strings.TrimSpace(os.Getenv("GPUINSPECT_NODEBOT_DIR")); d != "" {
		if hasSrc(d) {
			return d, nil
		}
		return "", fmt.Errorf("GPUINSPECT_NODEBOT_DIR=%q has no pyproject.toml", d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, "coreweave", "*", "team", "*", "gox", "frop", "nodebot", "pyproject.toml"))
		sort.Strings(matches)
		if len(matches) > 0 {
			return filepath.Dir(matches[0]), nil
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache dir for nodebot clone: %w", err)
	}
	dst := filepath.Join(cache, "gpuinspect", "nodebot-src")
	if hasSrc(dst) {
		return dst, nil
	}
	_ = os.RemoveAll(dst) // partial clone from an interrupted run
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	var lastErr error
	for _, url := range []string{
		"git@github.com:coreweave/nodebot.git",
		"https://github.com/coreweave/nodebot.git",
	} {
		cmd := exec.Command("git", "clone", "--depth", "1", url, dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("git clone %s: %v: %s", url, err, strings.TrimSpace(string(out)))
			continue
		}
		if hasSrc(dst) {
			return dst, nil
		}
		lastErr = fmt.Errorf("clone of %s has no pyproject.toml", url)
	}
	return "", fmt.Errorf("no nodebot source found (checked GPUINSPECT_NODEBOT_DIR, "+
		"~/coreweave/*/team/*/gox/frop/nodebot, and git clone of coreweave/nodebot): %w", lastErr)
}

// ensureNodebotVenv mirrors frop's EnsureManagedVenv: venv under
// <cache>/gpuinspect/nodebot-venv-<version>, `pip install -e src` plus the
// [mcp] extra, verified with `python -m nodebot.cli --version`. Returns the
// venv python path.
func ensureNodebotVenv(srcDir string, w io.Writer) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache dir: %w", err)
	}
	root := filepath.Join(cache, "gpuinspect", "nodebot-venv-"+nodebotBootstrapVersion)
	py := filepath.Join(root, "bin", "python3")

	versionOK := func() bool {
		cmd := exec.Command(py, "-m", "nodebot.cli", "--version")
		cmd.Dir = srcDir
		return cmd.Run() == nil
	}
	if fi, err := os.Stat(py); err == nil && !fi.IsDir() && versionOK() {
		return py, nil
	}

	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return "", err
	}
	// Remove a broken/partial venv so "python -m venv" can recreate it.
	if _, err := os.Stat(root); err == nil {
		_ = os.RemoveAll(root)
	}
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout, cmd.Stderr = w, w
		cmd.Env = os.Environ()
		return cmd.Run()
	}
	if err := run("python3", "-m", "venv", root); err != nil {
		return "", fmt.Errorf("python3 -m venv %q: %w", root, err)
	}
	for _, args := range [][]string{
		{"-m", "pip", "install", "-U", "pip"},
		{"-m", "pip", "install", "-e", srcDir},
		{"-m", "pip", "install", "-e", srcDir + "[mcp]"},
	} {
		if err := run(py, args...); err != nil {
			return "", fmt.Errorf("%s %s: %w", py, strings.Join(args, " "), err)
		}
	}
	if !versionOK() {
		return "", fmt.Errorf("nodebot still not runnable after install (try %s -m nodebot.cli --version)", py)
	}
	return py, nil
}

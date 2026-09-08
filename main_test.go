package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// base returns the option defaults parseArgs starts from, so wants only spell
// out the deltas.
func base(mod func(*options)) *options {
	o := &options{user: "acc", outDir: defaultOutDir(), parallel: 4, nodebot: true, dcgmLevel: 2}
	if mod != nil {
		mod(o)
	}
	return o
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg := t.TempDir() + "/config"
	content := "# comment\nGPUINSPECT_TEST_A=from-config\nGPUINSPECT_TEST_B = spaced \n\nbadline\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUINSPECT_CONFIG", cfg)
	t.Setenv("GPUINSPECT_TEST_B", "from-env") // real env must win
	t.Setenv("GPUINSPECT_TEST_A", "")
	loadConfigDefaults()
	if got := os.Getenv("GPUINSPECT_TEST_A"); got != "from-config" {
		t.Errorf("GPUINSPECT_TEST_A = %q; want from-config", got)
	}
	if got := os.Getenv("GPUINSPECT_TEST_B"); got != "from-env" {
		t.Errorf("GPUINSPECT_TEST_B = %q; want from-env (env wins)", got)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    *options
		wantErr string // substring of the expected error; empty = no error
	}{
		{
			name: "happy path direct-remote with mixed BDFs",
			args: []string{
				"--remote", "gmac-01", "--bmn", "BMN_1234", "--user", "ops-user",
				"--linux-bin", "./bin/gpuinspect-linux-amd64",
				"--post-drain", "--post-swap", "--post-reseat", "--triaged",
				"0000:5c:00.0", "e1:0a.7",
			},
			want: base(func(o *options) {
				o.bmn, o.remote, o.user = "BMN_1234", "gmac-01", "ops-user"
				o.linuxBin = "./bin/gpuinspect-linux-amd64"
				o.postDrain, o.postSwap, o.postReseat, o.triaged = true, true, true, true
				o.bdfs = []string{"0000:5c:00.0", "0000:e1:0a.7"}
			}),
		},
		{
			name: "BMN-first laptop mode with flags",
			args: []string{
				"--context", "us-east-01a-mgmt", "--out-dir", "/tmp/x",
				"--parallel", "8", "--no-ai", "--no-nodebot", "--no-color",
				"ss892297x4309312", "SQTWS8L0C261900582",
			},
			want: base(func(o *options) {
				o.kubeContext, o.outDir, o.parallel = "us-east-01a-mgmt", "/tmp/x", 8
				o.noAI, o.nodebot, o.noColor = true, false, true
				o.bmns = []string{"ss892297x4309312", "sqtws8l0c261900582"}
			}),
		},
		{
			name: "BMN with explicit BDF override",
			args: []string{"ss892297x4309312", "5c:00.0"},
			want: base(func(o *options) {
				o.bmns = []string{"ss892297x4309312"}
				o.bdfs = []string{"0000:5c:00.0"}
			}),
		},
		{
			name: "short-form BDF auto-prefixed (on-node mode)",
			args: []string{"--bmn", "b1", "5c:00.0"},
			want: base(func(o *options) { o.bmn = "b1"; o.bdfs = []string{"0000:5c:00.0"} }),
		},
		{
			name: "uppercase hex normalized to lowercase",
			args: []string{"--bmn", "b1", "0000:5C:00.0", "AB:CD.3"},
			want: base(func(o *options) { o.bmn = "b1"; o.bdfs = []string{"0000:5c:00.0", "0000:ab:cd.3"} }),
		},
		{
			// "banana" is a valid BMN-ish identifier now; it lands in bmns.
			name: "non-BDF word becomes a BMN arg",
			args: []string{"banana"},
			want: base(func(o *options) { o.bmns = []string{"banana"} }),
		},
		{
			// --nodebot is no longer a no-op: it must FORCE nodebot even when
			// piped frop context would auto-skip it (inspectOneBMN checks
			// nodebotForce alongside nodebot).
			name: "--nodebot sets nodebotForce",
			args: []string{"--nodebot", "ss892297x4309312"},
			want: base(func(o *options) {
				o.nodebot, o.nodebotForce = true, true
				o.bmns = []string{"ss892297x4309312"}
			}),
		},
		{
			name: "--copy sets copyMatrix",
			args: []string{"--copy", "ss892297x4309312"},
			want: base(func(o *options) {
				o.copyMatrix = true
				o.bmns = []string{"ss892297x4309312"}
			}),
		},
		{
			name: "copyMatrix defaults to false without --copy",
			args: []string{"ss892297x4309312"},
			want: base(func(o *options) { o.bmns = []string{"ss892297x4309312"} }),
		},
		{
			name: "--eval alone (safe collection needs no --yes-intrusive)",
			args: []string{"--eval", "ss892297x4309312"},
			want: base(func(o *options) {
				o.eval = true
				o.bmns = []string{"ss892297x4309312"}
			}),
		},
		{
			name: "--eval-dcgm with --yes-intrusive implies --eval, custom level",
			args: []string{"--eval-dcgm", "--yes-intrusive", "--dcgm-level", "3", "ss892297x4309312"},
			want: base(func(o *options) {
				o.eval, o.evalDCGM, o.yesIntrusive = true, true, true
				o.dcgmLevel = 3
				o.bmns = []string{"ss892297x4309312"}
			}),
		},
		{
			name: "--eval-bug-report implies --eval without intrusive gate",
			args: []string{"--eval-bug-report", "ss892297x4309312"},
			want: base(func(o *options) {
				o.eval, o.evalBugReport = true, true
				o.bmns = []string{"ss892297x4309312"}
			}),
		},
		{
			name:    "--eval-dcgm refused without --yes-intrusive",
			args:    []string{"--eval-dcgm", "ss892297x4309312"},
			wantErr: "--yes-intrusive",
		},
		{
			name:    "--eval-nvbandwidth refused without --yes-intrusive",
			args:    []string{"--eval-nvbandwidth", "ss892297x4309312"},
			wantErr: "--yes-intrusive",
		},
		{
			name:    "--dcgm-level out of range",
			args:    []string{"--dcgm-level", "9", "ss892297x4309312"},
			wantErr: "--dcgm-level",
		},
		{
			name:    "invalid arg with shell metachars",
			args:    []string{"b1; rm -rf /"},
			wantErr: "invalid argument",
		},
		{
			name:    "BDF without any target",
			args:    []string{"--triaged", "0000:5c:00.0"},
			wantErr: "BDF args need a target",
		},
		{
			name:    "BMN positionals cannot mix with --bmn",
			args:    []string{"--bmn", "b1", "ss892297x4309312"},
			wantErr: "cannot be combined",
		},
		{
			name:    "--remote requires --bmn",
			args:    []string{"--remote", "gmac-01"},
			wantErr: "--remote requires --bmn",
		},
		{
			name:    "invalid --bmn with quote",
			args:    []string{"--bmn", `b1"; rm -rf /`},
			wantErr: "invalid --bmn",
		},
		{
			name:    "invalid --user",
			args:    []string{"--bmn", "b1", "--user", "a c c"},
			wantErr: "invalid --user",
		},
		{
			name:    "invalid --parallel",
			args:    []string{"--parallel", "zero", "ss892297x4309312"},
			wantErr: "--parallel",
		},
		{
			name:    "flag missing its value",
			args:    []string{"--bmn"},
			wantErr: "--bmn requires a value",
		},
		{
			name:    "unknown flag",
			args:    []string{"--bmn", "b1", "--frobnicate"},
			wantErr: "unknown flag: --frobnicate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseArgs(%q) = %+v, want error containing %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseArgs(%q) error = %q, want it to contain %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%q) unexpected error: %v", tt.args, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseArgs(%q)\n got: %+v\nwant: %+v", tt.args, got, tt.want)
			}
		})
	}
}

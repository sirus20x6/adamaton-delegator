package contextmode

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func skipIfMissing(t *testing.T, cmd string) {
	t.Helper()
	if _, err := exec.LookPath(cmd); err != nil {
		t.Skipf("%s not in PATH; skipping", cmd)
	}
}

func TestRun_BashSimple(t *testing.T) {
	skipIfMissing(t, "bash")
	res, err := Run(context.Background(), ExecOptions{
		Language: LangBash,
		Script:   "echo hello && echo world",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
	got := string(res.Stdout)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("missing output: %q", got)
	}
}

func TestRun_ExitCode(t *testing.T) {
	skipIfMissing(t, "bash")
	res, err := Run(context.Background(), ExecOptions{
		Language: LangBash,
		Script:   "exit 42",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("expected exit 42, got %d", res.ExitCode)
	}
}

func TestRun_Stderr(t *testing.T) {
	skipIfMissing(t, "bash")
	res, err := Run(context.Background(), ExecOptions{
		Language: LangBash,
		Script:   "echo on stdout; echo on stderr 1>&2",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "on stdout") {
		t.Errorf("stdout missing: %q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "on stderr") {
		t.Errorf("stderr missing: %q", res.Stderr)
	}
}

func TestRun_Timeout(t *testing.T) {
	skipIfMissing(t, "bash")
	start := time.Now()
	res, err := Run(context.Background(), ExecOptions{
		Language: LangBash,
		Script:   "sleep 5",
		Timeout:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut=true")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout didn't fire promptly; elapsed %s", elapsed)
	}
}

func TestRun_OutputCap(t *testing.T) {
	skipIfMissing(t, "bash")
	res, err := Run(context.Background(), ExecOptions{
		Language: LangBash,
		Script:   `for i in $(seq 1 100000); do echo "line $i"; done`,
		MaxBytes: 1024,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated=true when output exceeds MaxBytes")
	}
	if len(res.Stdout) > 1024 {
		t.Errorf("stdout exceeded cap: %d bytes", len(res.Stdout))
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		hint, script string
		want         Language
	}{
		{"", "", LangBash},
		{"bash", "", LangBash},
		{"sh", "", LangBash},
		{"python", "", LangPython},
		{"py", "", LangPython},
		{"node", "", LangNode},
		{"go", "", LangGo},
		{"", "#!/usr/bin/env python3\nprint('hi')", LangPython},
		{"", "#!/bin/bash\necho hi", LangBash},
		{"", "#!/usr/bin/env node\nconsole.log('hi')", LangNode},
	}
	for _, c := range cases {
		got := Detect(c.hint, c.script)
		if got != c.want {
			t.Errorf("Detect(%q, ...) = %q, want %q", c.hint, got, c.want)
		}
	}
}

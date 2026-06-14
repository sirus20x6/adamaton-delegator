package main

import (
	"os"
	"testing"
)

func TestResolveTasksDSN(t *testing.T) {
	const cfg = "postgres://cfg/db"

	os.Unsetenv("DELEGATOR_TASKS_DSN")
	if got := resolveTasksDSN(cfg); got != cfg {
		t.Fatalf("unset: expected fallback to cfg, got %q", got)
	}

	t.Setenv("DELEGATOR_TASKS_DSN", "postgres://pi5/db")
	if got := resolveTasksDSN(cfg); got != "postgres://pi5/db" {
		t.Fatalf("set: expected override, got %q", got)
	}

	t.Setenv("DELEGATOR_TASKS_DSN", "")
	if got := resolveTasksDSN(cfg); got != cfg {
		t.Fatalf("empty: expected fallback to cfg, got %q", got)
	}
}

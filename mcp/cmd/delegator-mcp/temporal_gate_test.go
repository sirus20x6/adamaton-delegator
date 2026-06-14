package main

import (
	"net"
	"testing"
	"time"
)

func TestTemporalGateReachable(t *testing.T) {
	// A bound-then-closed port refuses connections fast → unreachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	g := &temporalGate{addr: closedAddr}
	start := time.Now()
	if g.reachable() {
		t.Errorf("expected unreachable for closed addr %s", closedAddr)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("reachable() on a closed addr should fail fast, took %v", d)
	}

	// An open listener → reachable.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	g2 := &temporalGate{addr: ln2.Addr().String()}
	if !g2.reachable() {
		t.Errorf("expected reachable for open addr %s", ln2.Addr())
	}
}

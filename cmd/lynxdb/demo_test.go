package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDemoListenCandidatesFallbackOnlyForDefaultAddr(t *testing.T) {
	got := demoListenCandidates(defaultDemoAddr, true)
	if len(got) != 11 {
		t.Fatalf("default candidates len = %d, want 11", len(got))
	}
	if got[0] != defaultDemoAddr || got[1] != "127.0.0.1:3101" || got[10] != "127.0.0.1:3110" {
		t.Fatalf("unexpected candidates: %v", got)
	}

	custom := demoListenCandidates("127.0.0.1:4100", true)
	if len(custom) != 1 || custom[0] != "127.0.0.1:4100" {
		t.Fatalf("custom candidates = %v, want only custom addr", custom)
	}
}

func TestDemoCommandAddsServerForFallbackAddr(t *testing.T) {
	base := "lynxdb query 'level=ERROR'"
	if got := demoCommand("http://"+defaultDemoAddr, base); got != base {
		t.Fatalf("default demo command = %q, want %q", got, base)
	}

	got := demoCommand("http://127.0.0.1:3101", base)
	if !strings.Contains(got, "--server http://127.0.0.1:3101") {
		t.Fatalf("fallback demo command missing --server: %q", got)
	}
}

func TestStartDemoServerFallsBackWhenFirstPortIsBusy(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occupied.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started, err := startDemoServer(ctx, logger, []string{occupied.Addr().String(), "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("startDemoServer: %v", err)
	}
	defer started.cancel()

	if started.srv.Addr() == occupied.Addr().String() {
		t.Fatalf("server used occupied addr %s", started.srv.Addr())
	}

	resp, err := http.Get(fmt.Sprintf("http://%s/health", started.srv.Addr()))
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}

	started.cancel()
	select {
	case err := <-started.errCh:
		if err != nil {
			t.Fatalf("server shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func environ(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestRun_AFixtureDirectoryThatDoesNotParseExitsNonZeroNamingTheFile(t *testing.T) {
	dir := writeFixtures(t, map[string]string{"10-ok.json": okChat, "20-broken.json": `{"match": {"schema": "file_summary"}, "response": `})

	t.Run("through the flag", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"-fixtures", dir, "-addr", "127.0.0.1:0"}, environ(), &stderr); code != 1 {
			t.Errorf("exit code %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "20-broken.json") || strings.Contains(stderr.String(), "10-ok.json") {
			t.Errorf("stderr = %q, want it to name 20-broken.json, and only it", stderr.String())
		}
	})

	t.Run("through the environment", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(t.Context(), nil, environ("LLMREPLAY_FIXTURES", dir, "LLMREPLAY_ADDR", "127.0.0.1:0"), &stderr); code != 1 || !strings.Contains(stderr.String(), "20-broken.json") {
			t.Errorf("exit code %d, stderr %q, want 1 naming 20-broken.json", code, stderr.String())
		}
	})

	t.Run("the flag wins over the environment", func(t *testing.T) {
		good := writeFixtures(t, map[string]string{"10-ok.json": okChat})
		var stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // serve returns at once: only the loading is under test
		if code := run(ctx, []string{"-fixtures", good, "-addr", "127.0.0.1:0"}, environ("LLMREPLAY_FIXTURES", dir), &stderr); code != 0 {
			t.Errorf("exit code %d, stderr %q, want 0: the flag names a good directory", code, stderr.String())
		}
	})

	t.Run("the default directory is /fixtures", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"-addr", "127.0.0.1:0"}, environ(), &stderr); code != 1 || !strings.Contains(stderr.String(), "/fixtures") {
			t.Errorf("exit code %d, stderr %q, want 1 naming /fixtures", code, stderr.String())
		}
	})

	t.Run("an unknown flag is a usage error", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"-nope"}, environ(), &stderr); code != 2 {
			t.Errorf("exit code %d, want 2", code)
		}
	})
}

func TestServe_HealthzIsOKAndShutdownIsGraceful(t *testing.T) {
	fx, err := loadFixtures(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var stderr bytes.Buffer
	exited := make(chan int, 1)
	go func() { exited <- serve(ctx, ln, newHandler(fx, slog.New(slog.DiscardHandler)), &stderr) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	stop() // what SIGTERM does through signal.NotifyContext
	select {
	case code := <-exited:
		if code != 0 || stderr.Len() != 0 {
			t.Errorf("exit code %d, stderr %q, want a clean 0", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
		t.Error("the server still answers after shutdown")
	}
}

func TestServe_AnInFlightRequestFinishesBeforeShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("done"))
	})
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	exited := make(chan int, 1)
	go func() { exited <- serve(ctx, ln, slow, &bytes.Buffer{}) }()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			body <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		body <- b.String()
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	stop()
	select {
	case <-exited:
		t.Fatal("serve returned while a request was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-body; got != "done" {
		t.Errorf("the in-flight request got %q, want done", got)
	}
	if code := <-exited; code != 0 {
		t.Errorf("exit code %d, want 0", code)
	}
}

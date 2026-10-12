package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// withCommands swaps the registry for the length of a test.
func withCommands(t *testing.T, cmds map[string]Command) {
	t.Helper()
	saved := commands
	commands = cmds
	t.Cleanup(func() { commands = saved })
}

func TestServeIsRegistered(t *testing.T) {
	if commands["serve"] == nil {
		t.Fatal("serve is not registered")
	}
}

func TestRun_UnknownCommandExits2ListingRegisteredNames(t *testing.T) {
	noop := func(context.Context, []string) error { return nil }
	withCommands(t, map[string]Command{"seed": noop, "serve": noop, "migrate": noop})

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"bogus"}, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	for _, want := range []string{`unknown command "bogus"`, "migrate, seed, serve"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
		}
	}
}

func TestRun_NoArgumentRunsServe(t *testing.T) {
	var served bool
	withCommands(t, map[string]Command{"serve": func(context.Context, []string) error {
		served = true
		return nil
	}})

	if code := run(context.Background(), nil, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !served {
		t.Fatal("serve did not run")
	}
}

func TestRun_PassesTheRemainingArgumentsAndMapsAnErrorToExit1(t *testing.T) {
	var got []string
	withCommands(t, map[string]Command{"seed": func(_ context.Context, args []string) error {
		got = args
		return errors.New("boom")
	}})

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"seed", "--domain", "acme.example"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := []string{"--domain", "acme.example"}; !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if want := "kiwi seed: boom"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not contain %q", stderr.String(), want)
	}
}

func TestRegister_PanicsOnADuplicateName(t *testing.T) {
	noop := func(context.Context, []string) error { return nil }
	withCommands(t, map[string]Command{})
	register("once", noop)

	defer func() {
		if recover() == nil {
			t.Fatal("registering the same name twice did not panic")
		}
	}()
	register("once", noop)
}

package main

import (
	"strings"
	"testing"
)

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, args := range [][]string{{"daemon", "--help"}, {"daemon", "install", "--help"}, {"daemon", "logs", "--help"}, {"service", "--help"}, {"serve", "--help"}, {"doctor", "--help"}, {"verify", "--help"}, {"backup", "--help"}, {"restore", "--help"}, {"diagnostics", "--help"}, {"protocol", "--help"}, {"help"}, {"--help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%q) returned %v", args, err)
		}
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("unknown command must fail")
	}
}

func TestVersionJSON(t *testing.T) {
	if err := run([]string{"version", "--json"}); err != nil {
		t.Fatalf("version --json: %v", err)
	}
}

func TestBrowserURLUsesFragmentBootstrapToken(t *testing.T) {
	value := browserURL("0.0.0.0:7332", "top-secret")
	if value != "http://127.0.0.1:7332/#token=top-secret" {
		t.Fatalf("browserURL=%q", value)
	}
	if got := browserURL("127.0.0.1:7332", ""); got != "http://127.0.0.1:7332/" {
		t.Fatalf("tokenless browserURL=%q", got)
	}
}

func TestServiceRejectsInvalidCapacityBeforeOpeningRegistry(t *testing.T) {
	for _, args := range [][]string{
		{"service", "--runtime-limit=0", "--no-browser"},
		{"service", "--idle-timeout=0s", "--no-browser"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}

func TestWebCommandsRejectNonLoopbackBeforeOpeningState(t *testing.T) {
	commands := map[string][]string{
		"service": {"--data-root=relative"},
		"serve":   {"--repo=missing-repository"},
	}
	for command, invalidStateArgs := range commands {
		for _, address := range []string{"0.0.0.0:7332", "[::]:7332", "192.168.1.20:7332", "pairroom.local:7332", "localhost:7332"} {
			args := append([]string{command, "--listen=" + address, "--no-browser"}, invalidStateArgs...)
			err := run(args)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "loopback") {
				t.Fatalf("%s accepted non-loopback listen %q: %v", command, address, err)
			}
		}
	}
}

func TestLoopbackListenAcceptsOnlyNumericLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7332", "127.0.0.2:7332", "[::1]:7332"} {
		if !isLoopbackListen(address) {
			t.Errorf("expected loopback listen %q", address)
		}
	}
	for _, address := range []string{"", ":7332", "0.0.0.0:7332", "[::]:7332", "localhost:7332", "192.168.1.20:7332", "pairroom.local:7332"} {
		if isLoopbackListen(address) {
			t.Errorf("unexpected loopback listen %q", address)
		}
	}
}

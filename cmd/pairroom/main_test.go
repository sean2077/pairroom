package main

import "testing"

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, args := range [][]string{{"daemon", "--help"}, {"daemon", "install", "--help"}, {"daemon", "logs", "--help"}, {"service", "--help"}, {"serve", "--help"}, {"doctor", "--help"}, {"verify", "--help"}, {"backup", "--help"}, {"restore", "--help"}, {"diagnostics", "--help"}, {"help"}, {"--help"}} {
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
		{"service", "--listen=0.0.0.0:7332", "--no-browser"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}

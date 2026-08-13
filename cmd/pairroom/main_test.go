package main

import "testing"

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, args := range [][]string{{"serve", "--help"}, {"doctor", "--help"}, {"verify", "--help"}, {"backup", "--help"}, {"restore", "--help"}, {"diagnostics", "--help"}, {"help"}, {"--help"}} {
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

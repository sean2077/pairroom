package relayclient

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRelayTopLevelHelpListsCommands(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		var out, diagnostic bytes.Buffer
		if err := Run(context.Background(), []string{arg}, strings.NewReader(""), &out, &diagnostic); err != nil {
			t.Fatalf("relay %s: %v", arg, err)
		}
		for _, action := range relayActions {
			if !strings.Contains(out.String(), action) {
				t.Fatalf("relay %s omits %q: %q", arg, action, out.String())
			}
		}
		if diagnostic.Len() != 0 {
			t.Fatalf("relay %s wrote diagnostics: %q", arg, diagnostic.String())
		}
	}
}

func TestRelayUnknownActionFailsBeforeFlagParsing(t *testing.T) {
	for _, args := range [][]string{{"nope"}, {"nope", "--help"}, {"exchang", "-h"}} {
		var out, diagnostic bytes.Buffer
		err := Run(context.Background(), args, strings.NewReader(""), &out, &diagnostic)
		if err == nil || !strings.Contains(err.Error(), "unknown relay operation") || !strings.Contains(err.Error(), "exchange") {
			t.Fatalf("relay %v error = %v", args, err)
		}
		if out.Len() != 0 || diagnostic.Len() != 0 {
			t.Fatalf("relay %v printed output %q / %q", args, out.String(), diagnostic.String())
		}
	}
}

func TestRelayKnownActionHelpPrintsFlags(t *testing.T) {
	for _, action := range relayActions {
		var out, diagnostic bytes.Buffer
		if err := Run(context.Background(), []string{action, "--help"}, strings.NewReader(""), &out, &diagnostic); err != nil {
			t.Fatalf("relay %s --help: %v", action, err)
		}
		if !strings.Contains(diagnostic.String(), "-repo") || out.Len() != 0 {
			t.Fatalf("relay %s --help output %q / %q", action, out.String(), diagnostic.String())
		}
	}
}

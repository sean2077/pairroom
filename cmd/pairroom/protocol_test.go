package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestWriteEmbeddedProtocolText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := writeProtocol([]string{"--host-mode", "embedded", "--actor", "codex"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		protocol.Version,
		"actor: slot2",
		"[collaboration.creation]",
		"[delivery.single-turn]",
		"[delivery.peer]",
		"exact peer_handle",
	} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("protocol output missing %q:\n%s", fragment, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "[role.driver]") {
		t.Fatalf("filtered output contains driver rule:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestWriteProtocolDefaultsToNative(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeProtocol([]string{"--actor=claude", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var contract protocol.Contract
	if err := json.Unmarshal(stdout.Bytes(), &contract); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if contract.Version != protocol.NativeDynamicVersion || contract.Actor != model.ActorSlot1 || len(contract.Rules) == 0 {
		t.Fatalf("unexpected contract: %+v", contract)
	}
}

func TestWriteLegacyNativeProtocolIsExplicit(t *testing.T) {
	var out, diagnostic bytes.Buffer
	if err := writeProtocol([]string{"--legacy-native", "--json"}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	var contract protocol.Contract
	if err := json.Unmarshal(out.Bytes(), &contract); err != nil || contract.Version != protocol.NativeVersion {
		t.Fatalf("legacy contract changed: %+v %v", contract, err)
	}
	if err := writeProtocol([]string{"--host-mode", "embedded", "--legacy-native"}, &out, &diagnostic); err == nil {
		t.Fatal("legacy Native option applied to Embedded")
	}
}

func TestWriteProtocolHelpAndValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeProtocol([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(stderr.String(), "usage: pairroom protocol") {
		t.Fatalf("help omitted usage: %s", stderr.String())
	}
	stderr.Reset()
	if err := writeProtocol([]string{"--actor", "other"}, &stdout, &stderr); err == nil {
		t.Fatal("invalid actor succeeded")
	}
	stdout.Reset()
	stderr.Reset()
	if err := writeProtocol([]string{"--routing", "turns"}, &stdout, &stderr); err == nil {
		t.Fatal("removed routing flag succeeded")
	}
}

func TestProtocolNoLongerOffersRoleModes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeProtocol([]string{"--role", "driver"}, &stdout, &stderr); err == nil {
		t.Fatal("removed role option accepted")
	}
}

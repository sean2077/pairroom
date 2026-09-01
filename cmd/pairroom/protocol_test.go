package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestWriteProtocolText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := writeProtocol([]string{"--actor", "codex", "--role", "reviewer", "--routing", "turns"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		protocol.Version,
		"actor: codex",
		"[role.reviewer]",
		"[delivery.single-turn]",
		"[delivery.next]",
		"[PAIRROOM:DONE]",
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

func TestWriteProtocolJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeProtocol([]string{"--actor=claude", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var contract protocol.Contract
	if err := json.Unmarshal(stdout.Bytes(), &contract); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if contract.Version != protocol.Version || contract.Actor != model.ActorClaude || len(contract.Rules) == 0 {
		t.Fatalf("unexpected contract: %+v", contract)
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
	if strings.Contains(stderr.String(), "legacy values are accepted as aliases") || !strings.Contains(stderr.String(), "only supported routing mode") {
		t.Fatalf("help advertises stale routing compatibility: %s", stderr.String())
	}
	stderr.Reset()
	if err := writeProtocol([]string{"--actor", "other"}, &stdout, &stderr); err == nil {
		t.Fatal("invalid actor succeeded")
	}
	for _, mode := range []string{"manual", "mentions", "roundtable"} {
		stdout.Reset()
		stderr.Reset()
		if err := writeProtocol([]string{"--routing", mode}, &stdout, &stderr); err == nil {
			t.Fatalf("legacy routing mode %q succeeded", mode)
		}
	}
}

package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestProductionCLIExcludesTestingRuntime(t *testing.T) {
	// go list without -test inspects the shipped dependency graph, not this
	// test binary. The relay fixture seam is also used by Service tests.
	output, err := exec.Command("go", "list", "-mod=readonly", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect production imports: %v\n%s", err, output)
	}
	for _, name := range strings.Fields(string(output)) {
		if name == "testing" || strings.HasPrefix(name, "testing/") {
			t.Fatalf("production CLI imports test infrastructure: %s", name)
		}
	}
}

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
)

func TestStopProcessTreeStillWaitsForReaders(t *testing.T) {
	// Even a no-op Kill cannot replace reader/wait completion evidence.
	done := make(chan struct{})
	started := time.Now()
	var tree *execx.Tree
	err := stopProcessTree(tree, done, "helper")
	if err == nil || !strings.Contains(err.Error(), "stop state is uncertain") {
		t.Fatalf("stop with open completion channel: %v", err)
	}
	if time.Since(started) < processTreeExitTimeout {
		t.Fatal("stop released ownership before the process-tree timeout")
	}
}

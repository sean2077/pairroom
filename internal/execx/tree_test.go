package execx

import (
	"os"
	"os/exec"
	"sync"
	"testing"
)

func TestTreeExitHelper(t *testing.T) {
	if os.Getenv("PAIRROOM_TREE_EXIT_HELPER") == "1" {
		os.Exit(0)
	}
}

func exitedTree(t *testing.T) *Tree {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTreeExitHelper$")
	cmd.Env = append(os.Environ(), "PAIRROOM_TREE_EXIT_HELPER=1")
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tree.Release)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestTreeKillAfterWaitAndRelease(t *testing.T) {
	tree := exitedTree(t)
	tree.Release()
	for range 3 {
		if err := tree.Kill(); err != nil {
			t.Fatalf("Kill after Wait and Release: %v", err)
		}
	}
}

func TestTreeConcurrentKillAndRelease(t *testing.T) {
	tree := exitedTree(t)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			for range 20 {
				// Before Release, Windows may reject a Kill on the already
				// waited process. Only post-Release success is contractual.
				_ = tree.Kill()
			}
		})
	}
	workers.Go(func() { <-start; tree.Release() })
	close(start)
	workers.Wait()
	if err := tree.Kill(); err != nil {
		t.Fatalf("Kill after concurrent Release: %v", err)
	}
}

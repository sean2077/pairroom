package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	required := []string{
		"frontend/index.html",
		"frontend/styles.css",
		"assets/icon.png",
		"build/appicon.png",
	}
	for _, name := range required {
		info, err := os.Stat(filepath.Clean(name))
		if err != nil {
			panic(fmt.Errorf("required desktop asset %s: %w", name, err))
		}
		if info.IsDir() || info.Size() == 0 {
			panic(fmt.Errorf("required desktop asset %s is empty or not a file", name))
		}
	}
	fmt.Printf("verified %d PairRoom desktop assets\n", len(required))
}

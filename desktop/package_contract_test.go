package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsInstallerShipsPairroomCLI(t *testing.T) {
	nsi, err := os.ReadFile(filepath.Join("build", "windows", "nsis", "project.nsi"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(nsi)
	if !strings.Contains(text, `File "/oname=pairroom.exe" "..\..\..\bin\pairroom.exe"`) {
		t.Fatal("Windows NSIS installer must ship pairroom.exe next to PairRoom.exe")
	}
	if !strings.Contains(text, `daemon uninstall`) {
		t.Fatal("Windows uninstaller must stop the bundled pairroom daemon")
	}

	collect, err := os.ReadFile(filepath.Join("scripts", "collect-artifacts.py"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(collect)
	if strings.Contains(script, `portable = binary_dir / "PairRoom.exe"`) {
		t.Fatal("Windows CI artifacts must not treat PairRoom.exe as a complete package")
	}
	if !strings.Contains(script, `binary_dir / "pairroom.exe"`) {
		t.Fatal("Windows CI collection must require the bundled pairroom CLI")
	}
	if !strings.Contains(script, "must not publish a standalone PairRoom.exe") {
		t.Fatal("Windows CI collection must refuse to upload PairRoom.exe")
	}

	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "desktop-wails.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(workflow), "wails3 task windows:build") {
		t.Fatal("desktop CI must not build a standalone Windows PairRoom.exe artifact")
	}
}

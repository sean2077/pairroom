package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsInstallerShipsPairroomCLI(t *testing.T) {
	iss, err := os.ReadFile(filepath.Join("build", "windows", "inno", "PairRoom.iss"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(iss)
	if !strings.Contains(text, `Source: "{#DesktopRoot}\bin\cli\pairroom.exe"; DestDir: "{app}\bin"`) {
		t.Fatal("Windows Inno installer must ship the CLI from bin/cli into {app}\\bin, separate from PairRoom.exe")
	}
	for _, contract := range []string{
		`AppId={#PairRoomId}`,
		`UninstallDisplayName=PairRoom`,
		`CloseApplications=no`,
		`RestartApplications=no`,
		`function InitializeUninstall: Boolean;`,
		`Result := PayloadError;`,
	} {
		if !strings.Contains(text, contract) {
			t.Fatalf("Windows installer must retain %q", contract)
		}
	}
	if _, err := os.Stat(filepath.Join("build", "windows", "nsis", "project.nsi")); !os.IsNotExist(err) {
		t.Fatal("the retired NSIS installer must not coexist with Inno Setup")
	}

	collect, err := os.ReadFile(filepath.Join("scripts", "collect-artifacts.py"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(collect)
	if strings.Contains(script, `portable = binary_dir / "PairRoom.exe"`) {
		t.Fatal("Windows CI artifacts must not treat PairRoom.exe as a complete package")
	}
	if !strings.Contains(script, `binary_dir / "cli" / "pairroom.exe"`) {
		t.Fatal("Windows CI collection must require the bundled pairroom CLI under bin/cli")
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
	if !strings.Contains(string(workflow), `startsWith(github.ref, 'refs/tags/v')`) {
		t.Fatal("desktop publishing must run only on version tags")
	}
	if !strings.Contains(string(workflow), "test_windows_installer.ps1") {
		t.Fatal("Windows PR CI must exercise the real Inno installer")
	}
	if !strings.Contains(string(workflow), "gh release upload") {
		t.Fatal("desktop packages must be attached to the GitHub Release")
	}
	if strings.Contains(string(workflow), "--clobber") {
		t.Fatal("published installer URLs must retain immutable content")
	}
	if !strings.Contains(string(workflow), "scripts/merge-checksums.py") ||
		!strings.Contains(string(workflow), `"pairroom-desktop-v${version}-SHA256SUMS"`) {
		t.Fatal("desktop packages must be published with a verified pairroom-desktop-vX.Y.Z-SHA256SUMS asset")
	}
	if !strings.Contains(script, "pairroom-desktop-v") {
		t.Fatal("published desktop files must use the pairroom-desktop- prefix")
	}
	if !strings.Contains(script, "-setup.exe") {
		t.Fatal("Windows desktop release files must use -setup.exe so they are not confused with the CLI .exe")
	}
}

func TestTrayMenuExposesServiceControls(t *testing.T) {
	text, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	source := string(text)
	for _, label := range []string{
		"Open PairRoom",
		"Open Management in Browser",
		"Restart Service",
		"Open Service Data Folder",
		"Quit PairRoom",
	} {
		if !strings.Contains(source, `menu.Add("`+label+`")`) {
			t.Fatalf("tray menu must offer %q", label)
		}
	}
	// Restarting from the tray may only touch the Service this Desktop owns; an
	// installed daemon stays under `pairroom daemon` control.
	if !strings.Contains(source, "restartItem.SetEnabled(value.Mode() == host.ModeEmbedded)") {
		t.Fatal("tray Restart Service must be enabled only for the embedded Service")
	}
	// Launch-at-login registration remains owned solely by the native Settings
	// switch; the tray must not grow a second autostart control.
	if strings.Contains(source, "AddCheckbox") {
		t.Fatal("tray menu must not add checkbox controls; autostart stays in native Settings")
	}
}

// desktop/go.mod is the single Wails pin. Copies of the version elsewhere drift
// on every Dependabot bump, so CI and setup docs derive it from the module.
func TestWailsCLIVersionIsDerivedFromGoMod(t *testing.T) {
	const derived = `wails3@$(go -C desktop list -m -f '{{.Version}}' github.com/wailsapp/wails/v3)`
	for _, path := range []string{
		filepath.Join("..", ".github", "workflows", "desktop-wails.yml"),
		"README.md",
		filepath.Join("build", "darwin", "Taskfile.yml"),
		filepath.Join("build", "linux", "Taskfile.yml"),
		filepath.Join("build", "windows", "Taskfile.yml"),
	} {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(text), "v3.0.0-") {
			t.Fatalf("%s hard-codes a Wails version; derive it from desktop/go.mod", path)
		}
		if installs := strings.Count(string(text), "cmd/wails3@"); installs != strings.Count(string(text), derived) {
			t.Fatalf("%s installs the Wails CLI without the go.mod-derived version", path)
		}
	}
}

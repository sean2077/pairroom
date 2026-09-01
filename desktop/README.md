# PairRoom Desktop

`desktop/` is a Tauri 2 thin shell around the existing PairRoom control plane. It does not fork the Management Shell, Room UI, HTTP/SSE protocol, registry, event log, or native Claude Code/Codex adapters.

## Runtime model

The installer bundles the existing Go binary as a target-specific Tauri sidecar.

On launch the desktop shell:

1. accepts an explicitly supplied `PAIRROOM_DESKTOP_URL` only after numeric-loopback and bearer-authentication checks;
2. otherwise looks for the authenticated URL of a healthy installed `pairroom daemon`;
3. otherwise starts the bundled `pairroom service` on an ephemeral numeric-loopback port;
4. navigates the native webview to the existing Management Shell.

Existing `window.open()` Room flows become native child webview windows. Closing the main window hides it to the system tray; **Quit PairRoom** asks an owned sidecar to drain through its normal daemon-control boundary before the desktop process exits. An externally managed daemon is never stopped by the desktop shell.

The desktop shell never guesses that a `service.lock` is stale. Startup failures keep PairRoom fail-closed and surface the recovery instruction instead.

## Local development

Prerequisites:

- Go version from the repository `go.mod`;
- Rust stable (1.85 or newer);
- Node.js 22 or newer;
- the normal Tauri platform dependencies.

Prepare the target-specific sidecar, then run Tauri:

```bash
python desktop/scripts/prepare-sidecar.py
cd desktop
npm install
npm run icons
npm run dev
```

Build installers for the host:

```bash
python desktop/scripts/prepare-sidecar.py
cd desktop
npm install
npm run icons
npm run build
```

For cross-target builds, pass the same Rust target to both steps:

```bash
python desktop/scripts/prepare-sidecar.py --target aarch64-apple-darwin
cd desktop
npm run icons
npm run build -- --target aarch64-apple-darwin
```

Generated sidecars, icons, Rust targets, collected payloads, and `node_modules` are ignored.

## Supported artifacts

`.github/workflows/desktop.yml` builds workflow artifacts for:

- Linux amd64: AppImage and Debian package;
- Windows amd64: MSI and NSIS installers;
- macOS Apple Silicon: DMG;
- macOS Intel: DMG.

Each uploaded workflow artifact contains only final installers plus `SHA256SUMS` and `manifest.json`; Tauri staging directories are excluded. These CI artifacts are unsigned development builds. Production Windows signing and Apple Developer ID signing/notarization require repository secrets and are intentionally not claimed by this change.

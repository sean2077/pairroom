#!/usr/bin/env python3
"""Build and update the local desktop installation without changing daemon state.

Only desktop-owned binaries (or the macOS app bundle) are replaced. User data,
OS login settings, the Windows uninstaller, and unrelated installation files
are left intact. No process is killed and no installer/daemon command is run.
"""
from __future__ import annotations

import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def windows_registry_directories() -> list[Path]:
    """Find custom NSIS installs without executing registry command strings."""
    import winreg

    found = []
    key = r"Software\Microsoft\Windows\CurrentVersion\Uninstall"
    for hive in (winreg.HKEY_CURRENT_USER, winreg.HKEY_LOCAL_MACHINE):
        for view in (winreg.KEY_WOW64_64KEY, winreg.KEY_WOW64_32KEY):
            try:
                parent = winreg.OpenKey(hive, key, 0, winreg.KEY_READ | view)
            except FileNotFoundError:
                continue
            with parent:
                for index in range(winreg.QueryInfoKey(parent)[0]):
                    with winreg.OpenKey(parent, winreg.EnumKey(parent, index)) as entry:
                        try:
                            name = winreg.QueryValueEx(entry, "DisplayName")[0]
                            if name != "PairRoom":
                                continue
                            try:
                                location = winreg.QueryValueEx(entry, "InstallLocation")[0]
                            except FileNotFoundError:
                                location = ""
                            if location:
                                found.append(Path(os.path.expandvars(location)))
                                continue
                            # Wails NSIS may only record the quoted uninstaller.
                            command = winreg.QueryValueEx(entry, "UninstallString")[0].strip()
                            if command.startswith('"') and '"' in command[1:]:
                                found.append(Path(command.split('"')[1]).parent)
                            elif command.lower().endswith(".exe"):
                                found.append(Path(command).parent)
                        except FileNotFoundError:
                            continue
    return found


def installation_directory(platform: str, override: str | None = None) -> Path:
    if override:
        return Path(override).expanduser().absolute()
    home = Path.home()
    if platform == "win32":
        local = os.environ.get("LOCALAPPDATA")
        if not local:
            raise RuntimeError("LOCALAPPDATA is unavailable; set DESKTOP_INSTALL_DIR explicitly")
        default = Path(local) / "Programs" / "PairRoom"
        candidates = windows_registry_directories() + [default]
        if os.environ.get("ProgramFiles"):
            candidates.append(Path(os.environ["ProgramFiles"]) / "PairRoom contributors" / "PairRoom")
        executable = Path("PairRoom.exe")
    elif platform == "darwin":
        default = home / "Applications"
        candidates = [default, Path("/Applications")]
        executable = Path("PairRoom.app/Contents/MacOS/PairRoom")
    elif platform == "linux":
        default = home / ".local" / "lib" / "pairroom-desktop"
        candidates = [default, home / ".local" / "bin", Path("/usr/local/bin")]
        executable = Path("PairRoom")
    else:
        raise RuntimeError(f"unsupported desktop platform: {platform}")
    matches = {path.resolve() for path in candidates if (path / executable).is_file()}
    if len(matches) > 1:
        paths = ", ".join(str(p) for p in sorted(matches))
        raise RuntimeError(f"multiple desktop installations found ({paths}); set DESKTOP_INSTALL_DIR")
    if not matches:
        raise RuntimeError("no desktop installation found; install a desktop package first, or set "
                           "DESKTOP_INSTALL_DIR to the existing/custom installation directory")
    return next(iter(matches))


def payload(platform: str, root: Path = ROOT) -> list[tuple[Path, Path]]:
    if platform == "win32":
        return [(root / "bin/PairRoom.exe", Path("PairRoom.exe")),
                (root / "bin/cli/pairroom.exe", Path("bin/pairroom.exe"))]
    if platform == "darwin":
        return [(root / "bin/PairRoom.app", Path("PairRoom.app"))]
    if platform == "linux":
        return [(root / "bin/PairRoom", Path("PairRoom")),
                (root / "bin/pairroom", Path("pairroom"))]
    raise RuntimeError(f"unsupported desktop platform: {platform}")


def validate_payload(platform: str, root: Path = ROOT) -> list[tuple[Path, Path]]:
    files = payload(platform, root)
    required = [source for source, _ in files]
    if platform == "darwin":
        required = [root / "bin/PairRoom.app" / suffix for suffix in (
            "Contents/MacOS/PairRoom", "Contents/MacOS/pairroom", "Contents/Info.plist")]
    for source in required:
        if not source.is_file() or source.stat().st_size == 0:
            raise RuntimeError(f"missing or empty desktop build artifact: {source}")
    return files


def build(platform: str, wails: str) -> None:
    executable = shutil.which(wails)
    if not executable:
        raise RuntimeError(f"Wails CLI not found: {wails}; install the version pinned in desktop/go.mod")
    executable = str(Path(executable).resolve())
    env = os.environ.copy()
    # Use the same executable for prepare-build and nested platform tasks.
    env["PAIRROOM_WAILS"] = executable
    env["PAIRROOM_DESKTOP_PYTHON"] = sys.executable
    env["PATH"] = str(Path(executable).parent) + os.pathsep + env.get("PATH", "")
    goos = {"linux": "linux", "darwin": "darwin", "win32": "windows"}[platform]
    if env.get("GOOS", goos) != goos:
        raise RuntimeError("desktop-update builds for this host; unset cross-compilation GOOS")
    host_arch = subprocess.check_output(["go", "env", "GOHOSTARCH"], text=True).strip()
    if env.get("GOARCH", host_arch) != host_arch:
        raise RuntimeError("desktop-update builds for this host; unset cross-compilation GOARCH")
    subprocess.run([sys.executable, "scripts/prepare-build.py"], cwd=ROOT, env=env, check=True)
    task = "darwin:package" if platform == "darwin" else f"{goos}:build"
    subprocess.run([executable, "task", task, "PRODUCTION=true", f"ARCH={host_arch}"],
                   cwd=ROOT, env=env, check=True)


def remove(path: Path) -> None:
    if path.is_dir() and not path.is_symlink():
        shutil.rmtree(path)
    else:
        path.unlink(missing_ok=True)


def replace_payload(directory: Path, files: list[tuple[Path, Path]]) -> None:
    """Stage on the same filesystem, then replace with rollback on any error."""
    directory.mkdir(parents=True, exist_ok=True)
    lock = directory / ".pairroom-desktop-update.lock"
    try:
        lock.mkdir()
    except FileExistsError as exc:
        raise RuntimeError(f"another update may be running; inspect {lock} before removing it") from exc
    stage: Path | None = None
    preserve = False
    originals: list[tuple[Path, Path]] = []
    installed: list[Path] = []
    try:
        stage = Path(tempfile.mkdtemp(prefix=".pairroom-desktop-update-", dir=directory))
        for index, (source, relative) in enumerate(files):
            target = directory / relative
            if source.resolve() == target.resolve():
                raise RuntimeError(f"install location is the build output itself: {target}")
            if target.is_symlink():
                raise RuntimeError(f"refusing to replace symlink {target}; select its real installation directory")
            staged = stage / f"new-{index}"
            if source.is_dir():
                shutil.copytree(source, staged, symlinks=True)
            else:
                shutil.copy2(source, staged)
        # Nothing in the installed application changes until staging succeeds.
        try:
            for index, (_, relative) in enumerate(files):
                target = directory / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                if target.exists():
                    backup = stage / f"old-{index}"
                    target.rename(backup)
                    originals.append((target, backup))
            for index, (_, relative) in enumerate(files):
                target = directory / relative
                (stage / f"new-{index}").rename(target)
                installed.append(target)
        except OSError as exc:
            try:
                for target in reversed(installed):
                    remove(target)
                for target, backup in reversed(originals):
                    backup.rename(target)
            except OSError as rollback_error:
                preserve = True
                raise RuntimeError(f"update and rollback failed; backups retained in {stage}: {rollback_error}") from exc
            raise RuntimeError("update rolled back. Quit PairRoom from the tray and retry; "
                               "if an installed daemon holds the CLI, stop it gracefully first. "
                               f"Check write permissions for {directory}. Original error: {exc}") from exc
    finally:
        try:
            if stage is not None and not preserve:
                try:
                    shutil.rmtree(stage)
                except OSError as cleanup_error:
                    print(f"Warning: update backup retained at {stage}: {cleanup_error}", file=sys.stderr)
        finally:
            lock.rmdir()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--install-dir", help="installation directory; on macOS, parent of PairRoom.app")
    parser.add_argument("--wails", default="wails3", help="pinned Wails CLI executable")
    parser.add_argument("--skip-build", action="store_true", help="install already-built artifacts")
    args = parser.parse_args(argv)
    try:
        destination = installation_directory(sys.platform, args.install_dir)
        print(f"Updating PairRoom Desktop in {destination}", flush=True)
        print("Quit PairRoom from its tray before updating. No process will be killed.", flush=True)
        if not args.skip_build:
            build(sys.platform, args.wails)
        replace_payload(destination, validate_payload(sys.platform))
        name = "PairRoom.app" if sys.platform == "darwin" else "PairRoom.exe" if sys.platform == "win32" else "PairRoom"
        print(f"Updated: {destination / name}")
        print("Reopen the desktop application to use the new build. User data and login settings were preserved.")
        return 0
    except (OSError, RuntimeError, subprocess.CalledProcessError) as exc:
        print(f"desktop-update: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

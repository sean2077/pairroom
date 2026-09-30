#!/usr/bin/env python3
"""Package only the public landing page and its canonical documentation images."""
import argparse
import shutil
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PUBLIC_FILES = ("index.html", "style.css", "app.js", "zh-CN.json", "favicon.svg")
SCREENS = ("pairroom-native-room", "pairroom-embedded-room", "pairroom-management")


def build(destination: Path) -> None:
    # Never remove an existing directory; a mistaken output path must not destroy source.
    destination.mkdir(parents=True, exist_ok=False)
    for name in PUBLIC_FILES:
        shutil.copyfile(ROOT / "website" / name, destination / name)
    (destination / "images").mkdir()
    for screen in SCREENS:
        for suffix in ("", "-zh"):
            name = screen + suffix + ".png"
            shutil.copyfile(ROOT / "docs" / "images" / name, destination / "images" / name)
    (destination / ".nojekyll").touch()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="New output directory (must not exist)")
    args = parser.parse_args()
    build(args.output.resolve())
    print(f"Website packaged: {args.output}")

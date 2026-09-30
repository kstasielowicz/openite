#!/usr/bin/env python3
"""Embed the UI's interface icons (Lucide, ISC licence, https://lucide.dev) into web/index.html.

Only the icons listed in ICONS are fetched, at a pinned version, and written between the markers
/*ICONS-START*/ and /*ICONS-END*/ as a JS object of inner SVG markup. Nothing is loaded at runtime.
Run:  python tools/fetch_ui_icons.py
"""
import json
import re
import sys
import urllib.request
from pathlib import Path

VERSION = "0.469.0"
CDN = f"https://cdn.jsdelivr.net/npm/lucide-static@{VERSION}/icons"
ROOT = Path(__file__).resolve().parents[1]

# UI name -> lucide file name
ICONS = {
    "apps": "layout-grid", "monitor": "monitor", "server": "server", "activity": "activity", "layers": "layers",
    "terminal": "square-terminal", "sync": "refresh-cw", "clock": "clock", "log": "scroll-text", "settings": "settings",
    "overview": "layout-dashboard", "cpu": "cpu", "disk": "hard-drive", "memory": "memory-stick", "network": "network",
    "info": "info", "search": "search", "x": "x", "check": "check", "chevron-right": "chevron-right",
    "chevron-down": "chevron-down", "chevron-left": "chevron-left", "plus": "plus", "trash": "trash-2", "pause": "pause",
    "play": "play", "download": "download", "arrow-up": "circle-arrow-up", "sun": "sun", "moon": "moon",
    "logout": "log-out", "tag": "tag", "edit": "pencil", "package": "package", "star": "star", "gamepad": "gamepad-2",
    "palette": "palette", "shield": "shield-check", "puzzle": "puzzle", "alert": "triangle-alert",
    "ok": "circle-check", "fail": "circle-x", "loader": "loader-circle", "history": "history", "key": "key-round",
    "copy": "copy", "external": "external-link", "wrench": "wrench", "zap": "zap", "filter": "list-filter",
    "more": "ellipsis", "laptop": "laptop", "globe": "globe", "list": "list-checks", "calendar": "calendar-clock",
    "box": "package-check", "user": "user", "lock": "lock", "code": "code-xml", "home": "house", "cloud": "cloud",
    "wifi": "wifi", "uptime": "timer", "board": "circuit-board", "rocket": "rocket", "minus": "minus",
    "panel": "panel-right-open", "menu": "menu", "undo": "undo-2", "eye": "eye", "book": "book-open",
}


def get(url):
    with urllib.request.urlopen(url, timeout=30) as r:
        return r.read().decode()


def main():
    out, missing = {}, []
    for name, file in sorted(ICONS.items()):
        try:
            svg = get(f"{CDN}/{file}.svg")
        except Exception:  # noqa: BLE001
            missing.append(f"{name} ({file})")
            continue
        inner = re.sub(r"<!--.*?-->", "", svg, flags=re.S)
        inner = re.sub(r"^\s*<svg[^>]*>", "", inner.strip())
        inner = re.sub(r"</svg>\s*$", "", inner).strip()
        out[name] = re.sub(r"\s+", " ", inner)
    block = "/*ICONS-START*/const ICONS = " + json.dumps(out, separators=(",", ":")) + ";/*ICONS-END*/"
    p = ROOT / "web" / "index.html"
    html = p.read_text("utf-8")
    new, n = re.subn(r"/\*ICONS-START\*/.*?/\*ICONS-END\*/", lambda m: block, html, flags=re.S)
    if n != 1:
        sys.exit("markers /*ICONS-START*/ ... /*ICONS-END*/ not found in web/index.html")
    p.write_text(new, "utf-8")
    print(f"{len(out)} icons embedded;", "missing: " + ", ".join(missing) if missing else "none missing")


if __name__ == "__main__":
    main()

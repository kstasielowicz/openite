#!/usr/bin/env python3
"""Fetch app icons from Simple Icons (CC0, https://simpleicons.org) into web/icons/<app key>.svg.

Pinned to one version so the result is reproducible. Each icon gets its brand colour baked in.
Brand logos are trademarks of their owners and are shown only to identify the software.
Apps without a brand icon keep the letter tile in the UI. Run:  python tools/fetch_icons.py
"""
import json
import re
import sys
import urllib.request
from pathlib import Path

VERSION = "16.33.0"
ROOT = Path(__file__).resolve().parents[1]
CDN = f"https://cdn.jsdelivr.net/npm/simple-icons@{VERSION}"

# app key -> candidate Simple Icons slugs (first one that exists wins)
SLUGS = {
    "firefox": ["firefox"], "chrome": ["googlechrome"], "brave": ["brave"], "vivaldi": ["vivaldi"],
    "discord": ["discord"], "telegram": ["telegram"], "signal": ["signal"], "slack": ["slack"], "zoom": ["zoom"],
    "teams": ["microsoftteams"], "vlc": ["vlcmediaplayer"], "spotify": ["spotify"], "obs": ["obsstudio"],
    "audacity": ["audacity"], "handbrake": ["handbrake"], "ffmpeg": ["ffmpeg"], "kodi": ["kodi"],
    "gimp": ["gimp"], "inkscape": ["inkscape"], "krita": ["krita"], "blender": ["blender"], "sharex": ["sharex"],
    "libreoffice": ["libreoffice"], "acrobat-reader": ["adobeacrobatreader"], "sumatrapdf": ["sumatrapdf"],
    "notepadpp": ["notepadplusplus"], "obsidian": ["obsidian"], "git": ["git"], "vscode": ["visualstudiocode"],
    "python": ["python"], "nodejs": ["nodedotjs"], "go": ["go"], "rustup": ["rust"], "docker": ["docker"],
    "powershell": ["powershell"], "windows-terminal": ["windowsterminal"], "github-cli": ["github"],
    "neovim": ["neovim"], "postman": ["postman"], "winscp": ["winscp"], "putty": ["putty"], "7zip": ["7zip"],
    "cpuz": ["cpuz"], "etcher": ["balenaetcher"], "qbittorrent": ["qbittorrent"], "bitwarden": ["bitwarden"],
    "keepassxc": ["keepassxc"], "malwarebytes": ["malwarebytes"], "tailscale": ["tailscale"],
    "wireguard": ["wireguard"], "teamviewer": ["teamviewer"], "anydesk": ["anydesk"], "dropbox": ["dropbox"],
    "google-drive": ["googledrive"], "syncthing": ["syncthing"], "steam": ["steam"], "epic": ["epicgames"],
    "gog": ["gogdotcom"], "intel-dsa": ["intel"], "dell-command-update": ["dell"], "lenovo-system-update": ["lenovo"],
    "webview2": ["microsoftedge"], "java-21": ["openjdk"], "java-17": ["openjdk"],
}
for v in ("8", "9", "10"):
    SLUGS[f"dotnet-desktop-{v}"] = SLUGS[f"dotnet-runtime-{v}"] = SLUGS[f"dotnet-sdk-{v}"] = ["dotnet"]
SLUGS["dotnet-aspnet-8"] = ["dotnet"]


def get(url):
    with urllib.request.urlopen(url, timeout=30) as r:
        return r.read()


def main():
    meta = {i["slug"]: i for i in json.loads(get(f"{CDN}/data/simple-icons.json"))}
    out = ROOT / "web" / "icons"
    out.mkdir(parents=True, exist_ok=True)
    catalog = {a["key"] for a in json.loads((ROOT / "catalog" / "catalog.json").read_text("utf-8"))}
    got, missing = [], []
    for key in sorted(catalog):
        slug = next((s for s in SLUGS.get(key, []) if s in meta), None)
        if not slug:
            missing.append(key)
            continue
        svg = get(f"{CDN}/icons/{slug}.svg").decode()
        svg = re.sub(r"<svg ", f'<svg fill="#{meta[slug]["hex"]}" ', svg, count=1)
        (out / f"{key}.svg").write_text(svg, "utf-8")
        got.append(key)
    print(f"{len(got)} icons written to {out}")
    print("no brand icon (letter tile used):", ", ".join(missing) or "none")


if __name__ == "__main__":
    sys.exit(main())

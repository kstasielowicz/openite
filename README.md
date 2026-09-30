# Openite

Open-source Ninite-style software manager. Pick apps, press Install; keep one PC, a home lab or a whole fleet installed and up to date. It uses your OS's own package manager (winget / Homebrew / apt), so software always comes from official sources. MIT licensed, zero dependencies.

**What you get:** 87 apps with icons · starter presets · runtimes (.NET, Visual C++, DirectX, Java) · driver guidance · silent uninstall · automatic updates · one static binary · optional multi-device server.

## Quick start (Windows)

**Easiest:** download `openite-setup-<version>.exe` from [Releases](../../releases), run it, open **Openite** from the Start menu. No admin rights needed.

**PowerShell one-liner** (installs the tool only; no apps unless you ask):
```powershell
irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1 | iex
openite pick
```
Tool + apps in one go (`-Yes` skips the question; add `-Version v0.4.0` to pin an exact release):
```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1))) -Preset developer -Apps vivaldi,obs -Yes
```
Linux/macOS: `curl -fsSL https://raw.githubusercontent.com/kstasielowicz/openite/main/install.sh | sh -s -- --preset essentials --yes`

> `irm | iex` runs code from the internet, so [read the script](install.ps1) first. It downloads one binary over HTTPS, **verifies its SHA-256** (and GitHub's signed build attestation if you have `gh`), installs per user, and does nothing else. Prefer not to? Download from Releases and verify by hand ([SECURITY.md](SECURITY.md)).

## Use it

| I want to… | Do this |
|---|---|
| Click around | `openite` opens a web page for this PC: pick a starter pack or apps, press **Install**. The bar at the bottom always shows what you picked. |
| Choose in the terminal | `openite pick`: arrow keys, **Space** to tick, **Enter** to install |
| One-liner install | `openite install firefox vlc vivaldi` or `openite install --preset developer` |
| See before doing | add `--dry-run` to anything |
| Update everything | `openite update` |
| Update automatically | `openite schedule daily 03:00` |
| Uninstall without clicking | `openite uninstall qbittorrent` |
| Fix "missing DLL" / game errors | `openite install --preset runtimes` (Visual C++, .NET, DirectX) |
| Get the right drivers | `openite drivers` |

Full guide: **[docs/CLI.md](docs/CLI.md)**.

## Several PCs, a home lab, a fleet

Run the server once, enroll devices with a code, manage everything from one page: tags, profiles, auto-sync ("these machines always have these apps"), scheduled updates, staged rollouts with automatic halt, audit log.
```bash
docker compose up -d        # then open http://<server>:8080 and follow Devices → Get enrollment command
```
- **[docs/SERVER.md](docs/SERVER.md)**: setup, HTTPS, adding devices, reusable keys, running multiple servers/sites, backups, troubleshooting
- **[docs/AUTO-UPDATES.md](docs/AUTO-UPDATES.md)**: nightly updates for one PC or a fleet, auto-sync, keeping Openite itself current

## What's in the box
- `cmd/openite/`: **one static Go binary**: CLI, local web UI (loopback only) and device agent.
- `catalog/`: apps and presets. `web/`: single-file UI and icons.
- `server/`: multi-device server (Python stdlib + SQLite). `Dockerfile`, `docker-compose.yml`, `deploy/`.
- `install.ps1`, `install.sh`, `installer/openite.iss`, `build.sh`: installers and reproducible builds with `SHA256SUMS`.

## Safety
See [SECURITY.md](SECURITY.md). Highlights: only package ids from official repositories (validated in CI, server and agent); argv-only command execution; no telemetry; drivers are never installed raw (vendor tools or official pages only); reproducible builds with checksums and build attestations; per-user installs without admin; draft-only release workflow; code-owner review on the catalog and installers.
**Not yet:** code-signed Windows binaries (SmartScreen may warn), 2FA/SSO/roles and login rate-limiting on the fleet server.

## Develop
```bash
go vet ./... && go test ./...          # catalog rules, parsers, uninstall logic, URL safety
python tests/test_e2e.py               # fleet server with a fake agent
python tests/test_go_agent.py          # real Go binary vs real server (dry-run: nothing installed)
sh build.sh                            # dist/openite-<os>-<arch> + SHA256SUMS
python tools/fetch_icons.py            # refresh app icons (Simple Icons, CC0, pinned version)
```
`OPENITE_DRY_RUN=1` (or `--dry-run`) logs commands instead of running them. To add an app see [CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap
Code signing (SignPath), RBAC + SSO, rate limiting, internal package mirror, dnf/zypper/choco backends, maintenance windows per device, Prometheus metrics, Postgres, CVE/compliance reports, per-app uninstall rules.

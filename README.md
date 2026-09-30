# Openite

Open-source Ninite-style software manager. Pick apps, press Install; keep one PC, a home lab or a fleet up to date. Uses your OS's own package manager (winget / Homebrew / apt), so software always comes from official sources. MIT licensed, zero dependencies.

## Quick start (Windows)

**Easiest:** download `openite-setup-<version>.exe` from [Releases](../../releases), run it, click the Start-menu shortcut "Openite". No admin rights needed.

**PowerShell one-liner** (installs the tool only; it installs no apps unless you ask):
```powershell
irm https://raw.githubusercontent.com/openite/openite/main/install.ps1 | iex
openite pick
```
Install tool + apps in one go (pin a version in real use; `-Yes` skips the confirmation):
```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/openite/openite/main/install.ps1))) -Version v0.3.0 -Preset developer -Apps vivaldi,obs -Yes
```
Linux/macOS: `curl -fsSL https://raw.githubusercontent.com/openite/openite/main/install.sh | sh -s -- --preset essentials --yes`

> `irm | iex` runs code from the internet: [read the script](install.ps1) first (it's ~80 lines). It downloads one binary over HTTPS, **verifies its SHA-256** (and GitHub's signed build attestation if you have `gh`), installs per-user, nothing else. Not comfortable with that? Download from Releases and verify by hand ([SECURITY.md](SECURITY.md)).
>
> *Replace `openite/openite` in the URLs with the real GitHub repository path once published.*

## Using it

```text
openite                         open the web UI for this PC
openite pick                    choose presets/apps in the terminal, then install
openite install firefox vlc     install by name (fuzzy: "vsc", "chrome")
openite install --preset developer --dry-run     preview without changing anything
openite update                  update everything   |   openite update git vscode
openite uninstall vlc
openite list | search <text> | presets | status
```
Presets: Everyday essentials, Developer, Gaming, Creator, Home lab, Privacy & security. `openite status` shows what's installed/outdated, including apps installed by other means (Vivaldi, Chrome, …), detected by name as well as package id.

Every command previews the plan and asks first (and cancels when there's no terminal to answer). `-y` skips the question, `--dry-run` changes nothing.

## Several PCs / home lab / fleets

Run the server on an always-on box (NAS, Pi, VM):
```bash
docker compose up -d                 # or: python server/server.py --host 0.0.0.0
```
Open `http://<server>:8080`, create your account, **Devices → Get enrollment command**, then on each device:
```bash
openite enroll --server http://<server>:8080 --code ABCD1234
openite install-service              # start at boot/login (Scheduled Task / systemd / launchd)
```
Select devices, pick apps, Install. Turn on **Advanced** for:

| Feature | What it does |
|---|---|
| **Tags** | `#kitchen`, `#gaming`, `#prod`: select or target groups in one click. |
| **Auto-sync** | "Devices tagged `#prod` should have profile *base* (kept updated)". Missing apps are installed automatically, incl. on machines that join later. Shows ✔ In sync / Out of sync. Identical failed attempts aren't retried for 6 h. |
| **Staged rollouts** | 4+ devices: canary (~10%) → ~40% → rest. If >20% of finished devices fail, the rest are halted, not started. |
| **Reusable enrollment keys** | One key per golden image / cloud-init / Ansible run; devices join with preset tags. |
| **Profiles & setup scripts** | App bundles plus PowerShell/bash/cmd scripts applied on demand. |
| **Audit log** | Logins, jobs, tag/rule/script changes, enrollments, halted rollouts. |
| **Multi-instance** | Several agents per machine: `--config a.json`, `install-service --name openite-b`. |

Agents only make outbound requests (polling with jitter and back-off). Plain `http://` is allowed on LAN/loopback addresses only; public servers need `https://`.

## What's in the box
- `cmd/openite/`: **one static Go binary**: CLI, local web UI (`openite ui`, loopback only) and fleet agent.
- `catalog/`: apps and presets (shared by everything). `web/`: the single-file UI.
- `server/`: fleet server, Python stdlib + SQLite. `Dockerfile`, `docker-compose.yml`, `deploy/`: home-lab deployment.
- `install.ps1`, `install.sh`, `installer/openite.iss`: bootstrap scripts and the Windows installer. `build.sh`: reproducible cross-compile + `SHA256SUMS`.

## Safety & open-source hygiene
See [SECURITY.md](SECURITY.md) for the threat model. Highlights: only package ids from the official repos (validated in CI, server and agent); argv-only command execution (no shell strings); no telemetry; reproducible builds published with checksums and build attestations; per-user installs without admin; draft-only release workflow; code-owner review on catalog/installers/CI. **Not yet:** code-signed Windows binaries (SmartScreen may warn), 2FA/SSO/RBAC, login rate-limiting on the fleet server.

## Develop
```bash
go vet ./... && go test ./...          # Go (catalog rules, parser, URL safety)
python tests/test_e2e.py               # fleet server with a fake agent
python tests/test_go_agent.py          # real Go binary vs real server (dry-run: nothing is installed)
./build.sh                             # dist/openite-<os>-<arch> + SHA256SUMS
iscc /DVersion=0.3.0 installer\openite.iss   # Windows installer (Inno Setup 6)
```
`OPENITE_DRY_RUN=1` (or `--dry-run`) makes everything log commands instead of running them. See [CONTRIBUTING.md](CONTRIBUTING.md) to add apps.

## Roadmap
Code signing (SignPath), RBAC + SSO, rate limiting, internal package mirror, dnf/zypper/choco backends, maintenance windows, Prometheus metrics, Postgres, Windows service wrapper, CVE/compliance reports.

# Security

## Reporting a vulnerability
Please **don't open a public issue**. Use GitHub's private vulnerability reporting ("Security" tab → "Report a vulnerability") or email the maintainers listed there. We aim to acknowledge within 3 days.

## What Openite does (and doesn't) do
| Mode | Runs | Never does |
|---|---|---|
| `openite install/update/pick/ui` (CLI + local UI) | Your OS package manager (`winget`, `brew`, `apt-get`) with package ids from the built-in catalog, as an argv list (no shell). | Download or execute anything itself; send telemetry; contact any server. The local UI listens on `127.0.0.1` only and is protected against DNS-rebinding and cross-site requests. |
| Fleet agent (`enroll/run`) | The same package commands, plus **setup scripts your own server sends**. | Talk to anything except the server you enrolled with. |
| Fleet server | Stores accounts, devices, scripts, jobs (SQLite). | Phone home. |

## Threat model and mitigations
- **Malicious/typo'd catalog entry or custom app.** Package ids must match `^[A-Za-z0-9][A-Za-z0-9._+@/-]*$`, are checked by CI (`catalog_test.go`), by the server on input, and again by the agent right before running (a server can't make an agent run `--some-flag`). Catalog changes require code-owner review; ids must exist in the official winget/Homebrew/Debian repositories. No URLs, no custom sources.
- **Tampered download.** Release binaries are built by GitHub Actions from the tagged commit with a reproducible build (`./build.sh`; same source gives the same bytes), published with `SHA256SUMS` and a build-provenance attestation. `install.ps1` / `install.sh` verify the checksum (and the attestation when `gh` is available) before installing, and install per-user without admin rights.
- **Server compromise / malicious scripts.** Scripts on the fleet server are remote code execution on every enrolled device by design. Keep the server private or behind HTTPS + your own auth; use tags and staged rollouts; review the audit log. The agent refuses plain `http://` to public addresses (LAN/loopback are fine) unless `--allow-insecure`.
- **Stolen device token.** Tokens are per device, stored `0600`, and can be revoked by removing the device in the UI.
- **Headless uninstall.** For apps installed outside winget, Openite runs the uninstall command Windows itself stored in the registry, adding only known silent switches (MSI `/qn`, Inno `/VERYSILENT`, NSIS `/S`). Nothing from the network is ever executed, and each step has a 10-minute limit.
- **Drivers.** Openite never downloads or installs raw driver packages. It installs vendors' own update tools from winget or opens fixed official vendor URLs.
- **Icons.** Brand icons are static SVGs from Simple Icons (CC0) shown only inside `<img>` tags, so they can't run script; fetched by `tools/fetch_icons.py` at a pinned version.
- **Accidental installs.** Commands preview the plan and ask first; with no terminal input they cancel instead of assuming yes. `--dry-run` shows everything without changing anything.

## Known gaps (alpha)
No login rate-limiting, 2FA/SSO or roles on the fleet server; no built-in TLS (use a reverse proxy); Windows binaries aren't code-signed yet (SmartScreen may warn); the agent runs with the privileges of the account that started it.

## Verifying a release
```powershell
Get-FileHash .\openite-windows-amd64.exe -Algorithm SHA256   # compare with SHA256SUMS
gh attestation verify .\openite-windows-amd64.exe --repo kstasielowicz/openite
```

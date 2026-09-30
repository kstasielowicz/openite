# Server mode

Server mode lets you manage **many devices from one web page**: your PCs, a home lab, VMs or a whole fleet. If you only want to set up the PC in front of you, you don't need any of this: run `openite` (see [CLI.md](CLI.md)).

```
   You (browser)                                  Each device
        │                                      ┌───────────────┐
        ▼                                      │ openite agent │── winget / brew / apt
  ┌────────────┐   devices ask "any work?"     └──────▲────────┘
  │  Openite   │◄──────────────────────────────────────┘   (outbound HTTPS/HTTP only,
  │  server    │   jobs, results, inventory                  nothing to open on devices)
  └────────────┘
   SQLite file
```
- The **server** stores accounts, devices, tags, profiles, scripts, schedules, jobs and the audit log in one SQLite file.
- The **agent** is the same `openite` binary you use on the command line, run as `openite run`. It checks in every ~15 s, runs the jobs it's given (install / update / uninstall / scripts) and reports the result plus what's installed.
- Devices **pull** work. You never need inbound ports or remote-access tools on managed machines.

## 1. Start a server

Pick one. You need Python 3.9+ or Docker; nothing else.

**Docker (recommended for home labs)**
```bash
git clone https://github.com/kstasielowicz/openite && cd openite
docker compose up -d                    # data lives in the "openite-data" volume
```
**Plain Python**
```bash
python server/server.py --host 0.0.0.0 --port 8080 --db /var/lib/openite/openite.db
```
**systemd service:** copy the repo to `/opt/openite` and use [deploy/openite-server.service](../deploy/openite-server.service).

Open `http://<server>:8080`, **Create account**, and you're in. Then stop strangers from registering:
```bash
# docker-compose.yml: uncomment  OPENITE_NO_REGISTRATION: "1"   then  docker compose up -d
# plain Python:               add --no-registration
```

### Put HTTPS in front (do this before leaving your LAN)
The server speaks plain HTTP. Anything beyond a trusted LAN needs a reverse proxy with TLS. Agents refuse plain `http://` to public addresses on their own.

Caddy (automatic certificates), `Caddyfile`:
```
openite.example.com {
    reverse_proxy 127.0.0.1:8080
}
```
Then enroll agents with `--server https://openite.example.com`.

## 2. Add devices

In the UI: **Devices → Get enrollment command**. On the device (Windows: open PowerShell; Linux: a shell):
```powershell
openite enroll --server http://192.168.1.10:8080 --code 1A2B3C4D     # code is single-use, valid 15 min
openite install-service                                               # start automatically from now on
```
- **Windows:** `install-service` creates a Scheduled Task that runs at logon with elevated rights. Run it from an *Administrator* terminal. `--system` runs it as SYSTEM at boot, but winget is unreliable as SYSTEM, so test first.
- **Linux:** run with `sudo` (creates a systemd unit). **macOS:** creates a LaunchAgent.
- Check it worked: the device appears in the UI with a green dot within seconds. To run in the foreground instead: `openite run`.

### Many machines at once (golden images, cloud-init, Ansible)
Turn on **Advanced → Devices → Create reusable key** (optionally with tags like `prod, dc:eu`). Every machine that enrolls with that key joins with those tags, no per-machine code:
```bash
openite enroll --server https://openite.example.com --code <REUSABLE_KEY> --name "$(hostname)"
```
A reusable key doesn't expire. Treat it like a password.

## 3. Organize and control

| Want to… | Use |
|---|---|
| Act on a group | **Tags** (`#kitchen`, `#prod`). Devices tab → Tags. "Select" chips pick all devices with a tag. |
| Install the same set everywhere | **Profiles** (Install tab → *Save as profile*), then *Apply*. |
| Keep machines identical automatically | **Auto-sync** (Advanced): "devices tagged X should have profile Y". See [AUTO-UPDATES.md](AUTO-UPDATES.md). |
| Update everything on a schedule | **Scheduled updates** (Advanced → Auto-sync tab). |
| Run a setup script | **Scripts** (PowerShell / bash / cmd), attach to a profile or run on selected devices. |
| Roll out safely | Automatic for 4+ devices: canary ~10% → ~40% → rest, **halts** if >20% of finished devices fail. |
| See what happened | **Activity** (per-device logs) and **Audit log**. |

> **Scripts are remote code execution** on every device they run on. Only add scripts you trust, keep the server private, and use tags + staged rollouts.

## 4. Running more than one server

Openite is built so **one server manages many devices**. A single small box comfortably handles a home lab or a few hundred machines (single process, SQLite). Use several servers when you want hard separation:

| Situation | Setup |
|---|---|
| **Separate environments or sites** (prod vs lab, office A vs B, customer 1 vs 2) | Run one server per environment. Each device enrolls with exactly one. They share nothing, so a mistake in lab can't touch prod. |
| **Separate people/teams, one server** | Each person creates their own **account** on the same server. Accounts are fully isolated (devices, scripts, jobs, audit). |
| **One machine managed by two servers** (e.g. a personal and a work server) | Run **two agents** with their own config and service name:<br>`openite enroll --server https://a --code X --config C:\openite\a.json`<br>`openite enroll --server https://b --code Y --config C:\openite\b.json`<br>`openite install-service --config C:\openite\a.json --name openite-a`<br>`openite install-service --config C:\openite\b.json --name openite-b`<br>Avoid rules from both servers touching the same apps; they would fight. |
| **Move a device to another server** | Remove it in the old UI, stop its service, `openite enroll` against the new server. |

There is no federation or cross-server dashboard yet. If you need one view over many sites, run a central server and enroll everything there, using tags (`site:berlin`, `env:prod`) to keep them apart.

## 5. Operating the server

- **Back up:** stop the server (or use `sqlite3 openite.db ".backup openite-backup.db"`) and copy the DB file. It holds everything.
- **Upgrade:** `git pull && docker compose up -d --build` (or restart the Python process). The database upgrades itself.
- **Update agents:** re-run the installer (`irm … | iex`, or replace `openite.exe`) and restart the service. Agents and server stay compatible across 0.3.x.
- **Time:** scheduled updates use the **server's local time**; set `TZ` (Docker) or the system clock accordingly.
- **Ports:** only the server port (default 8080, or 443 behind your proxy) needs to be reachable from devices.

## 6. Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Device never appears | Wrong `--server` URL or firewall; the code expired (15 min) or was already used. Generate a new one. |
| Device shows offline | Its agent isn't running. Start `openite run` or the service; check `openite install-service` ran elevated. |
| `refusing plain http://` on enroll | You pointed at a public address without TLS. Use `https://` (recommended) or `--allow-insecure`. |
| Job stays "waiting" | The device is off or its agent stopped; it runs when the agent returns. Cancel it in Activity if you no longer want it. |
| Rollout shows "not started" | The canary group failed and the rollout halted on purpose. Read the failed job log, fix, re-run. |
| Installs fail with access denied | The agent isn't elevated. On Windows install the service from an Administrator terminal. |
| An app isn't detected as installed | Add a `"match"` alias in `catalog/catalog.json` (see [CONTRIBUTING.md](../CONTRIBUTING.md)). |

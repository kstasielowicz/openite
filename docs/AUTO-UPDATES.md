# Automatic updates

There are three separate things people mean by "automatic updates". Openite covers each one.

| You want… | Use | Where it runs |
|---|---|---|
| This one PC updates itself every night | `openite schedule` | your PC's own scheduler |
| A group of devices updates everything on a schedule | **Scheduled updates** | server → agents |
| Machines always have *these* apps (and stay current) | **Auto-sync** | server → agents |
| Openite itself stays up to date | re-run the installer | manually (see the end) |

## 1. Just this PC

```powershell
openite schedule daily 03:00          # every day at 03:00
openite schedule weekly sun 03:00     # every Sunday
openite schedule status
openite schedule off
```
It runs `openite update -y`, which updates **everything** your package manager knows about (winget / Homebrew / apt), using your normal user rights.

- **Windows:** creates a Scheduled Task called *Openite Auto Update*. It runs while you're logged in, and does not catch up if the PC was off at 03:00. Choose a time the PC is usually on, like `12:30`. Creating the task may need an **Administrator** terminal (it uses the highest run level so installers can elevate).
- **Linux / macOS:** adds one line to your crontab (marked `# openite-auto-update`; `schedule off` removes only that line). System-wide apt upgrades need root, so for apt prefer root's crontab (`sudo openite schedule daily 03:00`).
- Try it safely first: `openite update --dry-run`.

## 2. Scheduled updates for a group of devices

Needs the server ([SERVER.md](SERVER.md)). **Advanced → Auto-sync tab → Scheduled updates**:

> Update **#home** at **03:00** on **sun** ☑ staged rollout

- At the set time (server clock) the server queues an **"update everything"** job on every device with that tag (or *all devices*). No days ticked = every day.
- Devices that are **off** pick the job up when they come back online, so nothing is skipped, but it may run at an odd hour. Cancel stale jobs in **Activity** if that matters.
- If the server was down at the scheduled minute it still fires up to 6 hours late, once per day.
- **Held apps are skipped.** Hold an app (`openite hold <app>`, or the **Hold** button) and neither "update everything" nor a schedule touches it until you release it.
- **Maintenance windows:** a device with a window (Device → Settings) only starts scheduled and auto-sync jobs inside it.
- Groups of **4+ devices** with *staged rollout* go canary → 40% → rest, and stop if more than 20% of finished devices fail. Put a low-risk machine first by tagging it.
- Everything is logged: **Activity** (per-device output) and the **Audit log** (`schedule.add`, `rollout.halted`, …).

## 3. Auto-sync: "these devices should have these apps"

Different from a schedule: Auto-sync is about **which apps should exist**, and it reacts to changes immediately.

1. Install tab → pick apps → **Save as profile**.
2. **Advanced → Auto-sync → Add rule**: *Devices in `#gaming` should have profile `Gaming PC`* and optionally ☑ *also keep them updated*.

Effect:
- Missing apps are installed on every matching device, **including new devices that join later** (enroll with a reusable key that carries the tag and they set themselves up).
- With *keep updated*, apps from that profile are upgraded whenever a newer version appears; other apps are left alone.
- **Pinned versions:** give an app in the profile an exact version. A device with an older one is upgraded to exactly that version, a device with the right one is left alone, and a device with a *newer* one is never downgraded automatically (it shows "Out of sync: Git 2.44.0 (has 2.50.0)" so you can decide).
- The Devices tab shows **✔ In sync** or **Out of sync, missing: …** per device.
- If an identical attempt already ran in the last 6 hours the server doesn't retry it, so a broken package can't loop forever.

Use both together for a hands-off fleet: Auto-sync defines the software, a nightly schedule keeps everything else patched.

## Good defaults

| Setup | Suggestion |
|---|---|
| Family PCs | `openite schedule weekly sun 12:30` on each. |
| Home lab (5-20 machines) | One server, tag by role, schedule `#all` nightly, Auto-sync a base profile. |
| Production servers | Schedule in a **maintenance window** per tag; staged rollout on; keep one canary machine per tag. Review the Audit log. |

## Keeping Openite itself current

- **CLI / agents:** re-run the installer; it replaces the binary in place (idempotent):
  ```powershell
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1)))
  ```
  Agents started as a service need a restart (`schtasks /End` + `/Run` on Windows, `systemctl restart openite-agent` on Linux).
- **Server:** `git pull && docker compose up -d --build`. The database migrates itself.
- There is no silent self-update on purpose: an updater that downloads and runs code by itself is exactly the kind of thing an open-source tool should not do unasked.

"""Server tests. A small fake agent speaks the real HTTP protocol, so no package manager is touched.
The Go binary is tested separately: `go test ./...` and tests/test_go_agent.py."""
import json
import os
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "server"))
import server  # noqa: E402


class FakeAgent:
    """Minimal device agent: check in with an inventory, poll, run jobs (optionally failing) and report."""

    def __init__(self, base, token, fail=False):
        self.base, self.token, self.fail = base, token, fail

    def call(self, method, path, body=None):
        req = urllib.request.Request(self.base + path, method=method, data=json.dumps(body or {}).encode())
        req.add_header("Authorization", "Bearer " + self.token)
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())

    def checkin(self, installed=None, names=None, upgradable=None, **extra):
        return self.call("POST", "/api/agent/checkin", {"installed": installed or {}, "installed_names": names or {},
                                                        "upgradable": upgradable or {}, "manager": "winget", **extra})

    def progress(self, jid, steps, log):
        return self.call("POST", f"/api/agent/jobs/{jid}/progress", {"steps": steps, "log": log})

    def process_once(self):
        job = self.call("GET", "/api/agent/poll")["job"]
        if not job:
            return None
        log = "\n".join(f"=== {s['type']} {s.get('app') or s.get('name') or ''}" for s in job["steps"])
        self.call("POST", f"/api/agent/jobs/{job['id']}/result", {"status": "failed" if self.fail else "done", "log": log})
        return job


class E2E(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.srv = server.make_server("127.0.0.1", 0, os.path.join(cls.tmp.name, "t.db"))
        cls.base = f"http://127.0.0.1:{cls.srv.server_address[1]}"
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()
        try:
            cls.tmp.cleanup()
        except OSError:
            pass  # sqlite WAL files may linger on Windows

    def call(self, method, path, body=None, token=None):
        req = urllib.request.Request(self.base + path, method=method, data=json.dumps(body or {}).encode())
        if token:
            req.add_header("Authorization", "Bearer " + token)
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def register(self, email):
        return self.call("POST", "/api/register", {"email": email, "password": "longenough"})[1]["token"]

    def enroll(self, tok, name, tags=None, key=None, fail=False):
        code = key or self.call("POST", "/api/enroll-code", {"tags": tags or []}, tok)[1]["code"]
        s, dev = self.call("POST", "/api/agent/enroll", {"code": code, "name": name, "manager": "winget"})
        self.assertEqual(s, 200)
        return FakeAgent(self.base, dev["token"], fail), dev["device_id"]

    # ------------------------------------------------------------------ tests
    def test_accounts_devices_jobs_and_isolation(self):
        s, r = self.call("POST", "/api/register", {"email": "a@b.co", "password": "longenough"})
        self.assertEqual(s, 200)
        tok = r["token"]
        self.assertEqual(self.call("POST", "/api/register", {"email": "a@b.co", "password": "longenough"})[0], 409)
        self.assertEqual(self.call("POST", "/api/login", {"email": "a@b.co", "password": "wrong-one"})[0], 401)
        self.assertEqual(self.call("GET", "/api/devices")[0], 401)

        a1, d1 = self.enroll(tok, "pc-1")
        a2, d2 = self.enroll(tok, "pc-2")
        code = self.call("POST", "/api/enroll-code", token=tok)[1]["code"]
        self.enroll(tok, "pc-3", key=code)
        self.assertEqual(self.call("POST", "/api/agent/enroll", {"code": code})[0], 403)  # single use

        _, sc = self.call("POST", "/api/scripts", {"name": "hello", "shell": "bash", "body": "echo hi"}, tok)
        self.call("POST", "/api/apps", {"key": "myapp", "name": "My App", "winget": "Me.MyApp"}, tok)
        _, pr = self.call("POST", "/api/profiles", {"name": "dev", "apps": ["git", "vscode", "myapp"], "script_ids": [sc["id"]]}, tok)
        s, r = self.call("POST", "/api/jobs", {"device_ids": [d1, d2], "steps": [{"type": "profile", "profile_id": pr["id"]}]}, tok)
        self.assertEqual((s, len(r["job_ids"])), (200, 2))
        self.assertEqual(self.call("POST", "/api/jobs", {"device_ids": [d1], "steps": [{"type": "install", "apps": ["nope"]}]}, tok)[0], 400)

        job = a1.process_once()
        self.assertIsNotNone(job)
        self.assertIsNone(a1.process_once())  # queue drained
        names = [s.get("app") or s.get("name") for s in job["steps"]]
        self.assertEqual(names, ["Git", "Visual Studio Code", "My App", "hello"])
        _, jobs = self.call("GET", "/api/jobs", token=tok)
        self.assertEqual(sorted(j["status"] for j in jobs), ["done", "queued"])  # pc-2 untouched

        other = self.register("x@y.co")  # accounts are isolated
        self.assertEqual(self.call("GET", "/api/devices", token=other)[1], [])
        self.assertEqual(self.call("GET", "/api/jobs", token=other)[1], [])

    def test_custom_app_ids_are_validated(self):
        tok = self.register("inj@b.co")
        for bad in ("--uninstall-all", "x; rm -rf /", "a b", "-h"):
            self.assertEqual(self.call("POST", "/api/apps", {"key": "evil", "name": "Evil", "winget": bad}, tok)[0], 400, bad)

    def test_detection_by_display_name(self):
        """Apps installed outside winget (e.g. Vivaldi) show up only as ARP ids; detect them by name."""
        tok = self.register("detect@b.co")
        ag, did = self.enroll(tok, "pc")
        ag.checkin(installed={"ARP\\User\\X64\\Vivaldi": "8.2", "GitHub.GitHubDesktop": "3"},
                   names={"Vivaldi": "8.2", "GitHub Desktop": "3", "Python 3.12.10 (64-bit)": "3.12.10", "Git Extensions": "5"})
        _, devs = self.call("GET", "/api/devices", token=tok)
        have = set(devs[0]["have"])
        self.assertIn("vivaldi", have)
        self.assertIn("python", have)
        self.assertNotIn("git", have)  # neither "GitHub Desktop" nor "Git Extensions" is Git

    def test_name_matching_rules(self):
        m = server.name_matches
        self.assertTrue(m("Python 3.12.10 (64-bit)", "Python 3"))
        self.assertTrue(m("7-Zip 26.03 (x64)", "7-Zip"))
        self.assertTrue(m("Git", "Git"))
        self.assertFalse(m("GitHub Desktop", "Git"))
        self.assertFalse(m("Git Extensions", "Git"))
        self.assertFalse(m("Gopher", "Go"))

    def test_reusable_key_tags_and_desired_state(self):
        tok = self.register("fleet@b.co")
        _, k = self.call("POST", "/api/enroll-code", {"reusable": True, "tags": ["prod", "dc:eu"]}, tok)
        (c1, id1), (c2, id2) = self.enroll(tok, "vm-1", key=k["code"]), self.enroll(tok, "vm-2", key=k["code"])  # key works twice
        _, devs = self.call("GET", "/api/devices", token=tok)
        self.assertEqual(devs[0]["tags"], ["prod", "dc:eu"])
        self.assertEqual(self.call("PUT", f"/api/devices/{devs[0]['id']}", {"tags": ["bad tag!"]}, tok)[0], 400)

        _, pr = self.call("POST", "/api/profiles", {"name": "base", "apps": ["git", "7zip"]}, tok)
        self.assertEqual(self.call("POST", "/api/assignments", {"tag": "prod", "profile_id": pr["id"]}, tok)[0], 200)

        def auto_jobs():
            return [j for j in self.call("GET", "/api/jobs", token=tok)[1] if j["source"] == "reconcile" and j["device_id"] == id1]
        c1.checkin()  # empty inventory -> server queues an auto-sync job
        self.assertEqual(len(auto_jobs()), 1)
        c1.checkin()  # must not queue a duplicate
        self.assertEqual(len(auto_jobs()), 1)
        self.assertTrue(c1.process_once())
        c1.checkin(installed={"Git.Git": "2"}, names={"7-Zip 26.03 (x64)": "26.03"})  # now in sync
        _, devs = self.call("GET", "/api/devices", token=tok)
        d = [x for x in devs if x["id"] == id1][0]
        self.assertEqual((d["managed"], d["missing"]), (True, []))
        _, r = self.call("POST", "/api/jobs", {"tags": ["prod"], "steps": [{"type": "upgrade", "apps": "all"}]}, tok)
        self.assertEqual(len(r["job_ids"]), 2)

    def test_staged_rollout_halts_on_canary_failure(self):
        tok = self.register("roll@b.co")
        agents = [self.enroll(tok, f"n{i}", fail=True) for i in range(5)]
        _, sc = self.call("POST", "/api/scripts", {"name": "boom", "shell": "bash", "body": "exit 1"}, tok)
        ids = [d for _, d in agents]
        s, r = self.call("POST", "/api/jobs", {"device_ids": ids, "steps": [{"type": "script", "script_id": sc["id"]}], "rollout": True}, tok)
        self.assertEqual(r["batches"], [1, 2, 2])
        ran = [a.process_once() is not None for a, _ in agents]
        self.assertEqual(sum(ran), 1)  # only the canary was released
        _, jobs = self.call("GET", "/api/jobs", token=tok)
        self.assertEqual(sorted(j["status"] for j in jobs if j["rollout_id"] == r["rollout_id"]),
                         ["failed", "halted", "halted", "halted", "halted"])
        self.assertFalse(any(a.process_once() for a, _ in agents))
        self.assertTrue(any(a["action"] == "rollout.halted" for a in self.call("GET", "/api/audit", token=tok)[1]))

    def test_scheduled_updates(self):
        import datetime
        tok = self.register("sched@b.co")
        ag, did = self.enroll(tok, "nightly", tags=["nas"])
        self.assertEqual(self.call("POST", "/api/schedules", {"tag": "nas", "time": "25:00"}, tok)[0], 400)
        self.assertEqual(self.call("POST", "/api/schedules", {"tag": "nas", "time": "03:00", "days": ["funday"]}, tok)[0], 400)
        s, r = self.call("POST", "/api/schedules", {"tag": "nas", "time": "03:00", "days": ["sun"]}, tok)
        self.assertEqual(s, 200)
        self.assertEqual(self.call("GET", "/api/schedules", token=tok)[1][0]["days"], ["sun"])
        sunday = datetime.datetime(2026, 10, 4, 3, 5)  # a Sunday
        self.assertEqual(server.run_due_schedules(datetime.datetime(2026, 10, 3, 3, 5)), 0)   # Saturday: wrong day
        self.assertEqual(server.run_due_schedules(datetime.datetime(2026, 10, 4, 2, 59)), 0)  # too early
        self.assertEqual(server.run_due_schedules(sunday), 1)                                # due
        self.assertEqual(server.run_due_schedules(sunday), 0)                                # only once per day
        job = ag.process_once()
        self.assertEqual(job["steps"], [{"type": "upgrade", "all": True}])
        _, jobs = self.call("GET", "/api/jobs", token=tok)
        self.assertEqual(jobs[0]["title"], "Scheduled update")
        self.assertTrue(any(a["action"] == "schedule.add" for a in self.call("GET", "/api/audit", token=tok)[1]))

    def test_runtime_catalog_detection(self):
        tok = self.register("rt@b.co")
        ag, did = self.enroll(tok, "gamer")
        ag.checkin(names={"Microsoft Visual C++ 2015-2022 Redistributable (x64) - 14.38.33135": "14.38",
                          "Microsoft Windows Desktop Runtime - 8.0.10 (x64)": "8.0.10",
                          "Microsoft Visual C++ 2010  x64 Redistributable - 10.0.40219": "10.0"})
        have = set(self.call("GET", "/api/devices", token=tok)[1][0]["have"])
        self.assertTrue({"vcredist-2015-x64", "dotnet-desktop-8", "vcredist-2010-x64"} <= have, have)
        self.assertNotIn("vcredist-2015-x86", have)
        self.assertNotIn("dotnet-desktop-9", have)

    def test_uninstall_steps_carry_aliases_for_headless_removal(self):
        tok = self.register("unin@b.co")
        ag, did = self.enroll(tok, "pc")
        self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [{"type": "uninstall", "apps": ["qbittorrent"]}]}, tok)
        step = ag.process_once()["steps"][0]
        self.assertIn("qBittorrent", step["aliases"])

    def test_icons_are_served(self):
        with urllib.request.urlopen(self.base + "/icons/firefox.svg") as r:
            self.assertEqual(r.headers["Content-Type"], "image/svg+xml")
            self.assertIn(b"<svg", r.read())
        with self.assertRaises(urllib.error.HTTPError):
            urllib.request.urlopen(self.base + "/icons/..%2f..%2fserver.py")


    def test_version_and_hold_steps(self):
        tok = self.register("ver@b.co")
        ag, did = self.enroll(tok, "pc")
        s, _ = self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [
            {"type": "install", "apps": ["git"], "versions": {"git": "2.44.0"}}, {"type": "pin", "apps": ["vlc"]}]}, tok)
        self.assertEqual(s, 200)
        steps = ag.process_once()["steps"]
        self.assertEqual((steps[0]["type"], steps[0]["pkg"], steps[0]["version"]), ("install", "Git.Git", "2.44.0"))
        self.assertEqual((steps[1]["type"], steps[1]["pkg"]), ("pin", "VideoLAN.VLC"))
        for bad in ("--force", "1 2", "a;b"):
            self.assertEqual(self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [
                {"type": "install", "apps": ["git"], "versions": {"git": bad}}]}, tok)[0], 400, bad)

    def test_profile_version_pins_drive_auto_sync(self):
        tok = self.register("pin@b.co")
        ag, did = self.enroll(tok, "pc")
        _, pr = self.call("POST", "/api/profiles", {"name": "pinned", "apps": [{"key": "git", "version": "2.44.0"}, "vlc"]}, tok)
        self.assertEqual(self.call("GET", "/api/profiles", token=tok)[1][0]["apps"][1], {"key": "vlc", "version": ""})
        ag.checkin(installed={"Git.Git": "2.40.0", "VideoLAN.VLC": "3.0"})     # the device reports what it has first
        self.call("POST", "/api/assignments", {"tag": "all", "profile_id": pr["id"]}, tok)  # git older than the pin -> upgrade to exactly it
        step = ag.process_once()["steps"]
        self.assertEqual([(x["type"], x["app"], x.get("version")) for x in step], [("upgrade", "Git", "2.44.0")])
        ag.checkin(installed={"Git.Git": "2.44.0", "VideoLAN.VLC": "3.0"})     # exactly the pin -> in sync, nothing queued
        self.assertIsNone(ag.process_once())
        ag.checkin(installed={"Git.Git": "2.50.0", "VideoLAN.VLC": "3.0"})     # newer than pin: never auto-downgrade, but report drift
        self.assertIsNone(ag.process_once())
        d = self.call("GET", "/api/devices", token=tok)[1][0]
        self.assertEqual(d["missing"], ["Git 2.44.0 (has 2.50.0)"])
        self.assertEqual(d["versions"]["git"], "2.50.0")

    def test_held_and_available_versions_are_reported(self):
        tok = self.register("held@b.co")
        ag, did = self.enroll(tok, "pc")
        ag.checkin(installed={"Git.Git": "2.44.0"}, upgradable={"Git.Git": "2.45.2"}, pinned={"Git.Git": "2.44.0"})
        d = self.call("GET", "/api/devices", token=tok)[1][0]
        self.assertEqual((d["held"], d["outdated"], d["available"]["git"]), (["git"], ["git"], "2.45.2"))

    def test_maintenance_window_defers_automatic_jobs_only(self):
        import datetime
        self.assertTrue(server.in_window({}))
        w = {"maintenance": {"start": "22:00", "end": "05:00"}}
        self.assertTrue(server.in_window(w, datetime.datetime(2026, 1, 1, 23, 30)))
        self.assertTrue(server.in_window(w, datetime.datetime(2026, 1, 1, 4, 59)))
        self.assertFalse(server.in_window(w, datetime.datetime(2026, 1, 1, 12, 0)))

        tok = self.register("maint@b.co")
        ag, did = self.enroll(tok, "pc")
        now = datetime.datetime.now()
        closed = {"maintenance": {"start": (now + datetime.timedelta(hours=2)).strftime("%H:%M"),
                                  "end": (now + datetime.timedelta(hours=3)).strftime("%H:%M")}}
        self.assertEqual(self.call("PUT", f"/api/devices/{did}", {"settings": {"maintenance": {"start": "bad", "end": "05:00"}}}, tok)[0], 400)
        self.assertEqual(self.call("PUT", f"/api/devices/{did}", {"settings": closed, "notes": "rack 4, unit 12"}, tok)[0], 200)
        d = self.call("GET", "/api/devices", token=tok)[1][0]
        self.assertEqual((d["settings"], d["notes"]), (closed, "rack 4, unit 12"))

        _, pr = self.call("POST", "/api/profiles", {"name": "p", "apps": ["git"]}, tok)
        self.call("POST", "/api/assignments", {"tag": "all", "profile_id": pr["id"]}, tok)   # queues an automatic job
        self.assertIsNone(ag.process_once())                                                 # window closed: waits
        self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [{"type": "install", "apps": ["vlc"]}]}, tok)
        self.assertEqual(ag.process_once()["steps"][0]["app"], "VLC")                        # manual job runs right away
        self.call("PUT", f"/api/devices/{did}", {"settings": {}}, tok)                       # clear the window
        self.assertEqual(ag.process_once()["steps"][0]["app"], "Git")                        # waiting job now runs

    def test_server_settings_admin_only_and_agent_interval(self):
        tok = self.register("admin@b.co")
        other = self.register("user2@b.co")
        first_admin = self.call("GET", "/api/settings", token=tok)[1]["is_admin"]
        s, st = self.call("GET", "/api/settings", token=other)
        self.assertFalse(st["is_admin"])
        self.assertEqual(self.call("PUT", "/api/settings", {"poll_interval": 30}, other)[0], 403)
        admin_tok = tok if first_admin else self.call("POST", "/api/login", {"email": "a@b.co", "password": "longenough"})[1]["token"]
        self.assertEqual(self.call("PUT", "/api/settings", {"poll_interval": 2}, admin_tok)[0], 400)
        self.assertEqual(self.call("PUT", "/api/settings", {"poll_interval": 45, "retention_days": 30}, admin_tok)[0], 200)
        ag, did = self.enroll(tok, "pc")
        self.assertEqual(ag.checkin()["interval"], 45)
        self.assertEqual(ag.call("GET", "/api/agent/poll")["interval"], 45)
        self.assertEqual(self.call("PUT", "/api/settings", {"registration": False}, admin_tok)[0], 200)
        self.assertEqual(self.call("POST", "/api/register", {"email": "late@b.co", "password": "longenough"})[0], 403)
        self.call("PUT", "/api/settings", {"registration": True, "poll_interval": 15, "retention_days": 90}, admin_tok)

    def test_sysinfo_and_overview(self):
        tok = self.register("fleet2@b.co")
        a1, d1 = self.enroll(tok, "web-01")
        a2, d2 = self.enroll(tok, "db-01")
        a1.checkin(upgradable={"Git.Git": "2.5"}, installed={"Git.Git": "2.4"},
                   sysinfo={"os": "Ubuntu 24.04 LTS", "cpu": "EPYC", "mem_total_mb": 16384, "disk_total_gb": 500, "disk_free_gb": 20, "hostname": "web-01"})
        a2.checkin(sysinfo={"os": "Ubuntu 24.04 LTS", "mem_total_mb": 8192, "disk_total_gb": 100, "disk_free_gb": 4})
        devs = self.call("GET", "/api/devices", token=tok)[1]
        self.assertEqual([d["sysinfo"]["cpu"] for d in devs if d["name"] == "web-01"], ["EPYC"])
        ov = self.call("GET", "/api/overview", token=tok)[1]
        self.assertEqual((ov["devices"], ov["online"], ov["with_updates"], ov["ram_total_gb"]), (2, 2, 1, 24))
        self.assertEqual(ov["pending"][0]["name"], "Git")
        self.assertEqual(ov["by_os"][0]["count"], 2)
        self.assertTrue(any("low disk" in a["reason"] for a in ov["attention"]))   # db-01: 4 GB of 100


    def test_live_progress_is_visible_before_the_job_finishes(self):
        tok = self.register("live@b.co")
        ag, did = self.enroll(tok, "pc")
        self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [{"type": "install", "apps": ["git", "vlc", "teams"]}]}, tok)
        job = ag.call("GET", "/api/agent/poll")["job"]
        self.assertEqual([x["key"] for x in job["steps"]][:2], ["git", "vlc"])           # steps carry the catalog key
        queued = self.call("GET", "/api/jobs", token=tok)[1][0]
        self.assertEqual([x["status"] for x in queued["steps"]], ["queued", "queued", "queued"])

        ag.progress(job["id"], [{"i": 0, "status": "done"}, {"i": 1, "status": "running"}], "=== [1/3] install Git\nSuccessfully installed\n=== [2/3] install VLC")
        mid = self.call("GET", "/api/jobs", token=tok)[1][0]
        self.assertEqual(mid["status"], "running")
        self.assertEqual([x["status"] for x in mid["steps"]][:2], ["done", "running"])
        self.assertIn("Successfully installed", mid["log"])                                # live log, not just the final one

        ag.progress(job["id"], [{"i": 0, "status": "done"}, {"i": 1, "status": "failed", "hint": "Windows is already installing something else."}], "log2")
        ag.call("POST", f"/api/agent/jobs/{job['id']}/result", {"status": "failed", "log": "final log"})
        end = self.call("GET", "/api/jobs", token=tok)[1][0]
        self.assertEqual((end["status"], end["log"]), ("failed", "final log"))
        self.assertEqual(end["steps"][1]["hint"], "Windows is already installing something else.")
        ag.progress(job["id"], [{"i": 0, "status": "running"}], "late")                    # ignored once the job is finished
        self.assertEqual(self.call("GET", "/api/jobs", token=tok)[1][0]["log"], "final log")

    def test_progress_endpoint_ignores_garbage_and_other_devices(self):
        tok = self.register("garb@b.co")
        a1, d1 = self.enroll(tok, "one")
        a2, d2 = self.enroll(tok, "two")
        self.call("POST", "/api/jobs", {"device_ids": [d1], "steps": [{"type": "install", "apps": ["git"]}]}, tok)
        job = a1.call("GET", "/api/agent/poll")["job"]
        a2.progress(job["id"], [{"i": 0, "status": "done"}], "not yours")                  # another device can't touch it
        a1.progress(job["id"], [{"i": "x", "status": "done"}, {"i": 0, "status": "exploded"}, "junk"], "ok")
        j = self.call("GET", "/api/jobs", token=tok)[1][0]
        self.assertEqual((j["log"], j["steps"][0]["status"]), ("ok", "queued"))

    def test_server_sent_events_wake_up_on_changes(self):
        import http.client
        import time as _t
        tok = self.register("sse@b.co")
        ag, did = self.enroll(tok, "pc")
        host, port = self.base.replace("http://", "").split(":")
        conn = http.client.HTTPConnection(host, int(port), timeout=6)
        conn.request("GET", "/api/events", headers={"Authorization": "Bearer " + tok})
        resp = conn.getresponse()
        self.assertEqual((resp.status, resp.getheader("Content-Type")), (200, "text/event-stream"))
        first = resp.fp.readline() + resp.fp.readline() + resp.fp.readline()               # retry:, blank, event: hello
        self.assertIn(b"retry", first)
        self.assertIn(b"hello", first)
        resp.fp.readline()                                                                 # data: {}
        resp.fp.readline()                                                                 # blank line ends the event

        def next_event():
            lines = []
            while True:
                line = resp.fp.readline()
                if line.startswith(b"event:"):
                    return line.strip()
                lines.append(line)

        t0 = _t.time()
        threading.Timer(0.3, lambda: self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [{"type": "install", "apps": ["git"]}]}, tok)).start()
        self.assertEqual(next_event(), b"event: change")                                   # a user action wakes the stream
        self.assertLess(_t.time() - t0, 3)
        threading.Timer(0.3, lambda: ag.call("GET", "/api/agent/poll")).start()
        self.assertEqual(next_event(), b"event: change")                                   # a device starting the job does too
        conn.close()
        other = self.register("sse2@b.co")
        c2 = http.client.HTTPConnection(host, int(port), timeout=3)
        c2.request("GET", "/api/events", headers={"Authorization": "Bearer nope"})
        self.assertEqual(c2.getresponse().status, 401)


    def test_long_poll_wakes_the_device_instantly_and_never_holds_the_write_lock(self):
        import time as _t
        tok = self.register("long@b.co")
        ag, did = self.enroll(tok, "pc")
        got = {}

        def waiter():
            t0 = _t.time()
            got["job"] = ag.call("GET", "/api/agent/poll?wait=10")["job"]
            got["dt"] = _t.time() - t0
        th = threading.Thread(target=waiter)
        th.start()
        _t.sleep(0.5)
        t0 = _t.time()
        self.register("lock-probe@b.co")                                                  # a write while a device is waiting must not block
        self.assertLess(_t.time() - t0, 2.0)
        self.call("POST", "/api/jobs", {"device_ids": [did], "steps": [{"type": "install", "apps": ["git"]}]}, tok)
        th.join(6)
        self.assertIsNotNone(got.get("job"), got)
        self.assertLess(got["dt"], 3.0)                                                   # delivered right away, not after the 10 s wait
        self.assertEqual(self.call("GET", "/api/jobs", token=tok)[1][0]["status"], "running")

        t0 = _t.time()
        idle = ag.call("GET", "/api/agent/poll?wait=1")                                   # nothing to do: held for the wait, then empty
        self.assertEqual((idle["job"], idle.get("long")), (None, True))
        self.assertGreaterEqual(_t.time() - t0, 0.9)
        quick = ag.call("GET", "/api/agent/poll")                                         # no wait param = old behaviour, answers at once
        self.assertNotIn("long", quick)

    def test_info_is_not_lite(self):
        s, r = self.call("GET", "/api/info")
        self.assertEqual((s, r["lite"], r["home"]), (200, False, False))
        self.assertEqual(self.call("GET", "/api/home")[0], 404)  # no auto-login on the multi-user server


if __name__ == "__main__":
    unittest.main()

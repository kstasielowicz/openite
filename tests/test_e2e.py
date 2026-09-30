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

    def checkin(self, installed=None, names=None, upgradable=None):
        self.call("POST", "/api/agent/checkin", {"installed": installed or {}, "installed_names": names or {},
                                                 "upgradable": upgradable or {}, "manager": "winget"})

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

    def test_info_is_not_lite(self):
        s, r = self.call("GET", "/api/info")
        self.assertEqual((s, r["lite"], r["home"]), (200, False, False))
        self.assertEqual(self.call("GET", "/api/home")[0], 404)  # no auto-login on the multi-user server


if __name__ == "__main__":
    unittest.main()

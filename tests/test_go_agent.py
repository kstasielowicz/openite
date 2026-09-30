"""Runs the real Go binary (dry-run mode: nothing is installed) against the real Python server."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "server"))
import server  # noqa: E402

GO = shutil.which("go") or (r"C:\Program Files\Go\bin\go.exe" if os.path.exists(r"C:\Program Files\Go\bin\go.exe") else None)


@unittest.skipUnless(GO, "Go toolchain not found")
class GoAgent(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        cls.exe = os.path.join(cls.tmp, "openite.exe" if os.name == "nt" else "openite")
        subprocess.run([GO, "build", "-o", cls.exe, "./cmd/openite"], cwd=ROOT, check=True)
        cls.srv = server.make_server("127.0.0.1", 0, os.path.join(cls.tmp, "t.db"))
        cls.base = f"http://127.0.0.1:{cls.srv.server_address[1]}"
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def call(self, method, path, body=None, token=None):
        req = urllib.request.Request(self.base + path, method=method, data=json.dumps(body or {}).encode())
        if token:
            req.add_header("Authorization", "Bearer " + token)
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())

    def agent(self, *args):
        env = dict(os.environ, OPENITE_DRY_RUN="1")
        return subprocess.run([self.exe, *args], env=env, capture_output=True, text=True, timeout=60)

    def test_enroll_job_and_malicious_package_id(self):
        tok = self.call("POST", "/api/register", {"email": "go@b.co", "password": "longenough"})["token"]
        code = self.call("POST", "/api/enroll-code", token=tok)["code"]
        cfg = os.path.join(self.tmp, "agent.json")
        r = self.agent("enroll", "--server", self.base, "--code", code, "--name", "go-pc", "--config", cfg)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        dev = self.call("GET", "/api/devices", token=tok)[0]
        self.assertEqual((dev["name"], dev["manager"]), ("go-pc", "winget"))

        self.call("POST", "/api/jobs", {"device_ids": [dev["id"]], "steps": [{"type": "install", "apps": ["git", "vlc"]}]}, tok)
        r = self.agent("once", "--config", cfg)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        job = self.call("GET", "/api/jobs", token=tok)[0]
        self.assertEqual(job["status"], "done")
        self.assertIn("install Git", job["log"])
        self.assertIn("Git.Git", job["log"])

    def test_agent_refuses_unsafe_package_id_from_server(self):
        """Even if a malicious/compromised server sends a flag-shaped package id, the agent won't run it."""
        tok = self.call("POST", "/api/register", {"email": "evil@b.co", "password": "longenough"})["token"]
        cfg = os.path.join(self.tmp, "agent2.json")
        code = self.call("POST", "/api/enroll-code", token=tok)["code"]
        self.assertEqual(self.agent("enroll", "--server", self.base, "--code", code, "--config", cfg).returncode, 0)
        dev = self.call("GET", "/api/devices", token=tok)[0]
        # bypass the API validation by writing straight to the DB, simulating a compromised server
        import sqlite3
        db = sqlite3.connect(server.STATE["db"])
        db.execute("INSERT INTO jobs(account_id,device_id,title,steps,status,created) VALUES(?,?,?,?,?,0)",
                   (1 + db.execute("SELECT id FROM accounts WHERE email='evil@b.co'").fetchone()[0] - 1, dev["id"], "evil",
                    json.dumps([{"type": "install", "app": "x", "pkg": "--source=http://evil"}]), "queued"))
        db.commit()
        db.close()
        self.agent("once", "--config", cfg)
        job = self.call("GET", "/api/jobs", token=tok)[0]
        self.assertEqual(job["status"], "failed")
        self.assertIn("refusing unsafe package id", job["log"])

    def test_refuses_plain_http_to_public_host(self):
        r = self.agent("enroll", "--server", "http://example.com", "--code", "X", "--config", os.path.join(self.tmp, "x.json"))
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("refusing plain http", r.stderr)


if __name__ == "__main__":
    unittest.main()

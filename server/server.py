#!/usr/bin/env python3
"""Openite server: accounts, devices, tags, catalog, profiles, scripts, desired state and a job queue.

Stdlib only.
  python server.py --host 0.0.0.0    # home lab / team: accounts, many devices
(Managing just this one PC? Use the `openite` binary instead: it needs no server.)
"""
import argparse
import hashlib
import hmac
import json
import math
import os
import re
import secrets
import sqlite3
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent  # catalog/ and web/ live at the repo root, shared with the Go binary
BUILTIN_CATALOG = json.loads((ROOT / "catalog" / "catalog.json").read_text("utf-8"))
PACKS = json.loads((ROOT / "catalog" / "packs.json").read_text("utf-8"))
SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+@/-]{0,127}$")
MANAGERS = ("winget", "brew", "apt")
SESSION_TTL = 30 * 86400
ENROLL_TTL = 15 * 60
ONLINE_WINDOW = 90
RECONCILE_COOLDOWN = 6 * 3600
MAX_BODY = 1_000_000
STATE = {"db": str(HERE / "openite.db"), "allow_registration": True}

SCHEMA = """
CREATE TABLE IF NOT EXISTS accounts(id INTEGER PRIMARY KEY, email TEXT UNIQUE NOT NULL, salt BLOB NOT NULL, pw BLOB NOT NULL, created REAL);
CREATE TABLE IF NOT EXISTS sessions(token TEXT PRIMARY KEY, account_id INTEGER NOT NULL, created REAL);
CREATE TABLE IF NOT EXISTS enroll_codes(code TEXT PRIMARY KEY, account_id INTEGER NOT NULL, expires REAL, reusable INTEGER DEFAULT 0, tags TEXT DEFAULT '[]');
CREATE TABLE IF NOT EXISTS devices(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, name TEXT, os TEXT, manager TEXT,
  token TEXT UNIQUE, last_seen REAL, inventory TEXT DEFAULT '{}', agent_version TEXT, created REAL, tags TEXT DEFAULT '[]');
CREATE TABLE IF NOT EXISTS custom_apps(account_id INTEGER, key TEXT, data TEXT, PRIMARY KEY(account_id, key));
CREATE TABLE IF NOT EXISTS scripts(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, name TEXT, shell TEXT, body TEXT);
CREATE TABLE IF NOT EXISTS profiles(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, name TEXT, apps TEXT, script_ids TEXT);
CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, device_id INTEGER NOT NULL, title TEXT,
  steps TEXT, status TEXT, log TEXT DEFAULT '', created REAL, started REAL, finished REAL,
  source TEXT DEFAULT 'manual', rollout_id TEXT, batch INTEGER DEFAULT 0, rollout_max_fail INTEGER DEFAULT 20);
CREATE TABLE IF NOT EXISTS assignments(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, tag TEXT, profile_id INTEGER, auto_update INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY, account_id INTEGER, ts REAL, actor TEXT, action TEXT, detail TEXT);
"""
# columns added after the first release: (table, column, definition)
MIGRATIONS = [
    ("devices", "tags", "TEXT DEFAULT '[]'"),
    ("enroll_codes", "reusable", "INTEGER DEFAULT 0"),
    ("enroll_codes", "tags", "TEXT DEFAULT '[]'"),
    ("jobs", "source", "TEXT DEFAULT 'manual'"),
    ("jobs", "rollout_id", "TEXT"),
    ("jobs", "batch", "INTEGER DEFAULT 0"),
    ("jobs", "rollout_max_fail", "INTEGER DEFAULT 20"),
]


class ApiError(Exception):
    def __init__(self, status, msg):
        super().__init__(msg)
        self.status, self.msg = status, msg


def connect():
    c = sqlite3.connect(STATE["db"], timeout=15)
    c.row_factory = sqlite3.Row
    c.execute("PRAGMA journal_mode=WAL")
    return c


def init_db():
    with connect() as c:
        c.executescript(SCHEMA)
        for table, col, ddl in MIGRATIONS:
            if col not in [r["name"] for r in c.execute(f"PRAGMA table_info({table})")]:
                c.execute(f"ALTER TABLE {table} ADD COLUMN {col} {ddl}")


def hash_pw(pw, salt):
    return hashlib.scrypt(pw.encode(), salt=salt, n=2**14, r=8, p=1)


def need(body, *keys):
    for k in keys:
        if body.get(k) in (None, ""):
            raise ApiError(400, f"missing field: {k}")
    return [body[k] for k in keys]


def clean_tags(tags):
    if not isinstance(tags, list):
        raise ApiError(400, "tags must be a list")
    out = []
    for t in tags:
        t = str(t).strip().lower()
        if t and not re.fullmatch(r"[a-z0-9][a-z0-9._:=-]{0,39}", t):
            raise ApiError(400, f"bad tag '{t}': use letters, digits and . _ : = -")
        if t and t != "all" and t not in out:
            out.append(t)
    return out


def audit(db, account, actor, action, detail=""):
    db.execute("INSERT INTO audit(account_id,ts,actor,action,detail) VALUES(?,?,?,?,?)",
               (account, time.time(), actor, action, detail))


def is_online(dev):
    return bool(dev["last_seen"]) and time.time() - dev["last_seen"] < ONLINE_WINDOW


# ---------------------------------------------------------------- catalog & desired state
def full_catalog(c, account_id):
    apps = {a["key"]: dict(a, custom=False) for a in BUILTIN_CATALOG}
    for r in c.execute("SELECT data FROM custom_apps WHERE account_id=?", (account_id,)):
        a = json.loads(r["data"])
        apps[a["key"]] = dict(a, custom=True)
    return apps


def load_assignments(db, account):
    rows = db.execute("SELECT a.tag,a.auto_update,p.apps FROM assignments a JOIN profiles p ON p.id=a.profile_id "
                      "WHERE a.account_id=?", (account,)).fetchall()
    return [(r["tag"], bool(r["auto_update"]), json.loads(r["apps"])) for r in rows]


def dev_tags(dev):
    return set(json.loads(dev["tags"] or "[]")) | {"all"}


def desired_keys(assigns, dev):
    """{app_key: auto_update} this device should have, from profiles assigned to its tags."""
    out, tags = {}, dev_tags(dev)
    for tag, au, apps in assigns:
        if tag in tags:
            for k in apps:
                out[k] = out.get(k, False) or au
    return out


def _norm(s):
    return " ".join(str(s).lower().split())


def name_matches(have, alias):
    """Installed display name == alias, optionally followed by a version/arch ("Python 3" ~ "Python 3.12.10 (64-bit)").
    Mirrors catalog.NameMatches in Go."""
    h, a = _norm(have), _norm(alias)
    if not a or not h.startswith(a):
        return False
    if len(h) == len(a):
        return True
    if h[len(a)] not in " (." and not h[len(a)].isdigit():
        return False
    rest = h[len(a):].strip()
    return not rest or rest[0].isdigit() or rest[0] in "(." or (rest[0] == "v" and len(rest) > 1 and rest[1].isdigit())


def app_state(app, manager, inv):
    """(installed, upgradable) for one catalog app. winget lists apps installed outside winget (Vivaldi, ...)
    under ARP ids with no package id, so besides the id we also match by display name."""
    aliases = [app["name"]] + list(app.get("match", []))
    installed = upgradable = False
    pkg = app.get(manager)
    if pkg:
        low = pkg.lower()
        installed = any(k.lower() == low for k in inv.get("installed", {}))
        upgradable = any(k.lower() == low for k in inv.get("upgradable", {}))
    if not installed:
        installed = any(name_matches(n, al) for n in inv.get("installed_names", {}) for al in aliases)
    if not upgradable:
        upgradable = any(name_matches(n, al) for n in inv.get("upgradable_names", {}) for al in aliases)
    return installed or upgradable, upgradable


def maybe_reconcile(db, account, dev):
    """Queue a job that brings the device in line with its assigned profiles (idempotent, rate-limited)."""
    want = desired_keys(load_assignments(db, account), dev)
    if not want:
        return
    catalog = full_catalog(db, account)
    inv = json.loads(dev["inventory"] or "{}")
    steps = []
    for k, auto in sorted(want.items()):
        app = catalog.get(k)
        pkg = app and app.get(dev["manager"])
        if not pkg:
            continue
        have, outdated = app_state(app, dev["manager"], inv)
        if not have:
            steps.append({"type": "install", "app": app["name"], "pkg": pkg})
        elif auto and outdated:
            steps.append({"type": "upgrade", "app": app["name"], "pkg": pkg})
    if not steps:
        return
    if db.execute("SELECT 1 FROM jobs WHERE device_id=? AND source='reconcile' AND status IN ('queued','running')",
                  (dev["id"],)).fetchone():
        return
    last = db.execute("SELECT steps, finished FROM jobs WHERE device_id=? AND source='reconcile' ORDER BY id DESC LIMIT 1",
                      (dev["id"],)).fetchone()
    if last and json.loads(last["steps"]) == steps and time.time() - (last["finished"] or 0) < RECONCILE_COOLDOWN:
        return  # identical attempt recently; don't loop forever on something that can't succeed
    db.execute("INSERT INTO jobs(account_id,device_id,title,steps,status,created,source) VALUES(?,?,?,?,?,?,'reconcile')",
               (account, dev["id"], "Auto-sync with assigned profiles", json.dumps(steps), "queued", time.time()))


def resolve_step(step, device, catalog, scripts, profiles):
    """Turn a UI-level step into concrete steps for one device."""
    kind, mgr = step.get("type"), device["manager"]
    out = []
    if kind in ("install", "uninstall", "upgrade"):
        keys = step.get("apps")
        if kind == "upgrade" and keys == "all":
            return [{"type": "upgrade", "all": True}]
        if not isinstance(keys, list) or not keys:
            raise ApiError(400, f"{kind}: 'apps' must be a non-empty list")
        for k in keys:
            app = catalog.get(k)
            if not app:
                raise ApiError(400, f"unknown app: {k}")
            pkg = app.get(mgr)
            if pkg:
                out.append({"type": kind, "app": app["name"], "pkg": pkg})
            else:
                out.append({"type": "skip", "app": app["name"], "reason": f"no {mgr or 'package manager'} package id"})
    elif kind == "script":
        s = scripts.get(step.get("script_id"))
        if not s:
            raise ApiError(400, "unknown script")
        out.append({"type": "script", "name": s["name"], "shell": s["shell"], "body": s["body"]})
    elif kind == "profile":
        p = profiles.get(step.get("profile_id"))
        if not p:
            raise ApiError(400, "unknown profile")
        apps = json.loads(p["apps"])
        if apps:
            out += resolve_step({"type": "install", "apps": apps}, device, catalog, scripts, profiles)
        for sid in json.loads(p["script_ids"]):
            if sid in scripts:
                out += resolve_step({"type": "script", "script_id": sid}, device, catalog, scripts, profiles)
    else:
        raise ApiError(400, f"unknown step type: {kind}")
    return out


def plan_batches(n):
    """Canary first (~10%), then ~40%, then the rest. Sizes sum to n."""
    first = max(1, math.ceil(n * 0.1))
    second = min(n - first, math.ceil(n * 0.4))
    return [s for s in (first, second, n - first - second) if s > 0]


# ---------------------------------------------------------------- auth / account handlers
def new_session(db, account_id):
    db.execute("DELETE FROM sessions WHERE created<?", (time.time() - SESSION_TTL,))
    tok = secrets.token_urlsafe(32)
    db.execute("INSERT INTO sessions VALUES(?,?,?)", (tok, account_id, time.time()))
    return tok


def h_register(ctx, body):
    if not STATE["allow_registration"]:
        raise ApiError(403, "registration is disabled on this server")
    email, pw = need(body, "email", "password")
    email = email.strip().lower()
    if len(pw) < 8:
        raise ApiError(400, "password must be at least 8 characters")
    salt = os.urandom(16)
    try:
        cur = ctx.db.execute("INSERT INTO accounts(email,salt,pw,created) VALUES(?,?,?,?)",
                             (email, salt, hash_pw(pw, salt), time.time()))
    except sqlite3.IntegrityError:
        raise ApiError(409, "account already exists")
    audit(ctx.db, cur.lastrowid, email, "account.create")
    return {"token": new_session(ctx.db, cur.lastrowid), "email": email}


def h_login(ctx, body):
    email, pw = need(body, "email", "password")
    row = ctx.db.execute("SELECT * FROM accounts WHERE email=?", (email.strip().lower(),)).fetchone()
    salt = row["salt"] if row else b"\0" * 16
    ok = hmac.compare_digest(hash_pw(pw, salt), row["pw"] if row else b"x")
    if not (row and ok):
        raise ApiError(401, "invalid email or password")
    audit(ctx.db, row["id"], row["email"], "login")
    return {"token": new_session(ctx.db, row["id"]), "email": row["email"]}


def h_info(ctx, body):
    return {"home": False, "lite": False, "registration": STATE["allow_registration"], "version": "0.3.0"}


# ---------------------------------------------------------------- catalog / apps
def h_catalog(ctx, body):
    return list(full_catalog(ctx.db, ctx.account).values())


def h_packs(ctx, body):
    return PACKS


def h_add_app(ctx, body):
    key, name = need(body, "key", "name")
    if not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,63}", key):
        raise ApiError(400, "key must be lowercase letters, digits, . _ -")
    if key in {a["key"] for a in BUILTIN_CATALOG}:
        raise ApiError(409, "key collides with a built-in app")
    app = {"key": key, "name": name, "category": body.get("category") or "Custom"}
    for m in MANAGERS:
        if body.get(m):
            if not SAFE_ID.match(str(body[m])):
                raise ApiError(400, f"invalid {m} package id (letters, digits and . _ + @ / - only)")
            app[m] = str(body[m])
    if body.get("match"):
        app["match"] = [str(x) for x in body["match"]][:10]
    if not any(m in app for m in MANAGERS):
        raise ApiError(400, "give at least one of: winget, brew, apt")
    ctx.db.execute("INSERT OR REPLACE INTO custom_apps VALUES(?,?,?)", (ctx.account, key, json.dumps(app)))
    audit(ctx.db, ctx.account, ctx.actor, "app.add", key)
    return app


def h_del_app(ctx, body, key):
    ctx.db.execute("DELETE FROM custom_apps WHERE account_id=? AND key=?", (ctx.account, str(key)))
    return {}


# ---------------------------------------------------------------- devices
def device_view(r, assigns, catalog):
    inv = json.loads(r["inventory"] or "{}")
    have, outdated = [], []
    for key, app in catalog.items():
        i, u = app_state(app, r["manager"], inv)
        if i:
            have.append(key)
        if u:
            outdated.append(key)
    want = desired_keys(assigns, r)
    missing = [catalog[k]["name"] for k in sorted(want) if k in catalog and catalog[k].get(r["manager"]) and k not in have]
    return {"id": r["id"], "name": r["name"], "os": r["os"], "manager": r["manager"], "tags": json.loads(r["tags"] or "[]"),
            "last_seen": r["last_seen"], "online": is_online(r), "agent_version": r["agent_version"],
            "have": have, "outdated": outdated, "n_installed": len(inv.get("installed", {})),
            "n_upgradable": len(inv.get("upgradable", {})), "managed": bool(want), "missing": missing}


def h_devices(ctx, body):
    assigns, catalog = load_assignments(ctx.db, ctx.account), full_catalog(ctx.db, ctx.account)
    return [device_view(r, assigns, catalog) for r in ctx.db.execute(
        "SELECT * FROM devices WHERE account_id=? ORDER BY name", (ctx.account,))]


def h_update_device(ctx, body, did):
    if "name" in body and body["name"]:
        ctx.db.execute("UPDATE devices SET name=? WHERE id=? AND account_id=?", (body["name"], did, ctx.account))
    if "tags" in body:
        ctx.db.execute("UPDATE devices SET tags=? WHERE id=? AND account_id=?",
                       (json.dumps(clean_tags(body["tags"])), did, ctx.account))
        audit(ctx.db, ctx.account, ctx.actor, "device.tags", f"{did}: {body['tags']}")
        dev = ctx.db.execute("SELECT * FROM devices WHERE id=?", (did,)).fetchone()
        if dev:
            maybe_reconcile(ctx.db, ctx.account, dev)
    return {}


def h_del_device(ctx, body, did):
    ctx.db.execute("DELETE FROM jobs WHERE device_id=? AND account_id=?", (did, ctx.account))
    ctx.db.execute("DELETE FROM devices WHERE id=? AND account_id=?", (did, ctx.account))
    audit(ctx.db, ctx.account, ctx.actor, "device.remove", str(did))
    return {}


def h_enroll_code(ctx, body):
    """One-time code (15 min) by default; {reusable:true} makes a long-lived key for images/automation."""
    reusable = bool(body.get("reusable"))
    tags = clean_tags(body.get("tags") or [])
    code = secrets.token_hex(16).upper() if reusable else secrets.token_hex(4).upper()
    ctx.db.execute("DELETE FROM enroll_codes WHERE expires<?", (time.time(),))
    ttl = 10 * 365 * 86400 if reusable else ENROLL_TTL
    ctx.db.execute("INSERT INTO enroll_codes VALUES(?,?,?,?,?)",
                   (code, ctx.account, time.time() + ttl, int(reusable), json.dumps(tags)))
    audit(ctx.db, ctx.account, ctx.actor, "enroll.key" if reusable else "enroll.code", f"tags={tags}")
    return {"code": code, "expires_in": ttl, "reusable": reusable}


# ---------------------------------------------------------------- scripts / profiles / assignments
def h_list_scripts(ctx, body):
    return [dict(r) for r in ctx.db.execute(
        "SELECT id,name,shell,body FROM scripts WHERE account_id=? ORDER BY name", (ctx.account,))]


def h_add_script(ctx, body):
    name, shell, text = need(body, "name", "shell", "body")
    if shell not in ("powershell", "bash", "cmd"):
        raise ApiError(400, "shell must be powershell, bash or cmd")
    cur = ctx.db.execute("INSERT INTO scripts(account_id,name,shell,body) VALUES(?,?,?,?)", (ctx.account, name, shell, text))
    audit(ctx.db, ctx.account, ctx.actor, "script.add", name)
    return {"id": cur.lastrowid}


def h_del_script(ctx, body, sid):
    ctx.db.execute("DELETE FROM scripts WHERE id=? AND account_id=?", (sid, ctx.account))
    audit(ctx.db, ctx.account, ctx.actor, "script.delete", str(sid))
    return {}


def h_list_profiles(ctx, body):
    return [{"id": r["id"], "name": r["name"], "apps": json.loads(r["apps"]), "script_ids": json.loads(r["script_ids"])}
            for r in ctx.db.execute("SELECT * FROM profiles WHERE account_id=? ORDER BY name", (ctx.account,))]


def h_add_profile(ctx, body):
    (name,) = need(body, "name")
    apps, sids = body.get("apps") or [], body.get("script_ids") or []
    cur = ctx.db.execute("INSERT INTO profiles(account_id,name,apps,script_ids) VALUES(?,?,?,?)",
                         (ctx.account, name, json.dumps(apps), json.dumps(sids)))
    audit(ctx.db, ctx.account, ctx.actor, "profile.add", name)
    return {"id": cur.lastrowid}


def h_update_profile(ctx, body, pid):
    row = ctx.db.execute("SELECT * FROM profiles WHERE id=? AND account_id=?", (pid, ctx.account)).fetchone()
    if not row:
        raise ApiError(404, "profile not found")
    ctx.db.execute("UPDATE profiles SET name=?, apps=?, script_ids=? WHERE id=?",
                   (body.get("name") or row["name"], json.dumps(body.get("apps", json.loads(row["apps"]))),
                    json.dumps(body.get("script_ids", json.loads(row["script_ids"]))), pid))
    audit(ctx.db, ctx.account, ctx.actor, "profile.update", str(pid))
    return {}


def h_del_profile(ctx, body, pid):
    ctx.db.execute("DELETE FROM assignments WHERE profile_id=? AND account_id=?", (pid, ctx.account))
    ctx.db.execute("DELETE FROM profiles WHERE id=? AND account_id=?", (pid, ctx.account))
    audit(ctx.db, ctx.account, ctx.actor, "profile.delete", str(pid))
    return {}


def h_list_assignments(ctx, body):
    return [{"id": r["id"], "tag": r["tag"], "profile_id": r["profile_id"], "auto_update": bool(r["auto_update"])}
            for r in ctx.db.execute("SELECT * FROM assignments WHERE account_id=? ORDER BY id", (ctx.account,))]


def h_add_assignment(ctx, body):
    """Keep devices with <tag> (or 'all') in sync with a profile's apps."""
    tag, pid = need(body, "tag", "profile_id")
    tag = str(tag).strip().lower()
    if tag != "all":
        clean_tags([tag])
    if not ctx.db.execute("SELECT 1 FROM profiles WHERE id=? AND account_id=?", (pid, ctx.account)).fetchone():
        raise ApiError(404, "profile not found")
    cur = ctx.db.execute("INSERT INTO assignments(account_id,tag,profile_id,auto_update) VALUES(?,?,?,?)",
                         (ctx.account, tag, pid, int(bool(body.get("auto_update")))))
    audit(ctx.db, ctx.account, ctx.actor, "assignment.add", f"tag={tag} profile={pid}")
    for dev in ctx.db.execute("SELECT * FROM devices WHERE account_id=?", (ctx.account,)).fetchall():
        maybe_reconcile(ctx.db, ctx.account, dev)
    return {"id": cur.lastrowid}


def h_del_assignment(ctx, body, aid):
    ctx.db.execute("DELETE FROM assignments WHERE id=? AND account_id=?", (aid, ctx.account))
    audit(ctx.db, ctx.account, ctx.actor, "assignment.delete", str(aid))
    return {}


def h_audit(ctx, body):
    return [dict(r) for r in ctx.db.execute(
        "SELECT ts,actor,action,detail FROM audit WHERE account_id=? ORDER BY id DESC LIMIT 200", (ctx.account,))]


# ---------------------------------------------------------------- jobs
def h_create_jobs(ctx, body):
    """body: {device_ids?:[..], tags?:[..], steps:[..], title?, rollout?:bool, max_failure_pct?:int}"""
    (steps,) = need(body, "steps")
    devs = {}
    for r in ctx.db.execute("SELECT * FROM devices WHERE account_id=?", (ctx.account,)):
        if r["id"] in (body.get("device_ids") or []) or dev_tags(r) & set(body.get("tags") or []):
            devs[r["id"]] = r
    missing = set(body.get("device_ids") or []) - set(devs)
    if missing:
        raise ApiError(404, f"device not found: {sorted(missing)}")
    if not devs:
        raise ApiError(400, "no matching devices")
    catalog = full_catalog(ctx.db, ctx.account)
    scripts = {r["id"]: dict(r) for r in ctx.db.execute("SELECT * FROM scripts WHERE account_id=?", (ctx.account,))}
    profiles = {r["id"]: dict(r) for r in ctx.db.execute("SELECT * FROM profiles WHERE account_id=?", (ctx.account,))}
    order = sorted(devs.values(), key=lambda d: (not is_online(d), d["id"]))  # online devices go first (canary)
    rollout = bool(body.get("rollout")) and len(order) > 1
    rid = secrets.token_hex(6) if rollout else None
    batch_of = []
    if rollout:
        for i, size in enumerate(plan_batches(len(order))):
            batch_of += [i] * size
    title = body.get("title") or ", ".join(str(s.get("type")) for s in steps)
    max_fail = int(body.get("max_failure_pct", 20))
    ids = []
    for i, dev in enumerate(order):
        resolved = []
        for s in steps:
            resolved += resolve_step(s, dev, catalog, scripts, profiles)
        cur = ctx.db.execute(
            "INSERT INTO jobs(account_id,device_id,title,steps,status,created,rollout_id,batch,rollout_max_fail) VALUES(?,?,?,?,?,?,?,?,?)",
            (ctx.account, dev["id"], title, json.dumps(resolved), "queued", time.time(), rid, batch_of[i] if rollout else 0, max_fail))
        ids.append(cur.lastrowid)
    audit(ctx.db, ctx.account, ctx.actor, "job.create",
          f"{title} on {len(order)} device(s)" + (f", staged rollout {plan_batches(len(order))}" if rollout else ""))
    return {"job_ids": ids, "rollout_id": rid, "batches": plan_batches(len(order)) if rollout else None}


def job_view(r):
    return {"id": r["id"], "device_id": r["device_id"], "title": r["title"], "status": r["status"], "log": r["log"],
            "created": r["created"], "started": r["started"], "finished": r["finished"], "source": r["source"],
            "rollout_id": r["rollout_id"], "batch": r["batch"]}


def h_jobs(ctx, body):
    q, args = "SELECT * FROM jobs WHERE account_id=?", [ctx.account]
    if ctx.query.get("device_id"):
        q += " AND device_id=?"
        args.append(int(ctx.query["device_id"]))
    return [job_view(r) for r in ctx.db.execute(q + " ORDER BY id DESC LIMIT 100", args)]


def h_cancel_job(ctx, body, jid):
    ctx.db.execute("UPDATE jobs SET status='cancelled', finished=? WHERE id=? AND account_id=? AND status IN ('queued','halted')",
                   (time.time(), jid, ctx.account))
    return {}


def evaluate_rollout(db, rid, max_fail, account):
    rows = db.execute("SELECT status FROM jobs WHERE rollout_id=?", (rid,)).fetchall()
    fin = [r["status"] for r in rows if r["status"] in ("done", "failed")]
    if fin and fin.count("failed") * 100 / len(fin) > max_fail:
        n = db.execute("UPDATE jobs SET status='halted', finished=? WHERE rollout_id=? AND status='queued'",
                       (time.time(), rid)).rowcount
        if n:
            audit(db, account, "system", "rollout.halted", f"{rid}: {fin.count('failed')} failed, {n} job(s) not started")


# ---------------------------------------------------------------- agent-facing
def h_agent_enroll(ctx, body):
    (code,) = need(body, "code")
    row = ctx.db.execute("SELECT * FROM enroll_codes WHERE code=? AND expires>?", (code.strip().upper(), time.time())).fetchone()
    if not row:
        raise ApiError(403, "invalid or expired enrollment code")
    if not row["reusable"]:
        ctx.db.execute("DELETE FROM enroll_codes WHERE code=?", (row["code"],))
    tok = secrets.token_urlsafe(32)
    name = body.get("name") or "device"
    cur = ctx.db.execute(
        "INSERT INTO devices(account_id,name,os,manager,token,last_seen,created,tags) VALUES(?,?,?,?,?,?,?,?)",
        (row["account_id"], name, body.get("os"), body.get("manager"), tok, time.time(), time.time(), row["tags"] or "[]"))
    audit(ctx.db, row["account_id"], f"device:{name}", "device.enroll", f"id={cur.lastrowid}")
    return {"device_id": cur.lastrowid, "token": tok}


def h_agent_checkin(ctx, body):
    inv = {k: body.get(k) or {} for k in ("installed", "upgradable", "installed_names", "upgradable_names")}
    ctx.db.execute("UPDATE devices SET last_seen=?, inventory=?, os=COALESCE(?,os), manager=COALESCE(?,manager), agent_version=? WHERE id=?",
                   (time.time(), json.dumps(inv), body.get("os"), body.get("manager"), body.get("agent_version"), ctx.device))
    maybe_reconcile(ctx.db, ctx.account, ctx.db.execute("SELECT * FROM devices WHERE id=?", (ctx.device,)).fetchone())
    return {}


def h_agent_poll(ctx, body):
    ctx.db.execute("UPDATE devices SET last_seen=? WHERE id=?", (time.time(), ctx.device))
    for row in ctx.db.execute("SELECT * FROM jobs WHERE device_id=? AND status='queued' ORDER BY id", (ctx.device,)).fetchall():
        if row["rollout_id"] and ctx.db.execute(
                "SELECT 1 FROM jobs WHERE rollout_id=? AND batch<? AND status IN ('queued','running') LIMIT 1",
                (row["rollout_id"], row["batch"])).fetchone():
            continue  # earlier batch still in flight
        ctx.db.execute("UPDATE jobs SET status='running', started=? WHERE id=?", (time.time(), row["id"]))
        return {"job": {"id": row["id"], "title": row["title"], "steps": json.loads(row["steps"])}}
    return {"job": None}


def h_agent_result(ctx, body, jid):
    status, log = need(body, "status", "log")
    if status not in ("done", "failed"):
        raise ApiError(400, "bad status")
    ctx.db.execute("UPDATE jobs SET status=?, log=?, finished=? WHERE id=? AND device_id=?",
                   (status, log[-200_000:], time.time(), jid, ctx.device))
    row = ctx.db.execute("SELECT rollout_id, rollout_max_fail FROM jobs WHERE id=?", (jid,)).fetchone()
    if row and row["rollout_id"]:
        evaluate_rollout(ctx.db, row["rollout_id"], row["rollout_max_fail"], ctx.account)
    return {}


# method, regex, handler, auth ("none" | "user" | "device")
ROUTES = [
    ("GET", r"/api/info", h_info, "none"),
    ("POST", r"/api/register", h_register, "none"),
    ("POST", r"/api/login", h_login, "none"),
    ("GET", r"/api/catalog", h_catalog, "user"),
    ("GET", r"/api/packs", h_packs, "user"),
    ("POST", r"/api/apps", h_add_app, "user"),
    ("DELETE", r"/api/apps/([^/]+)", h_del_app, "user"),
    ("GET", r"/api/devices", h_devices, "user"),
    ("PUT", r"/api/devices/(\d+)", h_update_device, "user"),
    ("DELETE", r"/api/devices/(\d+)", h_del_device, "user"),
    ("POST", r"/api/enroll-code", h_enroll_code, "user"),
    ("GET", r"/api/scripts", h_list_scripts, "user"),
    ("POST", r"/api/scripts", h_add_script, "user"),
    ("DELETE", r"/api/scripts/(\d+)", h_del_script, "user"),
    ("GET", r"/api/profiles", h_list_profiles, "user"),
    ("POST", r"/api/profiles", h_add_profile, "user"),
    ("PUT", r"/api/profiles/(\d+)", h_update_profile, "user"),
    ("DELETE", r"/api/profiles/(\d+)", h_del_profile, "user"),
    ("GET", r"/api/assignments", h_list_assignments, "user"),
    ("POST", r"/api/assignments", h_add_assignment, "user"),
    ("DELETE", r"/api/assignments/(\d+)", h_del_assignment, "user"),
    ("GET", r"/api/audit", h_audit, "user"),
    ("POST", r"/api/jobs", h_create_jobs, "user"),
    ("GET", r"/api/jobs", h_jobs, "user"),
    ("POST", r"/api/jobs/(\d+)/cancel", h_cancel_job, "user"),
    ("POST", r"/api/agent/enroll", h_agent_enroll, "none"),
    ("POST", r"/api/agent/checkin", h_agent_checkin, "device"),
    ("GET", r"/api/agent/poll", h_agent_poll, "device"),
    ("POST", r"/api/agent/jobs/(\d+)/result", h_agent_result, "device"),
]
ROUTES = [(m, re.compile(p + r"$"), f, a) for m, p, f, a in ROUTES]


class Ctx:
    db = account = device = handler = query = None
    actor = "anonymous"


class Handler(BaseHTTPRequestHandler):
    server_version = "Openite/0.2"

    def log_message(self, fmt, *args):
        if os.environ.get("OPENITE_VERBOSE"):
            super().log_message(fmt, *args)

    def send_json(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def bearer(self):
        a = self.headers.get("Authorization", "")
        return a[7:] if a.startswith("Bearer ") else ""

    def dispatch(self, method):
        path, _, qs = self.path.partition("?")
        if method == "GET" and path in ("/", "/index.html"):
            data = (ROOT / "web" / "index.html").read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
            self.end_headers()
            self.wfile.write(data)
            return
        try:
            for m, rx, fn, auth in ROUTES:
                mt = rx.match(path)
                if m == method and mt:
                    break
            else:
                raise ApiError(404, "not found")
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                raise ApiError(413, "body too large")
            try:
                body = json.loads(self.rfile.read(length) or b"{}")
            except ValueError:
                raise ApiError(400, "invalid JSON")
            if not isinstance(body, dict):
                raise ApiError(400, "body must be an object")
            db = connect()
            try:
                ctx = Ctx()
                ctx.db, ctx.handler = db, self
                ctx.query = dict(p.split("=", 1) for p in qs.split("&") if "=" in p)
                if auth == "user":
                    r = db.execute("SELECT s.account_id, a.email FROM sessions s JOIN accounts a ON a.id=s.account_id "
                                   "WHERE s.token=? AND s.created>?", (self.bearer(), time.time() - SESSION_TTL)).fetchone()
                    if not r:
                        raise ApiError(401, "not signed in")
                    ctx.account, ctx.actor = r["account_id"], r["email"]
                elif auth == "device":
                    r = db.execute("SELECT id, account_id FROM devices WHERE token=?", (self.bearer(),)).fetchone()
                    if not r:
                        raise ApiError(401, "unknown device")
                    ctx.device, ctx.account, ctx.actor = r["id"], r["account_id"], f"device:{r['id']}"
                args = [int(g) if g.isdigit() and fn is not h_del_app else g for g in mt.groups()]
                res = fn(ctx, body, *args)
                db.commit()
            except BaseException:
                db.rollback()
                raise
            finally:
                db.close()
            self.send_json(200, res)
        except ApiError as e:
            self.send_json(e.status, {"error": e.msg})
        except Exception as e:  # noqa: BLE001
            self.send_json(500, {"error": f"server error: {e}"})

    def do_GET(self): self.dispatch("GET")
    def do_POST(self): self.dispatch("POST")
    def do_PUT(self): self.dispatch("PUT")
    def do_DELETE(self): self.dispatch("DELETE")


def make_server(host, port, db=None, allow_registration=True):
    if db:
        STATE["db"] = db
    STATE["allow_registration"] = allow_registration
    init_db()
    return ThreadingHTTPServer((host, port), Handler)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--db", default=os.environ.get("OPENITE_DB"))
    ap.add_argument("--no-registration", action="store_true", help="disallow new accounts (after you've made yours)")
    a = ap.parse_args()
    srv = make_server(a.host, a.port, a.db, not (a.no_registration or os.environ.get("OPENITE_NO_REGISTRATION")))
    print(f"Openite server running at http://{a.host}:{srv.server_address[1]}   (data: {STATE['db']})\nPress Ctrl+C to stop.")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()

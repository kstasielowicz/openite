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
import threading
import time
import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent  # catalog/ and web/ live at the repo root, shared with the Go binary
BUILTIN_CATALOG = json.loads((ROOT / "catalog" / "catalog.json").read_text("utf-8"))
PACKS = json.loads((ROOT / "catalog" / "packs.json").read_text("utf-8"))
SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+@/-]{0,127}$")
SAFE_VERSION = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+~:-]{0,63}$")
VERSION = "0.5.0"
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
CREATE TABLE IF NOT EXISTS schedules(id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL, tag TEXT, hour INTEGER, minute INTEGER,
  days TEXT DEFAULT '[]', rollout INTEGER DEFAULT 1, last_run TEXT);
CREATE TABLE IF NOT EXISTS server_settings(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY, account_id INTEGER, ts REAL, actor TEXT, action TEXT, detail TEXT);
"""
# columns added after the first release: (table, column, definition)
MIGRATIONS = [
    ("devices", "tags", "TEXT DEFAULT '[]'"),
    ("devices", "sysinfo", "TEXT DEFAULT '{}'"),
    ("devices", "notes", "TEXT DEFAULT ''"),
    ("devices", "settings", "TEXT DEFAULT '{}'"),
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
def get_setting(db, key, default=None):
    r = db.execute("SELECT value FROM server_settings WHERE key=?", (key,)).fetchone()
    return json.loads(r["value"]) if r else default


def set_setting(db, key, value):
    db.execute("INSERT OR REPLACE INTO server_settings VALUES(?,?)", (key, json.dumps(value)))


def registration_open(db):
    if not db.execute("SELECT 1 FROM accounts LIMIT 1").fetchone():
        return True  # the very first account can always be created
    v = get_setting(db, "registration")
    return STATE["allow_registration"] if v is None else bool(v)


def poll_interval(db):
    return int(get_setting(db, "poll_interval", 15))


def is_admin(db, account):
    r = db.execute("SELECT MIN(id) AS m FROM accounts").fetchone()
    return r["m"] == account  # the first account administers the server


def _hhmm(text):
    m = re.fullmatch(r"([01]?\d|2[0-3]):([0-5]\d)", str(text or ""))
    return int(m.group(1)) * 60 + int(m.group(2)) if m else None


def in_window(settings, now=None):
    """True when the device's maintenance window (if any) is open. Windows may cross midnight."""
    w = (settings or {}).get("maintenance")
    s_, e_ = (_hhmm(w.get("start")), _hhmm(w.get("end"))) if isinstance(w, dict) else (None, None)
    if s_ is None or e_ is None:
        return True
    now = now or datetime.datetime.now()
    t = now.hour * 60 + now.minute
    return s_ <= t < e_ if s_ <= e_ else (t >= s_ or t < e_)


def norm_apps(apps):
    """Profile apps may be plain keys (old format) or {key, version}. Always returns [{key, version}]."""
    out = []
    for a in apps or []:
        if isinstance(a, str):
            out.append({"key": a, "version": ""})
        elif isinstance(a, dict) and a.get("key"):
            v = str(a.get("version") or "")
            if v and not SAFE_VERSION.match(v):
                raise ApiError(400, f"invalid version '{v}'")
            out.append({"key": str(a["key"]), "version": v})
        else:
            raise ApiError(400, "apps must be app keys or {key, version}")
    return out


def cmp_versions(a, b):
    """Numeric dotted-version compare: -1, 0, 1 ("2.9" < "2.10")."""
    x, y = [int(n) for n in re.findall(r"\d+", str(a))], [int(n) for n in re.findall(r"\d+", str(b))]
    for i in range(max(len(x), len(y))):
        p, q = (x[i] if i < len(x) else 0), (y[i] if i < len(y) else 0)
        if p != q:
            return -1 if p < q else 1
    return 0


def full_catalog(c, account_id):
    apps = {a["key"]: dict(a, custom=False) for a in BUILTIN_CATALOG}
    for r in c.execute("SELECT data FROM custom_apps WHERE account_id=?", (account_id,)):
        a = json.loads(r["data"])
        apps[a["key"]] = dict(a, custom=True)
    return apps


def load_assignments(db, account):
    rows = db.execute("SELECT a.tag,a.auto_update,p.apps FROM assignments a JOIN profiles p ON p.id=a.profile_id "
                      "WHERE a.account_id=?", (account,)).fetchall()
    return [(r["tag"], bool(r["auto_update"]), norm_apps(json.loads(r["apps"]))) for r in rows]


def dev_tags(dev):
    return set(json.loads(dev["tags"] or "[]")) | {"all"}


def desired_keys(assigns, dev):
    """{app_key: {auto, version}} this device should have, from profiles assigned to its tags."""
    out, tags = {}, dev_tags(dev)
    for tag, au, apps in assigns:
        if tag in tags:
            for e in apps:
                cur = out.setdefault(e["key"], {"auto": False, "version": ""})
                cur["auto"] = cur["auto"] or au
                cur["version"] = cur["version"] or e["version"]
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
    return not rest or rest[0].isdigit() or rest[0] in "(.-" or (rest[0] == "v" and len(rest) > 1 and rest[1].isdigit())


def app_status(app, manager, inv):
    """Installed / upgradable / held flags plus versions for one catalog app. winget lists apps installed outside winget
    (Vivaldi, ...) under ARP ids with no package id, so besides the id we also match by display name."""
    aliases = [app["name"]] + list(app.get("match", []))
    st = {"installed": False, "upgradable": False, "held": False, "version": "", "available": ""}
    pkg = app.get(manager)
    if pkg:
        low = pkg.lower()
        for k, v in inv.get("installed", {}).items():
            if k.lower() == low:
                st["installed"], st["version"] = True, str(v)
        for k, v in inv.get("upgradable", {}).items():
            if k.lower() == low:
                st["upgradable"], st["available"] = True, str(v)
        st["held"] = any(k.lower() == low for k in inv.get("pinned", {}))
    if not st["installed"]:
        for n, v in inv.get("installed_names", {}).items():
            if any(name_matches(n, al) for al in aliases):
                st["installed"], st["version"] = True, str(v)
                break
    if not st["upgradable"]:
        for n, v in inv.get("upgradable_names", {}).items():
            if any(name_matches(n, al) for al in aliases):
                st["upgradable"], st["available"] = True, str(v)
                break
    if not st["held"]:
        st["held"] = any(name_matches(n, al) for n in inv.get("pinned_names", {}) for al in aliases)
    st["installed"] = st["installed"] or st["upgradable"]
    return st


def app_state(app, manager, inv):
    st = app_status(app, manager, inv)
    return st["installed"], st["upgradable"]


def maybe_reconcile(db, account, dev):
    """Queue a job that brings the device in line with its assigned profiles (idempotent, rate-limited)."""
    want = desired_keys(load_assignments(db, account), dev)
    if not want:
        return
    catalog = full_catalog(db, account)
    inv = json.loads(dev["inventory"] or "{}")
    steps = []
    for k, cfg in sorted(want.items()):
        app = catalog.get(k)
        pkg = app and app.get(dev["manager"])
        if not pkg:
            continue
        st = app_status(app, dev["manager"], inv)
        ver = cfg["version"]
        step = {"type": "install", "app": app["name"], "pkg": pkg, "aliases": [app["name"]] + list(app.get("match", []))}
        if ver:
            step["version"] = ver
        if not st["installed"]:
            steps.append(step)
        elif ver:
            if st["version"] and cmp_versions(st["version"], ver) < 0:  # older than pinned: move up (never auto-downgrade)
                steps.append(dict(step, type="upgrade"))
        elif cfg["auto"] and st["upgradable"] and not st["held"]:
            steps.append(dict(step, type="upgrade"))
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
    if kind in ("install", "uninstall", "upgrade", "pin", "unpin"):
        keys = step.get("apps")
        if kind == "upgrade" and keys == "all":
            return [{"type": "upgrade", "all": True}]
        if not isinstance(keys, list) or not keys:
            raise ApiError(400, f"{kind}: 'apps' must be a non-empty list")
        versions = step.get("versions") or {}
        for k in keys:
            app = catalog.get(k)
            if not app:
                raise ApiError(400, f"unknown app: {k}")
            ver = str(versions.get(k) or "") if kind in ("install", "upgrade") else ""
            if ver and not SAFE_VERSION.match(ver):
                raise ApiError(400, f"invalid version for {app['name']}")
            pkg = app.get(mgr)
            if pkg:
                entry = {"type": kind, "app": app["name"], "pkg": pkg, "aliases": [app["name"]] + list(app.get("match", []))}
                if ver:
                    entry["version"] = ver
                out.append(entry)
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
        entries = norm_apps(json.loads(p["apps"]))
        if entries:
            out += resolve_step({"type": "install", "apps": [e["key"] for e in entries],
                                 "versions": {e["key"]: e["version"] for e in entries if e["version"]}}, device, catalog, scripts, profiles)
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
    if not registration_open(ctx.db):
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
    return {"home": False, "lite": False, "registration": registration_open(ctx.db), "version": VERSION}


# ---------------------------------------------------------------- catalog / apps
def h_catalog(ctx, body):
    return list(full_catalog(ctx.db, ctx.account).values())


def h_packs(ctx, body):
    return PACKS


def h_icons(ctx, body):
    d = ROOT / "web" / "icons"
    return sorted(p.stem for p in d.glob("*.svg")) if d.is_dir() else []


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
    mgr = r["manager"]
    have, outdated, held, versions, available, status = [], [], [], {}, {}, {}
    for key, app in catalog.items():
        st = app_status(app, mgr, inv)
        status[key] = st
        if st["installed"]:
            have.append(key)
            if st["version"]:
                versions[key] = st["version"]
        if st["upgradable"]:
            outdated.append(key)
            available[key] = st["available"]
        if st["held"]:
            held.append(key)
    want = desired_keys(assigns, r)
    missing = []
    for k in sorted(want):
        app = catalog.get(k)
        if not app or not app.get(mgr):
            continue
        st, ver = status[k], want[k]["version"]
        if not st["installed"]:
            missing.append(app["name"] + (f" {ver}" if ver else ""))
        elif ver and st["version"] and cmp_versions(st["version"], ver) != 0:
            missing.append(f"{app['name']} {ver} (has {st['version']})")
    return {"id": r["id"], "name": r["name"], "os": r["os"], "manager": mgr, "tags": json.loads(r["tags"] or "[]"),
            "last_seen": r["last_seen"], "online": is_online(r), "agent_version": r["agent_version"],
            "have": have, "outdated": outdated, "held": held, "versions": versions, "available": available,
            "n_installed": len(inv.get("installed", {})), "n_upgradable": len(inv.get("upgradable", {})),
            "managed": bool(want), "missing": missing, "sysinfo": json.loads(r["sysinfo"] or "{}"),
            "notes": r["notes"] or "", "settings": json.loads(r["settings"] or "{}")}


def h_devices(ctx, body):
    assigns, catalog = load_assignments(ctx.db, ctx.account), full_catalog(ctx.db, ctx.account)
    return [device_view(r, assigns, catalog) for r in ctx.db.execute(
        "SELECT * FROM devices WHERE account_id=? ORDER BY name", (ctx.account,))]


def h_update_device(ctx, body, did):
    if not ctx.db.execute("SELECT 1 FROM devices WHERE id=? AND account_id=?", (did, ctx.account)).fetchone():
        raise ApiError(404, "device not found")
    if "name" in body and body["name"]:
        ctx.db.execute("UPDATE devices SET name=? WHERE id=? AND account_id=?", (str(body["name"])[:80], did, ctx.account))
    if "notes" in body:
        ctx.db.execute("UPDATE devices SET notes=? WHERE id=? AND account_id=?", (str(body["notes"] or "")[:1000], did, ctx.account))
    if "settings" in body:
        raw = body["settings"] if isinstance(body["settings"], dict) else {}
        settings = {}
        w = raw.get("maintenance")
        if w:
            if not isinstance(w, dict) or _hhmm(w.get("start")) is None or _hhmm(w.get("end")) is None:
                raise ApiError(400, "maintenance window needs start and end like 22:00")
            settings["maintenance"] = {"start": w["start"], "end": w["end"]}
        ctx.db.execute("UPDATE devices SET settings=? WHERE id=? AND account_id=?", (json.dumps(settings), did, ctx.account))
        audit(ctx.db, ctx.account, ctx.actor, "device.settings", f"{did}: {settings or 'cleared'}")
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
    return [{"id": r["id"], "name": r["name"], "apps": norm_apps(json.loads(r["apps"])), "script_ids": json.loads(r["script_ids"])}
            for r in ctx.db.execute("SELECT * FROM profiles WHERE account_id=? ORDER BY name", (ctx.account,))]


def h_add_profile(ctx, body):
    (name,) = need(body, "name")
    apps, sids = norm_apps(body.get("apps")), body.get("script_ids") or []
    cur = ctx.db.execute("INSERT INTO profiles(account_id,name,apps,script_ids) VALUES(?,?,?,?)",
                         (ctx.account, name, json.dumps(apps), json.dumps(sids)))
    audit(ctx.db, ctx.account, ctx.actor, "profile.add", name)
    return {"id": cur.lastrowid}


def h_update_profile(ctx, body, pid):
    row = ctx.db.execute("SELECT * FROM profiles WHERE id=? AND account_id=?", (pid, ctx.account)).fetchone()
    if not row:
        raise ApiError(404, "profile not found")
    ctx.db.execute("UPDATE profiles SET name=?, apps=?, script_ids=? WHERE id=?",
                   (body.get("name") or row["name"], json.dumps(norm_apps(body["apps"]) if "apps" in body else json.loads(row["apps"])),
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
    return create_jobs(ctx.db, ctx.account, ctx.actor, body)


def create_jobs(db, account, actor, body, source="manual"):
    (steps,) = need(body, "steps")
    devs = {}
    for r in db.execute("SELECT * FROM devices WHERE account_id=?", (account,)):
        if r["id"] in (body.get("device_ids") or []) or dev_tags(r) & set(body.get("tags") or []):
            devs[r["id"]] = r
    missing = set(body.get("device_ids") or []) - set(devs)
    if missing:
        raise ApiError(404, f"device not found: {sorted(missing)}")
    if not devs:
        raise ApiError(400, "no matching devices")
    catalog = full_catalog(db, account)
    scripts = {r["id"]: dict(r) for r in db.execute("SELECT * FROM scripts WHERE account_id=?", (account,))}
    profiles = {r["id"]: dict(r) for r in db.execute("SELECT * FROM profiles WHERE account_id=?", (account,))}
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
        cur = db.execute(
            "INSERT INTO jobs(account_id,device_id,title,steps,status,created,rollout_id,batch,rollout_max_fail,source) VALUES(?,?,?,?,?,?,?,?,?,?)",
            (account, dev["id"], title, json.dumps(resolved), "queued", time.time(), rid, batch_of[i] if rollout else 0, max_fail, source))
        ids.append(cur.lastrowid)
    audit(db, account, actor, "job.create",
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


# ---------------------------------------------------------------- scheduled updates
DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"]


def h_list_schedules(ctx, body):
    return [{"id": r["id"], "tag": r["tag"], "time": f"{r['hour']:02d}:{r['minute']:02d}", "days": json.loads(r["days"]),
             "rollout": bool(r["rollout"])} for r in ctx.db.execute("SELECT * FROM schedules WHERE account_id=? ORDER BY id", (ctx.account,))]


def h_add_schedule(ctx, body):
    """Update everything on devices with <tag> (or 'all') at HH:MM server time, on the given weekdays (empty = daily)."""
    tag, at = need(body, "tag", "time")
    m = re.fullmatch(r"([01]?\d|2[0-3]):([0-5]\d)", str(at))
    if not m:
        raise ApiError(400, "time must look like 03:00")
    days = [str(d).lower() for d in (body.get("days") or [])]
    if any(d not in DAYS for d in days):
        raise ApiError(400, "days must be from: " + ", ".join(DAYS))
    tag = str(tag).strip().lower()
    if tag != "all":
        clean_tags([tag])
    cur = ctx.db.execute("INSERT INTO schedules(account_id,tag,hour,minute,days,rollout) VALUES(?,?,?,?,?,?)",
                         (ctx.account, tag, int(m.group(1)), int(m.group(2)), json.dumps(days), int(body.get("rollout", True))))
    audit(ctx.db, ctx.account, ctx.actor, "schedule.add", f"tag={tag} at {at} days={days or 'daily'}")
    return {"id": cur.lastrowid}


def h_del_schedule(ctx, body, sid):
    ctx.db.execute("DELETE FROM schedules WHERE id=? AND account_id=?", (sid, ctx.account))
    audit(ctx.db, ctx.account, ctx.actor, "schedule.delete", str(sid))
    return {}


def run_due_schedules(now=None):
    """Start 'update everything' jobs for schedules whose time has come today. Safe to call often; each runs once a day.
    A schedule still fires up to 6 hours late, so a short server outage doesn't skip a night's updates."""
    now = now or datetime.datetime.now()
    today = now.strftime("%Y-%m-%d")
    started = 0
    db = connect()
    try:
        for sc in db.execute("SELECT * FROM schedules").fetchall():
            due = now.replace(hour=sc["hour"], minute=sc["minute"], second=0, microsecond=0)
            days = json.loads(sc["days"])
            if sc["last_run"] == today or now < due or (now - due).total_seconds() > 6 * 3600:
                continue
            if days and DAYS[now.weekday()] not in days:
                continue
            db.execute("UPDATE schedules SET last_run=? WHERE id=?", (today, sc["id"]))
            try:
                create_jobs(db, sc["account_id"], "system", {"tags": [sc["tag"]], "steps": [{"type": "upgrade", "apps": "all"}],
                                                             "title": "Scheduled update", "rollout": bool(sc["rollout"])}, source="schedule")
                started += 1
            except ApiError:
                pass  # no devices with that tag right now
        db.commit()
    finally:
        db.close()
    return started


def prune_old(db):
    """Keep the database small: drop finished jobs older than the retention setting, audit entries older than a year."""
    days = int(get_setting(db, "retention_days", 90))
    db.execute("DELETE FROM jobs WHERE finished IS NOT NULL AND finished<? AND status IN ('done','failed','cancelled','halted')",
               (time.time() - days * 86400,))
    db.execute("DELETE FROM audit WHERE ts<?", (time.time() - 365 * 86400,))


def scheduler_loop():
    last_prune = 0
    while True:
        try:
            run_due_schedules()
            if time.time() - last_prune > 3600:
                db = connect()
                try:
                    prune_old(db)
                    db.commit()
                finally:
                    db.close()
                last_prune = time.time()
        except Exception as e:  # noqa: BLE001
            print("scheduler:", e)
        time.sleep(30)


# ---------------------------------------------------------------- settings & overview
def h_get_settings(ctx, body):
    return {"registration": registration_open(ctx.db), "poll_interval": poll_interval(ctx.db),
            "retention_days": int(get_setting(ctx.db, "retention_days", 90)), "is_admin": is_admin(ctx.db, ctx.account),
            "server_time": datetime.datetime.now().isoformat(timespec="seconds"), "timezone": time.tzname[0],
            "version": VERSION, "accounts": ctx.db.execute("SELECT COUNT(*) AS n FROM accounts").fetchone()["n"]}


def h_put_settings(ctx, body):
    if not is_admin(ctx.db, ctx.account):
        raise ApiError(403, "only the first account (the administrator) can change server settings")
    if "registration" in body:
        set_setting(ctx.db, "registration", bool(body["registration"]))
    if "poll_interval" in body:
        v = int(body["poll_interval"])
        if not 5 <= v <= 300:
            raise ApiError(400, "poll_interval must be 5-300 seconds")
        set_setting(ctx.db, "poll_interval", v)
    if "retention_days" in body:
        v = int(body["retention_days"])
        if not 7 <= v <= 3650:
            raise ApiError(400, "retention_days must be 7-3650")
        set_setting(ctx.db, "retention_days", v)
    audit(ctx.db, ctx.account, ctx.actor, "settings.update", json.dumps({k: body[k] for k in body if k in ("registration", "poll_interval", "retention_days")}))
    return {}


def h_overview(ctx, body):
    """One-screen fleet summary: health, what needs attention, where updates are pending."""
    assigns, catalog = load_assignments(ctx.db, ctx.account), full_catalog(ctx.db, ctx.account)
    rows = ctx.db.execute("SELECT * FROM devices WHERE account_id=?", (ctx.account,)).fetchall()
    views = [device_view(r, assigns, catalog) for r in rows]
    now = time.time()
    by_os, pending, attention = {}, {}, []
    ram_mb = 0
    for v in views:
        si = v["sysinfo"] or {}
        label = (si.get("os") or v["os"] or "unknown").split(" ")[0:3]
        by_os[" ".join(label)] = by_os.get(" ".join(label), 0) + 1
        ram_mb += si.get("mem_total_mb") or 0
        for k in v["outdated"]:
            pending[k] = pending.get(k, 0) + 1
        if not v["online"] and v["last_seen"] and now - v["last_seen"] > 86400:
            attention.append({"id": v["id"], "name": v["name"], "reason": "offline for over a day"})
        free, total = si.get("disk_free_gb") or 0, si.get("disk_total_gb") or 0
        if total and (free < 5 or (free < total * 0.1 and free < 50)):  # small in absolute terms, not just a percentage of a huge disk
            attention.append({"id": v["id"], "name": v["name"], "reason": f"low disk space ({si['disk_free_gb']} GB free)"})
        if v["managed"] and v["missing"]:
            attention.append({"id": v["id"], "name": v["name"], "reason": "out of sync: " + ", ".join(v["missing"][:3])})
    jobs = {}
    for r in ctx.db.execute("SELECT status, COUNT(*) AS n FROM jobs WHERE account_id=? AND created>? GROUP BY status", (ctx.account, now - 86400)):
        jobs[r["status"]] = r["n"]
    failed = [{"id": r["id"], "device_id": r["device_id"], "title": r["title"], "finished": r["finished"]} for r in ctx.db.execute(
        "SELECT id, device_id, title, finished FROM jobs WHERE account_id=? AND status='failed' ORDER BY id DESC LIMIT 5", (ctx.account,))]
    top = sorted(pending.items(), key=lambda kv: -kv[1])[:8]
    return {"devices": len(views), "online": sum(1 for v in views if v["online"]),
            "with_updates": sum(1 for v in views if v["outdated"]), "managed": sum(1 for v in views if v["managed"]),
            "by_os": sorted(({"name": k, "count": n} for k, n in by_os.items()), key=lambda x: -x["count"]),
            "pending": [{"key": k, "name": catalog[k]["name"], "count": n} for k, n in top if k in catalog],
            "attention": attention[:12], "jobs_24h": jobs, "recent_failed": failed, "ram_total_gb": round(ram_mb / 1024)}


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
    inv = {k: body.get(k) or {} for k in ("installed", "upgradable", "installed_names", "upgradable_names", "pinned", "pinned_names")}
    sysinfo = body.get("sysinfo")
    sysinfo = json.dumps(sysinfo) if isinstance(sysinfo, dict) and len(json.dumps(sysinfo)) < 20000 else None
    ctx.db.execute("UPDATE devices SET last_seen=?, inventory=?, os=COALESCE(?,os), manager=COALESCE(?,manager), agent_version=?, "
                   "sysinfo=COALESCE(?,sysinfo) WHERE id=?",
                   (time.time(), json.dumps(inv), body.get("os"), body.get("manager"), body.get("agent_version"), sysinfo, ctx.device))
    maybe_reconcile(ctx.db, ctx.account, ctx.db.execute("SELECT * FROM devices WHERE id=?", (ctx.device,)).fetchone())
    return {"interval": poll_interval(ctx.db)}


def h_agent_poll(ctx, body):
    ctx.db.execute("UPDATE devices SET last_seen=? WHERE id=?", (time.time(), ctx.device))
    settings = json.loads(ctx.db.execute("SELECT settings FROM devices WHERE id=?", (ctx.device,)).fetchone()["settings"] or "{}")
    window_open = in_window(settings)
    interval = poll_interval(ctx.db)
    for row in ctx.db.execute("SELECT * FROM jobs WHERE device_id=? AND status='queued' ORDER BY id", (ctx.device,)).fetchall():
        if row["source"] in ("reconcile", "schedule") and not window_open:
            continue  # automatic work waits for the device's maintenance window; jobs you start by hand run right away
        if row["rollout_id"] and ctx.db.execute(
                "SELECT 1 FROM jobs WHERE rollout_id=? AND batch<? AND status IN ('queued','running') LIMIT 1",
                (row["rollout_id"], row["batch"])).fetchone():
            continue  # earlier batch still in flight
        ctx.db.execute("UPDATE jobs SET status='running', started=? WHERE id=?", (time.time(), row["id"]))
        return {"job": {"id": row["id"], "title": row["title"], "steps": json.loads(row["steps"])}, "interval": interval}
    return {"job": None, "interval": interval}


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
    ("GET", r"/api/icons", h_icons, "user"),
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
    ("GET", r"/api/schedules", h_list_schedules, "user"),
    ("POST", r"/api/schedules", h_add_schedule, "user"),
    ("DELETE", r"/api/schedules/(\d+)", h_del_schedule, "user"),
    ("GET", r"/api/audit", h_audit, "user"),
    ("GET", r"/api/overview", h_overview, "user"),
    ("GET", r"/api/settings", h_get_settings, "user"),
    ("PUT", r"/api/settings", h_put_settings, "user"),
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
        if method == "GET" and re.fullmatch(r"/icons/[a-z0-9][a-z0-9._-]*\.svg", path):
            f = ROOT / "web" / "icons" / path.rsplit("/", 1)[1]
            if f.is_file():
                data = f.read_bytes()
                self.send_response(200)
                self.send_header("Content-Type", "image/svg+xml")
                self.send_header("Content-Length", str(len(data)))
                self.send_header("Cache-Control", "public, max-age=86400")
                self.send_header("X-Content-Type-Options", "nosniff")
                self.end_headers()
                self.wfile.write(data)
                return
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
    threading.Thread(target=scheduler_loop, daemon=True).start()
    print(f"Openite server running at http://{a.host}:{srv.server_address[1]}   (data: {STATE['db']})\nPress Ctrl+C to stop.")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()

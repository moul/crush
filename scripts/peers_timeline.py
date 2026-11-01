#!/usr/bin/env python3
"""Draw the peers demonstration's timelines (PARALLEL.md) as one SVG.

Each argument is LABEL=RUN_DIR[,RUN_DIR...]: a scenario run's directory
(the one with data/, data-p*/ and report.md), or several drawn as one group
on one clock, as for the control's three runs. For each session it reads
its database, for when it started and ended and for the waits its Nexts
made, and mem's journal, for every call that changed mem, with its time.
The time between two of a session's changes is drawn in the colour of the
todo the second one was about: a push, a pop or a done ends the work that
led to it. A hatched bar is a wait, ending when the call that waited came
back. All groups share one time scale, in minutes from each group's first
start.

    peers_timeline.py "one session=out/one/peers-geom-v0-..." \\
                      "three peers=out/three/peers-geom-v0-..." > timeline.svg
"""
import glob
import html
import json
import os
import re
import sqlite3
import sys

COLORS = {"circle": "#e8559c", "square": "#8b5cf6", "triangle": "#2bb5a0", "task": "#f2b134", "other": "#c9c6d4"}
WAITED = re.compile(r"waited ((?:\d+h)?(?:\d+m)?(?:\d+(?:\.\d+)?(?:ms|s))?) for ")
DUR = re.compile(r"(\d+(?:\.\d+)?)(h|ms|m|s)")


def seconds(text):
    unit = {"h": 3600, "m": 60, "s": 1, "ms": 0.001}
    return sum(float(n) * unit[u] for n, u in DUR.findall(text))


def todo_of(entry):
    for op in entry.get("ops") or []:
        if op.get("name"):
            return op["name"]
    for fr in entry.get("frames") or []:
        if fr.get("id"):
            return fr["id"]
    return ""


def load(run):
    """The journal's entries and, per session in the run's databases, its
    start, end and waits (end, length), all in epoch seconds."""
    journals = glob.glob(os.path.join(run, "data", "*", "gnovm", "*", "mem", "journal.jsonl"))
    if not journals:
        sys.exit(f"{run}: no mem journal")
    entries = []
    with open(journals[0]) as f:
        for line in f:
            line = line.strip()
            if line:
                e = json.loads(line)
                if "seq" in e:
                    entries.append(e)
    sessions = {}
    # A peer's database is in its own data directory (-D), one session's in
    # the project's .gnosh; a session found twice is the same session.
    dbs = glob.glob(os.path.join(run, "data*", "**", "gnosh.db"), recursive=True)
    dbs += glob.glob(os.path.join(run, "project", ".gnosh", "gnosh.db"))
    for db in sorted(dbs):
        c = opendb(db)
        for sid, title in c.execute("select id, title from sessions where parent_session_id is null or parent_session_id = ''"):
            rows = c.execute("select role, parts, created_at from messages where session_id = ? order by created_at, rowid", (sid,)).fetchall()
            if not rows:
                continue
            waits = []
            for role, parts, at in rows:
                if role != "tool" or "waited " not in parts:
                    continue
                for p in json.loads(parts):
                    text = (p.get("data") or {}).get("content") or ""
                    total = sum(seconds(m) for m in WAITED.findall(text))
                    if total:
                        waits.append((at, total))
            sessions[sid] = {"title": title, "start": rows[0][2], "end": rows[-1][2], "waits": waits}
        c.close()
    wall = ""
    report = os.path.join(run, "report.md")
    if os.path.exists(report):
        m = re.search(r"^## Turn 1 \([^,]*, ([0-9hms.]+)\)", open(report).read(), re.M)
        wall = m.group(1) if m else ""
    return entries, sessions, wall


def opendb(path):
    """A session database, read only. One closed cleanly has no -shm file,
    without which SQLite cannot open it read only in WAL mode; it is then
    read as a file that cannot change, which it is."""
    try:
        c = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
        c.execute("select 1 from sessions limit 1")
        return c
    except sqlite3.OperationalError:
        return sqlite3.connect(f"file:{path}?mode=ro&immutable=1", uri=True)


def shape(tid):
    for k in ("circle", "square", "triangle"):
        if k in tid:
            return k
    return "task" if tid else "other"


def label(tid):
    leaf = tid.rsplit("/", 1)[-1]
    for word in ("area", "perimeter"):
        if word in leaf:
            return word.capitalize()
    return shape(tid)


def group(dirs):
    """One group's rows: per session, its segments (start, end, todo) and
    waits (start, end), in seconds from the group's first start, and the
    group's length and wall times."""
    loaded = [load(d) for d in dirs]
    t0 = min(min([e["time"] for e in entries[:1]] + [s["start"] for s in sess.values()]) for entries, sess, _ in loaded)
    rows, end = [], 0
    for entries, sess, _ in loaded:
        for sid, s in sorted(sess.items(), key=lambda kv: (kv[1]["start"], kv[1]["title"])):
            marks = [(e["time"], todo_of(e)) for e in entries if e.get("session") == sid]
            segs, prev = [], s["start"]
            for t, tid in marks:
                if t > prev or not segs:
                    segs.append([prev - t0, t - t0, tid])
                prev = max(prev, t)
            if s["end"] > prev:
                segs.append([prev - t0, s["end"] - t0, ""])
            merged = []
            for a, b, tid in segs:
                if merged and merged[-1][2] == tid and merged[-1][1] == a:
                    merged[-1][1] = b
                else:
                    merged.append([a, b, tid])
            waits = [(at - t0 - w, at - t0) for at, w in s["waits"]]
            rows.append((s["title"], merged, waits))
            end = max(end, s["end"] - t0)
    return rows, end, [w for _, _, w in loaded]


def main(args):
    groups = []
    for a in args:
        name, _, dirs = a.partition("=")
        rows, end, walls = group(dirs.split(","))
        groups.append((name, rows, end, walls))
    longest = max(end for _, _, end, _ in groups) or 1
    W, left, rowh, gap = 900, 110, 24, 40
    scale = (W - left - 30) / longest
    height = 30 + sum(len(rows) * rowh + gap for _, rows, _, _ in groups) + 70
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{height}" viewBox="0 0 {W} {height}" font-family="ui-monospace, Menlo, monospace" font-size="12">',
           '<defs><pattern id="wait" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="6" height="6" fill="#f4f2f9"/><line x1="0" y1="0" x2="0" y2="6" stroke="#9d97b5" stroke-width="2"/></pattern></defs>',
           f'<rect width="{W}" height="{height}" fill="#ffffff"/>']
    y = 28
    for name, rows, end, walls in groups:
        took = ", ".join(w for w in walls if w)
        out.append(f'<text x="10" y="{y}" font-weight="bold" fill="#222">{html.escape(name)}</text>')
        if took:
            out.append(f'<text x="{W - 30}" y="{y}" fill="#555" text-anchor="end">{html.escape(took)}</text>')
        y += 8
        for i, (title, segs, waits) in enumerate(rows):
            out.append(f'<text x="10" y="{y + 16}" fill="#555">session {i + 1}</text>')
            for a, b, tid in segs:
                x, w = left + a * scale, max(1.5, (b - a) * scale)
                out.append(f'<rect x="{x:.1f}" y="{y + 3}" width="{w:.1f}" height="{rowh - 6}" fill="{COLORS[shape(tid)]}"><title>{html.escape(tid or "before its first todo, or after its last")}: {b - a:.0f}s</title></rect>')
                if tid and w > 70:
                    out.append(f'<text x="{x + 4:.1f}" y="{y + 16}" fill="#fff">{html.escape(label(tid))}</text>')
            for a, b in waits:
                out.append(f'<rect x="{left + a * scale:.1f}" y="{y + 3}" width="{max(1.5, (b - a) * scale):.1f}" height="{rowh - 6}" fill="url(#wait)"><title>waiting for a peer: {b - a:.0f}s</title></rect>')
            y += rowh
        y += gap
    # The time axis, a tick every 1, 2, 5 or 10 minutes.
    minutes = longest / 60
    step = next(s for s in (1, 2, 5, 10, 20, 30, 60) if minutes / s <= 10)
    m = 0
    while m * 60 <= longest + 1:
        x = left + m * 60 * scale
        out.append(f'<line x1="{x:.1f}" y1="34" x2="{x:.1f}" y2="{y - gap + 6}" stroke="#e8e8ee"/>')
        out.append(f'<text x="{x:.1f}" y="{y - gap + 22}" fill="#777" text-anchor="middle">{m} min</text>')
        m += step
    lx, ly = left, y - gap + 44
    for k, word in (("circle", "circle"), ("square", "square"), ("triangle", "triangle"), ("task", "the whole task"), ("other", "no todo yet, or done")):
        out.append(f'<rect x="{lx}" y="{ly - 10}" width="12" height="12" fill="{COLORS[k]}"/><text x="{lx + 16}" y="{ly}" fill="#444">{word}</text>')
        lx += 16 + 8 * len(word) + 18
    out.append(f'<rect x="{lx}" y="{ly - 10}" width="12" height="12" fill="url(#wait)"/><text x="{lx + 16}" y="{ly}" fill="#444">waiting</text>')
    out.append("</svg>")
    print("\n".join(out))


if __name__ == "__main__":
    main(sys.argv[1:])

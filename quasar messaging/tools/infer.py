"""Infer field paths from decoded frames.

usage: infer.py file.jsonl [svc] [field] [--dir C->S]
Prints per (svc, dir, top field): every nested path, count, value kinds, samples.
"""
import json
import sys
from collections import defaultdict

sys.stdout.reconfigure(encoding="utf-8")
args = sys.argv[1:]
path = args[0]
want_svc = args[1] if len(args) > 1 and not args[1].startswith("--") else None
want_f = args[2] if len(args) > 2 and not args[2].startswith("--") else None
want_dir = args[args.index("--dir") + 1] if "--dir" in args else None

stats = defaultdict(lambda: defaultdict(lambda: {"n": 0, "kinds": set(), "samples": []}))


def kind(v):
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, int):
        return "int"
    if isinstance(v, str):
        return "str"
    if isinstance(v, dict):
        if "$json" in v:
            return "json"
        if "$hex" in v:
            return "bytes"
        if "f32" in v or "f64" in v:
            return "fixed"
        return "msg"
    if isinstance(v, list):
        return "list"
    return type(v).__name__


def walk(key, v, prefix):
    k = kind(v)
    if k == "list":
        for x in v:
            walk(key, x, prefix + "[]")
        return
    st = stats[key][prefix]
    st["n"] += 1
    st["kinds"].add(k)
    if k in ("int", "str", "fixed", "bytes", "bool") and len(st["samples"]) < 4:
        s = v if k != "bytes" else f'<{v["$len"]}b {v["$hex"][:24]}>'
        if isinstance(s, str) and len(s) > 90:
            s = s[:90] + "…"
        if s not in st["samples"]:
            st["samples"].append(s)
    if k == "json" and len(st["samples"]) < 1:
        st["samples"].append(json.dumps(v["$json"], ensure_ascii=False)[:120] + "…")
    if k == "msg":
        for kk, vv in v.items():
            walk(key, vv, f"{prefix}.{kk}")


for line in open(path, encoding="utf-8"):
    r = json.loads(line)
    if want_svc and r["svc"] != want_svc:
        continue
    if want_dir and r["dir"] != want_dir:
        continue
    for m in r["msg"]:
        f = str(m.get("f"))
        if want_f and f != want_f:
            continue
        key = (r["svc"], r["dir"], f)
        walk(key, m.get("v"), f)

for key in sorted(stats):
    print(f"=== {key}")
    for p in sorted(stats[key], key=lambda s: [int(x) if x.isdigit() else 0 for x in s.replace("[]", "").split(".")]):
        st = stats[key][p]
        print(f"  {p:40s} n={st['n']:<5d} {','.join(sorted(st['kinds'])):10s} {st['samples']}")

"""Print decoded frames: show.py file.jsonl svc dir field [maxn] [maxchars] [skip_keys]"""
import json
import sys

sys.stdout.reconfigure(encoding="utf-8")
path, svc, direction, field = sys.argv[1:5]
maxn = int(sys.argv[5]) if len(sys.argv) > 5 else 5
maxc = int(sys.argv[6]) if len(sys.argv) > 6 else 1500
grep = sys.argv[7] if len(sys.argv) > 7 else None
n = 0
for line in open(path, encoding="utf-8"):
    r = json.loads(line)
    if r["svc"] != svc or (direction != "*" and r["dir"] != direction):
        continue
    if field != "*" and str(r["msg"][0]["f"]) != field:
        continue
    s = json.dumps(r["msg"], ensure_ascii=False)
    if grep and grep not in s:
        continue
    n += 1
    print(f'--- t={r["t"]} {r["dir"]} cport={r["cport"]} len={r["len"]}')
    print(s[:maxc])
    if n >= maxn:
        break

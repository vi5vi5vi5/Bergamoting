"""Decode quasar loopback IPC from a pcap.

Frames on every 127.0.0.1:98xx/99xx socket are [u32 LE length][QuasarMessage].
QuasarMessage may be wrapped in the envelope {252: body, 253: field number}.

usage: python qdecode.py capture.pcap quasar.cfg [--raw out.jsonl] [--skip PORT,...]
"""
import json
import struct
import sys
from collections import defaultdict

MAX_FRAME = 10 * 1024 * 1024


def load_ports(cfg_path):
    cfg = json.load(open(cfg_path, encoding="utf-8"))
    ports = {}
    for name, section in cfg.items():
        if isinstance(section, dict) and isinstance(section.get("port"), int):
            ports[section["port"]] = name
    ports.setdefault(1961, "glagol-ext")
    return ports


def pcap_packets(path):
    with open(path, "rb") as fh:
        hdr = fh.read(24)
        magic = struct.unpack("<I", hdr[:4])[0]
        if magic in (0xA1B2C3D4, 0xA1B23C4D):
            endian = "<"
        else:
            endian = ">"
        nano = magic in (0xA1B23C4D, 0x4D3CB2A1)
        linktype = struct.unpack(endian + "I", hdr[20:24])[0]
        while True:
            rec = fh.read(16)
            if len(rec) < 16:
                return
            sec, frac, incl, orig = struct.unpack(endian + "IIII", rec)
            data = fh.read(incl)
            if len(data) < incl:
                return
            ts = sec + frac / (1e9 if nano else 1e6)
            yield ts, linktype, data


def parse_tcp(linktype, data):
    if linktype == 1:  # ethernet
        eth_type = struct.unpack(">H", data[12:14])[0]
        off = 14
    elif linktype == 113:  # linux cooked
        eth_type = struct.unpack(">H", data[14:16])[0]
        off = 16
    elif linktype == 276:  # linux cooked v2
        eth_type = struct.unpack(">H", data[0:2])[0]
        off = 20
    else:
        return None
    if eth_type != 0x0800:
        return None
    ip = data[off:]
    ihl = (ip[0] & 0x0F) * 4
    total = struct.unpack(">H", ip[2:4])[0]
    if ip[9] != 6:
        return None
    src = ".".join(map(str, ip[12:16]))
    dst = ".".join(map(str, ip[16:20]))
    tcp = ip[ihl:total]
    sport, dport, seq, ack = struct.unpack(">HHII", tcp[:12])
    doff = (tcp[12] >> 4) * 4
    flags = tcp[13]
    payload = tcp[doff:]
    return src, sport, dst, dport, seq, flags, payload


# ---------------------------------------------------------------- protobuf

def varint(b, p):
    r = s = 0
    while True:
        if p >= len(b) or s > 63:
            raise ValueError("varint")
        c = b[p]
        p += 1
        r |= (c & 0x7F) << s
        if c < 0x80:
            return r, p
        s += 7


def fields(b):
    out = []
    p = 0
    while p < len(b):
        k, p = varint(b, p)
        n, wt = k >> 3, k & 7
        if n == 0:
            raise ValueError("field 0")
        if wt == 0:
            v, p = varint(b, p)
        elif wt == 1:
            if p + 8 > len(b):
                raise ValueError("i64")
            v = ("i64", b[p:p + 8])
            p += 8
        elif wt == 2:
            ln, p = varint(b, p)
            if p + ln > len(b):
                raise ValueError("len")
            v = b[p:p + ln]
            p += ln
        elif wt == 5:
            if p + 4 > len(b):
                raise ValueError("i32")
            v = ("i32", b[p:p + 4])
            p += 4
        else:
            raise ValueError("wiretype")
        out.append((n, wt, v))
    return out


def looks_text(b):
    if not b:
        return True
    try:
        s = b.decode("utf-8")
    except UnicodeDecodeError:
        return False
    bad = sum(1 for ch in s if ord(ch) < 32 and ch not in "\r\n\t")
    return bad == 0


def decode(b, depth=0, maxbytes=400):
    """Return a JSON-able tree for an unknown protobuf message."""
    res = defaultdict(list)
    for n, wt, v in fields(b):
        if wt == 0:
            res[n].append(v)
        elif wt in (1, 5):
            kind, raw = v
            if kind == "i64":
                d = struct.unpack("<d", raw)[0]
                q = struct.unpack("<q", raw)[0]
                res[n].append({"f64": d, "i64": q})
            else:
                f = struct.unpack("<f", raw)[0]
                i = struct.unpack("<i", raw)[0]
                res[n].append({"f32": round(f, 6), "i32": i})
        else:
            res[n].append(decode_bytes(v, depth, maxbytes))
    return {str(k): (v[0] if len(v) == 1 else v) for k, v in res.items()}


def decode_bytes(v, depth, maxbytes):
    text = looks_text(v)
    if text and v:
        s = v.decode("utf-8")
        st = s.lstrip()
        if st[:1] in "{[":
            try:
                return {"$json": json.loads(s)}
            except ValueError:
                pass
        # strings that are also valid protobuf are rare if printable; prefer text
        return s if len(s) <= 4000 else s[:4000] + f"...(+{len(s) - 4000})"
    if depth < 25 and v:
        try:
            return decode(v, depth + 1, maxbytes)
        except (ValueError, IndexError, struct.error):
            pass
    if not v:
        return ""
    h = v[:maxbytes].hex()
    return {"$hex": h, "$len": len(v)}


def unwrap(msg):
    """Return (field_number, body_bytes) list of top-level QuasarMessage fields."""
    fl = fields(msg)
    kinds = [v for n, wt, v in fl if n == 253 and wt == 0]
    bodies = [v for n, wt, v in fl if n == 252 and wt == 2]
    if kinds and bodies:
        rest = [(n, wt, v) for n, wt, v in fl if n not in (252, 253)]
        return [("env", kinds[0], bodies[0])] + [("plain", n, v) for n, wt, v in rest]
    return [("plain", n, v) for n, wt, v in fl]


# ---------------------------------------------------------------- streams

class Flow:
    def __init__(self):
        self.next_seq = None
        self.pending = {}
        self.buf = b""
        self.synced = False

    def add(self, seq, flags, payload, ts, emit):
        if flags & 0x02:  # SYN
            self.next_seq = (seq + 1) & 0xFFFFFFFF
            self.buf = b""
            self.synced = True
            return
        if not payload:
            return
        if self.next_seq is None:
            self.next_seq = seq
            self.synced = False
        self.pending[seq] = payload
        while self.next_seq in self.pending:
            data = self.pending.pop(self.next_seq)
            self.next_seq = (self.next_seq + len(data)) & 0xFFFFFFFF
            self.feed(data, ts, emit)
        if len(self.pending) > 500:  # lost segment: resync
            self.pending.clear()
            self.next_seq = None
            self.buf = b""
            self.synced = False

    def feed(self, data, ts, emit):
        if not self.synced:
            # assume segments start on frame boundaries; validate
            if len(data) >= 4:
                ln = struct.unpack("<I", data[:4])[0]
                if 0 < ln <= MAX_FRAME and (len(data) < 4 + ln or self._valid(data[4:4 + ln])):
                    self.synced = True
                    self.buf = b""
            if not self.synced:
                return
        self.buf += data
        while len(self.buf) >= 4:
            ln = struct.unpack("<I", self.buf[:4])[0]
            if ln == 0 or ln > MAX_FRAME:
                self.synced = False
                self.buf = b""
                return
            if len(self.buf) < 4 + ln:
                return
            frame = self.buf[4:4 + ln]
            self.buf = self.buf[4 + ln:]
            emit(ts, frame)

    @staticmethod
    def _valid(b):
        try:
            fields(b)
            return True
        except (ValueError, IndexError):
            return False


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    args = sys.argv[1:]
    pcap, cfg = args[0], args[1]
    raw_out = None
    skip = set()
    if "--raw" in args:
        raw_out = open(args[args.index("--raw") + 1], "w", encoding="utf-8")
    if "--skip" in args:
        skip = {int(x) for x in args[args.index("--skip") + 1].split(",")}
    ports = load_ports(cfg)
    flows = defaultdict(Flow)
    t0 = None
    for ts, lt, data in pcap_packets(pcap):
        p = parse_tcp(lt, data)
        if not p:
            continue
        src, sport, dst, dport, seq, flags, payload = p
        if sport in ports:
            service, direction, client = ports[sport], "S->C", dport
        elif dport in ports:
            service, direction, client = ports[dport], "C->S", sport
        else:
            continue
        if sport in skip or dport in skip:
            continue
        t0 = t0 or ts
        key = (src, sport, dst, dport)

        def emit(fts, frame, service=service, direction=direction, client=client):
            try:
                parts = unwrap(frame)
                out = []
                for kind, num, body in parts:
                    if isinstance(body, (bytes, bytearray)):
                        out.append({"f": num, "env": kind == "env", "v": decode_bytes(body, 0, 200)})
                    else:
                        out.append({"f": num, "env": kind == "env", "v": body})
            except (ValueError, IndexError, struct.error) as e:
                out = [{"err": str(e), "hex": frame[:64].hex()}]
            rec = {"t": round(fts - t0, 3), "svc": service, "dir": direction, "cport": client,
                   "len": len(frame), "msg": out}
            if raw_out:
                rec2 = dict(rec)
                rec2["hex"] = frame.hex()
                raw_out.write(json.dumps(rec2, ensure_ascii=False) + "\n")
            print(json.dumps(rec, ensure_ascii=False))

        flows[key].add(seq, flags, payload, ts, emit)


if __name__ == "__main__":
    main()

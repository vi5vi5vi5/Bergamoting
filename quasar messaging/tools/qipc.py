"""Minimal client for quasar loopback IPC (through an SSH tunnel).

Frame: [u32 LE length][QuasarMessage]; QuasarMessage here is the envelope
{252: body bytes, 253: QuasarMessage field number}.
"""
import socket
import struct
import time


def varint(v):
    out = bytearray()
    while v >= 0x80:
        out.append((v & 0x7F) | 0x80)
        v >>= 7
    out.append(v)
    return bytes(out)


def f_bytes(num, data):
    if isinstance(data, str):
        data = data.encode()
    return varint(num << 3 | 2) + varint(len(data)) + data


def f_int(num, value):
    return varint(num << 3) + varint(value)


def envelope(field, body):
    return f_bytes(252, body) + f_int(253, field)


def frame(msg):
    return struct.pack("<I", len(msg)) + msg


def remoting(obj, method_field, method_body=b"", extra=b""):
    return envelope(210, f_bytes(7, obj) + (f_bytes(method_field, method_body) if method_field else b"") + extra)


def send(port, msg, linger=0.3, host="127.0.0.1"):
    s = socket.create_connection((host, port), timeout=5)
    s.sendall(frame(msg))
    time.sleep(linger)
    s.close()

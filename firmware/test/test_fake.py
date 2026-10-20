#!/usr/bin/env python3
"""Protocol test for a2b-master v2 against the native fake_pico (stdlib only).

    python3 test_fake.py --fake <dir>/fake_pico [--xml ../../examples/adi_a2b_commandlist.xml]
                         [--tunnel-port 9500]
    python3 test_fake.py --connect 127.0.0.1:9600     # an already running fake_pico

With --fake the script starts fake_pico itself (on --port, default 9611) with a temporary
--flash file, and also checks autorun by restarting it.  --tunnel-port N additionally runs
the tunnel checks against a tunnel device simulator already listening on 127.0.0.1:N
(e.g. native_sim --port 9500 --node 0).

xml_to_records() is the reference SigmaStudio(+) XML -> record conversion from
docs/USB_PROTOCOL.md.
"""
import argparse
import os
import socket
import subprocess
import sys
import tempfile
import time
import xml.etree.ElementTree as ET

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_XML = os.path.join(HERE, "..", "..", "examples", "adi_a2b_commandlist.xml")


# ------------------------------------------------------------------ helpers

def crc16_ccitt(data, crc=0xFFFF):
    for b in data:
        crc ^= b << 8
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1021) & 0xFFFF if crc & 0x8000 else (crc << 1) & 0xFFFF
    return crc


def crc8(data, crc=0):
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = ((crc << 1) ^ 0x07) & 0xFF if crc & 0x80 else (crc << 1) & 0xFF
    return crc


def xml_to_records(path):
    """Return (records bytes, [(label, record bytes)], warnings, errors).  n = action count."""
    root = ET.parse(path).getroot()
    out, items, warns, errs = bytearray(), [], [], []
    n = 0
    for page in root.iter("page"):
        for act in page.iter("action"):
            instr = act.get("instr")
            label = "%s/%s" % (act.get("CellName", ""), (act.get("ParamName") or "").strip())
            body = bytes(int(t, 16) for t in (act.text or "").split())
            if act.get("Protocol", "I2C") != "I2C" and instr != "delay":
                errs.append("action %d (%s): protocol %s not supported" % (n, label, act.get("Protocol")))
                n += 1
                continue
            if instr == "writeXbytes":
                aw, addr, i2c = int(act.get("addr_width")), int(act.get("addr")), int(act.get("i2caddr"))
                if int(act.get("len")) != aw + len(body):
                    warns.append("action %d (%s): len %s != %d" % (n, label, act.get("len"), aw + len(body)))
                # n <= 255: record 01; longer writes (e.g. DSP program download): record 04
                # (n u16 LE, up to 4096), still one I2C transaction.  Never split.
                payload = addr.to_bytes(aw, "big") + body
                if len(payload) <= 255:
                    rec = bytes([0x01, i2c, len(payload)]) + payload
                elif len(payload) <= 4096:
                    rec = bytes([0x04, i2c, len(payload) & 0xFF, len(payload) >> 8]) + payload
                else:
                    errs.append("action %d (%s): %d bytes > 4096" % (n, label, len(payload)))
                    n += 1
                    continue
            elif instr == "read":
                aw, addr, i2c = int(act.get("addr_width")), int(act.get("addr")), int(act.get("i2caddr"))
                rn = int(act.get("len")) - aw
                rec = bytes([0x02, i2c, aw]) + addr.to_bytes(aw, "big") + bytes([rn])
            elif instr == "delay":
                ms = int.from_bytes(body, "big")
                if ms > 0xFFFF:
                    errs.append("action %d (%s): delay %d ms too long" % (n, label, ms))
                rec = bytes([0x03, ms & 0xFF, (ms >> 8) & 0xFF])
            else:
                errs.append("action %d (%s): unknown instr %r" % (n, label, instr))
                n += 1
                continue
            out += rec
            items.append((label, rec))
            n += 1
    xml_to_records.actions = n
    return bytes(out), items, warns, errs


class Conn:
    def __init__(self, host, port, timeout=10.0):
        deadline = time.time() + 5
        while True:
            try:
                self.s = socket.create_connection((host, port), timeout=timeout)
                break
            except OSError:
                if time.time() > deadline:
                    raise
                time.sleep(0.05)
        self.buf = b""

    def readline(self):
        while b"\n" not in self.buf:
            d = self.s.recv(4096)
            if not d:
                raise EOFError("connection closed")
            self.buf += d
        line, self.buf = self.buf.split(b"\n", 1)
        assert line.endswith(b"\r"), "reply not CRLF terminated: %r" % line
        return line[:-1].decode()

    def cmd(self, line):
        self.s.sendall(line.encode() + b"\n")
        return self.readline()

    def close(self):
        self.s.close()


FAILS = []


def check(cond, what):
    print("  %-4s %s" % ("ok" if cond else "FAIL", what))
    if not cond:
        FAILS.append(what)


def expect(c, line, want):
    got = c.cmd(line)
    ok = got == want if not want.endswith("*") else got.startswith(want[:-1])
    check(ok, "%-28s -> %-22s (want %s)" % (line[:28], got[:40], want))
    return got


def load_list(c, data, chunk=300):
    c.cmd("LC")
    total = None
    for i in range(0, len(data), chunk):
        total = c.cmd("LA " + data[i:i + chunk].hex().upper())
    return total


def run_lx(c, arg=""):
    c.s.sendall(("LX " + arg).strip().encode() + b"\n")
    lines = []
    while True:
        l = c.readline()
        lines.append(l)
        if l.startswith("DONE") or l.startswith("ERR"):
            return lines


# ------------------------------------------------------------------ tests

def test_raw(c):
    print("raw I2C (v1 commands)")
    expect(c, "P", "PONG a2b-master v2 400000")
    expect(c, "W 6A 01 00", "OK")
    expect(c, "R 6A 3 02", "OK AD3721")
    expect(c, "r 6a 3 02", "OK AD3721")
    expect(c, "R 6A 2 -", "OK 0000")          # pointer now at 0x05
    expect(c, "R 50 1 00", "NAK")
    expect(c, "W 50 00", "NAK")
    expect(c, "W 38 F5 00 00 01", "OK")
    expect(c, "R 06 2 00", "OK 0000")
    expect(c, "W ZZ 00", "ERR ADDR")
    expect(c, "W 6A", "ERR DATA")
    expect(c, "W 6A 0", "ERR DATA")
    expect(c, "R 6A 0 02", "ERR LEN")
    expect(c, "R 6A 301 02", "ERR LEN")
    expect(c, "R 6A 1", "ERR DATA")
    expect(c, "W 6A " + "00" * 301, "ERR LEN")
    expect(c, "X", "ERR CMD")
    expect(c, "PP", "ERR CMD")
    expect(c, "B 100", "OK 100000")
    expect(c, "B 5", "ERR ARG")
    expect(c, "B 400", "OK 400000")
    # sub-node register access through the BUS proxy, PERI=0
    expect(c, "W 6A 01 01", "OK")
    expect(c, "R 6B 3 02", "OK AD3721")
    expect(c, "W 6A 01 05", "OK")
    expect(c, "R 6B 1 02", "NAK")              # node 5 does not exist
    expect(c, "W 6A 01 21", "OK")              # PERI, node 1, CHIP=0 -> nothing there
    expect(c, "R 6B 1 00", "NAK")
    expect(c, "W 6A 01 00", "OK")


def test_list(c, xml):
    print("command list from %s" % os.path.basename(xml))
    data, items, warns, errs = xml_to_records(xml)
    for w in warns:
        print("  warn:", w)
    check(not errs, "XML converts without errors (%d)" % len(errs))
    nact = xml_to_records.actions
    check(nact == 161 and len(items) == 161, "161 actions -> %d records, %d bytes" % (len(items), len(data)))
    lw = [i for i, (l, r) in enumerate(items) if r[0] == 0x04]
    check(len(lw) == 1 and items[lw[0]][1][2] | items[lw[0]][1][3] << 8 == 366,
          "one long write (op 04, 366 bytes) at record %s" % lw)
    N = len(items)
    expect(c, "LC", "OK")
    expect(c, "LI", "OK 0 0 FFFF *")
    got = load_list(c, data, chunk=300)
    check(got == "OK %d" % len(data), "LA total %r" % got)
    crc = crc16_ccitt(data)
    li = c.cmd("LI").split()
    check(li[:4] == ["OK", str(len(data)), str(N), "%04X" % crc], "LI %s (crc %04X)" % (" ".join(li), crc))

    t0 = time.time()
    lines = run_lx(c)
    dt = time.time() - t0
    s_lines = [l for l in lines if l.startswith("S ")]
    check(len(s_lines) == N, "LX streamed %d S lines" % len(s_lines))
    check(all(l.split()[1] == str(i) for i, l in enumerate(s_lines)), "S lines numbered 0..%d" % (N - 1))
    done = lines[-1].split()
    check(done[:4] == ["DONE", str(N), str(N), "0"], "%s (%.2f s wall)" % (lines[-1], dt))
    bad = [l for l in s_lines if l.split()[2] not in ("OK", "D")]
    check(not bad, "no failed records %s" % bad[:3])
    # spot checks: delays and the ID / INTTYPE reads
    for i, (label, rec) in enumerate(items):
        if rec[0] == 0x03:
            want = "S %d D %d" % (i, rec[1] | rec[2] << 8)
            if s_lines[i] != want:
                check(False, "delay line %r != %r" % (s_lines[i], want))
    vend = [i for i, (l, r) in enumerate(items) if r[0] == 2 and r[1] == 0x6A and r[3] == 0x02]
    check(vend and all(s_lines[i] == "S %d OK AD3721" % i for i in vend),
          "main VENDOR reads -> AD3721 (%d of them)" % len(vend))
    sub = [i for i, (l, r) in enumerate(items) if r[0] == 2 and r[1] == 0x6B and r[3] == 0x02]
    check(sub and all(s_lines[i].startswith("S %d OK AD3721" % i) for i in sub),
          "sub-node VENDOR reads via 0x6B: %s" % (s_lines[sub[0]] if sub else None))
    it = [i for i, (l, r) in enumerate(items) if r[0] == 2 and r[1] == 0x6A and r[3] == 0x17]
    disc = next(i for i, (l, r) in enumerate(items) if r[0] == 1 and r[1] == 0x6A and r[3] == 0x13)
    after = [s_lines[i] for i in it if i > disc]
    check(after and all(l.endswith("OK 18") for l in after), "INTTYPE after DISCVRY -> 18 (%s)" % after[:1])
    return data, N


def test_list_errors(c):
    print("list errors / STOP")
    good = bytes.fromhex("01 6A 02 01 00".replace(" ", ""))
    nak = bytes.fromhex("01 50 01 00")
    rd = bytes.fromhex("02 6A 01 02 03")
    dly = bytes.fromhex("03 05 00")
    load_list(c, good + nak + rd + dly)
    lines = run_lx(c)
    check(lines == ["S 0 OK", "S 1 NAK", "S 2 OK AD3721", "S 3 D 5", lines[-1]] and
          lines[-1].startswith("DONE 4 3 1 "), "LX: %s" % lines)
    lines = run_lx(c, "STOP")
    check(lines[:2] == ["S 0 OK", "S 1 NAK"] and lines[2].startswith("DONE 4 1 1 "), "LX STOP: %s" % lines)
    lines = run_lx(c, "stop")
    check(len(lines) == 3, "LX stop (lowercase)")
    expect(c, "LX GO", "ERR ARG")
    # plain read (wn = 0) and end record with trailing junk ignored
    load_list(c, bytes.fromhex("01 6A 01 02") + bytes.fromhex("02 6A 00 03") + b"\x00\xff\xff")
    lines = run_lx(c)
    check(lines[:2] == ["S 0 OK", "S 1 OK AD3721"] and lines[2].startswith("DONE 2 2 0"), "plain read + end: %s" % lines)
    check(c.cmd("LI").split()[2] == "2", "LI counts 2 records before 00")
    # long write (op 04) spanning several LA lines; 4096 max
    big = bytes([0x04, 0x38, 300 & 0xFF, 300 >> 8]) + bytes(range(256)) + bytes(44)
    big4k = bytes([0x04, 0x38, 0x00, 0x10]) + bytes(4096)
    load_list(c, big + rd + big4k)
    lines = run_lx(c)
    check(lines[:3] == ["S 0 OK", "S 1 OK AD3721", "S 2 OK"] and lines[3].startswith("DONE 3 3 0"),
          "long writes 300 + 4096 bytes: %s" % lines)
    load_list(c, bytes([0x04, 0x50, 0x00, 0x01]) + bytes(256))
    check(run_lx(c)[0] == "S 0 NAK", "long write to absent device -> NAK")
    load_list(c, bytes([0x04, 0x38, 0x01, 0x10]) + bytes(4097))
    expect(c, "LX", "ERR LIST 0")
    # malformed lists
    for hexs, off in (("04 38 00 00", 0), ("04 38 05 00 01 02", 0), ("04 B8 01 00 00", 0), ("03 01 00 04 38", 3),("05", 0), ("01 6A 03 01", 0), ("03 05 00 01 6A 00", 3), ("01 80 01 00", 0),
                      ("02 6A 01 02 00", 0), ("03 05", 0)):
        load_list(c, bytes.fromhex(hexs.replace(" ", "")))
        expect(c, "LX", "ERR LIST %d" % off)
    expect(c, "LS", "ERR LIST 0")
    expect(c, "LC", "OK")
    expect(c, "LA", "ERR DATA")
    expect(c, "LA 0", "ERR DATA")
    expect(c, "LA " + "00" * 301, "ERR LEN")
    for _ in range(54):
        c.cmd("LA " + "00" * 300)
    expect(c, "LA " + "00" * 300, "ERR FULL")
    expect(c, "LI", "OK 16200 0 *")
    expect(c, "LC", "OK")
    expect(c, "LX", "DONE 0 0 0 0")
    long_line = "LA " + "0" * 2000
    expect(c, long_line, "ERR LEN")


def test_flash(c, data, N):
    print("flash store")
    load_list(c, data)
    expect(c, "LS", "OK %d" % len(data))
    crc = "%04X" % crc16_ccitt(data)
    expect(c, "LC", "OK")
    expect(c, "LI", "OK 0 0 FFFF 0 %d %s" % (len(data), crc))
    expect(c, "LL", "OK %d" % len(data))
    expect(c, "LI", "OK %d %d %s 0 %d %s" % (len(data), N, crc, len(data), crc))
    expect(c, "LAUTO 2", "ERR ARG")
    expect(c, "LAUTO", "ERR ARG")
    expect(c, "LAUTO 1", "OK")
    expect(c, "LI", "OK %d %d %s 1 %d %s" % (len(data), N, crc, len(data), crc))
    # LS keeps the autorun flag
    small = bytes.fromhex("0301 00".replace(" ", ""))
    load_list(c, small)
    expect(c, "LS", "OK 3")
    expect(c, "LI", "OK 3 1 %04X 1 3 %04X" % (crc16_ccitt(small), crc16_ccitt(small)))
    load_list(c, data)
    expect(c, "LS", "OK %d" % len(data))


def test_tunnel(c, tport):
    print("tunnel via BUS proxy -> native_sim on port %d" % tport)
    expect(c, "W 6A 01 00", "OK")           # NODEADR = node 0
    expect(c, "W 6B 00 2A", "OK")           # CHIP = 0x2A on node 0
    expect(c, "W 6A 01 20", "OK")           # PERI | node 0
    op = bytes([0x01])
    req = (op + bytes([crc8(op)])).hex().upper()
    r = c.cmd("R 6B 6 " + req)
    ok = r.startswith("OK ") and len(r) == 15
    resp = bytes.fromhex(r[3:]) if ok else b""
    check(ok and resp[0] == 0xA2 and resp[1] == 0x01 and crc8(resp[:5]) == resp[5],
          "SUMMARY -> %s (magic A2, crc ok)" % r)
    # SOCK_STATUS 0
    w = bytes([0x02, 0x00])
    r = c.cmd("R 6B 14 " + (w + bytes([crc8(w)])).hex())
    resp = bytes.fromhex(r[3:]) if r.startswith("OK ") else b""
    check(len(resp) == 14 and resp[0] == 0 and crc8(resp[:13]) == resp[13], "SOCK_STATUS 0 -> %s" % r)
    # write op with a valid crc (RESET_ALL) is ACKed
    w = bytes([0x1F, 0xA5])
    expect(c, "W 6B " + (w + bytes([crc8(w)])).hex(), "OK")
    # a CHIP that is not the tunnel address -> NAK
    expect(c, "W 6A 01 00", "OK")
    expect(c, "W 6B 00 39", "OK")
    expect(c, "W 6A 01 20", "OK")
    expect(c, "R 6B 6 " + req, "NAK")
    expect(c, "W 6A 01 00", "OK")
    expect(c, "W 6B 00 2A", "OK")
    # same thing from a command list
    load_list(c, bytes([0x01, 0x6A, 2, 0x01, 0x20, 0x02, 0x6B, 2, 0x01, crc8(op), 6]))
    lines = run_lx(c)
    check(len(lines) == 3 and lines[1].startswith("S 1 OK A201") and lines[2].startswith("DONE 2 2 0"),
          "tunnel SUMMARY from LX: %s" % lines[1])
    expect(c, "W 6A 01 00", "OK")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fake", help="fake_pico binary to start")
    ap.add_argument("--connect", help="HOST:PORT of a running fake_pico")
    ap.add_argument("--port", type=int, default=9611, help="port for a started fake_pico")
    ap.add_argument("--xml", default=DEFAULT_XML)
    ap.add_argument("--tunnel-port", type=int, help="native_sim port for the tunnel test")
    ap.add_argument("--log", action="store_true", help="pass --log to fake_pico")
    a = ap.parse_args()
    if not a.fake and not a.connect:
        ap.error("need --fake or --connect")

    procs = []
    tmp = tempfile.mkdtemp(prefix="fake_pico_")
    flash = os.path.join(tmp, "flash.bin")

    def start():
        args = [a.fake, "--port", str(a.port), "--flash", flash]
        if a.tunnel_port:
            args += ["--tunnel", "sim:127.0.0.1:%d" % a.tunnel_port]
        if a.log:
            args.append("--log")
        p = subprocess.Popen(args, stdout=subprocess.PIPE, text=True)
        procs.append(p)
        return p

    try:
        if a.fake:
            start()
            host, port = "127.0.0.1", a.port
        else:
            host, port = a.connect.rsplit(":", 1)
            port = int(port)
        c = Conn(host, port)
        test_raw(c)
        data, N = test_list(c, a.xml)
        test_list_errors(c)
        if a.tunnel_port:
            test_tunnel(c, a.tunnel_port)
        test_flash(c, data, N)   # leaves the example stored with autorun on
        c.close()
        if a.fake:
            print("autorun (restart fake_pico with the stored flash)")
            p = procs.pop()
            p.terminate()
            p.wait()
            p = start()
            c = Conn("127.0.0.1", a.port)   # connects once autorun finished (single thread)
            li = c.cmd("LI").split()
            check(li[1] == str(len(data)) and li[4] == "1", "after restart LI %s" % " ".join(li))
            c.close()
            p.terminate()
            out = p.communicate()[0].splitlines()
            procs.remove(p)
            auto = [l for l in out if l.startswith("AUTO ")]
            check(len(auto) == N + 1 and auto[-1].startswith("AUTO DONE %d %d 0 " % (N, N)),
                  "autorun output: %d lines, %s" % (len(auto), auto[-1] if auto else None))
    finally:
        for p in procs:
            p.terminate()
            p.wait()
        for f in os.listdir(tmp):
            os.unlink(os.path.join(tmp, f))
        os.rmdir(tmp)
    print("\n%s (%d failures)" % ("PASS" if not FAILS else "FAIL", len(FAILS)))
    return 1 if FAILS else 0


if __name__ == "__main__":
    sys.exit(main())

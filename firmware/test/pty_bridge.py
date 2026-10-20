#!/usr/bin/env python3
"""Expose a TCP line-protocol endpoint (e.g. fake_pico) as a pseudo-terminal, so tools that
want a serial port (pyserial, the reference a2b_web_tunnel.py 'serial:' transport, picocom)
can talk to it.  Linux/macOS, stdlib only.

    python3 pty_bridge.py [--connect 127.0.0.1:9600] [--link /tmp/fake_pico_tty]

Prints the pty path (and creates the optional symlink); Ctrl-C quits.
"""
import argparse
import os
import select
import socket
import sys
import tty


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--connect", default="127.0.0.1:9600")
    ap.add_argument("--link", help="also create this symlink to the pty")
    a = ap.parse_args()
    host, port = a.connect.rsplit(":", 1)
    sock = socket.create_connection((host, int(port)))
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    master, slave = os.openpty()
    tty.setraw(slave)
    path = os.ttyname(slave)
    if a.link:
        try:
            os.unlink(a.link)
        except FileNotFoundError:
            pass
        os.symlink(path, a.link)
    print(path, flush=True)
    try:
        while True:
            r, _, _ = select.select([master, sock], [], [])
            if master in r:
                try:
                    d = os.read(master, 4096)
                except OSError:      # no client has the slave open right now
                    d = b""
                if d:
                    sock.sendall(d)
            if sock in r:
                d = sock.recv(4096)
                if not d:
                    print("TCP side closed", file=sys.stderr)
                    return
                os.write(master, d)
    except KeyboardInterrupt:
        pass
    finally:
        if a.link:
            try:
                os.unlink(a.link)
            except OSError:
                pass


if __name__ == "__main__":
    main()

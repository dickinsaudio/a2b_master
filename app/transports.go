package main

// Node-level transports for the tunnel (port of reference/transports.py).
//
//   Write(node, data)          one tunnel write transaction (op, args, crc)
//   WriteRead(node, data, n)   write + repeated start + read n bytes
//
// Errors: ErrNack (just retry) or *I2CError (anything else).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

type NodeTransport interface {
	Write(node int, data []byte) error
	WriteRead(node int, data []byte, n int) ([]byte, error)
	Close()
	Describe() string
}

// ---- sim:HOST:PORT --------------------------------------------------------------
// Request : [kind 'W'|'R'][node u8][wlen u16 LE][rlen u16 LE][wdata]
// Response: [status 0=ACK 1=NACK][rdata (rlen bytes, ACK only)]

type SimTransport struct {
	addr    string
	timeout time.Duration
	conn    net.Conn
}

func NewSimTransport(addr string) *SimTransport {
	if !strings.Contains(addr, ":") {
		addr += ":9500"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return &SimTransport{addr: addr, timeout: time.Second}
}

func (s *SimTransport) Describe() string { return "sim:" + s.addr }

func (s *SimTransport) xfer(kind byte, node int, w []byte, rlen int) ([]byte, error) {
	if s.conn == nil {
		c, err := net.DialTimeout("tcp", s.addr, s.timeout)
		if err != nil {
			return nil, &I2CError{fmt.Sprintf("sim connect %s: %v", s.addr, err)}
		}
		c.(*net.TCPConn).SetNoDelay(true)
		s.conn = c
	}
	fail := func(err error) ([]byte, error) {
		s.Close()
		return nil, &I2CError{"sim transport: " + err.Error()}
	}
	s.conn.SetDeadline(time.Now().Add(s.timeout))
	buf := make([]byte, 6+len(w))
	buf[0], buf[1] = kind, byte(node)
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(w)))
	binary.LittleEndian.PutUint16(buf[4:], uint16(rlen))
	copy(buf[6:], w)
	if _, err := s.conn.Write(buf); err != nil {
		return fail(err)
	}
	var st [1]byte
	if _, err := io.ReadFull(s.conn, st[:]); err != nil {
		return fail(err)
	}
	if st[0] == 1 {
		return nil, ErrNack
	}
	if st[0] != 0 {
		return fail(fmt.Errorf("bad sim status byte 0x%02x", st[0]))
	}
	if rlen == 0 {
		return nil, nil
	}
	r := make([]byte, rlen)
	if _, err := io.ReadFull(s.conn, r); err != nil {
		return fail(err)
	}
	return r, nil
}

func (s *SimTransport) Write(node int, data []byte) error {
	_, err := s.xfer('W', node, data, 0)
	return err
}

func (s *SimTransport) WriteRead(node int, data []byte, n int) ([]byte, error) {
	return s.xfer('R', node, data, n)
}

func (s *SimTransport) Close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// ---- A2B: through the Pico (raw I2C) and the main AD242x ------------------------
//
//  1. BASE: NODEADR(0x01) = N
//  2. BUS : CHIP(0x00)    = tunnel_addr          (once per node)
//  3. BUS : I2CCFG(0x3F) |= 0x01 (400 kHz)       (optional, once per node)
//  4. BASE: NODEADR(0x01) = 0x20 | N             (PERI=1)
//  5. tunnel transactions -> BUS
//
// The NODEADR value is cached. Every non-tunnel port job (console, command list) may
// have changed NODEADR/CHIP, so the cache and the per-node setup are dropped whenever
// the port generation changes.

const (
	regChip     = 0x00
	regNodeAdr  = 0x01
	regI2CCfg   = 0x3F
	nodeAdrPeri = 0x20
)

type A2BNodeTransport struct {
	port       *Port
	base, bus  byte
	tunnelAddr byte
	set400k    bool

	nodeadr   int // -1 unknown
	setupDone map[int]bool
	logged    map[int]bool
	gen       uint64
}

func NewA2BNodeTransport(p *Port, base, tunnelAddr byte, set400k bool) *A2BNodeTransport {
	return &A2BNodeTransport{port: p, base: base, bus: base + 1, tunnelAddr: tunnelAddr,
		set400k: set400k, nodeadr: -1, setupDone: map[int]bool{}, logged: map[int]bool{}}
}

func (t *A2BNodeTransport) Describe() string {
	return fmt.Sprintf("a2b(%s, base=0x%02X, chip=0x%02X)", t.port.URL, t.base, t.tunnelAddr)
}

func (t *A2BNodeTransport) setNodeAdr(l *LineIO, v int) error {
	if t.nodeadr != v {
		t.nodeadr = -1 // unknown until the write is known to have worked
		if err := I2CWrite(l, t.base, []byte{regNodeAdr, byte(v)}); err != nil {
			return err
		}
		t.nodeadr = v
	}
	return nil
}

func (t *A2BNodeTransport) selectNode(l *LineIO, node int) error {
	if node < 0 || node > 15 {
		return &I2CError{"A2B node number must be 0..15"}
	}
	if t.gen != t.port.gen {
		t.gen = t.port.gen
		t.nodeadr = -1
		t.setupDone = map[int]bool{}
	}
	if !t.setupDone[node] {
		if err := t.setNodeAdr(l, node); err != nil {
			return err
		}
		if err := I2CWrite(l, t.bus, []byte{regChip, t.tunnelAddr}); err != nil {
			return err
		}
		if t.set400k {
			cfg, err := I2CWriteRead(l, t.bus, []byte{regI2CCfg}, 1)
			if err != nil {
				return err
			}
			if cfg[0]&1 == 0 {
				if err := I2CWrite(l, t.bus, []byte{regI2CCfg, cfg[0] | 1}); err != nil {
					return err
				}
			}
		}
		t.setupDone[node] = true
		if !t.logged[node] {
			t.logged[node] = true
			logf("tunnel: node %d set up (CHIP=0x%02X%s)", node, t.tunnelAddr, map[bool]string{true: ", I2CCFG 400 kHz", false: ""}[t.set400k])
		}
	}
	return t.setNodeAdr(l, nodeAdrPeri|node)
}

func (t *A2BNodeTransport) guard(err error) error {
	if err == nil || errors.Is(err, ErrNack) {
		return err // NACK from the tunnel device: state is fine
	}
	t.nodeadr = -1 // be safe: re-write NODEADR next time
	return err
}

func (t *A2BNodeTransport) Write(node int, data []byte) error {
	var err error
	if perr := t.port.Do(true, func(l *LineIO) {
		if err = t.selectNode(l, node); err == nil {
			err = I2CWrite(l, t.bus, data)
		}
		err = t.guard(err)
	}); perr != nil {
		return &I2CError{perr.Error()}
	}
	return err
}

func (t *A2BNodeTransport) WriteRead(node int, data []byte, n int) ([]byte, error) {
	var err error
	var r []byte
	if perr := t.port.Do(true, func(l *LineIO) {
		if err = t.selectNode(l, node); err == nil {
			r, err = I2CWriteRead(l, t.bus, data, n)
		}
		err = t.guard(err)
	}); perr != nil {
		return nil, &I2CError{perr.Error()}
	}
	return r, err
}

func (t *A2BNodeTransport) Close() {}

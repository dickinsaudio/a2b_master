package main

// PC side of the A2B I2C web tunnel - a port of reference/a2b_web_tunnel.py.
//
// One scheduler goroutine runs every tunnel transaction and owns all connection state.
// Browser sockets are served by small reader/writer goroutines that only move bytes in
// and out of the per-connection buffers. A single mutex (t.mu) protects everything; the
// scheduler holds it except while a bus transaction (or a retry back-off) is in flight.
//
// Polling: a node with "hot" connections (recent traffic, pending upload, closing ...)
// is serviced back to back; otherwise it gets one SUMMARY every IdlePollMs. Only
// sockets flagged by SUMMARY (attn) or with pending work get further transactions, and
// while every socket streams full READ chunks the SUMMARY is skipped altogether.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	upLimit         = 16384 // max browser->device bytes buffered per connection
	downLimit       = 32768 // stop READing when this much waits for the browser
	hotTime         = 250 * time.Millisecond
	nodeDownBackoff = 250 * time.Millisecond
	nodeDeadTime    = 5 * time.Second
	connectTries    = 5
	closeTries      = 10
	protoErrLimit   = 20
)

type TunnelConfig struct {
	Transport  string  `json:"transport"` // "pico" (the connected port) or "sim:HOST:PORT"
	A2BBase    int     `json:"a2b_base"`
	TunnelAddr int     `json:"tunnel_addr"`
	Nodes      string  `json:"nodes"`
	BasePort   int     `json:"base_port"`
	Bind       string  `json:"bind"`
	Chunk      int     `json:"chunk"`
	IdlePollMs float64 `json:"idle_poll_ms"`
	Set400k    bool    `json:"set_400k"`
	Retries    int     `json:"retries"`
	BackoffMs  float64 `json:"backoff_ms"`
	StatsS     float64 `json:"stats_s"`
	Verbose    bool    `json:"verbose"`
}

func DefaultTunnelConfig() TunnelConfig {
	return TunnelConfig{Transport: "pico", A2BBase: 0x6A, TunnelAddr: 0x2A, Nodes: "0", BasePort: 8000,
		Bind: "127.0.0.1", Chunk: 32, IdlePollMs: 200, Set400k: true, Retries: 8, BackoffMs: 1, StatsS: 5}
}

func ParseNodes(s string) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.Split(strings.ReplaceAll(s, " ", ""), ",") {
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n > 15 {
			return nil, fmt.Errorf("nodes must be a comma list of 0..15 (got %q)", f)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no nodes given")
	}
	sort.Ints(out)
	return out, nil
}

// ---- protocol layer -------------------------------------------------------------------

type protoError struct{ msg string }

func (e *protoError) Error() string { return e.msg }

type txnFailed struct{ err error }

type Summary struct{ Listen, Active, Attn byte }

type SockStatus struct {
	Sn, Sr, Flags, Cid   byte
	Port, RxNext, RxFree uint16
	TxBase               uint16
	TxAvail              byte
}

func (s *SockStatus) active() bool      { return s.Flags&fConnActive != 0 }
func (s *SockStatus) localFin() bool    { return s.Flags&fLocalFin != 0 }
func (s *SockStatus) peerFin() bool     { return s.Flags&fPeerFin != 0 }
func (s *SockStatus) listenReady() bool { return s.Flags&fListenReady != 0 }

type NodeStats struct {
	Txn, Retries, Nacks, CRCErrs, IOErrs, Failed int64
	Up, Down, Conns                              int64
	// rate snapshot (1 s) and stats-line snapshot
	rTxn, rUp, rDown                    int64
	rT                                  time.Time
	TxnRate, UpRate, DownRate           float64
	pTxn, pDown, pRetries, pUp, pFailed int64
	pT                                  time.Time
}

type Bus struct {
	t       NodeTransport
	retries int
	backoff time.Duration
	verbose bool
	mu      *sync.Mutex // held by the caller; released around I/O
	stats   map[int]*NodeStats
}

func (b *Bus) st(node int) *NodeStats {
	s := b.stats[node]
	if s == nil {
		now := time.Now()
		s = &NodeStats{rT: now, pT: now}
		b.stats[node] = s
	}
	return s
}

// run executes fn (one transaction + parsing), retrying per the protocol; panics with
// txnFailed after the last retry (recovered by the scheduler, like Python's TxnFailed).
func (b *Bus) run(node int, desc string, fn func() (string, error)) {
	st := b.st(node)
	var last error
	for attempt := 0; attempt <= b.retries; attempt++ {
		if attempt > 0 {
			st.Retries++
			b.mu.Unlock()
			time.Sleep(b.backoff)
			b.mu.Lock()
		}
		st.Txn++
		b.mu.Unlock()
		res, err := fn()
		b.mu.Lock()
		if err == nil {
			if b.verbose {
				tr := ""
				if attempt > 0 {
					tr = fmt.Sprintf("  (try %d)", attempt+1)
				}
				logf("  n%d %-34s -> %s%s", node, desc, res, tr)
			}
			return
		}
		var pe *protoError
		switch {
		case errors.Is(err, ErrNack):
			st.Nacks++
		case errors.As(err, &pe):
			st.CRCErrs++
		default:
			st.IOErrs++
		}
		last = err
		if b.verbose {
			logf("  n%d %-34s !! %v", node, desc, err)
		}
	}
	st.Failed++
	panic(txnFailed{fmt.Errorf("node %d %s: %v", node, desc, last)})
}

func (b *Bus) write(node int, desc string, payload []byte) {
	frame := withCRC(payload)
	b.run(node, desc, func() (string, error) { return "ok", b.t.Write(node, frame) })
}

func (b *Bus) read(node int, desc string, payload []byte, n int, parse func([]byte) (string, error)) {
	frame := withCRC(payload)
	b.run(node, desc, func() (string, error) {
		r, err := b.t.WriteRead(node, frame, n)
		if err != nil {
			return "", err
		}
		if len(r) != n {
			return "", &I2CError{fmt.Sprintf("short read %d/%d", len(r), n)}
		}
		return parse(r)
	})
}

func (b *Bus) summary(node int) Summary {
	var s Summary
	b.read(node, "SUMMARY", []byte{opSummary}, summaryLen, func(r []byte) (string, error) {
		if CRC8(r[:5]) != r[5] || r[0] != summaryMagic {
			return "", &protoError{fmt.Sprintf("SUMMARY bad crc/magic % x", r)}
		}
		if r[1] != protoVersion {
			return "", &protoError{fmt.Sprintf("SUMMARY protocol version %d (want %d)", r[1], protoVersion)}
		}
		s = Summary{r[2], r[3], r[4]}
		return fmt.Sprintf("listen %02x active %02x attn %02x", s.Listen, s.Active, s.Attn), nil
	})
	return s
}

func (b *Bus) sockStatus(node, sn int) *SockStatus {
	var st SockStatus
	b.read(node, fmt.Sprintf("SOCK_STATUS s%d", sn), []byte{opSockStatus, byte(sn)}, sockStatusLen,
		func(r []byte) (string, error) {
			if CRC8(r[:13]) != r[13] || int(r[0]) != sn {
				return "", &protoError{fmt.Sprintf("SOCK_STATUS bad crc/sn % x", r)}
			}
			st = SockStatus{Sn: r[0], Sr: r[1], Flags: r[2], Cid: r[3], Port: u16(r, 4), RxNext: u16(r, 6),
				RxFree: u16(r, 8), TxBase: u16(r, 10), TxAvail: r[12]}
			return fmt.Sprintf("sr %02x fl %02x cid %d rx_next %d rx_free %d tx_base %d tx_avail %d",
				st.Sr, st.Flags, st.Cid, st.RxNext, st.RxFree, st.TxBase, st.TxAvail), nil
		})
	return &st
}

func (b *Bus) connect(node, sn int, cid byte, ip [4]byte, port int) {
	b.write(node, fmt.Sprintf("CONNECT s%d cid%d", sn, cid),
		[]byte{opConnect, byte(sn), cid, ip[0], ip[1], ip[2], ip[3], byte(port), byte(port >> 8)})
}

func (b *Bus) data(node, sn int, cid byte, off uint16, payload []byte) {
	p := append([]byte{opData, byte(sn), cid, byte(off), byte(off >> 8)}, payload...)
	b.write(node, fmt.Sprintf("DATA s%d cid%d off%d len%d", sn, cid, off, len(payload)), p)
}

// read returns (cid, off, data).
func (b *Bus) readOp(node, sn int, cid byte, ack uint16, maxlen int) (byte, uint16, []byte) {
	var rcid byte
	var off uint16
	var data []byte
	// Fixed-length I2C read: header + maxlen + crc; the crc sits right after the
	// `len` valid data bytes, anything after it is padding.
	b.read(node, fmt.Sprintf("READ s%d cid%d ack%d max%d", sn, cid, ack, maxlen),
		[]byte{opRead, byte(sn), cid, byte(ack), byte(ack >> 8), byte(maxlen)}, readHdrLen+maxlen+1,
		func(r []byte) (string, error) {
			ln := int(r[4])
			if int(r[0]) != sn || ln > maxlen || CRC8(r[:readHdrLen+ln]) != r[readHdrLen+ln] {
				return "", &protoError{fmt.Sprintf("READ bad crc/sn/len % x", r[:readHdrLen+1])}
			}
			rcid, off = r[1], u16(r, 2)
			data = append([]byte(nil), r[readHdrLen:readHdrLen+ln]...)
			return fmt.Sprintf("cid %d off %d len %d", rcid, off, ln), nil
		})
	return rcid, off, data
}

func (b *Bus) close(node, sn int, cid byte) {
	b.write(node, fmt.Sprintf("CLOSE s%d cid%d", sn, cid), []byte{opClose, byte(sn), cid})
}

func (b *Bus) resetAll(node int) {
	b.write(node, "RESET_ALL", []byte{opResetAll, resetAllKey})
}

// ---- connections and nodes ---------------------------------------------------------

type connState int

const (
	stQueued connState = iota
	stConnecting
	stOpen
	stFinDrain
	stClosing
	stDone
)

type Conn struct {
	node      *Node
	sock      net.Conn
	addr      *net.TCPAddr
	state     connState
	sn        int
	cid       byte
	up        []byte // browser -> device, not yet taken by the device
	upOff     int    // stream offset of up[0] (sent mod 2^16)
	down      []byte // device -> browser, not yet written to the browser
	ack       uint16
	eof       bool // browser sent FIN
	dead      bool // browser socket errored / closed by us
	streaming bool
	tries     int
	protoErrs int
	hotUntil  time.Time

	closed bool
	upSig  chan struct{} // reader: space in up
	dnSig  chan struct{} // writer: data in down
	quit   chan struct{}
}

func (c *Conn) String() string {
	return fmt.Sprintf("n%d s%d cid%d %s", c.node.num, c.sn, c.cid, c.addr)
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type Node struct {
	num       int
	listener  net.Listener
	pending   []*Conn
	conns     map[int]*Conn
	nextCid   int
	nextPoll  time.Time
	online    bool
	downUntil time.Time
	lastOK    time.Time
	warned    bool
	lastErr   string
	stats     *NodeStats
}

func (n *Node) allocCid() byte {
	used := map[byte]bool{}
	for _, c := range n.conns {
		used[c.cid] = true
	}
	for {
		cid := byte(n.nextCid)
		n.nextCid = n.nextCid%255 + 1
		if !used[cid] {
			return cid
		}
	}
}

func (n *Node) hot(now time.Time) bool {
	for _, c := range n.conns {
		if c.hotUntil.After(now) || len(c.up) > 0 || c.state == stConnecting || c.state == stFinDrain || c.state == stClosing {
			return true
		}
	}
	return false
}

func (n *Node) sortedSns() []int {
	sns := make([]int, 0, len(n.conns))
	for sn := range n.conns {
		sns = append(sns, sn)
	}
	sort.Ints(sns)
	return sns
}

type Tunnel struct {
	cfg   TunnelConfig
	tr    NodeTransport
	bus   *Bus
	nodes []*Node
	chunk int
	idle  time.Duration

	mu      sync.Mutex
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	started time.Time
}

func NewTunnel(cfg TunnelConfig, tr NodeTransport) (*Tunnel, error) {
	nodes, err := ParseNodes(cfg.Nodes)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{cfg: cfg, tr: tr, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	t.chunk = cfg.Chunk
	if t.chunk < 1 {
		t.chunk = 1
	}
	if t.chunk > maxPayload {
		t.chunk = maxPayload
	}
	t.idle = time.Duration(cfg.IdlePollMs * float64(time.Millisecond))
	if t.idle < time.Millisecond {
		t.idle = time.Millisecond
	}
	t.bus = &Bus{t: tr, retries: cfg.Retries, backoff: time.Duration(cfg.BackoffMs * float64(time.Millisecond)),
		verbose: cfg.Verbose, mu: &t.mu, stats: map[int]*NodeStats{}}
	for _, n := range nodes {
		addr := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.BasePort+n))
		ls, err := net.Listen("tcp4", addr)
		if err != nil {
			for _, nd := range t.nodes {
				nd.listener.Close()
			}
			return nil, fmt.Errorf("node %d: listen %s: %v", n, addr, err)
		}
		node := &Node{num: n, listener: ls, conns: map[int]*Conn{}, nextCid: 1, lastOK: time.Now(), stats: t.bus.st(n)}
		t.nodes = append(t.nodes, node)
		logf("tunnel node %d: http://%s/", n, addr)
	}
	return t, nil
}

func (t *Tunnel) Start() {
	t.started = time.Now()
	for _, n := range t.nodes {
		go t.acceptLoop(n)
	}
	go t.run()
}

func (t *Tunnel) Stop() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	<-t.done
}

func (t *Tunnel) Done() <-chan struct{} { return t.done }

func (t *Tunnel) vlog(f string, a ...any) {
	if t.cfg.Verbose {
		logf(f, a...)
	}
}

// ---- browser side -----------------------------------------------------------------

func (t *Tunnel) acceptLoop(n *Node) {
	for {
		s, err := n.listener.Accept()
		if err != nil {
			return
		}
		if tc, ok := s.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		ra, _ := s.RemoteAddr().(*net.TCPAddr)
		if ra == nil {
			ra = &net.TCPAddr{IP: net.IPv4zero}
		}
		c := &Conn{node: n, sock: s, addr: ra, sn: -1, upSig: make(chan struct{}, 1),
			dnSig: make(chan struct{}, 1), quit: make(chan struct{})}
		t.mu.Lock()
		n.pending = append(n.pending, c)
		n.nextPoll = time.Time{} // look for a listening socket right away
		t.vlog("accept %s -> node %d (queued %d)", ra, n.num, len(n.pending))
		t.mu.Unlock()
		go t.reader(c)
		go t.writer(c)
		notify(t.wake)
	}
}

func (t *Tunnel) reader(c *Conn) {
	buf := make([]byte, 4096)
	for {
		t.mu.Lock()
		for !c.closed && len(c.up) >= upLimit {
			t.mu.Unlock()
			select {
			case <-c.upSig:
			case <-c.quit:
			}
			t.mu.Lock()
		}
		if c.closed || c.eof {
			t.mu.Unlock()
			return
		}
		room := upLimit - len(c.up)
		t.mu.Unlock()
		if room > len(buf) {
			room = len(buf)
		}
		n, err := c.sock.Read(buf[:room])
		t.mu.Lock()
		if n > 0 && !c.closed && !c.eof && (c.state == stQueued || c.state == stConnecting || c.state == stOpen) {
			c.up = append(c.up, buf[:n]...)
			c.hotUntil = time.Now().Add(hotTime)
		}
		if err != nil {
			if !c.closed {
				if isEOF(err) {
					c.eof = true
					c.hotUntil = time.Now().Add(hotTime)
				} else {
					t.browserGone(c)
				}
			}
			t.mu.Unlock()
			notify(t.wake)
			return
		}
		t.mu.Unlock()
		notify(t.wake)
	}
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) }

func (t *Tunnel) writer(c *Conn) {
	for {
		t.mu.Lock()
		for !c.closed && len(c.down) == 0 {
			t.mu.Unlock()
			select {
			case <-c.dnSig:
			case <-c.quit:
			}
			t.mu.Lock()
		}
		if c.closed {
			t.mu.Unlock()
			return
		}
		data := c.down
		t.mu.Unlock()
		n, err := c.sock.Write(data)
		t.mu.Lock()
		if len(c.down) >= n {
			c.down = c.down[n:]
		}
		if err != nil && !c.closed {
			t.browserGone(c)
		}
		t.mu.Unlock()
		if err != nil {
			notify(t.wake)
			return
		}
		notify(t.wake)
	}
}

// with t.mu held
func (t *Tunnel) browserGone(c *Conn) {
	c.dead = true
	c.eof = true
	c.up = nil
	c.down = nil
	t.closeBrowser(c)
}

// with t.mu held
func (t *Tunnel) closeBrowser(c *Conn) {
	if c.closed {
		return
	}
	c.closed = true
	close(c.quit)
	c.sock.Close()
}

// ---- tunnel side -------------------------------------------------------------------

func (t *Tunnel) bringUp(n *Node) {
	var s Summary
	for i := 0; i < 3; i++ {
		t.bus.resetAll(n.num)
		s = t.bus.summary(n.num)
		if s.Active == 0 {
			break
		}
	}
	logf("tunnel node %d: online (listen %02x active %02x)", n.num, s.Listen, s.Active)
	n.online = true
	n.warned = false
	n.lastErr = ""
}

func (t *Tunnel) service(n *Node, now time.Time) {
	b := t.bus
	if !n.online {
		t.bringUp(n)
	}
	// Drop queued browsers that went away before getting a socket.
	keep := n.pending[:0]
	for _, c := range n.pending {
		if (c.eof && len(c.up) == 0) || c.dead {
			t.closeBrowser(c)
		} else {
			keep = append(keep, c)
		}
	}
	n.pending = keep

	streamingOnly := len(n.conns) > 0
	for _, c := range n.conns {
		if !(c.state == stOpen && c.streaming && len(c.up) == 0 && !c.eof) {
			streamingOnly = false
		}
	}
	var s *Summary
	if !(streamingOnly && len(n.pending) == 0) {
		sm := b.summary(n.num)
		s = &sm
	}

	for _, sn := range n.sortedSns() {
		c := n.conns[sn]
		if c == nil {
			continue
		}
		if c.state == stConnecting {
			t.doConnect(c, s)
		} else if c.state == stOpen {
			t.doOpen(c, s, now)
		}
		if c.state == stFinDrain && (len(c.down) == 0 || c.dead) {
			t.closeBrowser(c) // browser sees EOF only after all data
			c.state = stClosing
		}
		if c.state == stClosing {
			t.doClose(c)
		}
		if c.state == stDone {
			t.closeBrowser(c)
			if n.conns[sn] == c {
				delete(n.conns, sn)
			}
		}
	}

	// Pair queued browsers with free listening sockets.
	if s != nil && len(n.pending) > 0 {
		free := int(s.Listen)
		for sn := range n.conns {
			free &^= 1 << sn
		}
		for sn := 0; free != 0 && len(n.pending) > 0; sn++ {
			if free&1 != 0 {
				c := n.pending[0]
				n.pending = n.pending[1:]
				c.sn, c.cid, c.state, c.tries = sn, n.allocCid(), stConnecting, 0
				n.conns[sn] = c
				t.doConnect(c, s)
				if c.state == stDone {
					delete(n.conns, sn)
					t.closeBrowser(c)
				}
			}
			free >>= 1
		}
	}
}

func (t *Tunnel) doConnect(c *Conn, s *Summary) {
	b, n := t.bus, c.node
	if c.dead || (c.eof && len(c.up) == 0) {
		c.state = stDone // browser gave up meanwhile; nothing sent yet
		return
	}
	var ip [4]byte
	if v4 := c.addr.IP.To4(); v4 != nil {
		copy(ip[:], v4)
	}
	b.connect(n.num, c.sn, c.cid, ip, c.addr.Port)
	st := b.sockStatus(n.num, c.sn)
	if st.active() && st.Cid == c.cid {
		c.state = stOpen
		c.hotUntil = time.Now().Add(hotTime)
		n.stats.Conns++
		t.vlog("open %s", c)
		return
	}
	c.tries++
	if !st.listenReady() || c.tries >= connectTries {
		// Lost the race (or device busy): back to the head of the queue.
		t.vlog("connect %s failed (sr %02x fl %02x cid %d), requeue", c, st.Sr, st.Flags, st.Cid)
		delete(n.conns, c.sn)
		c.sn, c.cid, c.state = -1, 0, stQueued
		n.pending = append([]*Conn{c}, n.pending...)
	}
}

func (t *Tunnel) lost(c *Conn, why string) {
	logf("tunnel node %d: connection %s lost (%s)", c.node.num, c, why)
	t.closeBrowser(c)
	c.state = stDone
}

func (t *Tunnel) doOpen(c *Conn, s *Summary, now time.Time) {
	b, n, sn := t.bus, c.node.num, c.sn
	var st *SockStatus
	attn := false
	if s != nil {
		if s.Active>>sn&1 == 0 {
			t.lost(c, "device released socket")
			return
		}
		attn = s.Attn>>sn&1 != 0
	}

	// -- upload: send from the device's rx_next, as much as fits
	if len(c.up) > 0 {
		st = b.sockStatus(n, sn)
		if !st.active() || st.Cid != c.cid {
			t.lost(c, fmt.Sprintf("status cid %d", st.Cid))
			return
		}
		if st.localFin() {
			// The server is gone; nobody will take the rest of the upload. Checked before
			// rx_next: the device may already have reset rx_next for the re-listening
			// socket (seen with the native harness), which would look inconsistent.
			c.up = nil
			notify(c.upSig)
		} else {
			if !t.syncUp(c, st) {
				return
			}
			if len(c.up) > 0 && !st.peerFin() && st.RxFree > 0 {
				ln := min(len(c.up), int(st.RxFree), t.chunk)
				b.data(n, sn, c.cid, uint16(c.upOff), c.up[:ln])
				c.hotUntil = now.Add(hotTime)
			}
		}
	}

	// -- download
	wantRead := attn || c.streaming || (st != nil && st.TxAvail > 0)
	if wantRead && len(c.down) < downLimit && !c.dead {
		cid, off, data := b.readOp(n, sn, c.cid, c.ack, t.chunk)
		if cid != c.cid {
			t.lost(c, fmt.Sprintf("READ cid %d", cid))
			return
		}
		if off != c.ack {
			// Should not happen: tx_base is exactly what we acked.
			c.protoErrs++
			c.node.stats.CRCErrs++
			if c.protoErrs > protoErrLimit {
				t.lost(c, fmt.Sprintf("READ off %d != ack %d", off, c.ack))
			}
			return
		}
		c.protoErrs = 0
		if len(data) > 0 {
			if !c.dead {
				c.down = append(c.down, data...)
				notify(c.dnSig)
			}
			c.ack = off + uint16(len(data))
			c.node.stats.Down += int64(len(data))
			c.hotUntil = now.Add(hotTime)
		}
		c.streaming = len(data) == t.chunk
		st = nil // the ack moved tx_base: any status is stale
		if len(data) == 0 {
			// Everything acked and nothing left, yet we were flagged: usually local_fin.
			st = b.sockStatus(n, sn)
			if !st.active() || st.Cid != c.cid {
				t.lost(c, fmt.Sprintf("status cid %d", st.Cid))
				return
			}
		}
	}

	// -- device finished: drain, then close browser, then CLOSE
	if st != nil && st.localFin() && st.TxAvail == 0 && st.TxBase == c.ack && len(c.up) == 0 {
		t.vlog("local_fin %s", c)
		c.state = stFinDrain
		return
	}
	// -- browser finished (or vanished): CLOSE once upload is delivered
	if c.dead || (c.eof && len(c.up) == 0) {
		t.vlog("browser closed %s", c)
		c.state = stClosing
	}
}

func (t *Tunnel) syncUp(c *Conn, st *SockStatus) bool {
	d := int((st.RxNext - uint16(c.upOff)) & 0xFFFF)
	if d > len(c.up) {
		t.lost(c, fmt.Sprintf("rx_next %d beyond sent data", st.RxNext))
		return false
	}
	if d > 0 {
		c.up = c.up[d:]
		c.upOff += d
		c.node.stats.Up += int64(d)
		notify(c.upSig)
	}
	return true
}

func (t *Tunnel) doClose(c *Conn) {
	b, n := t.bus, c.node.num
	b.close(n, c.sn, c.cid)
	st := b.sockStatus(n, c.sn)
	if st.active() && st.Cid == c.cid && !st.peerFin() {
		c.tries++ // CLOSE was lost (e.g. bad crc): resend next cycle
		if c.tries < closeTries {
			return
		}
		logf("tunnel node %d: CLOSE %s not confirmed, giving up", n, c)
	}
	t.vlog("closed %s", c)
	c.state = stDone
}

func (t *Tunnel) nodeFailed(n *Node, err error, now time.Time) {
	n.downUntil = now.Add(nodeDownBackoff)
	n.lastErr = err.Error()
	if !n.warned {
		logf("tunnel node %d: %v", n.num, err)
		n.warned = true
	}
	if now.Sub(n.lastOK) > nodeDeadTime && (len(n.conns) > 0 || n.online) {
		logf("tunnel node %d: not responding, dropping %d connection(s)", n.num, len(n.conns))
		for _, c := range n.conns {
			t.closeBrowser(c)
		}
		n.conns = map[int]*Conn{}
		n.online = false
	}
}

// safeService runs fn and converts a txnFailed panic into an error.
func safeService(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if tf, ok := r.(txnFailed); ok {
				err = tf.err
				return
			}
			panic(r)
		}
	}()
	fn()
	return nil
}

// ---- main loop -----------------------------------------------------------------------

func (t *Tunnel) run() {
	defer close(t.done)
	t.mu.Lock()
	defer t.mu.Unlock()
	statsEvery := time.Duration(t.cfg.StatsS * float64(time.Second))
	nextStats := time.Now().Add(statsEvery)
	nextRate := time.Now().Add(time.Second)
	logf("tunnel: transport %s, nodes %s, chunk %d, idle poll %v", t.tr.Describe(), t.cfg.Nodes, t.chunk, t.idle)
	for _, n := range t.nodes {
		if err := safeService(func() { t.bringUp(n) }); err != nil {
			logf("tunnel node %d: not responding at start-up (%v); will keep trying", n.num, err)
			n.warned = true
			n.lastErr = err.Error()
			n.downUntil = time.Now().Add(nodeDownBackoff)
		} else {
			n.lastOK = time.Now()
		}
		if t.stopped() {
			break
		}
	}
	timer := time.NewTimer(time.Hour)
	for !t.stopped() {
		now := time.Now()
		hot := false
		for _, n := range t.nodes {
			if n.online && n.hot(now) && !n.downUntil.After(now) {
				hot = true
			}
		}
		if !hot {
			var wake time.Time
			for i, n := range t.nodes {
				w := n.nextPoll
				if n.downUntil.After(w) {
					w = n.downUntil
				}
				if i == 0 || w.Before(wake) {
					wake = w
				}
			}
			d := wake.Sub(now)
			if d > 50*time.Millisecond {
				d = 50 * time.Millisecond
			}
			if d > 0 {
				timer.Reset(d)
				t.mu.Unlock()
				select {
				case <-t.wake:
				case <-timer.C:
				case <-t.stop:
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				t.mu.Lock()
			}
		}
		now = time.Now()
		for _, n := range t.nodes {
			if n.downUntil.After(now) {
				continue
			}
			if !(n.hot(now) && n.online) && now.Before(n.nextPoll) {
				continue
			}
			n.nextPoll = now.Add(t.idle)
			if err := safeService(func() { t.service(n, now) }); err != nil {
				t.nodeFailed(n, err, now)
			} else {
				n.lastOK = time.Now()
				n.warned = false
				n.lastErr = ""
			}
		}
		now = time.Now()
		if now.After(nextRate) {
			t.updateRates(now)
			nextRate = now.Add(time.Second)
		}
		if statsEvery > 0 && now.After(nextStats) {
			t.printStats(now)
			nextStats = now.Add(statsEvery)
		}
	}
	t.shutdown()
}

func (t *Tunnel) stopped() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

func (t *Tunnel) updateRates(now time.Time) {
	for _, n := range t.nodes {
		st := n.stats
		dt := now.Sub(st.rT).Seconds()
		if dt <= 0 {
			continue
		}
		st.TxnRate = float64(st.Txn-st.rTxn) / dt
		st.UpRate = float64(st.Up-st.rUp) / dt
		st.DownRate = float64(st.Down-st.rDown) / dt
		st.rTxn, st.rUp, st.rDown, st.rT = st.Txn, st.Up, st.Down, now
	}
}

func kb(n int64) string {
	switch {
	case n < 10000:
		return fmt.Sprintf("%dB", n)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fkB", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/1e6)
}

func (t *Tunnel) printStats(now time.Time) {
	var parts []string
	active := false
	for _, n := range t.nodes {
		st := n.stats
		dt := now.Sub(st.pT).Seconds()
		if dt <= 0 {
			dt = 1e-6
		}
		if st.Down != st.pDown || st.Up != st.pUp || len(n.conns) > 0 || len(n.pending) > 0 || st.Failed != st.pFailed {
			active = true
		}
		off := ""
		if !n.online {
			off = "(off)"
		}
		parts = append(parts, fmt.Sprintf("n%d%s conn %d (act %d q %d) up %s down %s (%.1f kB/s) %.0f txn/s retry %d [nack %d crc %d io %d fail %d]",
			n.num, off, st.Conns, len(n.conns), len(n.pending), kb(st.Up), kb(st.Down),
			float64(st.Down-st.pDown)/dt/1000, float64(st.Txn-st.pTxn)/dt, st.Retries,
			st.Nacks, st.CRCErrs, st.IOErrs, st.Failed))
		st.pTxn, st.pDown, st.pRetries, st.pUp, st.pFailed, st.pT = st.Txn, st.Down, st.Retries, st.Up, st.Failed, now
	}
	if active || t.cfg.Verbose {
		logf("[stats] %s", strings.Join(parts, " | "))
	}
}

// shutdown: best effort release of device sockets so the web server is not left hanging.
func (t *Tunnel) shutdown() {
	for _, n := range t.nodes {
		n.listener.Close()
		for _, c := range n.conns {
			if c.state == stOpen || c.state == stFinDrain || c.state == stClosing {
				_ = safeService(func() { t.bus.close(n.num, c.sn, c.cid) })
			}
			t.closeBrowser(c)
		}
		for _, c := range n.pending {
			t.closeBrowser(c)
		}
		n.conns = map[int]*Conn{}
		n.pending = nil
	}
	t.tr.Close()
	logf("tunnel: stopped")
}

// ---- status for the UI ---------------------------------------------------------------

type NodeStatus struct {
	Main     int     `json:"main"` // A2B main node I2C address
	Node     int     `json:"node"`
	URL      string  `json:"url"`
	Online   bool    `json:"online"`
	Err      string  `json:"err"`
	Conns    int64   `json:"conns"`
	Active   int     `json:"active"`
	Queued   int     `json:"queued"`
	Up       int64   `json:"up"`
	Down     int64   `json:"down"`
	UpRate   float64 `json:"up_rate"`
	DownRate float64 `json:"down_rate"`
	TxnRate  float64 `json:"txn_rate"`
	Txn      int64   `json:"txn"`
	Retries  int64   `json:"retries"`
	Nacks    int64   `json:"nacks"`
	CRCErrs  int64   `json:"crc"`
	IOErrs   int64   `json:"io"`
	Failed   int64   `json:"failed"`
}

func (t *Tunnel) Status() []NodeStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []NodeStatus
	for _, n := range t.nodes {
		st := n.stats
		host := t.cfg.Bind
		if host == "0.0.0.0" || host == "" {
			host = "localhost"
		}
		if host == "127.0.0.1" {
			host = "localhost"
		}
		out = append(out, NodeStatus{Main: t.cfg.A2BBase, Node: n.num, URL: fmt.Sprintf("http://%s:%d/", host, t.cfg.BasePort+n.num),
			Online: n.online, Err: n.lastErr, Conns: st.Conns, Active: len(n.conns), Queued: len(n.pending),
			Up: st.Up, Down: st.Down, UpRate: st.UpRate, DownRate: st.DownRate, TxnRate: st.TxnRate, Txn: st.Txn,
			Retries: st.Retries, Nacks: st.Nacks, CRCErrs: st.CRCErrs, IOErrs: st.IOErrs, Failed: st.Failed})
	}
	return out
}

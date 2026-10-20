package main

// The Pico port: exactly one goroutine owns the link; every user (command-list replay,
// console, tunnel) submits jobs. UI jobs have priority over tunnel jobs, so the tunnel
// pauses automatically while a command list runs or a console op is queued.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNotConnected = errors.New("Pico not connected")
	ErrNack         = errors.New("NAK")
)

// I2CError is a non-NACK failure (bridge ERR, timeout, bad reply, link lost).
type I2CError struct{ Msg string }

func (e *I2CError) Error() string { return e.Msg }

const lineTimeout = 500 * time.Millisecond

type job struct {
	fn     func(l *LineIO)
	tunnel bool
	err    error
	done   chan struct{}
}

type Port struct {
	URL  string
	Pong string

	link   *LineIO
	hi, lo chan *job
	mu     sync.Mutex
	closed bool
	quit   chan struct{}
	exited chan struct{}

	gen       uint64 // bumped before every non-tunnel job; owner goroutine only
	hiPending atomic.Int32
	busy      atomic.Value // string: description of the running UI job
	jobs      atomic.Uint64
}

func ConnectPort(url string) (*Port, error) {
	url = strings.TrimSpace(url)
	l, err := OpenLink(url)
	if err != nil {
		return nil, err
	}
	pong := ""
	for try := 0; try < 3; try++ {
		pong, err = l.Cmd("P", time.Second)
		if err == nil && strings.HasPrefix(pong, "PONG") {
			break
		}
	}
	if err != nil || !strings.HasPrefix(pong, "PONG") {
		l.Close()
		if err == nil {
			err = fmt.Errorf("unexpected reply %q", pong)
		}
		return nil, fmt.Errorf("%s: no Pico bridge answering P: %v", url, err)
	}
	p := &Port{URL: url, Pong: pong, link: l, hi: make(chan *job, 64), lo: make(chan *job, 64),
		quit: make(chan struct{}), exited: make(chan struct{})}
	p.busy.Store("")
	go p.owner()
	return p, nil
}

func (p *Port) owner() {
	defer close(p.exited)
	run := func(j *job) {
		if !j.tunnel {
			p.gen++
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					j.err = fmt.Errorf("panic in port job: %v", r)
				}
			}()
			j.fn(p.link)
		}()
		p.jobs.Add(1)
		if !j.tunnel {
			p.hiPending.Add(-1)
		}
		close(j.done)
	}
	for {
		select {
		case j := <-p.hi:
			run(j)
			continue
		default:
		}
		select {
		case j := <-p.hi:
			run(j)
		case j := <-p.lo:
			// A UI job may have arrived meanwhile: it goes first.
			for {
				select {
				case h := <-p.hi:
					run(h)
					continue
				default:
				}
				break
			}
			run(j)
		case <-p.link.dead:
			logf("port %s: link lost: %v", p.URL, p.link.err)
			p.shutdown()
			return
		case <-p.quit:
			p.shutdown()
			return
		}
	}
}

func (p *Port) shutdown() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.link.Close()
	for {
		select {
		case j := <-p.hi:
			j.err = ErrNotConnected
			p.hiPending.Add(-1)
			close(j.done)
		case j := <-p.lo:
			j.err = ErrNotConnected
			close(j.done)
		default:
			return
		}
	}
}

func (p *Port) Close() {
	p.mu.Lock()
	if !p.closed {
		select {
		case <-p.quit:
		default:
			close(p.quit)
		}
	}
	p.mu.Unlock()
	<-p.exited
}

func (p *Port) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed
}

// Do runs fn on the owner goroutine with exclusive use of the link.
func (p *Port) Do(tunnel bool, fn func(l *LineIO)) error {
	if p == nil {
		return ErrNotConnected
	}
	j := &job{fn: fn, tunnel: tunnel, done: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrNotConnected
	}
	if tunnel {
		p.mu.Unlock()
		select {
		case p.lo <- j:
		case <-p.exited:
			return ErrNotConnected
		}
	} else {
		p.hiPending.Add(1)
		p.mu.Unlock()
		select {
		case p.hi <- j:
		case <-p.exited:
			p.hiPending.Add(-1)
			return ErrNotConnected
		}
	}
	select {
	case <-j.done:
		return j.err
	case <-p.exited:
		select {
		case <-j.done:
			return j.err
		default:
			return ErrNotConnected
		}
	}
}

// DoUI runs a UI job and records what it is doing for the status display.
func (p *Port) DoUI(desc string, fn func(l *LineIO)) error {
	return p.Do(false, func(l *LineIO) {
		p.busy.Store(desc)
		defer p.busy.Store("")
		fn(l)
	})
}

// ---- raw I2C over the line protocol -----------------------------------------

func checkReply(resp string) (string, error) {
	switch {
	case resp == "NAK":
		return "", ErrNack
	case strings.HasPrefix(resp, "ERR"):
		return "", &I2CError{"bridge: " + resp}
	case resp == "OK":
		return "", nil
	case strings.HasPrefix(resp, "OK "):
		return strings.TrimSpace(resp[3:]), nil
	}
	return "", &I2CError{fmt.Sprintf("bridge: unexpected reply %q", resp)}
}

func bridgeCmd(l *LineIO, line string) (string, error) {
	resp, err := l.Cmd(line, lineTimeout)
	if err != nil {
		if err == ErrTimeout {
			return "", &I2CError{"bridge timeout (" + line + ")"}
		}
		return "", &I2CError{err.Error()}
	}
	return resp, nil
}

func I2CWrite(l *LineIO, addr byte, data []byte) error {
	resp, err := bridgeCmd(l, fmt.Sprintf("W %02X %s", addr, strings.ToUpper(hex.EncodeToString(data))))
	if err != nil {
		return err
	}
	_, err = checkReply(resp)
	return err
}

func I2CWriteRead(l *LineIO, addr byte, w []byte, n int) ([]byte, error) {
	ws := "-"
	if len(w) > 0 {
		ws = strings.ToUpper(hex.EncodeToString(w))
	}
	resp, err := bridgeCmd(l, fmt.Sprintf("R %02X %d %s", addr, n, ws))
	if err != nil {
		return nil, err
	}
	h, err := checkReply(resp)
	if err != nil {
		return nil, err
	}
	rd, err := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
	if err != nil {
		return nil, &I2CError{fmt.Sprintf("bridge: bad hex %q", h)}
	}
	if len(rd) != n {
		return nil, &I2CError{fmt.Sprintf("bridge: asked %d bytes, got %d", n, len(rd))}
	}
	return rd, nil
}

// parseHexInt parses "6A", "0x6A" (hex by default).
func parseHexInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	return strconv.ParseInt(s, 16, 64)
}

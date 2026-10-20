package main

// Line-oriented link to the Pico: USB CDC serial port or TCP (fake Pico).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

var ErrTimeout = errors.New("timeout")

// LineIO sends command lines and receives reply lines. A reader goroutine feeds the
// lines channel; stale lines (late replies after a timeout) are dropped before each command.
type LineIO struct {
	name  string
	rw    io.ReadWriteCloser
	lines chan string
	dead  chan struct{}
	err   error
}

func OpenLink(url string) (*LineIO, error) {
	var rw io.ReadWriteCloser
	switch {
	case strings.HasPrefix(url, "tcp:"):
		addr := url[4:]
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return nil, err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		rw = c
	case strings.HasPrefix(url, "serial:"):
		p, err := serial.Open(url[7:], &serial.Mode{BaudRate: 115200})
		if err != nil {
			return nil, err
		}
		p.SetReadTimeout(serial.NoTimeout)
		p.ResetInputBuffer()
		rw = p
	default:
		// A bare port name (COM5, /dev/ttyACM0)
		if url == "" {
			return nil, errors.New("no port given")
		}
		return OpenLink("serial:" + url)
	}
	l := &LineIO{name: url, rw: rw, lines: make(chan string, 4096), dead: make(chan struct{})}
	go l.reader()
	return l, nil
}

func (l *LineIO) reader() {
	br := bufio.NewReaderSize(l.rw, 8192)
	for {
		s, err := br.ReadString('\n')
		if err != nil {
			l.err = err
			close(l.dead)
			return
		}
		s = strings.TrimRight(s, "\r\n")
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "AUTO ") {
			// Output of the power-up autorun of the flash list: informational only.
			logf("pico: %s", s)
			continue
		}
		select {
		case l.lines <- s:
		default: // nobody reading; drop
		}
	}
}

func (l *LineIO) Close() { l.rw.Close() }

func (l *LineIO) Drain() {
	for {
		select {
		case <-l.lines:
		default:
			return
		}
	}
}

func (l *LineIO) Send(line string) error {
	select {
	case <-l.dead:
		return fmt.Errorf("link closed: %v", l.err)
	default:
	}
	_, err := l.rw.Write([]byte(line + "\n"))
	if err != nil {
		return fmt.Errorf("link write: %v", err)
	}
	return nil
}

func (l *LineIO) ReadLine(timeout time.Duration) (string, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s := <-l.lines:
		return s, nil
	case <-l.dead:
		// deliver anything already received first
		select {
		case s := <-l.lines:
			return s, nil
		default:
		}
		return "", fmt.Errorf("link closed: %v", l.err)
	case <-t.C:
		return "", ErrTimeout
	}
}

// Cmd sends one command and waits for its single reply line.
func (l *LineIO) Cmd(line string, timeout time.Duration) (string, error) {
	l.Drain()
	if err := l.Send(line); err != nil {
		return "", err
	}
	return l.ReadLine(timeout)
}

type PortInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
	VID  string `json:"vid"`
	PID  string `json:"pid"`
	Pico bool   `json:"pico"`
}

func ListPorts() ([]PortInfo, error) {
	var out []PortInfo
	d, err := enumerator.GetDetailedPortsList()
	if err == nil {
		for _, p := range d {
			pi := PortInfo{Name: p.Name}
			if p.IsUSB {
				pi.VID, pi.PID = strings.ToUpper(p.VID), strings.ToUpper(p.PID)
				pi.Desc = strings.TrimSpace(fmt.Sprintf("USB %s:%s %s", pi.VID, pi.PID, p.Product))
				pi.Pico = pi.VID == "2E8A"
			}
			out = append(out, pi)
		}
	} else {
		names, err2 := serial.GetPortsList()
		if err2 != nil {
			return nil, err2
		}
		for _, n := range names {
			out = append(out, PortInfo{Name: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pico != out[j].Pico {
			return out[i].Pico
		}
		return portLess(out[i].Name, out[j].Name)
	})
	return out, nil
}

func portLess(a, b string) bool {
	// COM2 < COM10
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

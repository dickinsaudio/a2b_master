package main

// Command-list operations on the Pico (LC/LA/LI/LX/LS/LL/LAUTO).

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const laChunk = 300

type ListInfo struct {
	Bytes      int    `json:"bytes"`
	Records    int    `json:"records"`
	CRC        uint16 `json:"crc"`
	Autorun    bool   `json:"autorun"`
	FlashBytes int    `json:"flash_bytes"`
	FlashCRC   uint16 `json:"flash_crc"`
	Raw        string `json:"raw"`
}

func cmdOK(l *LineIO, line string, timeout time.Duration) (string, error) {
	resp, err := l.Cmd(line, timeout)
	if err != nil {
		return "", fmt.Errorf("%s: %v", strings.SplitN(line, " ", 2)[0], err)
	}
	if resp == "OK" {
		return "", nil
	}
	if strings.HasPrefix(resp, "OK ") {
		return strings.TrimSpace(resp[3:]), nil
	}
	return "", fmt.Errorf("%s: %s", strings.SplitN(line, " ", 2)[0], resp)
}

func listInfo(l *LineIO) (*ListInfo, error) {
	r, err := cmdOK(l, "LI", 2*time.Second)
	if err != nil {
		return nil, err
	}
	f := strings.Fields(r)
	if len(f) < 6 {
		return nil, fmt.Errorf("LI: unexpected reply %q", r)
	}
	li := &ListInfo{Raw: "OK " + r}
	var e [6]error
	li.Bytes, e[0] = strconv.Atoi(f[0])
	li.Records, e[1] = strconv.Atoi(f[1])
	c, e2 := strconv.ParseUint(f[2], 16, 16)
	e[2] = e2
	li.CRC = uint16(c)
	li.Autorun = f[3] == "1"
	li.FlashBytes, e[4] = strconv.Atoi(f[4])
	c, e5 := strconv.ParseUint(f[5], 16, 16)
	e[5] = e5
	li.FlashCRC = uint16(c)
	for _, x := range e {
		if x != nil {
			return nil, fmt.Errorf("LI: unexpected reply %q", r)
		}
	}
	return li, nil
}

// uploadList clears the RAM list, appends bin and verifies size and CRC with LI.
func uploadList(l *LineIO, bin []byte) error {
	if _, err := cmdOK(l, "LC", 2*time.Second); err != nil {
		return err
	}
	for off := 0; off < len(bin); off += laChunk {
		end := min(off+laChunk, len(bin))
		r, err := cmdOK(l, "LA "+strings.ToUpper(hex.EncodeToString(bin[off:end])), 2*time.Second)
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(strings.Fields(r + " x")[0]); err != nil || n != end {
			return fmt.Errorf("LA: Pico reports %q bytes, expected %d", r, end)
		}
	}
	li, err := listInfo(l)
	if err != nil {
		return err
	}
	if li.Bytes != len(bin) || li.CRC != CRC16(bin) {
		return fmt.Errorf("verify failed: Pico has %d bytes CRC %04X, file %d bytes CRC %04X",
			li.Bytes, li.CRC, len(bin), CRC16(bin))
	}
	return nil
}

type RecResult struct {
	Wire   int    `json:"wire"`
	Idx    int    `json:"idx"`
	Status string `json:"status"` // OK D NAK ERR
	Data   string `json:"data,omitempty"`
	Text   string `json:"text,omitempty"`
}

type RunSummary struct {
	Records int    `json:"records"`
	OK      int    `json:"ok"`
	Failed  int    `json:"failed"`
	Ms      int    `json:"ms"`
	Err     string `json:"err,omitempty"`
	Line    string `json:"line,omitempty"`
}

var runActive atomic.Bool

// RunList uploads and executes the list on the Pico, calling onRec for each record.
func RunList(p *Port, cl *CmdList, stop bool, onRec func(RecResult)) (*RunSummary, error) {
	if !cl.OK() {
		return nil, fmt.Errorf("the list has errors; not running it")
	}
	if !runActive.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("a command list is already running")
	}
	defer runActive.Store(false)
	sum := &RunSummary{}
	var runErr error
	err := p.DoUI("command list run", func(l *LineIO) {
		t0 := time.Now()
		if err := uploadList(l, cl.Bin); err != nil {
			runErr = err
			return
		}
		logf("cmdlist: uploaded %d bytes, %d records, CRC %04X (%.0f ms)", len(cl.Bin), cl.NWire, cl.CRC,
			time.Since(t0).Seconds()*1000)
		cmd := "LX"
		if stop {
			cmd = "LX STOP"
		}
		l.Drain()
		if err := l.Send(cmd); err != nil {
			runErr = err
			return
		}
		next := 0
		for {
			to := 3 * time.Second
			if next < len(cl.WireMap) {
				r := cl.Records[cl.WireMap[next]]
				if r.Op == "D" {
					to += time.Duration(r.DelayMs) * time.Millisecond
				}
			}
			line, err := l.ReadLine(to)
			if err != nil {
				runErr = fmt.Errorf("LX: waiting for record %d: %v", next, err)
				return
			}
			f := strings.Fields(line)
			switch {
			case len(f) >= 3 && f[0] == "S":
				w, err := strconv.Atoi(f[1])
				if err != nil {
					logf("cmdlist: bad line %q", line)
					continue
				}
				rr := RecResult{Wire: w, Idx: -1, Status: f[2]}
				if w >= 0 && w < len(cl.WireMap) {
					rr.Idx = cl.WireMap[w]
				}
				if len(f) > 3 {
					if f[2] == "OK" {
						rr.Data = strings.Join(f[3:], "")
					} else {
						rr.Text = strings.Join(f[3:], " ")
					}
				}
				next = w + 1
				onRec(rr)
			case len(f) >= 5 && f[0] == "DONE":
				sum.Records, _ = strconv.Atoi(f[1])
				sum.OK, _ = strconv.Atoi(f[2])
				sum.Failed, _ = strconv.Atoi(f[3])
				sum.Ms, _ = strconv.Atoi(f[4])
				sum.Line = line
				return
			case strings.HasPrefix(line, "ERR"):
				runErr = fmt.Errorf("LX: %s", line)
				return
			default:
				logf("cmdlist: unexpected line %q", line)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if runErr != nil {
		sum.Err = runErr.Error()
		return sum, runErr
	}
	return sum, nil
}

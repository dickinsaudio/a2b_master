package main

// SigmaStudio(+) XML command list -> Pico command-list records (USB_PROTOCOL.md v2).

import (
	"bytes"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	OpEnd       = 0x00
	OpWrite     = 0x01
	OpRead      = 0x02
	OpDelay     = 0x03
	OpLongWrite = 0x04

	MaxListBytes  = 16384
	MaxRecordData = 255  // n / wn / rn are single bytes
	MaxLongWrite  = 4096 // n of op 0x04
)

// Record is one XML <action>. It encodes to exactly one wire record, except a delay
// > 65535 ms, which becomes several delay records (never seen in SigmaStudio exports).
type Record struct {
	Idx       int      `json:"idx"`
	Line      int      `json:"line"`
	Instr     string   `json:"instr"`
	Cell      string   `json:"cell"`
	Param     string   `json:"param"`
	Op        string   `json:"op"` // W R D or ?
	Addr      int      `json:"addr"`
	RegW      int      `json:"regw"`
	Reg       uint32   `json:"reg"`
	Data      []byte   `json:"-"`
	ReadN     int      `json:"readn"`
	DelayMs   int      `json:"delay"`
	Bytes     []byte   `json:"-"` // all wire records of this action, concatenated
	Wire      [][]byte `json:"-"` // the individual wire records
	WireFirst int      `json:"wire"`
	Detail    string   `json:"detail"`
	Hex       string   `json:"hex"`
	Err       string   `json:"err,omitempty"`
	Warn      string   `json:"warn,omitempty"`
}

type CmdList struct {
	Name     string    `json:"name"`
	Records  []*Record `json:"records"`
	Warnings []string  `json:"warnings"`
	Errors   []string  `json:"errors"`
	Bin      []byte    `json:"-"`
	WireMap  []int     `json:"-"` // wire record index -> logical index
	CRC      uint16    `json:"crc"`
	Bytes    int       `json:"bytes"`
	NWire    int       `json:"nwire"`
	NWrites  int       `json:"writes"`
	NReads   int       `json:"reads"`
	NDelays  int       `json:"delays"`
}

// OK is true when the list can be run (no errors).
func (l *CmdList) OK() bool { return len(l.Errors) == 0 && len(l.Records) > 0 }

// CRC16 is CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF, no reflection, no xor-out).
func CRC16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// parseNum parses a decimal number, or hex with a 0x prefix (leading zeros stay decimal).
func parseNum(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return strconv.ParseInt(s[2:], 16, 64)
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseHexBody parses "00 FF 1A", "0x00 0xFF", "00FF" or with commas.
func parseHexBody(s string) ([]byte, error) {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
	})
	var out []byte
	for _, t := range f {
		t = strings.TrimPrefix(strings.TrimPrefix(t, "0x"), "0X")
		if len(t)%2 == 1 {
			t = "0" + t
		}
		b, err := hex.DecodeString(t)
		if err != nil {
			return nil, fmt.Errorf("bad hex %q", t)
		}
		out = append(out, b...)
	}
	return out, nil
}

func beBytes(v uint32, w int) []byte {
	b := make([]byte, w)
	for i := w - 1; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return b
}

func hexSp(b []byte) string {
	var sb strings.Builder
	for i, x := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", x)
	}
	return sb.String()
}

type xmlAction struct {
	attrs map[string]string
	body  string
	line  int
}

// ParseCmdListXML parses a SigmaStudio command list export.
func ParseCmdListXML(r io.Reader, name string) (*CmdList, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) { return in, nil }
	var acts []xmlAction
	var cur *xmlAction
	depthROM := false
	for {
		line, _ := dec.InputPos()
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("XML: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "ROM":
				depthROM = true
			case "action":
				a := xmlAction{attrs: map[string]string{}, line: line}
				for _, at := range t.Attr {
					a.attrs[at.Name.Local] = at.Value
				}
				acts = append(acts, a)
				cur = &acts[len(acts)-1]
			}
		case xml.CharData:
			if cur != nil {
				cur.body += string(t)
			}
		case xml.EndElement:
			if t.Name.Local == "action" {
				cur = nil
			}
		}
	}
	if !depthROM && len(acts) == 0 {
		return nil, fmt.Errorf("no <ROM>/<action> elements found - not a SigmaStudio command list?")
	}
	l := &CmdList{Name: filepath.Base(name)}
	for i := range acts {
		l.Records = append(l.Records, buildRecord(i, &acts[i]))
	}
	l.finish()
	return l, nil
}

func (l *CmdList) finish() {
	l.Bin = nil
	l.WireMap = nil
	l.Warnings, l.Errors = nil, nil
	l.NWrites, l.NReads, l.NDelays = 0, 0, 0
	for _, r := range l.Records {
		switch r.Op {
		case "W":
			l.NWrites++
		case "R":
			l.NReads++
		case "D":
			l.NDelays++
		}
		if r.Warn != "" {
			l.Warnings = append(l.Warnings, fmt.Sprintf("record %d (line %d, %s): %s", r.Idx, r.Line, r.Param, r.Warn))
		}
		if r.Err != "" {
			l.Errors = append(l.Errors, fmt.Sprintf("record %d (line %d, %s): %s", r.Idx, r.Line, r.Param, r.Err))
			continue
		}
		r.WireFirst = len(l.WireMap)
		for _, w := range r.Wire {
			l.Bin = append(l.Bin, w...)
			l.WireMap = append(l.WireMap, r.Idx)
		}
	}
	if len(l.Bin) > MaxListBytes {
		l.Errors = append(l.Errors, fmt.Sprintf("list is %d bytes, the Pico holds at most %d", len(l.Bin), MaxListBytes))
	}
	l.CRC = CRC16(l.Bin)
	l.Bytes = len(l.Bin)
	l.NWire = len(l.WireMap)
	if l.Warnings == nil {
		l.Warnings = []string{}
	}
	if l.Errors == nil {
		l.Errors = []string{}
	}
}

func buildRecord(idx int, a *xmlAction) *Record {
	at := a.attrs
	r := &Record{Idx: idx, Line: a.line, Instr: at["instr"],
		Cell: strings.TrimSpace(at["CellName"]), Param: strings.TrimSpace(at["ParamName"]), Op: "?"}
	fail := func(f string, args ...any) *Record {
		r.Err = fmt.Sprintf(f, args...)
		if r.Detail == "" {
			r.Detail = r.Err
		}
		return r
	}
	if p := at["Protocol"]; p != "" && !strings.EqualFold(p, "I2C") {
		return fail("Protocol=%q not supported (I2C only)", p)
	}
	body, err := parseHexBody(a.body)
	if err != nil {
		return fail("%v", err)
	}
	num := func(k string) (int64, error) {
		v, ok := at[k]
		if !ok {
			return 0, fmt.Errorf("missing attribute %s", k)
		}
		n, err := parseNum(v)
		if err != nil {
			return 0, fmt.Errorf("attribute %s=%q: %v", k, v, err)
		}
		return n, nil
	}
	switch r.Instr {
	case "writeXbytes", "read":
		addr, err := num("i2caddr")
		if err != nil {
			return fail("%v", err)
		}
		if addr < 0 || addr > 0x7F {
			return fail("i2caddr %d is not a 7-bit address", addr)
		}
		aw, err := num("addr_width")
		if err != nil {
			return fail("%v", err)
		}
		if aw < 0 || aw > 4 {
			return fail("addr_width %d not supported", aw)
		}
		reg := int64(0)
		if aw > 0 {
			if reg, err = num("addr"); err != nil {
				return fail("%v", err)
			}
		}
		ln, err := num("len")
		if err != nil {
			return fail("%v", err)
		}
		r.Addr, r.RegW, r.Reg = int(addr), int(aw), uint32(reg)
		regb := beBytes(uint32(reg), int(aw))
		if r.Instr == "read" {
			r.Op = "R"
			rn := int(ln - aw)
			r.ReadN = rn
			r.Detail = fmt.Sprintf("read %d", rn)
			if aw > 0 {
				r.Detail = fmt.Sprintf("[%0*X] read %d", 2*aw, reg, rn)
			}
			if rn < 1 || rn > MaxRecordData {
				return fail("read length %d (len %d - addr_width %d) out of range 1..%d", rn, ln, aw, MaxRecordData)
			}
			if len(body) > 0 {
				r.Warn = "read action has a body; ignored"
			}
			w := []byte{OpRead, byte(addr), byte(aw)}
			w = append(w, regb...)
			w = append(w, byte(rn))
			r.Wire = [][]byte{w}
		} else {
			r.Op = "W"
			r.Data = body
			if int(ln) != int(aw)+len(body) {
				r.Warn = fmt.Sprintf("len=%d but addr_width %d + %d data bytes = %d", ln, aw, len(body), int(aw)+len(body))
			}
			r.Detail = fmt.Sprintf("[%0*X] %s", 2*aw, reg, hexSp(body))
			if aw == 0 {
				r.Detail = hexSp(body)
			}
			if int(aw)+len(body) == 0 {
				return fail("empty write")
			}
			w, err := encodeWrite(byte(addr), int(aw), uint32(reg), body)
			if err != nil {
				return fail("%v", err)
			}
			r.Wire = [][]byte{w}
		}
	case "delay":
		r.Op = "D"
		if len(body) == 0 || len(body) > 4 {
			return fail("delay body must be 1..4 hex bytes (got %d)", len(body))
		}
		ms := 0
		for _, b := range body {
			ms = ms<<8 | int(b)
		}
		r.DelayMs = ms
		r.Detail = fmt.Sprintf("%d ms", ms)
		left := ms
		for {
			d := left
			if d > 0xFFFF {
				d = 0xFFFF
			}
			r.Wire = append(r.Wire, []byte{OpDelay, byte(d), byte(d >> 8)})
			left -= d
			if left <= 0 {
				break
			}
		}
		if len(r.Wire) > 1 {
			r.Warn = fmt.Sprintf("delay %d ms split into %d delay records", ms, len(r.Wire))
		}
	default:
		return fail("instr=%q not supported", r.Instr)
	}
	for _, w := range r.Wire {
		r.Bytes = append(r.Bytes, w...)
	}
	r.Hex = hexSp(r.Bytes)
	return r
}

// encodeWrite emits op 0x01 for writes of up to 255 bytes (register address bytes
// included) and op 0x04 (n u16 LE, up to 4096) for longer ones. Writes are never split.
func encodeWrite(addr byte, aw int, reg uint32, data []byte) ([]byte, error) {
	n := aw + len(data)
	var w []byte
	switch {
	case n <= MaxRecordData:
		w = []byte{OpWrite, addr, byte(n)}
	case n <= MaxLongWrite:
		w = []byte{OpLongWrite, addr, byte(n), byte(n >> 8)}
	default:
		return nil, fmt.Errorf("write of %d bytes exceeds the %d-byte maximum of record 0x04", n, MaxLongWrite)
	}
	w = append(w, beBytes(reg, aw)...)
	return append(w, data...), nil
}

// ExportC renders the list as a C header.
func (l *CmdList) ExportC() string {
	var b bytes.Buffer
	clean := func(s string) string {
		s = strings.ReplaceAll(s, "*/", "* /")
		s = strings.ReplaceAll(s, "\n", " ")
		return strings.TrimSpace(s)
	}
	fmt.Fprintf(&b, "/* A2B command list generated by a2b_master from %s\n", clean(l.Name))
	fmt.Fprintf(&b, " * %d actions (%d writes, %d reads, %d delays), %d records, %d bytes, CRC-16/CCITT-FALSE 0x%04X\n",
		len(l.Records), l.NWrites, l.NReads, l.NDelays, l.NWire, len(l.Bin), l.CRC)
	b.WriteString(" * Records: 01 addr7 n bytes[n] | 02 addr7 wn wbytes[wn] rn | 03 ms_lo ms_hi |\n *          04 addr7 n_lo n_hi bytes[n] | 00 end\n */\n")
	b.WriteString("#pragma once\n#include <stdint.h>\n\n")
	fmt.Fprintf(&b, "#define A2B_CMDLIST_LEN %d\n#define A2B_CMDLIST_RECORDS %d\n#define A2B_CMDLIST_CRC16 0x%04X\n\n",
		len(l.Bin), l.NWire, l.CRC)
	b.WriteString("const uint8_t a2b_cmdlist[] = {\n")
	wi := 0
	for _, r := range l.Records {
		if r.Err != "" {
			fmt.Fprintf(&b, "    /* action %d SKIPPED: %s */\n", r.Idx, clean(r.Err))
			continue
		}
		for k, w := range r.Wire {
			part := ""
			if len(r.Wire) > 1 {
				part = fmt.Sprintf(" (part %d/%d)", k+1, len(r.Wire))
			}
			fmt.Fprintf(&b, "    /* %3d %s %s: %s%s */\n", wi, r.Op, clean(r.Cell), clean(r.Param), part)
			for i := 0; i < len(w); i += 16 {
				b.WriteString("   ")
				end := i + 16
				if end > len(w) {
					end = len(w)
				}
				for _, x := range w[i:end] {
					fmt.Fprintf(&b, " 0x%02X,", x)
				}
				b.WriteString("\n")
			}
			wi++
		}
	}
	b.WriteString("    0x00 /* end */\n};\n")
	return b.String()
}

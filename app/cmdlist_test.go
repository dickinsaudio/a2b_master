package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const examplePath = "../examples/adi_a2b_commandlist.xml"

func TestCRC16(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("CRC16 check value %04X, want 29B1", got)
	}
	if got := CRC16(nil); got != 0xFFFF {
		t.Fatalf("CRC16(empty) %04X", got)
	}
}

func TestCRC8(t *testing.T) {
	if got := CRC8([]byte("123456789")); got != 0xF4 {
		t.Fatalf("CRC8 check value %02X, want F4", got)
	}
	f := withCRC([]byte{opSummary})
	if len(f) != 2 || f[1] != CRC8([]byte{opSummary}) {
		t.Fatal("withCRC")
	}
}

func TestExampleXML(t *testing.T) {
	f, err := os.Open(examplePath)
	if err != nil {
		t.Skip("example not found:", err)
	}
	defer f.Close()
	l, err := ParseCmdListXML(f, examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Records) != 161 || l.NWrites != 138 || l.NReads != 12 || l.NDelays != 11 {
		t.Fatalf("got %d records (%d W %d R %d D), want 161 (138/12/11)", len(l.Records), l.NWrites, l.NReads, l.NDelays)
	}
	if len(l.Errors) != 0 {
		t.Fatalf("errors: %v", l.Errors)
	}
	// Record 0: SOFT_RESET, DSP 0x38, 2-byte register 0xF890 (63632), data 00 00.
	r := l.Records[0]
	if r.Param != "SOFT_RESET" || !bytes.Equal(r.Bytes, []byte{0x01, 0x38, 0x04, 0xF8, 0x90, 0x00, 0x00}) {
		t.Fatalf("record 0: %s % X", r.Param, r.Bytes)
	}
	// Record 2: delay "00 FF" = 255 ms.
	if r := l.Records[2]; r.Op != "D" || !bytes.Equal(r.Bytes, []byte{0x03, 0xFF, 0x00}) {
		t.Fatalf("record 2: % X", r.Bytes)
	}
	// Delay body "19" (hex) = 25 ms; then INTTYPE read: 02 6A 01 17 01.
	var sawDelay25, sawInttype, sawVendor bool
	for _, r := range l.Records {
		if r.Op == "D" && r.DelayMs == 25 && bytes.Equal(r.Bytes, []byte{0x03, 25, 0}) {
			sawDelay25 = true
		}
		if r.Op == "R" && bytes.Equal(r.Bytes, []byte{0x02, 0x6A, 0x01, 0x17, 0x01}) {
			sawInttype = true
		}
		if r.Op == "R" && bytes.Equal(r.Bytes, []byte{0x02, 0x6A, 0x01, 0x02, 0x03}) {
			sawVendor = true
		}
	}
	if !sawDelay25 || !sawInttype || !sawVendor {
		t.Fatalf("delay25 %v inttype %v vendor %v", sawDelay25, sawInttype, sawVendor)
	}
	// Last record: CONTROL (0x12) = 0x92 on 0x6A.
	if r := l.Records[160]; !bytes.Equal(r.Bytes, []byte{0x01, 0x6A, 0x02, 0x12, 0x92}) {
		t.Fatalf("record 160: % X", r.Bytes)
	}
	// One record per action; the 366-byte program write (2 address + 364 data) uses 0x04.
	if r := l.Records[48]; r.Param != "Program Data" || len(r.Bytes) != 4+366 || r.Bytes[0] != OpLongWrite ||
		r.Bytes[2] != 0x6E || r.Bytes[3] != 0x01 || r.Bytes[4] != 0xC0 || r.Bytes[5] != 0x00 {
		t.Fatalf("record 48: %s % X", r.Param, r.Bytes[:6])
	}
	if len(l.Warnings) != 0 {
		t.Fatalf("warnings: %v", l.Warnings)
	}
	if n := walkRecords(t, l.Bin); n != 161 || l.NWire != 161 || len(l.WireMap) != 161 {
		t.Fatalf("walk %d records, NWire %d", n, l.NWire)
	}
	if l.CRC != CRC16(l.Bin) {
		t.Fatal("crc")
	}
	h := l.ExportC()
	if !strings.Contains(h, "const uint8_t a2b_cmdlist[] = {") || !strings.Contains(h, "SOFT_RESET") {
		t.Fatal("export")
	}
}

// walkRecords parses a record stream like the Pico does and returns the record count.
func walkRecords(t *testing.T, b []byte) int {
	n := 0
	for i := 0; i < len(b); n++ {
		switch b[i] {
		case OpWrite:
			i += 3 + int(b[i+2])
		case OpRead:
			i += 3 + int(b[i+2]) + 1
		case OpDelay:
			i += 3
		case OpLongWrite:
			i += 4 + (int(b[i+2]) | int(b[i+3])<<8)
		case OpEnd:
			return n
		default:
			t.Fatalf("bad op %02X at %d", b[i], i)
		}
		if i > len(b) {
			t.Fatalf("record %d overruns", n)
		}
	}
	return n
}

func TestLongWrite(t *testing.T) {
	data := make([]byte, 364)
	w, err := encodeWrite(0x38, 2, 0xC000, data)
	if err != nil || len(w) != 4+366 || w[0] != OpLongWrite || w[1] != 0x38 || w[2] != 366&0xFF || w[3] != 1 || w[4] != 0xC0 || w[5] != 0 {
		t.Fatalf("long write: %v % X", err, w[:6])
	}
	w, err = encodeWrite(0x38, 2, 0xC000, make([]byte, 253))
	if err != nil || w[0] != OpWrite || w[2] != 255 {
		t.Fatalf("255-byte write: %v % X", err, w[:4])
	}
	if w, _ = encodeWrite(0x38, 2, 0xC000, make([]byte, 254)); w[0] != OpLongWrite {
		t.Fatal("256-byte write must use 0x04")
	}
	if _, err := encodeWrite(0x38, 2, 0, make([]byte, 4095)); err == nil {
		t.Fatal("4097 bytes must fail")
	}
}

func TestXMLErrors(t *testing.T) {
	x := `<?xml version="1.0"?><ROM><page>
<action instr="writeXbytes" addr_width="1" len="3" addr="5" i2caddr="104" Protocol="I2C" ParamName="A">01 02</action>
<action instr="writeXbytes" addr_width="1" len="2" addr="5" i2caddr="104" Protocol="SPI" ParamName="B">01</action>
<action instr="foo" ParamName="C"/>
<action instr="writeXbytes" addr_width="1" len="5" addr="5" i2caddr="104" Protocol="I2C" ParamName="D">01</action>
<action instr="read" addr_width="2" len="4" addr="258" i2caddr="56" Protocol="I2C" ParamName="E"/>
</page><page><action instr="delay" ParamName="F">01 00</action></page></ROM>`
	l, err := ParseCmdListXML(strings.NewReader(x), "t.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Records) != 6 || len(l.Errors) != 2 || len(l.Warnings) != 1 || l.OK() {
		t.Fatalf("records %d errors %v warnings %v", len(l.Records), l.Errors, l.Warnings)
	}
	if !bytes.Equal(l.Records[0].Bytes, []byte{1, 0x68, 3, 5, 1, 2}) {
		t.Fatalf("% X", l.Records[0].Bytes)
	}
	if !bytes.Equal(l.Records[4].Bytes, []byte{2, 56, 2, 1, 2, 2}) {
		t.Fatalf("% X", l.Records[4].Bytes)
	}
	if !bytes.Equal(l.Records[5].Bytes, []byte{3, 0x00, 0x01}) || l.Records[5].DelayMs != 256 {
		t.Fatalf("% X", l.Records[5].Bytes)
	}
}

package main

// A2B I2C web tunnel wire protocol v1 (docs/TUNNEL_PROTOCOL.md). Little-endian.

const (
	opSummary    = 0x01
	opSockStatus = 0x02
	opConnect    = 0x10
	opData       = 0x11
	opRead       = 0x12
	opClose      = 0x13
	opResetAll   = 0x1F

	resetAllKey  = 0xA5
	summaryMagic = 0xA2
	protoVersion = 0x01

	summaryLen    = 6
	sockStatusLen = 14
	readHdrLen    = 5
	maxPayload    = 240

	fConnActive  = 0x01
	fLocalFin    = 0x02
	fPeerFin     = 0x04
	fListenReady = 0x08
	fVirtual     = 0x80
)

var crc8Table = func() (t [256]byte) {
	for i := 0; i < 256; i++ {
		c := byte(i)
		for k := 0; k < 8; k++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

// CRC8: poly 0x07, init 0, no reflection, no xor-out.
func CRC8(data []byte) byte {
	var c byte
	for _, b := range data {
		c = crc8Table[c^b]
	}
	return c
}

func withCRC(p []byte) []byte {
	out := make([]byte, len(p)+1)
	copy(out, p)
	out[len(p)] = CRC8(p)
	return out
}

func u16(b []byte, i int) uint16 { return uint16(b[i]) | uint16(b[i+1])<<8 }

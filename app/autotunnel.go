package main

// Automatic port forwarding for the GUI: scan the A2B bus for sub-nodes, find the ones
// whose RP2350 answers as the tunnel slave, pick the local ports and start the tunnel.
// Runs after connecting and after every command-list run (which redoes discovery).

import (
	"fmt"
	"net"
	"strings"
)

// The four I2C addresses an AD24xx/AD243x main node can have (ADR pins); each gets a block of
// ten ports: main i, sub-node n -> base + 10*i + n (8000-8007, 8010-8017, 8020-8027, 8030-8037).
var a2bMains = []int{0x68, 0x6A, 0x6C, 0x6E}

var basePorts = []int{8000, 8100, 8200} // First base with all 32 ports free is used

const (
	tunnelSlaveAddr = 0x2A // RP2350 I2C slave on each sub-node (A2B_TUNNEL_I2C_ADDR in the firmware)
	maxSubNodes     = 8
	regVendor       = 0x02 // regChip, regNodeAdr, nodeAdrPeri: transports.go
)

type MainScan struct {
	Addr    int   `json:"addr"`
	Nodes   []int `json:"nodes"`   // sub-nodes that answered (VENDOR = 0xAD)
	Tunnels []int `json:"tunnels"` // of those, the ones with a tunnel slave
}

type BusScan struct {
	Mains []MainScan `json:"mains"` // main nodes that answered
	Base  int        `json:"base"`  // port for main 0x68 sub-node 0
	Msg   string     `json:"msg"`
}

// probeTunnel sends SUMMARY to the peripheral selected by NODEADR (PERI set) and checks the reply.
func probeTunnel(l *LineIO, bus byte) bool {
	req := []byte{0x01, CRC8([]byte{0x01})}
	for try := 0; try < 4; try++ { // the RP2350 NAKs while it is briefly I2C master for its amp
		r, err := I2CWriteRead(l, bus, req, 6)
		if err == nil && len(r) == 6 && r[0] == 0xA2 && CRC8(r[:5]) == r[5] {
			return true
		}
	}
	return false
}

func scanMain(l *LineIO, main byte) (MainScan, bool) {
	m := MainScan{Addr: int(main)}
	if v, err := I2CWriteRead(l, main, []byte{regVendor}, 1); err != nil || len(v) != 1 || v[0] != 0xAD {
		return m, false
	}
	bus := main + 1
	for n := 0; n < maxSubNodes; n++ { // sub-nodes are numbered from 0 in discovery order
		if I2CWrite(l, main, []byte{regNodeAdr, byte(n)}) != nil {
			break
		}
		v, e := I2CWriteRead(l, bus, []byte{regVendor}, 1)
		if e != nil || len(v) != 1 || v[0] != 0xAD {
			break
		}
		m.Nodes = append(m.Nodes, n)
	}
	for _, n := range m.Nodes {
		I2CWrite(l, main, []byte{regNodeAdr, byte(n)})
		chip, e := I2CWriteRead(l, bus, []byte{regChip}, 1)
		if e != nil {
			continue
		}
		I2CWrite(l, bus, []byte{regChip, tunnelSlaveAddr})
		I2CWrite(l, main, []byte{regNodeAdr, nodeAdrPeri | byte(n)})
		if probeTunnel(l, bus) {
			m.Tunnels = append(m.Tunnels, n)
		} else if chip[0] != tunnelSlaveAddr { // not ours - put its CHIP back
			I2CWrite(l, main, []byte{regNodeAdr, byte(n)})
			I2CWrite(l, bus, []byte{regChip, chip[0]})
		}
	}
	I2CWrite(l, main, []byte{regNodeAdr, 0})
	return m, true
}

func scanBus(p *Port) (BusScan, error) {
	var s BusScan
	err := p.DoUI("scan A2B bus", func(l *LineIO) {
		for _, a := range a2bMains {
			if m, ok := scanMain(l, byte(a)); ok {
				s.Mains = append(s.Mains, m)
			}
		}
	})
	return s, err
}

// The first base port with every slot (4 mains x 8 sub-nodes) free.
func pickBasePort(bind string) int {
	for _, base := range basePorts {
		free := true
		for i := range a2bMains {
			for n := 0; n < maxSubNodes && free; n++ {
				ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, base+10*i+n))
				if err != nil {
					free = false
					break
				}
				ln.Close()
			}
		}
		if free {
			return base
		}
	}
	return basePorts[len(basePorts)-1]
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, ",")
}

// AutoTunnel (re)starts port forwarding for whatever the buses have now.
func (a *App) AutoTunnel() {
	a.StopTunnel()
	p := a.Port()
	if p == nil {
		return
	}
	a.setScan(BusScan{Msg: "scanning the A2B buses..."})
	s, err := scanBus(p)
	nodes, tunnels := 0, 0
	for _, m := range s.Mains {
		nodes += len(m.Nodes)
		tunnels += len(m.Tunnels)
	}
	switch {
	case err != nil:
		s.Msg = "bus scan failed: " + err.Error()
	case len(s.Mains) == 0:
		s.Msg = "no A2B main node found at 0x68 / 0x6A / 0x6C / 0x6E"
	case nodes == 0:
		s.Msg = "no sub-nodes found - run the command list to bring the bus up"
	case tunnels == 0:
		s.Msg = fmt.Sprintf("%d sub-node(s), none with a web tunnel", nodes)
	}
	if s.Msg != "" {
		logf("tunnel: %s", s.Msg)
		a.setScan(s)
		return
	}
	s.Base = pickBasePort(DefaultTunnelConfig().Bind)
	s.Msg = fmt.Sprintf("%d sub-node(s), %d with a web tunnel", nodes, tunnels)
	logf("tunnel: %s, ports from %d", s.Msg, s.Base)
	a.setScan(s)
	for _, m := range s.Mains {
		if len(m.Tunnels) == 0 {
			continue
		}
		cfg := DefaultTunnelConfig()
		cfg.A2BBase = m.Addr
		cfg.Nodes = joinInts(m.Tunnels)
		for i, addr := range a2bMains {
			if addr == m.Addr {
				cfg.BasePort = s.Base + 10*i
			}
		}
		if err := a.addTunnel(cfg); err != nil {
			s.Msg = fmt.Sprintf("tunnel start failed for main 0x%02X: %v", m.Addr, err)
			a.setScan(s)
		}
	}
}

func (a *App) setScan(s BusScan) {
	a.mu.Lock()
	a.scan = s
	a.mu.Unlock()
}

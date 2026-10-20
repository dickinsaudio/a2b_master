package main

// Automatic port forwarding for the GUI: scan the A2B bus for sub-nodes, find the ones
// whose RP2350 answers as the tunnel slave, pick the local ports and start the tunnel.
// Runs after connecting and after every command-list run (which redoes discovery).

import (
	"fmt"
	"net"
	"strings"
)

const (
	a2bMainAddr     = 0x6A // AD2437 main node; its BUS proxy address is +1
	tunnelSlaveAddr = 0x2A // RP2350 I2C slave on each sub-node (A2B_TUNNEL_I2C_ADDR in the firmware)
	maxSubNodes     = 16
	regVendor       = 0x02 // regChip, regNodeAdr, nodeAdrPeri: transports.go
)

type BusScan struct {
	Nodes   []int  `json:"nodes"`   // sub-nodes that answered (VENDOR = 0xAD)
	Tunnels []int  `json:"tunnels"` // of those, the ones with a tunnel slave
	Base    int    `json:"base"`    // local port for node 0 (node N -> Base+N)
	Msg     string `json:"msg"`
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

func scanBus(p *Port) (BusScan, error) {
	var s BusScan
	var err error
	bus := byte(a2bMainAddr + 1)
	e := p.DoUI("scan A2B bus", func(l *LineIO) {
		for n := 0; n < maxSubNodes; n++ { // sub-nodes are numbered from 0 in discovery order
			if err = I2CWrite(l, a2bMainAddr, []byte{regNodeAdr, byte(n)}); err != nil {
				return
			}
			v, e := I2CWriteRead(l, bus, []byte{regVendor}, 1)
			if e != nil || len(v) != 1 || v[0] != 0xAD {
				break
			}
			s.Nodes = append(s.Nodes, n)
		}
		for _, n := range s.Nodes {
			I2CWrite(l, a2bMainAddr, []byte{regNodeAdr, byte(n)})
			chip, e := I2CWriteRead(l, bus, []byte{regChip}, 1)
			if e != nil {
				continue
			}
			I2CWrite(l, bus, []byte{regChip, tunnelSlaveAddr})
			I2CWrite(l, a2bMainAddr, []byte{regNodeAdr, nodeAdrPeri | byte(n)})
			if probeTunnel(l, bus) {
				s.Tunnels = append(s.Tunnels, n)
			} else if chip[0] != tunnelSlaveAddr { // not ours - put its CHIP back
				I2CWrite(l, a2bMainAddr, []byte{regNodeAdr, byte(n)})
				I2CWrite(l, bus, []byte{regChip, chip[0]})
			}
		}
		I2CWrite(l, a2bMainAddr, []byte{regNodeAdr, 0})
	})
	if e != nil {
		err = e
	}
	return s, err
}

// 8080+N, or 8880+N if any of those is taken.
func pickBasePort(bind string, nodes []int) int {
	for _, base := range []int{8080, 8880} {
		free := true
		for _, n := range nodes {
			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, base+n))
			if err != nil {
				free = false
				break
			}
			ln.Close()
		}
		if free {
			return base
		}
	}
	return 8880
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, ",")
}

// AutoTunnel (re)starts port forwarding for whatever the bus has now.
func (a *App) AutoTunnel() {
	a.StopTunnel()
	p := a.Port()
	if p == nil {
		return
	}
	a.setScan(BusScan{Msg: "scanning the A2B bus..."})
	s, err := scanBus(p)
	switch {
	case err != nil:
		s.Msg = "bus scan failed: " + err.Error()
	case len(s.Nodes) == 0:
		s.Msg = "no sub-nodes found - run the command list to bring the bus up"
	case len(s.Tunnels) == 0:
		s.Msg = fmt.Sprintf("%d sub-node(s), none with a web tunnel", len(s.Nodes))
	}
	if s.Msg != "" {
		logf("tunnel: %s", s.Msg)
		a.setScan(s)
		return
	}
	cfg := DefaultTunnelConfig()
	cfg.Nodes = joinInts(s.Tunnels)
	cfg.BasePort = pickBasePort(cfg.Bind, s.Tunnels)
	s.Base = cfg.BasePort
	s.Msg = fmt.Sprintf("%d sub-node(s); web tunnel on node %s", len(s.Nodes), cfg.Nodes)
	logf("tunnel: %s, ports from %d", s.Msg, cfg.BasePort)
	a.setScan(s)
	if err := a.StartTunnel(cfg); err != nil {
		s.Msg = "tunnel start failed: " + err.Error()
		a.setScan(s)
	}
}

func (a *App) setScan(s BusScan) {
	a.mu.Lock()
	a.scan = s
	a.mu.Unlock()
}

package main

// Application state shared by the web UI and the CLI.

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type App struct {
	mu           sync.Mutex
	port         *Port
	list         *CmdList
	tunnels      []*Tunnel      // One per A2B main node in auto mode, one from the CLI
	tunnelCfgs   []TunnelConfig
	tunnelErr    string
	settings     Settings
	settingsPath string
	runResults   []RecResult
	runSummary   *RunSummary
	runRunning   bool
	quit         chan struct{}
	auto         bool    // GUI mode: start port forwarding automatically (scan the bus)
	pages        int       // Browser pages connected to the event stream
	pageSeen     bool      // At least one page has connected
	pagesGone    time.Time // When the last page disconnected
	scan         BusScan // last bus scan / port forwarding status
	boot         BootInfo
}

// What the Pico runs at power up (from LI)
type BootInfo struct {
	Known bool `json:"known"`
	On    bool `json:"on"`
	Bytes int  `json:"bytes"`
}

func NewApp(path string) *App {
	return &App{settingsPath: path, settings: LoadSettings(path), quit: make(chan struct{})}
}

func (a *App) save() { SaveSettings(a.settingsPath, a.settings) }

func (a *App) Port() *Port {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.port != nil && !a.port.Alive() {
		a.port = nil
	}
	return a.port
}

func (a *App) Connect(url string) (*Port, error) {
	a.Disconnect()
	url = strings.TrimSpace(url)
	if url != "" && !strings.HasPrefix(url, "serial:") && !strings.HasPrefix(url, "tcp:") {
		url = "serial:" + url
	}
	p, err := ConnectPort(url)
	if err != nil {
		logf("connect %s: %v", url, err)
		return nil, err
	}
	logf("connected %s: %s", url, p.Pong)
	a.mu.Lock()
	a.port = p
	a.settings.Port = url
	a.boot = BootInfo{}
	auto := a.auto
	a.mu.Unlock()
	a.save()
	if auto {
		go func() { a.ReadBoot(); a.AutoTunnel() }()
	}
	return p, nil
}

func (a *App) ReadBoot() {
	p := a.Port()
	if p == nil {
		return
	}
	var li *ListInfo
	var err error
	if e := p.DoUI("boot info", func(l *LineIO) { li, err = listInfo(l) }); e != nil {
		err = e
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.boot = BootInfo{}
		return
	}
	a.boot = BootInfo{Known: true, On: li.Autorun && li.FlashBytes > 0, Bytes: li.FlashBytes}
}

// SaveBoot stores the loaded list in Pico flash and makes it run at every power up.
func (a *App) SaveBoot() error {
	p, l := a.Port(), a.List()
	if p == nil {
		return ErrNotConnected
	}
	if l == nil || !l.OK() {
		return fmt.Errorf("load a command list without errors first")
	}
	var err error
	if e := p.DoUI("save boot list", func(io *LineIO) {
		if err = uploadList(io, l.Bin); err == nil {
			if _, err = cmdOK(io, "LS", 10*time.Second); err == nil {
				_, err = cmdOK(io, "LAUTO 1", 5*time.Second)
			}
		}
	}); e != nil {
		err = e
	}
	if err == nil {
		logf("Pico boot list: %s (%d bytes), runs at power up", l.Name, l.Bytes)
	}
	a.ReadBoot()
	return err
}

// ClearBoot stops the Pico running a list at power up.
func (a *App) ClearBoot() error {
	p := a.Port()
	if p == nil {
		return ErrNotConnected
	}
	var err error
	if e := p.DoUI("clear boot list", func(io *LineIO) { _, err = cmdOK(io, "LAUTO 0", 5*time.Second) }); e != nil {
		err = e
	}
	if err == nil {
		logf("Pico boot list cleared - nothing runs at power up")
	}
	a.ReadBoot()
	return err
}

func (a *App) Disconnect() {
	a.mu.Lock()
	p := a.port
	a.port = nil
	usesPort := false
	for _, c := range a.tunnelCfgs {
		usesPort = usesPort || c.Transport == "pico"
	}
	a.mu.Unlock()
	if usesPort {
		a.StopTunnel()
	}
	if p != nil {
		p.Close()
		logf("disconnected %s", p.URL)
	}
}

func (a *App) LoadList(data []byte, name string) (*CmdList, error) {
	l, err := ParseCmdListXML(bytes.NewReader(data), name)
	if err != nil {
		logf("command list %s: %v", name, err)
		return nil, err
	}
	a.mu.Lock()
	a.list = l
	a.runResults = nil
	a.runSummary = nil
	a.mu.Unlock()
	logf("command list %s: %d actions (%d writes, %d reads, %d delays), %d records, %d bytes, CRC %04X, %d warnings, %d errors",
		l.Name, len(l.Records), l.NWrites, l.NReads, l.NDelays, l.NWire, l.Bytes, l.CRC, len(l.Warnings), len(l.Errors))
	return l, nil
}

func (a *App) LoadListFile(path string) (*CmdList, error) {
	b, err := os.ReadFile(strings.Trim(strings.TrimSpace(path), `"`))
	if err != nil {
		return nil, err
	}
	l, err := a.LoadList(b, path)
	if err == nil {
		a.mu.Lock()
		a.settings.XMLPath = path
		a.mu.Unlock()
		a.save()
	}
	return l, err
}

func (a *App) List() *CmdList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.list
}

// Run executes the loaded list; results go to the event stream and the log.
func (a *App) Run(stop bool) (*RunSummary, error) {
	p := a.Port()
	l := a.List()
	if p == nil {
		return nil, ErrNotConnected
	}
	if l == nil {
		return nil, fmt.Errorf("no command list loaded")
	}
	a.mu.Lock()
	a.runResults = nil
	a.runSummary = nil
	a.runRunning = true
	auto := a.auto
	a.mu.Unlock()
	if auto {
		a.StopTunnel() // the list redoes discovery - restart forwarding afterwards
		a.setScan(BusScan{Msg: "command list running..."})
	}
	defer func() {
		a.mu.Lock()
		a.runRunning = false
		a.mu.Unlock()
		if auto {
			go a.AutoTunnel()
		}
	}()
	events.publish(Event{"runstart", map[string]any{"records": l.NWire, "actions": len(l.Records)}})
	logf("cmdlist: running %s on %s (stop on failure: %v)", l.Name, p.URL, stop)
	sum, err := RunList(p, l, stop, func(r RecResult) {
		a.mu.Lock()
		a.runResults = append(a.runResults, r)
		a.mu.Unlock()
		events.publish(Event{"rec", r})
		if r.Data != "" && r.Idx >= 0 {
			logf("cmdlist: record %d (action %d %s): OK %s", r.Wire, r.Idx, l.Records[r.Idx].Param, r.Data)
		}
		if r.Status != "OK" && r.Status != "D" {
			name := ""
			if r.Idx >= 0 {
				name = l.Records[r.Idx].Param
			}
			logf("cmdlist: record %d (action %d %s): %s %s", r.Wire, r.Idx, name, r.Status, r.Text)
		}
	})
	if sum == nil {
		sum = &RunSummary{}
	}
	if err != nil {
		sum.Err = err.Error()
		logf("cmdlist: run failed: %v", err)
	} else {
		logf("cmdlist: %s", sum.Line)
	}
	a.mu.Lock()
	a.runSummary = sum
	a.mu.Unlock()
	events.publish(Event{"done", sum})
	return sum, err
}

// StartTunnel replaces any running tunnels with this one.
func (a *App) StartTunnel(cfg TunnelConfig) error {
	a.StopTunnel()
	return a.addTunnel(cfg)
}

// addTunnel starts another tunnel alongside the running ones (they share the port queue).
func (a *App) addTunnel(cfg TunnelConfig) error {
	var tr NodeTransport
	switch {
	case strings.HasPrefix(cfg.Transport, "sim:"):
		tr = NewSimTransport(cfg.Transport[4:])
	case cfg.Transport == "pico" || cfg.Transport == "":
		cfg.Transport = "pico"
		p := a.Port()
		if p == nil {
			return fmt.Errorf("connect the Pico first (or use transport sim:HOST:PORT)")
		}
		if cfg.A2BBase < 0 || cfg.A2BBase > 0x7E || cfg.TunnelAddr < 0 || cfg.TunnelAddr > 0x7F {
			return fmt.Errorf("bad I2C address")
		}
		tr = NewA2BNodeTransport(p, byte(cfg.A2BBase), byte(cfg.TunnelAddr), cfg.Set400k)
	default:
		return fmt.Errorf("unknown tunnel transport %q (use pico or sim:HOST:PORT)", cfg.Transport)
	}
	t, err := NewTunnel(cfg, tr)
	if err != nil {
		tr.Close()
		a.mu.Lock()
		a.tunnelErr = err.Error()
		a.mu.Unlock()
		return err
	}
	a.mu.Lock()
	a.tunnels = append(a.tunnels, t)
	a.tunnelCfgs = append(a.tunnelCfgs, cfg)
	a.tunnelErr = ""
	a.settings.Tunnel = cfg
	a.mu.Unlock()
	a.save()
	t.Start()
	return nil
}

func (a *App) StopTunnel() {
	a.mu.Lock()
	ts := a.tunnels
	a.tunnels, a.tunnelCfgs = nil, nil
	a.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for _, t := range ts {
			t.Stop()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logf("tunnel: stop timed out")
	}
}

func (a *App) Tunnels() ([]*Tunnel, []TunnelConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Tunnel(nil), a.tunnels...), append([]TunnelConfig(nil), a.tunnelCfgs...)
}

// The GUI keeps an event stream open, so "no page connected" means the browser tab was closed
// (a reload reconnects within a second). QuitWhenNoPage ends the program after grace without one.
func (a *App) pageOpened() {
	a.mu.Lock()
	a.pages++
	a.pageSeen = true
	a.mu.Unlock()
}

func (a *App) pageClosed() {
	a.mu.Lock()
	a.pages--
	if a.pages == 0 {
		a.pagesGone = time.Now()
	}
	a.mu.Unlock()
}

func (a *App) QuitWhenNoPage(grace time.Duration) {
	for range time.Tick(time.Second) {
		a.mu.Lock()
		gone := a.pageSeen && a.pages == 0 && time.Since(a.pagesGone) > grace
		a.mu.Unlock()
		if gone {
			logf("browser page closed - quitting")
			select {
			case <-a.quit:
			default:
				close(a.quit)
			}
			return
		}
	}
}

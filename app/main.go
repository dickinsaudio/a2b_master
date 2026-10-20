// a2b_master: PC tool for the a2b_master Pico (command lists, I2C console, A2B web tunnel).
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var version = "1.0"

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		logf("cannot open a browser (%v); browse to %s", err, url)
		return
	}
	go cmd.Wait()
}

func main() {
	var (
		httpAddr  = flag.String("http", "", "UI address (default from settings, 127.0.0.1:8000)")
		noBrowser = flag.Bool("no-browser", false, "do not open the browser")
		noWeb     = flag.Bool("no-web", false, "headless: no web UI")
		portURL   = flag.String("port", "", "connect at start: COM5 | serial:COM5 | tcp:HOST:PORT")
		runXML    = flag.String("run", "", "run this SigmaStudio XML command list after connecting")
		stopFail  = flag.Bool("stop-on-fail", false, "with -run: stop at the first failure")
		exportC   = flag.String("export-c", "", "with -run or -list: write the C header to this file")
		listXML   = flag.String("list", "", "parse an XML command list, print it and exit")
		tunnelTr  = flag.String("tunnel", "", "start the tunnel: pico | sim:HOST:PORT")
		nodes     = flag.String("nodes", "", "tunnel nodes, e.g. 0,1")
		basePort  = flag.Int("base-port", 0, "tunnel TCP port for node 0 (node N -> base+N)")
		bind      = flag.String("bind", "", "tunnel listen address")
		chunk     = flag.Int("chunk", 0, "tunnel DATA/READ payload (<= 240)")
		idlePoll  = flag.Float64("idle-poll-ms", 0, "SUMMARY interval for idle nodes")
		a2bBase   = flag.String("a2b-base", "", "AD242x base address (hex, default 6A)")
		tunAddr   = flag.String("tunnel-addr", "", "tunnel slave address (hex, default 2A)")
		set400k   = flag.Bool("set-400k", false, "switch sub-node I2C to 400 kHz")
		retries   = flag.Int("retries", -1, "retries per tunnel transaction")
		statsS    = flag.Float64("stats-s", -1, "stats line interval (s)")
		verbose   = flag.Bool("v", false, "log every tunnel transaction")
		settingsF = flag.String("settings", "", "settings file (default a2b_master.json next to the exe)")
		duration  = flag.Float64("duration", 0, "headless: exit after this many seconds")
		showVer   = flag.Bool("version", false, "print version")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "a2b_master %s - Pico A2B master tool\n\nUsage: a2b_master [options]\n\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVer {
		fmt.Println("a2b_master", version)
		return
	}

	if *listXML != "" {
		os.Exit(cliList(*listXML, *exportC))
	}

	sp := *settingsF
	if sp == "" {
		sp = settingsPath()
	}
	a := NewApp(sp)
	logf("a2b_master %s (settings %s)", version, sp)

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg := a.settings.Tunnel
	if set["nodes"] {
		cfg.Nodes = *nodes
	}
	if set["base-port"] {
		cfg.BasePort = *basePort
	}
	if set["bind"] {
		cfg.Bind = *bind
	}
	if set["chunk"] {
		cfg.Chunk = *chunk
	}
	if set["idle-poll-ms"] {
		cfg.IdlePollMs = *idlePoll
	}
	if set["a2b-base"] {
		v, err := parseHexInt(*a2bBase)
		if err != nil {
			fatal("bad -a2b-base")
		}
		cfg.A2BBase = int(v)
	}
	if set["tunnel-addr"] {
		v, err := parseHexInt(*tunAddr)
		if err != nil {
			fatal("bad -tunnel-addr")
		}
		cfg.TunnelAddr = int(v)
	}
	if set["set-400k"] {
		cfg.Set400k = *set400k
	}
	if set["retries"] {
		cfg.Retries = *retries
	}
	if set["stats-s"] {
		cfg.StatsS = *statsS
	}
	cfg.Verbose = *verbose
	if *tunnelTr != "" {
		cfg.Transport = *tunnelTr
	}
	a.settings.Tunnel = cfg

	exit := 0
	if *portURL != "" {
		if _, err := a.Connect(*portURL); err != nil {
			if *noWeb {
				fatal(err.Error())
			}
		}
	}
	if *runXML != "" {
		l, err := a.LoadListFile(*runXML)
		if err != nil {
			fatal(err.Error())
		}
		printListProblems(l)
		if *exportC != "" {
			writeExport(l, *exportC)
		}
		if a.Port() == nil {
			fatal("-run needs -port")
		}
		sum, err := a.Run(*stopFail)
		if err != nil || sum.Failed > 0 {
			exit = 1
		}
	}
	if *tunnelTr != "" {
		if err := a.StartTunnel(cfg); err != nil {
			fatal("tunnel: " + err.Error())
		}
	}

	if *noWeb {
		if *tunnelTr == "" {
			a.Disconnect()
			os.Exit(exit)
		}
	} else {
		addr := *httpAddr
		if addr == "" {
			addr = a.settings.HTTP
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Probably another instance: try the next few ports.
			host, ps, _ := net.SplitHostPort(addr)
			pn, _ := strconv.Atoi(ps)
			for i := 1; i <= 10 && err != nil; i++ {
				ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(pn+i)))
			}
			if err != nil {
				fatal(fmt.Sprintf("cannot listen on %s: %v", addr, err))
			}
		}
		url := "http://" + strings.Replace(ln.Addr().String(), "0.0.0.0", "127.0.0.1", 1) + "/"
		logf("UI at %s  (close this window or press Ctrl-C to quit)", url)
		srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		if !*noBrowser {
			openBrowser(url)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	var timeout <-chan time.Time
	if *duration > 0 {
		timeout = time.After(time.Duration(*duration * float64(time.Second)))
	}
	select {
	case <-sig:
		logf("interrupted")
	case <-a.quit:
		logf("quit requested")
	case <-timeout:
	}
	a.StopTunnel()
	a.Disconnect()
	os.Exit(exit)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "a2b_master:", msg)
	os.Exit(2)
}

func printListProblems(l *CmdList) {
	for _, w := range l.Warnings {
		fmt.Println("warning:", w)
	}
	for _, e := range l.Errors {
		fmt.Println("error:", e)
	}
}

func writeExport(l *CmdList, path string) {
	if err := os.WriteFile(path, []byte(l.ExportC()), 0o644); err != nil {
		fatal(err.Error())
	}
	logf("wrote %s", path)
}

func cliList(path, export string) int {
	logStdout = false
	f, err := os.Open(path)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	l, err := ParseCmdListXML(f, path)
	if err != nil {
		fatal(err.Error())
	}
	for _, r := range l.Records {
		fmt.Printf("%4d %-2s 0x%02X %-22s %-28s %s\n", r.Idx, r.Op, r.Addr, r.Cell, r.Param, r.Detail)
	}
	printListProblems(l)
	fmt.Printf("%d actions: %d writes, %d reads, %d delays; %d records, %d bytes, CRC %04X; %d warnings, %d errors\n",
		len(l.Records), l.NWrites, l.NReads, l.NDelays, l.NWire, l.Bytes, l.CRC, len(l.Warnings), len(l.Errors))
	if export != "" {
		writeExport(l, export)
	}
	if !l.OK() {
		return 1
	}
	return 0
}

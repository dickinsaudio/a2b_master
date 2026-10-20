package main

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	post := func(path string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			h(w, r)
		})
	}

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.state()) })
	mux.HandleFunc("/api/ports", func(w http.ResponseWriter, r *http.Request) {
		ps, err := ListPorts()
		if err != nil {
			writeErr(w, err)
			return
		}
		if ps == nil {
			ps = []PortInfo{}
		}
		writeJSON(w, ps)
	})
	post("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Port string }
		if err := readJSON(r, &q); err != nil {
			writeErr(w, err)
			return
		}
		p, err := a.Connect(q.Port)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"pong": p.Pong, "port": p.URL})
	})
	post("/api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		a.Disconnect()
		writeJSON(w, map[string]any{"ok": true})
	})
	post("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		p := a.Port()
		var resp string
		var err error
		if e := p.DoUI("ping", func(l *LineIO) { resp, err = l.Cmd("P", time.Second) }); e != nil {
			err = e
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		logf("P -> %s", resp)
		writeJSON(w, map[string]any{"pong": resp})
	})

	// ---- command list
	post("/api/list/upload", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "upload.xml"
		}
		l, err := a.LoadList(b, name)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, l)
	})
	post("/api/list/load", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Path string }
		if err := readJSON(r, &q); err != nil {
			writeErr(w, err)
			return
		}
		l, err := a.LoadListFile(q.Path)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, l)
	})
	mux.HandleFunc("/api/list", func(w http.ResponseWriter, r *http.Request) {
		l := a.List()
		if l == nil {
			writeJSON(w, nil)
			return
		}
		writeJSON(w, l)
	})
	post("/api/list/run", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Stop bool }
		readJSON(r, &q)
		if a.Port() == nil {
			writeErr(w, ErrNotConnected)
			return
		}
		l := a.List()
		if l == nil || !l.OK() {
			writeErr(w, errors.New("no runnable command list loaded"))
			return
		}
		if runActive.Load() {
			writeErr(w, errors.New("a command list is already running"))
			return
		}
		a.mu.Lock()
		a.settings.StopOnFail = q.Stop
		a.mu.Unlock()
		a.save()
		go a.Run(q.Stop)
		writeJSON(w, map[string]any{"started": true})
	})
	post("/api/list/save", func(w http.ResponseWriter, r *http.Request) {
		p, l := a.Port(), a.List()
		if l == nil || !l.OK() {
			writeErr(w, errors.New("no runnable command list loaded"))
			return
		}
		var res string
		var err error
		if e := p.DoUI("save list", func(io *LineIO) {
			if err = uploadList(io, l.Bin); err == nil {
				res, err = cmdOK(io, "LS", 10*time.Second)
			}
		}); e != nil {
			err = e
		}
		if err != nil {
			logf("save to flash: %v", err)
			writeErr(w, err)
			return
		}
		logf("saved %s to Pico flash: OK %s", l.Name, res)
		writeJSON(w, map[string]any{"bytes": res})
	})
	post("/api/list/autorun", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ On bool }
		readJSON(r, &q)
		v := "0"
		if q.On {
			v = "1"
		}
		var err error
		if e := a.Port().DoUI("autorun", func(l *LineIO) { _, err = cmdOK(l, "LAUTO "+v, 5*time.Second) }); e != nil {
			err = e
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		logf("LAUTO %s: OK", v)
		writeJSON(w, map[string]any{"autorun": q.On})
	})
	post("/api/list/info", func(w http.ResponseWriter, r *http.Request) {
		var li *ListInfo
		var err error
		if e := a.Port().DoUI("list info", func(l *LineIO) { li, err = listInfo(l) }); e != nil {
			err = e
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		res := map[string]any{"info": li}
		cmp := ""
		if l := a.List(); l != nil {
			res["file_crc"] = l.CRC
			res["file_bytes"] = l.Bytes
			res["ram_match"] = li.Bytes == l.Bytes && li.CRC == l.CRC
			res["flash_match"] = li.FlashBytes == l.Bytes && li.FlashCRC == l.CRC
			cmp = fmt.Sprintf("; file %d bytes CRC %04X: RAM %s, flash %s", l.Bytes, l.CRC,
				map[bool]string{true: "MATCH", false: "differs"}[res["ram_match"].(bool)],
				map[bool]string{true: "MATCH", false: "differs"}[res["flash_match"].(bool)])
		}
		logf("LI: RAM %d bytes %d records CRC %04X, flash %d bytes CRC %04X, autorun %v%s",
			li.Bytes, li.Records, li.CRC, li.FlashBytes, li.FlashCRC, li.Autorun, cmp)
		writeJSON(w, res)
	})
	mux.HandleFunc("/api/list/export.h", func(w http.ResponseWriter, r *http.Request) {
		l := a.List()
		if l == nil {
			http.Error(w, "no list loaded", http.StatusNotFound)
			return
		}
		name := strings.TrimSuffix(filepath.Base(l.Name), filepath.Ext(l.Name)) + ".h"
		w.Header().Set("Content-Type", "text/x-c; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		io.WriteString(w, l.ExportC())
	})

	// ---- register console
	post("/api/i2c", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Op   string `json:"op"`
			Addr string `json:"addr"`
			RegW int    `json:"regw"`
			Reg  string `json:"reg"`
			Data string `json:"data"`
			N    int    `json:"n"`
			Line string `json:"line"`
		}
		if err := readJSON(r, &q); err != nil {
			writeErr(w, err)
			return
		}
		res, err := a.consoleOp(q.Op, q.Addr, q.RegW, q.Reg, q.Data, q.N, q.Line)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, res)
	})
	post("/api/node", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Base string `json:"base"`
			Node int    `json:"node"`
			Reg  string `json:"reg"`
			N    int    `json:"n"`
			Data string `json:"data"`
		}
		if err := readJSON(r, &q); err != nil {
			writeErr(w, err)
			return
		}
		res, err := a.nodeOp(q.Base, q.Node, q.Reg, q.N, q.Data)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, res)
	})

	// ---- tunnel
	post("/api/tunnel/start", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		cfg := a.settings.Tunnel
		a.mu.Unlock()
		if err := readJSON(r, &cfg); err != nil {
			writeErr(w, err)
			return
		}
		if err := a.StartTunnel(cfg); err != nil {
			logf("tunnel: %v", err)
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	post("/api/tunnel/stop", func(w http.ResponseWriter, r *http.Request) {
		a.StopTunnel()
		writeJSON(w, map[string]any{"ok": true})
	})
	post("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		s := a.settings
		a.mu.Unlock()
		if err := readJSON(r, &s); err != nil {
			writeErr(w, err)
			return
		}
		a.mu.Lock()
		a.settings = s
		a.mu.Unlock()
		a.save()
		writeJSON(w, s)
	})
	post("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			select {
			case <-a.quit:
			default:
				close(a.quit)
			}
		}()
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		c, logs := events.subscribe()
		defer events.unsubscribe(c)
		for _, m := range logs {
			b, _ := json.Marshal(Event{"log", m})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fl.Flush()
		ka := time.NewTicker(15 * time.Second)
		defer ka.Stop()
		for {
			select {
			case b := <-c:
				fmt.Fprintf(w, "data: %s\n\n", b)
				// coalesce bursts (command-list results)
				for i := 0; i < 256; i++ {
					select {
					case b = <-c:
						fmt.Fprintf(w, "data: %s\n\n", b)
						continue
					default:
					}
					break
				}
				fl.Flush()
			case <-ka.C:
				io.WriteString(w, ": ka\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			case <-a.quit:
				return
			}
		}
	})
	return mux
}

func (a *App) state() map[string]any {
	p := a.Port()
	t, tcfg := a.Tunnel()
	a.mu.Lock()
	st := map[string]any{
		"connected":  p != nil,
		"settings":   a.settings,
		"run":        map[string]any{"running": a.runRunning, "results": a.runResults, "summary": a.runSummary},
		"tunnel_err": a.tunnelErr,
	}
	if l := a.list; l != nil {
		st["list"] = map[string]any{"name": l.Name, "actions": len(l.Records), "records": l.NWire, "bytes": l.Bytes,
			"crc": l.CRC, "writes": l.NWrites, "reads": l.NReads, "delays": l.NDelays,
			"warnings": len(l.Warnings), "errors": len(l.Errors)}
	}
	a.mu.Unlock()
	if p != nil {
		st["port"] = map[string]any{"url": p.URL, "pong": p.Pong, "busy": p.busy.Load(), "queue": p.hiPending.Load(),
			"jobs": p.jobs.Load()}
	}
	if t != nil {
		st["tunnel"] = map[string]any{"running": true, "cfg": tcfg, "nodes": t.Status()}
	}
	return st
}

func parseHexBytes(s string) ([]byte, error) {
	s = strings.NewReplacer(" ", "", ",", "", "0x", "", "0X", "", "\t", "").Replace(s)
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return hex.DecodeString(s)
}

func (a *App) consoleOp(op, addrS string, regw int, regS, dataS string, n int, line string) (map[string]any, error) {
	p := a.Port()
	if p == nil {
		return nil, ErrNotConnected
	}
	if op == "raw" {
		line = strings.TrimSpace(line)
		if line == "" {
			return nil, errors.New("empty command")
		}
		if strings.HasPrefix(strings.ToUpper(line), "LX") {
			return nil, errors.New("use Run on Pico for LX")
		}
		var resp string
		var err error
		if e := p.DoUI("raw", func(l *LineIO) { resp, err = l.Cmd(line, 10*time.Second) }); e != nil {
			err = e
		}
		if err != nil {
			return nil, err
		}
		logf("> %s  < %s", line, resp)
		return map[string]any{"reply": resp}, nil
	}
	addr, err := parseHexInt(addrS)
	if err != nil || addr < 0 || addr > 0x7F {
		return nil, fmt.Errorf("bad 7-bit address %q", addrS)
	}
	var regb []byte
	if regw > 0 {
		reg, err := parseHexInt(regS)
		if err != nil || regw > 4 {
			return nil, fmt.Errorf("bad register %q", regS)
		}
		regb = beBytes(uint32(reg), regw)
	}
	switch op {
	case "write":
		data, err := parseHexBytes(dataS)
		if err != nil {
			return nil, fmt.Errorf("bad data hex: %v", err)
		}
		buf := append(append([]byte{}, regb...), data...)
		if len(buf) == 0 {
			return nil, errors.New("nothing to write")
		}
		if e := p.DoUI("i2c write", func(l *LineIO) { err = I2CWrite(l, byte(addr), buf) }); e != nil {
			err = e
		}
		logf("W %02X %s -> %s", addr, hexSp(buf), errText(err, "OK"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"reply": "OK"}, nil
	case "read":
		if n < 1 || n > 255 {
			return nil, errors.New("read length 1..255")
		}
		var rd []byte
		if e := p.DoUI("i2c read", func(l *LineIO) { rd, err = I2CWriteRead(l, byte(addr), regb, n) }); e != nil {
			err = e
		}
		logf("R %02X [%s] %d -> %s", addr, hexSp(regb), n, errText(err, hexSp(rd)))
		if err != nil {
			return nil, err
		}
		return map[string]any{"reply": "OK", "data": hexSp(rd)}, nil
	}
	return nil, fmt.Errorf("unknown op %q", op)
}

func errText(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

// nodeOp selects node N's registers (BASE NODEADR = N) and reads (or writes) a register
// through the BUS address (BASE+1), in one port job.
func (a *App) nodeOp(baseS string, node int, regS string, n int, dataS string) (map[string]any, error) {
	p := a.Port()
	if p == nil {
		return nil, ErrNotConnected
	}
	base, err := parseHexInt(baseS)
	if err != nil || base < 0 || base > 0x7E {
		return nil, fmt.Errorf("bad base address %q", baseS)
	}
	if node < 0 || node > 15 {
		return nil, errors.New("node 0..15")
	}
	reg, err := parseHexInt(regS)
	if err != nil || reg < 0 || reg > 0xFF {
		return nil, fmt.Errorf("bad register %q", regS)
	}
	var data []byte
	if strings.TrimSpace(dataS) != "" {
		if data, err = parseHexBytes(dataS); err != nil {
			return nil, err
		}
	}
	if len(data) == 0 && (n < 1 || n > 255) {
		return nil, errors.New("read length 1..255")
	}
	var rd []byte
	if e := p.DoUI("node register", func(l *LineIO) {
		if err = I2CWrite(l, byte(base), []byte{regNodeAdr, byte(node)}); err != nil {
			err = fmt.Errorf("NODEADR=%d: %w", node, err)
			return
		}
		if len(data) > 0 {
			err = I2CWrite(l, byte(base+1), append([]byte{byte(reg)}, data...))
		} else {
			rd, err = I2CWriteRead(l, byte(base+1), []byte{byte(reg)}, n)
		}
	}); e != nil {
		err = e
	}
	if len(data) > 0 {
		logf("node %d reg %02X <- %s : %s", node, reg, hexSp(data), errText(err, "OK"))
	} else {
		logf("node %d reg %02X (%d) -> %s", node, reg, n, errText(err, hexSp(rd)))
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"reply": "OK", "data": hexSp(rd)}, nil
}

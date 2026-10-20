package main

// End-to-end tunnel test against the native device harness (real tunnel_proto.c,
// vw5500.c and http_server.cpp):
//
//   /home/g/git/da_squid-tunnel/a2b_tunnel/test/build_native.sh /tmp/nat
//   A2B_NATIVE_SIM=/tmp/nat/native_sim go test -run TunnelE2E -v
//
// Optional: A2B_SIM_ARGS="--byte-us 25 --nack-rate 0.02" (the default).
// A2B_TUNNEL_TRANSPORT=pico A2B_PICO=tcp:127.0.0.1:9600 runs the same test through a
// (fake) Pico with the A2B NODEADR/CHIP layer instead of the sim: link; then the sim
// must already be running behind the fake Pico.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func bigPage() []byte {
	var b bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "<p>%d The quick brown fox jumps over the lazy dog over A2B.</p>\n", i)
	}
	return b.Bytes()
}

func chunksPage() []byte {
	var b bytes.Buffer
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "line %02d of the chunked response\n", i)
	}
	return b.Bytes()
}

func fetch(url string) ([]byte, time.Duration, error) {
	t0 := time.Now()
	c := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	r, err := c.Get(url)
	if err != nil {
		return nil, 0, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		return b, 0, fmt.Errorf("status %d", r.StatusCode)
	}
	return b, time.Since(t0), err
}

func waitPort(addr string) error {
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s not listening", addr)
}

func TestTunnelE2E(t *testing.T) {
	trMode := os.Getenv("A2B_TUNNEL_TRANSPORT")
	sim := os.Getenv("A2B_NATIVE_SIM")
	if sim == "" && trMode != "pico" {
		t.Skip("set A2B_NATIVE_SIM=<path to native_sim> (see the file comment)")
	}
	logStdout = testing.Verbose()
	cfg := DefaultTunnelConfig()
	cfg.BasePort = 18080
	cfg.StatsS = 2
	var tr NodeTransport
	if trMode == "pico" {
		p, err := ConnectPort(os.Getenv("A2B_PICO"))
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		tr = NewA2BNodeTransport(p, 0x6A, 0x2A, false)
	} else {
		args := os.Getenv("A2B_SIM_ARGS")
		if args == "" {
			args = "--byte-us 25 --nack-rate 0.02"
		}
		cmd := exec.Command(sim, append([]string{"--port", "19500"}, strings.Fields(args)...)...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer cmd.Process.Kill()
		if err := waitPort("127.0.0.1:19500"); err != nil {
			t.Fatal(err)
		}
		tr = NewSimTransport("127.0.0.1:19500")
	}
	tun, err := NewTunnel(cfg, tr)
	if err != nil {
		t.Fatal(err)
	}
	tun.Start()
	defer tun.Stop()
	base := "http://127.0.0.1:18080"

	big := bigPage()
	if len(big) != 12890 {
		t.Fatalf("expected page is %d bytes", len(big))
	}
	b, d, err := fetch(base + "/big.html")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, big) {
		t.Fatalf("big.html mismatch: got %d bytes", len(b))
	}
	t.Logf("big.html %d bytes in %v = %.1f kB/s", len(b), d, float64(len(b))/d.Seconds()/1000)

	b, _, err = fetch(base + "/index.html")
	if err != nil || !bytes.Contains(b, []byte("<h1>Hello over A2B</h1>")) {
		t.Fatalf("index.html: %v %q", err, b)
	}
	b, _, err = fetch(base + "/chunks.txt")
	if err != nil || !bytes.Equal(b, chunksPage()) {
		t.Fatalf("chunks.txt: %v %q", err, b)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	t0 := time.Now()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _, err := fetch(base + "/big.html")
			if err == nil && !bytes.Equal(b, big) {
				err = fmt.Errorf("fetch %d: mismatch (%d bytes)", i, len(b))
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	el := time.Since(t0)
	t.Logf("6 parallel big.html: %v = %.1f kB/s aggregate", el, 6*float64(len(big))/el.Seconds()/1000)
	st := tun.Status()[0]
	t.Logf("stats: conns %d txn %d retries %d [nack %d crc %d io %d fail %d]", st.Conns, st.Txn, st.Retries, st.Nacks, st.CRCErrs, st.IOErrs, st.Failed)
}

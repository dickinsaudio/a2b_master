# a2b_master (PC app)

A single portable Windows program, `a2b_master.exe`, for the a2b_master Pico. Nothing needs to
be installed. It serves its user interface as a local web page and opens that page in the
default browser (Edge or Chrome). The app can:

* connect to the Pico over its USB serial port (or over TCP to the native `fake_pico`) and
  check the link with `P`;
* load a SigmaStudio(+) XML command list, run it on the Pico with live results for each
  record, save it to flash, set autorun, compare the Pico's list CRC with the file, and export
  the list as a C header;
* read and write I2C registers, with quick reads for the main node and a helper for sub-node
  registers;
* run the A2B I2C web tunnel, which forwards `http://localhost:8080.../` (one port per sub-node) to the web server on
  A2B node N.

Protocols: [`../docs/USB_PROTOCOL.md`](../docs/USB_PROTOCOL.md) (Pico line protocol v2) and
[`../docs/TUNNEL_PROTOCOL.md`](../docs/TUNNEL_PROTOCOL.md). The tunnel engine is a port of
the tested Python reference in [`../reference/`](../reference/).

## Usage (Windows)

Run `a2b_master.exe`. It opens the GUI in your browser (close the console window to quit).

**Top bar.** Pick the Pico's COM port from the list; that connects (and pings) straight away and
shows CONNECTED. Picking it again reconnects. The last port is reconnected automatically on start.
The version shown is the git hash the exe was built from.

**Command list.** Choose a SigmaStudio(+) XML export; it is parsed as soon as it is chosen (choose
it again to reload). *RUN* runs it through the Pico (nothing is stored), with a result per
command. *Save as boot* stores the list on the Pico and runs it at every power up, with no PC
needed; *Clear boot* stops that.

**Register console.** Quick reads of the main node (0x6A) and of any sub-node (named and decoded),
a generic I2C read/write, and a raw Pico command line.

**Port forwarding.** Automatic. After connecting, and again after every command-list run (which redoes
discovery), the app scans the four A2B main-node addresses (0x68, 0x6A, 0x6C, 0x6E) and up to 8
sub-nodes on each, and finds the sub-nodes whose RP2350 answers as the tunnel slave (0x2A). Each one
gets the next free local port from 8080 in scan order (a port in use is skipped); the links are shown
in the table. Fixed settings: sub-node I2C 400 kHz, 32-byte chunks, 200 ms idle poll. *Rescan bus*
repeats the scan. Retries are shown against the total number of tunnel transactions.

### How the port is shared

Exactly one goroutine owns the serial port, and everything else submits jobs to it. Console
and command-list jobs have priority over tunnel jobs. While a command list runs, which is one
job from `LC` to `DONE`, or while console operations are queued, tunnel polling simply waits.
The tunnel's AD242x layer caches NODEADR and the per-node CHIP setup. Any non-tunnel job
invalidates that cache, so the tunnel rewrites `NODEADR=N`, `CHIP=0x2A` and
`NODEADR=0x20|N` before its next transaction.

**Note:** while the tunnel runs (or after it ran), NODEADR is left at `0x20|N` (PERI set) and
node N's CHIP register points at the tunnel slave (0x2A). Any SigmaStudio peripheral access
on that node is therefore redirected to the tunnel. A raw console access to BUS (0x6B) then
reaches the peripheral, not the node's registers. Use the **Node registers** helper, which
sets NODEADR first.

### Command line

```
a2b_master.exe [options]
  -http 127.0.0.1:8000        UI address (if busy, the next 10 ports are tried)
  -no-browser                 do not open the browser
  -no-web                     headless (no UI); runs until Ctrl-C / -duration
  -port COM5|tcp:HOST:PORT    connect at start
  -run file.xml               run a command list after connecting (exit code 1 if any record fails)
  -stop-on-fail               with -run
  -list file.xml              parse and print a command list, then exit (no Pico needed)
  -export-c out.h             with -list / -run: write the C header
  -tunnel pico|sim:HOST:PORT  start the tunnel
  -nodes 0,1 -base-port 8080 -bind 127.0.0.1 -chunk 32 -idle-poll-ms 200
  -a2b-base 6A -tunnel-addr 2A -set-400k -retries 8 -stats-s 5 -v
  -settings path.json         settings file (default: next to the exe)
  -duration 10                headless: exit after N seconds
```

Examples:

```
a2b_master.exe -no-web -port COM5 -run adi_a2b_commandlist.xml
a2b_master.exe -no-web -port COM5 -tunnel pico -nodes 0,1
a2b_master -no-web -tunnel sim:127.0.0.1:9500 -nodes 0        (Linux, against native_sim)
```

## XML conversion

The conversion follows USB_PROTOCOL.md. A write of up to 255 bytes (register address bytes
included) becomes record `0x01`. A longer write becomes record `0x04` with a u16 length (up
to 4096 bytes) and is never split. The example's 366-byte DSP "Program Data" write is a
`0x04` record. Reads become `0x02` and delays become `0x03`. Each XML action becomes exactly
one record, so Pico record numbers equal table row numbers. The parser warns when
`len != addr_width + data bytes`. `Protocol="SPI"`, unknown instructions, out-of-range
lengths and lists over 16384 bytes are errors.

## Build

You need Go 1.25 or later (golang.org/x/sys requires it). The only dependency is `go.bug.st/serial`, which includes its
enumerator.

```
cd app
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/a2b_master.exe .
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/a2b_master .      # Linux, for testing
```

The UI is a single file, `web/index.html`, embedded with `go:embed`. `dist/` is git-ignored.

## Tests (Linux)

```
go test ./...                                    # XML parser (example: 161 actions = 138 W / 12 R / 11 D,
                                                 # no errors), CRC-16/CRC-8, record encoding

# tunnel end to end against the real device code (tunnel_proto.c + vw5500 + http_server):
../device/test/build_native.sh /tmp/nat     # with this repo at <project>/extern/a2b_master
A2B_NATIVE_SIM=/tmp/nat/native_sim go test -run TunnelE2E -v

# the same, scripted with the headless binary (big.html, index.html, chunks.txt, 6 parallel):
test/tunnel_cli_test.sh dist/a2b_master /tmp/nat/native_sim 9531 8131

# through the fake Pico (raw I2C + NODEADR/CHIP layer):
../firmware/test/build_fake.sh /tmp/fake
/tmp/fake/fake_pico --port 9643 --tunnel sim:127.0.0.1:9534 &
APPARGS="-port tcp:127.0.0.1:9643 -tunnel pico" test/tunnel_cli_test.sh dist/a2b_master /tmp/nat/native_sim 9534 8134

# command-list replay through the fake Pico:
dist/a2b_master -no-web -port tcp:127.0.0.1:9643 -run ../examples/adi_a2b_commandlist.xml
```

Typical results with `--byte-us 25 --nack-rate 0.02` and chunk 64: about 28 kB/s for a single
`big.html` and about 28 kB/s aggregate for 6 parallel fetches. The numbers are the same through
the fake Pico. Without a bus delay, the device-side 1 ms tick limits throughput to about
110 kB/s.

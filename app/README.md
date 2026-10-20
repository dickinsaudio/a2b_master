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
* run the A2B I2C web tunnel, which forwards `http://localhost:8080+N/` to the web server on
  A2B node N.

Protocols: [`../docs/USB_PROTOCOL.md`](../docs/USB_PROTOCOL.md) (Pico line protocol v2) and
[`../docs/TUNNEL_PROTOCOL.md`](../docs/TUNNEL_PROTOCOL.md). The tunnel engine is a port of
the tested Python reference in [`../reference/`](../reference/).

## Usage (Windows)

1. Copy `a2b_master.exe` into any folder and double-click it. A console window opens and shows
   the log. The browser opens `http://127.0.0.1:8000/`. To quit, close the console window,
   press Ctrl-C in it, or click **Quit** in the page.
2. **Connect.** Pick the Pico's COM port (Raspberry Pi USB devices, VID 2E8A, are listed
   first) and click **Connect**. The reply to `P` appears next to the button, for example
   `PONG a2b-master v2 400000`. You can also type a port URL in the text box instead:
   `COM5`, `serial:COM5` or `tcp:127.0.0.1:9600`.
3. **Command list.** Choose the XML file, or type its path and click **Load**. The table
   lists each action. Errors (an SPI action or an unknown instruction) and warnings appear
   above the table, and a list with errors cannot be run.
   * **Run on Pico** clears the Pico list and uploads it (`LC`, then `LA` in 300-byte chunks),
     checks the upload against `LI` (byte count and CRC), then runs `LX`. Each row turns green
     or red as its result arrives, and read data is shown in the row. The summary line shows
     the `DONE` totals. Check **stop on first failure** to run `LX STOP` instead.
   * **Save to Pico flash** uploads the list and then sends `LS`. Use the **autorun on boot**
     checkbox with **Apply** to send `LAUTO 1` or `LAUTO 0`.
   * **Read Pico list info** sends `LI` and shows MATCH or differs for the RAM and flash
     lists against the loaded file.
   * **Export C array** downloads a `.h` file containing `const uint8_t a2b_cmdlist[]` (the
     record bytes, a comment per record with its ParamName, and a trailing `0x00` end
     record) plus `A2B_CMDLIST_LEN`, `A2B_CMDLIST_RECORDS` and `A2B_CMDLIST_CRC16`.
4. **Register console.**
   * Generic read and write: give the address, the register address width (1, 2 or none), the
     register, and either the data or the read length.
   * Quick reads on the main node (default 0x6A): VENDOR/PRODUCT/VERSION (registers
     0x02..0x04) and INTTYPE (0x17).
   * **Node registers** writes NODEADR (0x01) = N on the main node, then reads or writes the
     register through BUS = main + 1, all in one queued job.
   * **Raw protocol line** sends any single-reply command (`B 100`, `LI`, ...).
5. **Port forwarding (web tunnel).**
   * Settings: transport `pico` (the connected Pico), A2B base 0x6A, tunnel slave 0x2A, nodes
     (for example `0` or `0,1`), base port 8080, bind 127.0.0.1, chunk 64, idle poll 50 ms.
     **set sub-node I2C 400 kHz** is off by default for the AD2437.
   * Click **Start**. The table shows each node: whether it is reachable, connection count,
     bytes up and down, download rate, transactions per second and retries (split into NACK,
     CRC, I/O and failed). Click a node's link to open its web page.
   * Bind `0.0.0.0` to expose the tunnel ports on the LAN.

Settings are saved in `a2b_master.json` next to the exe: last port, XML path, stop on
failure, console address and tunnel configuration.

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
  -nodes 0,1 -base-port 8080 -bind 127.0.0.1 -chunk 64 -idle-poll-ms 50
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
<da_squid>/a2b_tunnel/test/build_native.sh /tmp/nat
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

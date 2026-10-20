# a2b_master firmware — USB-CDC I2C master + command-list player for an A2B main node

Raspberry Pi Pico (RP2040) / Pico 2 (RP2350) firmware that replaces an ADI USBi on the I2C
header of an AD2437 A2B evaluation board. A PC drives it over a USB serial port. It can:

* do raw I2C writes and reads for the PC (protocol v1 commands, unchanged), including the
  tunnel traffic of the web tunnel (see `../docs/TUNNEL_PROTOCOL.md`; the tunnel logic runs
  on the PC),
* hold a **command list** (a SigmaStudio+ command-list export converted to binary records by
  the PC app), replay it, store it in flash and replay it automatically at power-up.

Protocol specification: [`../docs/USB_PROTOCOL.md`](../docs/USB_PROTOCOL.md) (v2).

| file | purpose |
|---|---|
| `main.c` | RP2040 glue: USB stdio, LED, autorun, platform hooks |
| `proto.c/.h` | line protocol (portable C, also used by `test/fake_pico.c`) |
| `cmdlist.c/.h` | record parser/validator, player (ops struct), CRC-16 (portable C) |
| `i2c_master.c/.h` | register-level I2C master: arbitration-loss detection, timeouts, bus recovery |
| `flash_store.c/.h` | list + autorun flag in the last 64 KB of flash |
| `i2c_status.h` | shared I2C status codes |
| `test/` | native `fake_pico` simulator, protocol test, pty bridge |

`cmdlist.*`, `proto.*` and `flash_store.h` have no SDK dependencies. They are meant to be
reused by an embedded RP2040 A2B master.

## Wiring to the EVK

Connect the Pico to the EVK header that the USBi normally plugs into. **The pinout of that
header is not given here. Check it in the EVK schematic** (find the SDA, SCL and GND pins of
the USBi/I2C connector).

| Pico pin            | EVK USBi header |
|---------------------|-----------------|
| GP4 (pin 6) — SDA   | SDA             |
| GP5 (pin 7) — SCL   | SCL             |
| GND (e.g. pin 8)    | GND             |

* Use 3.3 V logic only. Connect only SDA, SCL and GND. **Do not** connect the Pico's 3V3 or
  VBUS to the board. Power each side separately (Pico from USB, EVK from its own supply).
* The EVK has its own pull-ups on this bus. The Pico's weak internal pull-ups are also
  enabled, which is harmless.
* Unplug the USBi. If both masters are connected, the controller arbitrates and the Pico
  reports `ERR ARB` when it loses. However, neither master knows about the other's
  multi-transaction sequences, so those can still be corrupted.
* To change the pins or controller at configure time, use `-DBRIDGE_I2C_INDEX=1
  -DBRIDGE_SDA_PIN=2 -DBRIDGE_SCL_PIN=3` (the pair must be valid I2Cn SDA/SCL pins). To set
  the power-on speed, use `-DBRIDGE_DEFAULT_KHZ=100` (default 400).

AD2437 EVK 7-bit addresses: main-node transceiver `0x6A`, its BUS (sub-node and peripheral)
proxy `0x6B`, DSP `0x38`, codec `0x06`.

## Build

```sh
export PICO_SDK_PATH=/path/to/pico-sdk          # SDK 2.x
cmake -S . -B /tmp/a2b_master_build -DPICO_BOARD=pico     # or pico2
make -C /tmp/a2b_master_build -j
# -> /tmp/a2b_master_build/a2b_master.uf2
```

SDK 2.x needs `picotool` to make the `.uf2`. If none is installed, the SDK downloads and
builds it, which needs network access. To reuse an existing build, pass
`-Dpicotool_DIR=<dir containing picotoolConfig.cmake>`. Build out of tree so that no
artifacts end up in the repo.

## Flashing

Hold BOOTSEL while plugging the Pico in. It mounts as `RPI-RP2` (`RP2350` for a Pico 2).
Copy `a2b_master.uf2` onto it, or run `picotool load -x a2b_master.uf2`. The Pico then
enumerates as a USB CDC serial port: `/dev/ttyACM*` on Linux, or `COMx` on Windows 10 and
later (no driver needed). The baud setting is ignored.

Loading a new `.uf2` does not touch the stored command list, because the image is far
smaller than the flash. `picotool erase` does remove it.

## Protocol summary

These are ASCII lines ending in `\n` or `\r\n`. Replies end in `\r\n`, and hex in replies is
uppercase. The full specification is in `../docs/USB_PROTOCOL.md`.

| command | reply |
|---|---|
| `P` | `PONG a2b-master v2 <i2c Hz>` |
| `W <addr> <hex>` | `OK` / `NAK` / `ERR …` |
| `R <addr> <n> <hex>` / `R <addr> <n> -` | `OK <hex>` / `NAK` / `ERR …` |
| `B <kHz>` | `OK <actual Hz>` (10–1000) |
| `LC` | `OK` (clears the RAM list) |
| `LA <hex>` | `OK <total bytes>` (≤ 300 bytes per line, 16384 total) |
| `LI` | `OK <bytes> <records> <crc16> <autorun> <flash bytes> <flash crc16>` |
| `LX [STOP]` | `S <idx> …` lines, then `DONE <records> <ok> <failed> <ms>` |
| `LS` / `LL` | `OK <bytes>` (save RAM list to flash / load flash list into RAM) |
| `LAUTO 0\|1` | `OK` |

Implementation details that the spec leaves open:

* Errors: `ERR ADDR`, `ERR LEN`, `ERR DATA`, `ERR ARG`, `ERR CMD` (parse errors), `ERR FULL`
  (`LA` would exceed 16384 bytes), `ERR LIST <offset>` (`LX`/`LS` on a list that does not
  parse), `ERR NOLIST` (`LL` with nothing valid in flash), `ERR FLASH` (erase, program or
  verify failed). I2C results are `NAK`, `ERR ARB`, `ERR TIMEOUT` (followed by a 9-clock bus
  recovery) and `ERR ABRT 0x<source>`.
* A record is malformed if its op is unknown, it is truncated, its address is > 0x7F, it is
  a write with `n = 0`, a long write with `n = 0` or `n > 4096`, or a read with `rn = 0`.
  Bytes after an `00` end record are ignored.
* `DONE <records>` is the number of records in the list. After `LX STOP` hits a failure,
  `ok + failed` is less than `records`.
* For a list that does not parse, `LI <records>` counts the valid records before the error.
  With nothing in flash, `LI` reports `0 FFFF` for flash and `autorun 0`.
* `LS` refuses a list that does not parse and keeps the autorun flag. `LAUTO 1` with nothing
  in flash stores an empty list with the flag set, so nothing runs at power-up.
* The RAM list survives a closed and reopened serial port. It is lost at reset (use `LS`).
* Long writes use record `04 addr7 n_lo n_hi bytes[n]` (n up to 4096), sent as one I2C
  transaction. The PC converter emits it for any SigmaStudio write longer than 255 bytes and
  never splits a write. The 161-action example has one: DSP program data, 366 bytes. It
  becomes 161 records, 1415 bytes, CRC `1B8E` (`test/test_fake.py:xml_to_records()` is a
  reference converter). The I2C timeout scales with length: about 380 ms for 4096 bytes at
  400 kHz.
* These details are also listed in the "Details" section of `../docs/USB_PROTOCOL.md`.

## Autorun

```
LC
LA 01380402F69000...        (as many LA lines as needed)
LX                          (try it)
LS                          (save to flash)
LAUTO 1                     (enable autorun)
```

At the next power-up, the Pico runs the stored list once, 500 ms after boot. To change the
delay, use `-DAUTORUN_DELAY_MS=...`. The list is also copied into the RAM list, so `LI`/`LX`
show it. If a USB host already has the port open (DTR set), it sees the normal `LX` lines
prefixed with `AUTO ` (`AUTO S 0 OK` … `AUTO DONE …`). Usually the host is not connected yet
at 500 ms and the output is dropped.

LED (GP25): on while the list runs. Then it stays on for 2 s if every record passed. If any
record failed, it blinks fast (5 Hz) until the first host command. The LED also blinks
briefly on every command.

Flash layout: the last 64 KB of flash hold a 256-byte header (magic `A2BL`, version, flags,
length, CRC-16 of the data, CRC-16 of the header) followed by the list. Only the sectors
needed are erased. Erase and program run through the SDK's `flash_safe_execute`, which in
this single-core program disables interrupts. USB is therefore unserviced for roughly
50–300 ms during `LS`/`LAUTO`, which hosts tolerate. The data is verified after programming.

## Testing without hardware: fake_pico

`test/fake_pico.c` is a native Linux build of the same `proto.c` and `cmdlist.c`. It serves
the line protocol over TCP (one client at a time) against a simulated I2C bus:

* `0x38` and `0x06` ACK every write, and reads return zeros.
* `0x6A` is a simulated AD2437 main node: 256 registers with auto-increment, `VENDOR`/
  `PRODUCT`/`VERSION` = `AD 37 21`, `NODEADR` (0x01), and `INTTYPE` (0x17) = `0x18` once
  `DISCVRY` (0x13) has been written.
* `0x6B` is the BUS proxy. With `NODEADR.PERI=0` it accesses the register file of sub-node 0
  or 1, and broadcast writes reach both. With `PERI=1` it accesses the peripheral at that
  node's `CHIP` (0x00): if `CHIP` equals `--tunnel-addr` (default 0x2A) and `--tunnel` is
  given, the transaction is forwarded to a tunnel device simulator, otherwise it gets a NAK.
* Any other address gets a NAK.

```sh
test/build_fake.sh /tmp/fake                          # -> /tmp/fake/fake_pico
/tmp/fake/fake_pico --port 9600 [--log] [--no-delay] [--flash /tmp/flash.bin] \
                    [--tunnel sim:127.0.0.1:9500] [--tunnel-addr 0x2A]
```

* `--log` prints every simulated transaction to stderr.
* `--no-delay` skips list delays.
* `--flash FILE` keeps the "flash" in a file. With `LAUTO 1`, autorun output goes to stdout
  at the next start.

Tunnel forwarding uses one TCP request per transaction:
`[kind 'W'|'R'][node][wlen u16 LE][rlen u16 LE][wdata]`, answered with
`[0 ACK | 1 NACK][rdata if ACK]`. This is the `sim:` framing of `../reference/transports.py`.

To run the protocol test (Python 3, stdlib only), which starts fake_pico itself and covers
raw I2C, the 161-action example via `LC`/`LA`/`LX`, list errors, `LX STOP`, flash and
autorun:

```sh
python3 test/test_fake.py --fake /tmp/fake/fake_pico
# optional tunnel checks against the tunnel device simulator:
../device/test/build_native.sh /tmp/nsim && /tmp/nsim/native_sim --port 9500 --node 0 &
python3 test/test_fake.py --fake /tmp/fake/fake_pico --tunnel-port 9500
```

To run the reference PC tunnel app (which wants a serial port) against fake_pico, bridge it
to a pty:

```sh
/tmp/fake/fake_pico --port 9600 --tunnel sim:127.0.0.1:9500 &
python3 test/pty_bridge.py --connect 127.0.0.1:9600 --link /tmp/fake_tty &
python3 ../reference/a2b_web_tunnel.py --transport serial:/tmp/fake_tty --a2b-base 0x6A --nodes 0
curl http://127.0.0.1:8080/index.html
```

# a2b_master Pico ⇄ PC protocol v2

Transport: USB CDC-ACM (a plain virtual COM port; Windows 10+ needs no driver).
The baud setting is ignored. Lines are ASCII terminated by `\n` (accept `\r\n`; replies
end `\r\n`). Hex is case-insensitive. For testing, the identical protocol is also served
over TCP by the native `fake_pico` (`tcp:127.0.0.1:9600`).

The bridge commands from v1 are unchanged. Every command gets exactly one reply line,
except `LX`, which streams lines.

## Raw I2C (v1)

| command | reply |
|---|---|
| `P` | `PONG a2b-master v2 <i2c Hz>` |
| `W <addr hex2> <data hex>` | `OK` / `NAK` / `ERR <text>` (write, STOP) |
| `R <addr hex2> <nread dec> <wdata hex or ->` | `OK <hex>` / `NAK` / `ERR <text>` (write, repeated start, read) |
| `B <kHz>` | `OK <actual Hz>` |

`ERR ARB` = arbitration lost, `ERR TIMEOUT`, `ERR ABRT 0x..`, `ERR <parse error>`.

## Command list (new in v2)

A command list is a byte string of records. This exact binary format is also what an
embedded master stores and replays (and what the app's "Export C" writes):

| op | record | meaning |
|---|---|---|
| `0x01` | `01 addr7 n bytes[n]` | I2C write of n bytes with STOP (bytes include the register address bytes) |
| `0x02` | `02 addr7 wn wbytes[wn] rn` | write wn bytes, repeated start, read rn bytes (wn may be 0 = plain read) |
| `0x03` | `03 ms_lo ms_hi` | delay ms (u16 LE) |
| `0x04` | `04 addr7 n_lo n_hi bytes[n]` | long I2C write (n u16 LE, 1..4096), one transaction with STOP. Use when n > 255 (e.g. DSP program download); never split a SigmaStudio write |
| `0x00` | `00` | end (optional; the list length also ends it) |

Records are numbered from 0 in order. Max list size 16384 bytes.

| command | reply |
|---|---|
| `LC` | `OK` – clear the RAM list |
| `LA <hex>` | `OK <total bytes>` – append bytes (≤ 300 bytes per line; records may span lines) |
| `LI` | `OK <bytes> <records> <crc16 hex4> <autorun 0/1> <flash bytes> <flash crc16 hex4>` – RAM and flash info (CRC-16/CCITT-FALSE: poly 0x1021, init 0xFFFF) |
| `LX [STOP]` | streams one line per record, then a summary (below). `STOP` = stop at the first failure |
| `LS` | `OK <bytes>` – save the RAM list to flash |
| `LL` | `OK <bytes>` – load the flash list into RAM |
| `LAUTO 0\|1` | `OK` – run the flash list automatically 500 ms after power up (flag kept in flash) |

`LX` lines:
```
S <idx> OK              write ok
S <idx> OK <hex>        read ok, data
S <idx> D <ms>          delay done
S <idx> NAK | S <idx> ERR <text>
DONE <records> <ok> <failed> <elapsed ms>
```
`LX` is only accepted when the list parses cleanly; otherwise `ERR LIST <byte offset>`.
No other command is processed while it runs.

## Details (as implemented in firmware v2)

* `P` replies `PONG a2b-master v2 <Hz>` (v1 replied `a2b-i2c-bridge v1`; hosts should only
  check the `PONG` prefix). All other v1 commands and replies are unchanged.
* Extra errors: `ERR FULL` (`LA` would exceed 16384 bytes), `ERR LIST <offset>` (`LX`/`LS` on
  a list that does not parse), `ERR NOLIST` (`LL` with nothing valid in flash), `ERR FLASH`
  (erase/program/verify failed). Parse errors: `ERR ADDR`, `ERR LEN`, `ERR DATA`, `ERR ARG`,
  `ERR CMD`; an over-long line gives `ERR LEN`.
* A record is malformed (→ `ERR LIST <offset of that record>`) if its op is unknown, it is
  truncated, addr7 > 0x7F, `01` has n = 0, `04` has n = 0 or n > 4096, or `02` has rn = 0.
  Bytes after an `00` end record are ignored. A long write (`04`) reports like a write:
  `S <idx> OK`.
* `DONE <records>` is the number of records in the list; after `LX STOP` hits a failure,
  `ok + failed < records`.
* `LI <records>` counts the valid records before the first malformed one. With nothing
  stored, the flash fields read `0 FFFF` and autorun `0`.
* `LS` refuses a list that does not parse and keeps the autorun flag. `LAUTO 1` with nothing
  stored stores an empty list with the flag set (nothing runs).
* Autorun: the flash list runs once 500 ms after power-up (also copied into the RAM list).
  If a host has the port open (DTR) by then, it receives the usual `LX` lines prefixed with
  `AUTO ` (`AUTO S 0 OK` … `AUTO DONE …`). LED: on while running, then 2 s on = all OK, fast
  blink until the next host command = a record failed.
* The RAM list survives closing/reopening the port; it is lost at reset.

## SigmaStudio(+) XML command list → records (done in the app)

`<ROM><page><action .../>…` in document order, across all pages:

* `instr="writeXbytes"`: `i2caddr` is a decimal 7-bit address, `addr` a decimal register
  address, `addr_width` the number of register address bytes (big-endian), and the body the
  data bytes in hex. Record `01 i2caddr n [addr bytes BE][data]`, or
  `04 i2caddr n_lo n_hi [addr bytes BE][data]` when n > 255 (never split). Check
  `len == addr_width + data bytes` (warn if not).
* `instr="read"`: record `02 i2caddr addr_width [addr BE] (len - addr_width)`.
* `instr="delay"`: body hex bytes form a big-endian value in ms → record `03`.
* `Protocol="SPI"` or an unknown instr: not supported – list it as an error and do not run.
* `CellName` and `ParamName` are kept by the app for display (record index ↔ label).

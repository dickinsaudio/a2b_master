# a2b_master

A2B bus bring-up and debug tool. It replaces the ADI USBi with a Raspberry Pi Pico.
Separate from the sub-node firmware (da_squid). The two share only the A2B
configuration and the web tunnel protocol.

```
a2b_master.exe ──USB CDC──► Pico (RP2040) ──I2C──► EVK USBi header ─► AD2437 main 0x6A/0x6B, DSP 0x38, codec 0x06
 (browser GUI,                (bridge + command-list                       ══A2B══► sub-nodes (RP2350 tunnel slave 0x2A)
  localhost:8080+node)         player, flash autorun)
```

| folder | what |
|---|---|
| `firmware/` | Pico firmware: USB→I2C bridge, command-list player (`cmdlist.c`, portable, the seed of the embedded master), flash store and autorun. `test/fake_pico` is a native build that simulates the AD2437 bus. |
| `app/` | `a2b_master.exe`: portable Windows tool (Go, one static exe, GUI in the browser). Replays SigmaStudio+ XML, has a register console and a C-array export, and does the tunnel port forwarding. |
| `docs/USB_PROTOCOL.md` | Pico ⇄ PC line protocol and the binary command-list format. |
| `docs/TUNNEL_PROTOCOL.md` | Web tunnel protocol (copy; the device side is in da_squid `a2b_tunnel/`). |
| `examples/` | SigmaStudio+ 2.3 exports for the AD2437 EVK (command list and bus config). |
| `reference/` | Python reference implementation of the tunnel PC side, plus a device simulator. |

## Quick start

1. Flash `a2b_master.uf2` to a Pico (hold BOOTSEL, then copy the file).
2. Wire GP4 = SDA, GP5 = SCL, GND to the EVK's USBi header. Check the pinout on the EVK
   schematic. Don't connect power; logic is 3.3 V. Leave the USBi unplugged.
3. Run `a2b_master.exe`. It opens the GUI in your browser. Pick the COM port, load
   `adi_a2b_commandlist.xml`, and **Run on Pico** (expect `DONE 161 161 0`).
4. **Port forwarding → Start**, then browse `http://localhost:8080+node/`.

## Status

Everything has been tested end to end in simulation only: app → fake Pico (the
real parser and player) → simulated AD2437 → da_squid tunnel harness running the
real HTTP server. The XML replay was byte-exact and the tunnel ran at about
28 kB/s at 25 µs/byte. **Nothing has run on real hardware yet.**

Next steps: a reset GPIO, if the EVK header has one; lean bus code driven by
`busconfig.xml`, which covers the per-node slot and channel maps.

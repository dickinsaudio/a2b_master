// a2b-master line protocol v2 (portable C, no SDK dependencies).  See docs/USB_PROTOCOL.md.
//
// Shared by the RP2040 firmware (main.c) and the native simulator (test/fake_pico.c).  The
// platform supplies the I2C transfer, timing and output hooks; the persistent list store is
// bound at link time through flash_store.h.
#ifndef PROTO_H
#define PROTO_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "cmdlist.h"

#define PROTO_MAX_DATA 300                          // data bytes per W / R / LA line
#define PROTO_LINE_MAX (16 + 2 * PROTO_MAX_DATA + 64)

typedef struct {
    void *ctx;
    // Write wn bytes (if any), repeated START, read rn bytes (if any), STOP.  wn + rn > 0.
    // Returns an enum i2c_status; *detail = abort source for I2C_ST_ABRT.
    int (*xfer)(void *ctx, uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r, size_t rn,
                uint32_t *detail);
    uint32_t (*set_baud)(void *ctx, uint32_t hz);   // returns the actual rate
    uint32_t (*get_baud)(void *ctx);
    void (*delay_ms)(void *ctx, uint32_t ms);
    uint32_t (*now_ms)(void *ctx);
    void (*out)(void *ctx, const char *line);       // emit one line; platform adds "\r\n"
    void (*activity)(void *ctx);                    // optional: once per command (LED)
} proto_platform_t;

typedef struct {
    const proto_platform_t *pf;
    char line[PROTO_LINE_MAX];
    size_t line_len;
    bool overflow;
    uint8_t list[CMDLIST_MAX_BYTES];                // the RAM command list
    size_t list_len;
    const char *prefix;                             // prepended to streamed LX lines
} proto_t;

void proto_init(proto_t *p, const proto_platform_t *pf);

// Feed one received character; complete lines are executed (replies go to pf->out).
void proto_feed(proto_t *p, int c);

// Execute one command line (modified in place).
void proto_handle_line(proto_t *p, char *line);

// Run a list, streaming "S ..." lines and the "DONE ..." summary, each prefixed by prefix
// (may be NULL).  Returns 0 if it ran, -1 if it does not parse ("ERR LIST <off>" emitted).
int proto_run_list(proto_t *p, const uint8_t *list, size_t len, unsigned flags,
                   const char *prefix, cmdlist_stats_t *stats);

// Copy the flash list into the RAM list.  Returns its length, or -1 if none is stored.
int proto_load_flash(proto_t *p);

#endif

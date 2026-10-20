// Command-list records (portable C, no SDK dependencies).
//
// A command list is a byte string of records (see docs/USB_PROTOCOL.md):
//   01 addr7 n bytes[n]              I2C write of n bytes with STOP
//   02 addr7 wn wbytes[wn] rn        write wn bytes, repeated START, read rn bytes
//                                    (wn = 0: plain read)
//   03 ms_lo ms_hi                   delay (u16 LE milliseconds)
//   04 addr7 n_lo n_hi bytes[n]      long I2C write (n u16 LE, 1..4096) with STOP
//   00                               end (optional; the list length also ends it)
// Records are numbered from 0.  Bytes after an 00 end record are ignored.
#ifndef CMDLIST_H
#define CMDLIST_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define CMDLIST_MAX_BYTES 16384u

enum {
    CMDLIST_OP_END = 0x00,
    CMDLIST_OP_WRITE = 0x01,
    CMDLIST_OP_WRITE_READ = 0x02,
    CMDLIST_OP_DELAY = 0x03,
    CMDLIST_OP_WRITE_LONG = 0x04,
};

#define CMDLIST_MAX_LONG_WRITE 4096u

typedef struct {
    uint8_t op;             // CMDLIST_OP_*
    uint8_t addr;           // 7-bit I2C address (WRITE / WRITE_READ)
    const uint8_t *wdata;   // points into the list
    uint16_t wn;            // bytes to write (up to 4096 for WRITE_LONG)
    uint8_t rn;             // bytes to read (WRITE_READ)
    uint16_t ms;            // DELAY
} cmdlist_rec_t;

// Decode the record at *off.  Returns 1 and advances *off on success, 0 at the end of the
// list (length reached or an 00 record), -1 if the record is malformed (*off unchanged).
// Malformed: unknown op, truncated, address > 0x7F, write with n = 0, long write with
// n = 0 or n > 4096, read with rn = 0.
int cmdlist_next(const uint8_t *list, size_t len, size_t *off, cmdlist_rec_t *rec);

// Check a whole list.  Returns true if it parses cleanly.  *records (optional) receives the
// number of valid records (up to the error), *err_off (optional) the byte offset of the first
// malformed record (or len if none).
bool cmdlist_validate(const uint8_t *list, size_t len, uint32_t *records, size_t *err_off);

// One executed record, passed to the report callback.
typedef struct {
    uint32_t idx;           // record number
    const cmdlist_rec_t *rec;
    int status;             // enum i2c_status (always I2C_ST_OK for a delay)
    uint32_t detail;        // back-end detail (abort source) for I2C_ST_ABRT
    const uint8_t *rdata;   // read data (rec->rn bytes) when status == OK and op == WRITE_READ
} cmdlist_event_t;

typedef struct {
    // Return an enum i2c_status; *detail may be set for I2C_ST_ABRT.
    int (*write)(void *ctx, uint8_t addr, const uint8_t *buf, size_t n, uint32_t *detail);
    int (*write_read)(void *ctx, uint8_t addr, const uint8_t *wbuf, size_t wn, uint8_t *rbuf,
                      size_t rn, uint32_t *detail);
    void (*delay_ms)(void *ctx, uint32_t ms);
    uint32_t (*now_ms)(void *ctx);                          // optional (elapsed time)
    void (*report)(void *ctx, const cmdlist_event_t *ev);   // optional
    void *ctx;
} cmdlist_ops_t;

typedef struct {
    uint32_t records;       // records in the list
    uint32_t executed;      // records executed (< records if stopped early)
    uint32_t ok;
    uint32_t failed;
    uint32_t elapsed_ms;
    bool stopped;           // stop_on_error triggered
} cmdlist_stats_t;

#define CMDLIST_STOP_ON_ERROR 0x01u

// Validate, then run the list.  Returns 0 when the list ran (check stats->failed), or -1 if it
// does not parse (nothing executed; *err_off = offset of the bad record).
int cmdlist_run(const uint8_t *list, size_t len, const cmdlist_ops_t *ops, unsigned flags,
                cmdlist_stats_t *stats, size_t *err_off);

// CRC-16/CCITT-FALSE: poly 0x1021, init 0xFFFF, no reflection, no xor-out.
// Pass crc = 0xFFFF to start; chain calls to continue.
uint16_t crc16_ccitt(uint16_t crc, const uint8_t *data, size_t len);

#endif

// Persistent command-list store in the last 64 KB of flash (RP2040 / RP2350).
//
// Layout at FLASH_STORE_OFFSET (= flash size - 64 KB):
//   page 0 (256 bytes): header
//     u32 magic 'A2BL'  u16 version (1)  u16 flags (bit0 autorun)  u32 len
//     u16 crc16(data)   u16 crc16(header bytes 0..13)
//   offset 256: len bytes of command list
// Only the sectors that are needed are erased.  Erase/program run through the SDK's
// flash_safe_execute, which (single-core program, no pico_multicore) disables interrupts for
// the duration; USB is briefly unserviced (tens to a few hundred ms), which hosts tolerate.
#include "flash_store.h"

#include <string.h>

#include "cmdlist.h"
#include "hardware/flash.h"
#include "pico/flash.h"
#include "pico/stdlib.h"

#define STORE_SIZE (64u * 1024u)
#define FLASH_STORE_OFFSET (PICO_FLASH_SIZE_BYTES - STORE_SIZE)
#define HDR_SIZE FLASH_PAGE_SIZE
#define MAGIC 0x4C423241u  // "A2BL" little-endian
#define VERSION 1u

_Static_assert(HDR_SIZE + CMDLIST_MAX_BYTES <= STORE_SIZE, "store too small");

typedef struct {
    uint32_t magic;
    uint16_t version;
    uint16_t flags;
    uint32_t len;
    uint16_t data_crc;
    uint16_t hdr_crc;
} store_hdr_t;

_Static_assert(sizeof(store_hdr_t) == 16, "header layout");

static const uint8_t *store_base(void) { return (const uint8_t *)(XIP_BASE + FLASH_STORE_OFFSET); }

bool flash_store_get(const uint8_t **data, uint32_t *len, bool *autorun) {
    store_hdr_t h;
    memcpy(&h, store_base(), sizeof h);
    *data = NULL;
    *len = 0;
    *autorun = false;
    if (h.magic != MAGIC || h.version != VERSION || h.len > CMDLIST_MAX_BYTES) return false;
    if (crc16_ccitt(0xFFFF, (const uint8_t *)&h, 14) != h.hdr_crc) return false;
    const uint8_t *d = store_base() + HDR_SIZE;
    if (crc16_ccitt(0xFFFF, d, h.len) != h.data_crc) return false;
    *data = d;
    *len = h.len;
    *autorun = (h.flags & 1u) != 0;
    return true;
}

static uint8_t __attribute__((aligned(4))) g_img[HDR_SIZE + CMDLIST_MAX_BYTES];

typedef struct {
    uint32_t prog_len;
    uint32_t erase_len;
} flash_job_t;

static void do_flash(void *param) {
    const flash_job_t *j = param;
    flash_range_erase(FLASH_STORE_OFFSET, j->erase_len);
    flash_range_program(FLASH_STORE_OFFSET, g_img, j->prog_len);
}

int flash_store_put(const uint8_t *data, uint32_t len, bool autorun) {
    if (len > CMDLIST_MAX_BYTES || (len && !data)) return -1;
    // Build the whole image in RAM first: data may point into the store being erased.
    memset(g_img, 0xFF, sizeof g_img);
    if (len) memmove(g_img + HDR_SIZE, data, len);
    store_hdr_t h = {MAGIC, VERSION, autorun ? 1u : 0u, len,
                     crc16_ccitt(0xFFFF, g_img + HDR_SIZE, len), 0};
    h.hdr_crc = crc16_ccitt(0xFFFF, (const uint8_t *)&h, 14);
    memcpy(g_img, &h, sizeof h);

    const uint32_t total = HDR_SIZE + len;
    flash_job_t job = {
        (total + FLASH_PAGE_SIZE - 1) / FLASH_PAGE_SIZE * FLASH_PAGE_SIZE,
        (total + FLASH_SECTOR_SIZE - 1) / FLASH_SECTOR_SIZE * FLASH_SECTOR_SIZE,
    };
    if (flash_safe_execute(do_flash, &job, 100) != PICO_OK) return -2;
    if (memcmp(store_base(), g_img, job.prog_len) != 0) return -3;
    return 0;
}

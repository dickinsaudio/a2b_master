// Persistent command-list store (portable interface).
//
// RP2040/RP2350 implementation: flash_store.c (last 64 KB of flash).  The native fake_pico
// supplies its own implementation (RAM, optionally backed by a file).
#ifndef FLASH_STORE_H
#define FLASH_STORE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// Look up the stored list.  Returns true if a valid record exists (header magic and both CRCs
// check out); *data then points at the stored bytes (read-only, may be memory-mapped flash),
// *len is its length (may be 0) and *autorun the flag.  Returns false (len 0, autorun false)
// if nothing valid is stored.
bool flash_store_get(const uint8_t **data, uint32_t *len, bool *autorun);

// Replace the stored list and flag.  Returns 0 on success, < 0 on error (too long, erase /
// program failed, verify mismatch).  data may point into the store itself (it is copied
// to RAM before anything is erased).
int flash_store_put(const uint8_t *data, uint32_t len, bool autorun);

#endif

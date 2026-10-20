// Register-level I2C master for the RP2040 / RP2350 DW_apb_i2c controller.
//
// Drives the controller registers directly (rather than the SDK i2c_*_blocking helpers) so
// the abort source is preserved: arbitration loss (another master such as an ADI USBi on the
// bus) is reported separately from a NAK.  A timed-out transfer triggers a 9-clock bus
// recovery and re-initialisation of the controller before returning.
#ifndef I2C_MASTER_H
#define I2C_MASTER_H

#include <stddef.h>
#include <stdint.h>

#include "hardware/i2c.h"
#include "i2c_status.h"

typedef struct {
    i2c_inst_t *inst;
    unsigned sda_pin;
    unsigned scl_pin;
    uint32_t baud;      // actual rate (Hz)
} i2cm_t;

// Configure the controller and pins (internal pull-ups enabled) at hz.
void i2cm_init(i2cm_t *m, i2c_inst_t *inst, unsigned sda_pin, unsigned scl_pin, uint32_t hz);

// Change the bus rate; returns the actual rate.
uint32_t i2cm_set_baud(i2cm_t *m, uint32_t hz);

// Write wn bytes (if any), repeated START, read rn bytes (if any), STOP.  wn + rn must be > 0;
// any length is fine (the timeout scales with it).
// Returns an enum i2c_status; *abrt (optional) receives the abort source register.
int i2cm_xfer(i2cm_t *m, uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r, size_t rn,
              uint32_t *abrt);

// Clock out a slave holding SDA low (up to 9 SCL pulses + STOP), then re-init the controller.
void i2cm_recover(i2cm_t *m);

#endif

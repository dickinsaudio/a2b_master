// Register-level I2C master (RP2040 / RP2350).  See i2c_master.h.
#include "i2c_master.h"

#include "hardware/gpio.h"
#include "pico/stdlib.h"

static void bus_setup(i2cm_t *m) {
    m->baud = i2c_init(m->inst, m->baud);
    gpio_set_function(m->sda_pin, GPIO_FUNC_I2C);
    gpio_set_function(m->scl_pin, GPIO_FUNC_I2C);
    gpio_pull_up(m->sda_pin);
    gpio_pull_up(m->scl_pin);
}

void i2cm_init(i2cm_t *m, i2c_inst_t *inst, unsigned sda_pin, unsigned scl_pin, uint32_t hz) {
    m->inst = inst;
    m->sda_pin = sda_pin;
    m->scl_pin = scl_pin;
    m->baud = hz;
    bus_setup(m);
}

uint32_t i2cm_set_baud(i2cm_t *m, uint32_t hz) {
    m->baud = i2c_set_baudrate(m->inst, hz);
    return m->baud;
}

// If a slave is holding SDA low, clock SCL up to 9 times (open-drain emulation via pull-up /
// drive-low) and issue a STOP, then re-init the controller.
void i2cm_recover(i2cm_t *m) {
    const unsigned sda = m->sda_pin, scl = m->scl_pin;
    i2c_deinit(m->inst);
    gpio_init(sda);
    gpio_init(scl);
    gpio_pull_up(sda);
    gpio_pull_up(scl);
    gpio_set_dir(sda, GPIO_IN);
    gpio_set_dir(scl, GPIO_IN);
    gpio_put(sda, 0);
    gpio_put(scl, 0);
    sleep_us(10);
    if (gpio_get(scl) && !gpio_get(sda)) {
        for (int i = 0; i < 9 && !gpio_get(sda); i++) {
            gpio_set_dir(scl, GPIO_OUT);  // SCL low
            sleep_us(5);
            gpio_set_dir(scl, GPIO_IN);   // SCL released
            sleep_us(5);
        }
        // STOP: SDA low -> SCL high -> SDA high
        gpio_set_dir(sda, GPIO_OUT);
        sleep_us(5);
        gpio_set_dir(sda, GPIO_IN);
        sleep_us(5);
    }
    bus_setup(m);
}

int i2cm_xfer(i2cm_t *m, uint8_t addr, const uint8_t *w, size_t wlen, uint8_t *r, size_t rlen,
              uint32_t *abrt_out) {
    i2c_hw_t *hw = i2c_get_hw(m->inst);
    const size_t total = wlen + rlen;
    uint32_t dummy;
    if (!abrt_out) abrt_out = &dummy;
    *abrt_out = 0;
    if (total == 0) return I2C_ST_ABRT;

    hw->enable = 0;
    hw->tar = addr;
    hw->enable = 1;
    (void)hw->clr_intr;  // clear stale STOP_DET / TX_ABRT etc.

    // Generous timeout: 4x nominal byte time per byte plus 20 ms slack (covers waiting for a
    // busy bus held by another master, and clock stretching).  Scales with length: a
    // 4096-byte write at 400 kHz gets 20 ms + 4097 * 22 us * 4 = ~380 ms (at 10 kHz ~15 s).
    // If the TX FIFO runs dry mid-write the controller holds SCL low (no early STOP).
    uint32_t byte_us = (uint32_t)(9ull * 1000000ull / (m->baud ? m->baud : 100000));
    absolute_time_t deadline =
        make_timeout_time_us(20000 + (uint64_t)(total + 1) * byte_us * 4);

    size_t issued = 0, received = 0;
    uint32_t abrt = 0;

    for (;;) {
        uint32_t raw = hw->raw_intr_stat;
        if (raw & I2C_IC_RAW_INTR_STAT_TX_ABRT_BITS) {
            abrt = hw->tx_abrt_source;
            break;
        }
        // Queue commands while there is TX FIFO room (and RX room for outstanding reads).
        while (issued < total && i2c_get_write_available(m->inst) > 0) {
            bool is_read = issued >= wlen;
            if (is_read && (issued - wlen) - received >= 8) break;
            uint32_t cmd = 0;
            if (is_read) {
                cmd |= I2C_IC_DATA_CMD_CMD_BITS;
                if (issued == wlen && wlen > 0) cmd |= I2C_IC_DATA_CMD_RESTART_BITS;
            } else {
                cmd |= w[issued];
            }
            if (issued == total - 1) cmd |= I2C_IC_DATA_CMD_STOP_BITS;
            hw->data_cmd = cmd;
            issued++;
        }
        while (received < rlen && i2c_get_read_available(m->inst) > 0)
            r[received++] = (uint8_t)hw->data_cmd;

        // Done once everything is queued/received, our STOP has gone out (STOP_DET) and the
        // controller itself is idle (guards against a STOP from another master).
        if (issued == total && received == rlen &&
            (raw & I2C_IC_RAW_INTR_STAT_STOP_DET_BITS) && (hw->status & I2C_IC_STATUS_TFE_BITS) &&
            !(hw->status & I2C_IC_STATUS_MST_ACTIVITY_BITS))
            break;
        if (time_reached(deadline)) {
            i2cm_recover(m);
            return I2C_ST_TIMEOUT;
        }
        tight_loop_contents();
    }

    if (abrt) {
        (void)hw->clr_tx_abrt;
        // Hardware issues STOP on abort (if it owned the bus); wait briefly for it.
        absolute_time_t t = make_timeout_time_us(2000);
        while (!(hw->raw_intr_stat & I2C_IC_RAW_INTR_STAT_STOP_DET_BITS) && !time_reached(t))
            tight_loop_contents();
    }
    (void)hw->clr_stop_det;
    *abrt_out = abrt;

    if (!abrt) return I2C_ST_OK;
    if (abrt & I2C_IC_TX_ABRT_SOURCE_ARB_LOST_BITS) return I2C_ST_ARB;
    if (abrt & (I2C_IC_TX_ABRT_SOURCE_ABRT_7B_ADDR_NOACK_BITS |
                I2C_IC_TX_ABRT_SOURCE_ABRT_TXDATA_NOACK_BITS |
                I2C_IC_TX_ABRT_SOURCE_ABRT_GCALL_NOACK_BITS))
        return I2C_ST_NAK;
    return I2C_ST_ABRT;
}

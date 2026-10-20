/*******************************************************************
 * Copyright (C) 2026 DickinsAudio
 *
 * A2B I2C WEB TUNNEL - RP2350 platform layer
 *
 *  - I2C0 slave driver feeding tunnel_proto (ISR on the core that starts it)
 *  - Dual role: slave by default, master only around amplifier transactions
 *  - W5500 probe and virtual chip install
 *******************************************************************/

#include "a2b_tunnel.h"
#include "tunnel_proto.h"
#include "vw5500.h"

#include <stdio.h>
#include "pico/stdlib.h"
#include "hardware/i2c.h"
#include "hardware/irq.h"
#include "hardware/sync.h"

#include "wizchip_conf.h"
#include "wizchip_spi.h"
#include "hardware/spi.h"
#ifdef USE_PIO
#include "wizchip_qspi_pio.h"
#endif

#define TUNNEL_I2C      i2c0
#define TUNNEL_IRQ      I2C0_IRQ
#ifndef I2C_MASTER_BAUD
#define I2C_MASTER_BAUD 50000                   // Same as peripherals.cpp I2C_SPEED (used after a block reset)
#endif

#define SLAVE_INTR_MASK (I2C_IC_INTR_MASK_M_RX_FULL_BITS  | \
                         I2C_IC_INTR_MASK_M_RD_REQ_BITS   | \
                         I2C_IC_INTR_MASK_M_STOP_DET_BITS | \
                         I2C_IC_INTR_MASK_M_TX_ABRT_BITS)

static volatile bool slave_on;
static bool          tunnel_only;
static uint32_t      master_switches;
static uint32_t      master_wait_timeouts;


////////////////////////////////////////////////////////////////////////////////
// LOCK FOR THE VIRTUAL CHIP

static spin_lock_t *vw_lock;
static uint32_t     vw_irq_save;

void vw_platform_lock(void)
{
    uint32_t save = spin_lock_blocking(vw_lock);
    vw_irq_save = save;
}

void vw_platform_unlock(void)
{
    spin_unlock(vw_lock, vw_irq_save);
}


////////////////////////////////////////////////////////////////////////////////
// I2C SLAVE

static void drain_rx(i2c_hw_t *hw)
{
    while (hw->rxflr)
    {
        uint32_t d = hw->data_cmd;
        tp_rx((uint8_t)d, (d & I2C_IC_DATA_CMD_FIRST_DATA_BYTE_BITS) != 0);
    }
}

static volatile uint32_t isr_count;             // Progress marker for the stuck-bus watchdog

static void __isr tunnel_i2c_irq(void)
{
    isr_count++;
    i2c_hw_t *hw = i2c_get_hw(TUNNEL_I2C);
    uint32_t st = hw->intr_stat;

    if (st & I2C_IC_INTR_STAT_R_TX_ABRT_BITS) (void)hw->clr_tx_abrt;
    drain_rx(hw);
    if (st & I2C_IC_INTR_STAT_R_RD_REQ_BITS)
    {
        (void)hw->clr_rd_req;
        hw->data_cmd = tp_tx();
    }
    if (st & I2C_IC_INTR_STAT_R_STOP_DET_BITS)
    {
        (void)hw->clr_stop_det;
        drain_rx(hw);
        tp_stop();
    }
}

// The block ignores IC_CON / IC_SAR writes until IC_ENABLE_STATUS.IC_EN reads 0, and it
// will not disable while a transfer is pending. The SDK's i2c_set_slave_mode() does not
// wait for that, and a master transfer that timed out is left stalled holding SCL - so a
// mode change could silently not happen, leaving I2C0 a master and the tunnel NAKing.

static uint32_t aborts, hard_resets, fixups, bus_recoveries;
static volatile bool in_master;                 // Between i2c_master_acquire and _release

static void configure_mode(i2c_hw_t *hw, bool slave)
{
    uint32_t con = hw->con & ~(I2C_IC_CON_MASTER_MODE_BITS | I2C_IC_CON_IC_SLAVE_DISABLE_BITS |
                               I2C_IC_CON_RX_FIFO_FULL_HLD_CTRL_BITS);
    con |= slave ? (I2C_IC_CON_RX_FIFO_FULL_HLD_CTRL_BITS | I2C_IC_CON_STOP_DET_IFADDRESSED_BITS)
                 : (I2C_IC_CON_MASTER_MODE_BITS | I2C_IC_CON_IC_SLAVE_DISABLE_BITS);
    hw->con   = con;
    hw->sar   = A2B_TUNNEL_I2C_ADDR;
    hw->rx_tl = 0;
    hw->tx_tl = 0;                          // SDK master transfers wait on TX_EMPTY per byte
}

static bool wait_disabled(i2c_hw_t *hw, uint32_t us)
{
    absolute_time_t t = make_timeout_time_us(us);
    while (hw->enable_status & I2C_IC_ENABLE_STATUS_IC_EN_BITS)
        if (time_reached(t)) return false;
    return true;
}

// Last resort: reset the whole block (releases SCL/SDA whatever state it was in)
static void hard_reset(bool slave)
{
    i2c_hw_t *hw = i2c_get_hw(TUNNEL_I2C);
    hw->intr_mask = 0;
    i2c_init(TUNNEL_I2C, I2C_MASTER_BAUD);                  // Resets the block, sets master timing
    hw->enable = 0;
    wait_disabled(hw, 1000);
    configure_mode(hw, slave);
    hw->enable = 1;
    tp_abort();
    hard_resets++;
}

static void set_mode(bool slave)
{
    i2c_hw_t *hw = i2c_get_hw(TUNNEL_I2C);
    hw->intr_mask = 0;
    if (hw->con & I2C_IC_CON_MASTER_MODE_BITS)                         // Let a normal STOP finish first
    {
        absolute_time_t t = make_timeout_time_us(1000);
        while ((hw->status & I2C_IC_STATUS_MST_ACTIVITY_BITS) && !time_reached(t)) tight_loop_contents();
    }
    if ((hw->con & I2C_IC_CON_MASTER_MODE_BITS) && (hw->status & I2C_IC_STATUS_MST_ACTIVITY_BITS))
    {
        hw->enable = I2C_IC_ENABLE_ENABLE_BITS | I2C_IC_ENABLE_ABORT_BITS;   // Stalled master: STOP it
        absolute_time_t t = make_timeout_time_us(2000);
        while ((hw->enable & I2C_IC_ENABLE_ABORT_BITS) && !time_reached(t)) tight_loop_contents();
        (void)hw->clr_tx_abrt;
        aborts++;
    }
    hw->enable = 0;
    if (!wait_disabled(hw, 5000)) { hard_reset(slave); }
    else
    {
        configure_mode(hw, slave);
        hw->enable = 1;
    }
    if (slave)
    {
        (void)hw->clr_intr;
        hw->intr_mask = SLAVE_INTR_MASK;        // Masked at the block, so this is safe from either core
    }
}

static bool is_slave(i2c_hw_t *hw)
{
    return (hw->enable_status & I2C_IC_ENABLE_STATUS_IC_EN_BITS) &&
           !(hw->con & (I2C_IC_CON_MASTER_MODE_BITS | I2C_IC_CON_IC_SLAVE_DISABLE_BITS)) &&
           hw->sar == A2B_TUNNEL_I2C_ADDR && hw->intr_mask == SLAVE_INTR_MASK;
}

// Strong versions of the weak hooks in peripherals.cpp. Called with the i2c mutex held.
void i2c_master_acquire(void)
{
    if (!slave_on) return;
    i2c_hw_t *hw = i2c_get_hw(TUNNEL_I2C);
    in_master = true;
    absolute_time_t t = make_timeout_time_us(3000);           // Let any remote transaction finish
    while ((hw->status & I2C_IC_STATUS_SLV_ACTIVITY_BITS) && !time_reached(t)) tight_loop_contents();
    if (hw->status & I2C_IC_STATUS_SLV_ACTIVITY_BITS) master_wait_timeouts++;
    set_mode(false);
    tp_abort();
    master_switches++;
}

void i2c_master_release(void)
{
    if (!slave_on) return;
    set_mode(true);                                         // Also aborts a master transfer left stalled
    in_master = false;
}

// Every 10 ms: make sure we really are an enabled slave, and recover a bus left stuck by
// a remote access that was abandoned mid-byte (the AD24xx gives up after ~0.6 ms stretch).
#define STUCK_TICKS  5

static bool watchdog(repeating_timer_t *rt)
{
    (void)rt;
    static int stuck;
    if (!slave_on || in_master) { stuck = 0; return true; }
    i2c_hw_t *hw = i2c_get_hw(TUNNEL_I2C);
    if (!is_slave(hw))
    {
        set_mode(true);
        fixups++;                               // Reported by a2b_tunnel_poll() (no printf in IRQ context)
        return true;
    }
    // Stuck = mid transfer, NO interrupt activity at all, and the same line low on every sample.
    // (A busy but healthy bus has lines low at random instants - that must never trip this,
    // as resetting the block aborts a live transaction.)
    static uint32_t last_isr;
    static int      low_mask;                   // bit0 SCL, bit1 SDA - low on every sample so far
    int now_low = (!gpio_get(PICO_I2C_SCL_PIN) ? 1 : 0) | (!gpio_get(PICO_I2C_SDA_PIN) ? 2 : 0);
    bool idle_isr = isr_count == last_isr;
    last_isr = isr_count;
    if ((hw->status & I2C_IC_STATUS_SLV_ACTIVITY_BITS) && idle_isr && now_low)
    {
        low_mask = stuck ? (low_mask & now_low) : now_low;
        stuck = low_mask ? stuck + 1 : 0;
    }
    else stuck = 0;
    if (stuck >= STUCK_TICKS)
    {
        hard_reset(true);
        (void)hw->clr_intr;
        hw->intr_mask = SLAVE_INTR_MASK;
        stuck = 0;
        bus_recoveries++;
    }
    return true;
}

static repeating_timer_t watchdog_timer;


////////////////////////////////////////////////////////////////////////////////
// W5500 PROBE AND START

extern void i2c_initialize(void);               // peripherals.cpp

// Read VERSIONR (common block 0x0039) straight off the SPI bus - expect 0x04
bool a2b_tunnel_w5500_present(void)
{
    uint8_t hdr[3] = { 0x00, 0x39, 0x00 };      // Address 0x0039, BSB 0 (common), read, variable length
    uint8_t v = 0;
#ifdef USE_PIO
    extern wiznet_spi_handle_t spi_handle;      // wizchip_spi.c
    (*spi_handle)->frame_end();
    (*spi_handle)->frame_start();
    (*spi_handle)->write_buffer(hdr, 3);
    v = (*spi_handle)->read_byte();
    (*spi_handle)->frame_end();
#else
    gpio_put(PIN_CS, 0);
    spi_write_blocking(SPI_PORT, hdr, 3);
    spi_read_blocking(SPI_PORT, 0xFF, &v, 1);
    gpio_put(PIN_CS, 1);
#endif
    printf("A2B TUNNEL: W5500 VERSIONR = 0x%02X\n", v);
    return v == 0x04;
}

void a2b_tunnel_start(bool w5500_present)
{
    if (!vw_lock) vw_lock = spin_lock_init(spin_lock_claim_unused(true));

    tunnel_only = !w5500_present;
    if (tunnel_only)
    {
        // No real chip, so nothing to capture - the whole register map is virtual.
        // Bring it up through the normal driver so the ioLibrary state is consistent.
        vw_install(A2B_TUNNEL_SOCKETS_ONLY, false);
        uint8_t memsize[2][8] = {{2,2,2,2,2,2,2,2},{2,2,2,2,2,2,2,2}};
        ctlwizchip(CW_INIT_WIZCHIP, (void *)memsize);
    }
    else vw_install(A2B_TUNNEL_SOCKETS_WITH_W5500, true);

    i2c_initialize();                                       // Pins, pull ups, master baud (no-op if done)
    irq_set_exclusive_handler(TUNNEL_IRQ, tunnel_i2c_irq);
    irq_set_priority(TUNNEL_IRQ, PICO_HIGHEST_IRQ_PRIORITY);   // Nothing else on this core may delay a reply
    set_mode(true);
    slave_on = true;
    irq_set_enabled(TUNNEL_IRQ, true);                      // ISR runs on this core (core 0, the web core)
    add_repeating_timer_ms(10, watchdog, 0, &watchdog_timer);

    printf("A2B TUNNEL: %s, I2C slave 0x%02X, virtual sockets 0x%02X\n",
           tunnel_only ? "NO W5500 - TUNNEL ONLY" : "W5500 + TUNNEL", A2B_TUNNEL_I2C_ADDR, vw_virtual_mask());
}

bool a2b_tunnel_only(void) { return tunnel_only; }

int a2b_tunnel_stats_text(char *buf, int len)
{
    const vw_stats_t *v = vw_get_stats();
    const tp_stats_t *t = tp_get_stats();
    return snprintf(buf, len,
        "A2B TUNNEL  mode %s  addr 0x%02X  mask 0x%02X\n"
        "  i2c transactions %lu  crc errors %lu  bad ops %lu  overruns %lu\n"
        "  connects %lu  rx bytes %lu  tx bytes %lu\n"
        "  frames real %lu  virtual %lu  master switches %lu  wait timeouts %lu\n"
        "  master aborts %lu  hard resets %lu  mode fixups %lu  bus recoveries %lu\n",
        tunnel_only ? "tunnel only" : "w5500+tunnel", A2B_TUNNEL_I2C_ADDR, vw_virtual_mask(),
        t->transactions, t->crc_errors, t->bad_ops, t->overruns,
        v->connects, v->rx_bytes, v->tx_bytes,
        v->frames_real, v->frames_virtual, master_switches, master_wait_timeouts,
        aborts, hard_resets, fixups, bus_recoveries);
}

// Call from the main loop (~1 s). Silent in normal operation; prints one line whenever an
// error or recovery counter moves.
void a2b_tunnel_poll(void)
{
    if (!slave_on) return;
    static uint32_t last_sum;
    const tp_stats_t *t = tp_get_stats();
    uint32_t sum = t->crc_errors + t->bad_ops + t->overruns + aborts + hard_resets + fixups + bus_recoveries;
    if (sum == last_sum) return;
    last_sum = sum;
    printf("A2B TUNNEL: txn %lu crc %lu bad %lu ovr %lu | master switches %lu, aborts %lu, block resets %lu, "
           "mode fixups %lu, stuck-bus recoveries %lu\n",
           t->transactions, t->crc_errors, t->bad_ops, t->overruns,
           master_switches, aborts, hard_resets, fixups, bus_recoveries);
}

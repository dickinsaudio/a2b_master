// a2b-master v2: USB-CDC (stdio) -> I2C master bridge + command-list player for an AD242x /
// AD243x A2B main node.  Protocol: docs/USB_PROTOCOL.md (handled by proto.c).
//
// Modules:  proto.c       line protocol (portable, shared with test/fake_pico.c)
//           cmdlist.c     command-list records / player (portable)
//           i2c_master.c  register-level I2C master (arbitration, timeouts, recovery)
//           flash_store.c list + autorun flag in the last 64 KB of flash
//
// Autorun: if enabled (LAUTO 1) the flash list runs once 500 ms after power-up.  Its lines are
// printed with an "AUTO " prefix if a USB host has the port open by then.  LED: on while it
// runs; then on for 2 s = all OK, fast blink until the first host command = a record failed.

#include <stdio.h>

#include "flash_store.h"
#include "hardware/gpio.h"
#include "i2c_master.h"
#include "pico/stdlib.h"
#include "proto.h"

#ifndef BRIDGE_I2C_INDEX
#define BRIDGE_I2C_INDEX 0
#endif
#ifndef BRIDGE_SDA_PIN
#define BRIDGE_SDA_PIN 4
#endif
#ifndef BRIDGE_SCL_PIN
#define BRIDGE_SCL_PIN 5
#endif
#ifndef BRIDGE_DEFAULT_KHZ
#define BRIDGE_DEFAULT_KHZ 400
#endif
#ifndef AUTORUN_DELAY_MS
#define AUTORUN_DELAY_MS 500
#endif

static i2cm_t g_i2c;
static proto_t g_proto;
static bool g_quiet;  // suppress output (autorun with no USB host)

// ---------------------------------------------------------------- LED

enum { LED_IDLE, LED_PULSE, LED_FAIL_BLINK };
static int g_led_mode = LED_IDLE;
static absolute_time_t g_led_until;

static void led_set(bool on) {
#ifdef PICO_DEFAULT_LED_PIN
    gpio_put(PICO_DEFAULT_LED_PIN, on);
#else
    (void)on;
#endif
}

static void led_init(void) {
#ifdef PICO_DEFAULT_LED_PIN
    gpio_init(PICO_DEFAULT_LED_PIN);
    gpio_set_dir(PICO_DEFAULT_LED_PIN, GPIO_OUT);
#endif
    led_set(false);
}

static void led_pulse(uint32_t ms) {
    led_set(true);
    g_led_mode = LED_PULSE;
    g_led_until = make_timeout_time_ms(ms);
}

static void led_poll(void) {
    if (g_led_mode == LED_PULSE && time_reached(g_led_until)) {
        led_set(false);
        g_led_mode = LED_IDLE;
    } else if (g_led_mode == LED_FAIL_BLINK) {
        led_set((to_ms_since_boot(get_absolute_time()) / 100) & 1);
    }
}

// ---------------------------------------------------------------- platform hooks

static int pf_xfer(void *ctx, uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r, size_t rn,
                   uint32_t *detail) {
    (void)ctx;
    return i2cm_xfer(&g_i2c, addr, w, wn, r, rn, detail);
}

static uint32_t pf_set_baud(void *ctx, uint32_t hz) {
    (void)ctx;
    return i2cm_set_baud(&g_i2c, hz);
}

static uint32_t pf_get_baud(void *ctx) {
    (void)ctx;
    return g_i2c.baud;
}

static void pf_delay_ms(void *ctx, uint32_t ms) {
    (void)ctx;
    sleep_ms(ms);
}

static uint32_t pf_now_ms(void *ctx) {
    (void)ctx;
    return to_ms_since_boot(get_absolute_time());
}

static void pf_out(void *ctx, const char *line) {
    (void)ctx;
    if (g_quiet) return;
    puts(line);  // stdio CRLF translation turns '\n' into "\r\n"
    fflush(stdout);
}

static void pf_activity(void *ctx) {
    (void)ctx;
    led_pulse(30);  // also ends an autorun result pattern
}

static const proto_platform_t g_pf = {
    NULL, pf_xfer, pf_set_baud, pf_get_baud, pf_delay_ms, pf_now_ms, pf_out, pf_activity,
};

// ---------------------------------------------------------------- autorun

static void autorun(void) {
    const uint8_t *d;
    uint32_t n;
    bool enabled;
    if (!flash_store_get(&d, &n, &enabled) || !enabled || n == 0) return;
    proto_load_flash(&g_proto);  // also leave it in the RAM list for LI / LX
    g_quiet = !stdio_usb_connected();
    led_set(true);
    cmdlist_stats_t st;
    int rc = proto_run_list(&g_proto, g_proto.list, g_proto.list_len, 0, "AUTO ", &st);
    g_quiet = false;
    if (rc == 0 && st.failed == 0) {
        led_pulse(2000);
    } else {
        g_led_mode = LED_FAIL_BLINK;
    }
}

int main(void) {
    stdio_init_all();
    led_init();
    i2cm_init(&g_i2c, I2C_INSTANCE(BRIDGE_I2C_INDEX), BRIDGE_SDA_PIN, BRIDGE_SCL_PIN,
              BRIDGE_DEFAULT_KHZ * 1000u);
    proto_init(&g_proto, &g_pf);

    const absolute_time_t autorun_at = make_timeout_time_ms(AUTORUN_DELAY_MS);
    bool autorun_done = false;

    for (;;) {
        led_poll();
        if (!autorun_done && time_reached(autorun_at)) {
            autorun_done = true;
            autorun();
        }
        int c = getchar_timeout_us(1000);
        if (c == PICO_ERROR_TIMEOUT) continue;
        proto_feed(&g_proto, c);
    }
}

// a2b-master line protocol v2 (portable C).  See docs/USB_PROTOCOL.md.
#include "proto.h"

#include <stdio.h>
#include <string.h>

#include "flash_store.h"
#include "i2c_status.h"

#define VERSION_STR "a2b-master v2"

static const char HEX[] = "0123456789ABCDEF";

static void out(proto_t *p, const char *s) { p->pf->out(p->pf->ctx, s); }

static int hexval(int c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static bool is_space(int c) { return c == ' ' || c == '\t' || c == '\r' || c == '\n'; }

static int upper(int c) { return (c >= 'a' && c <= 'z') ? c - 'a' + 'A' : c; }

static bool str_ieq(const char *a, const char *b) {
    for (; *a && *b; a++, b++)
        if (upper((unsigned char)*a) != upper((unsigned char)*b)) return false;
    return *a == *b;
}

// Next whitespace-separated token; *pp advances past it and one delimiter (like strtok_r).
static char *next_tok(char **pp) {
    char *s = *pp;
    while (*s == ' ' || *s == '\t') s++;
    if (!*s) {
        *pp = s;
        return NULL;
    }
    char *t = s;
    while (*s && *s != ' ' && *s != '\t') s++;
    if (*s) *s++ = 0;
    *pp = s;
    return t;
}

// Parse hex digits (whitespace between bytes tolerated) into buf.  Returns the byte count,
// -1 on bad hex / odd digit count, -2 if longer than max.
static int parse_hex(const char *s, uint8_t *buf, size_t max) {
    size_t n = 0;
    int hi = -1;
    for (; *s; s++) {
        if (is_space((unsigned char)*s)) continue;
        int v = hexval((unsigned char)*s);
        if (v < 0) return -1;
        if (hi < 0) {
            hi = v;
        } else {
            if (n >= max) return -2;
            buf[n++] = (uint8_t)((hi << 4) | v);
            hi = -1;
        }
    }
    return hi < 0 ? (int)n : -1;
}

static bool parse_uint(const char *tok, int base, unsigned long max, unsigned long *val) {
    if (!tok || !*tok) return false;
    unsigned long v = 0;
    for (const char *s = tok; *s; s++) {
        int d = hexval((unsigned char)*s);
        if (d < 0 || d >= base) return false;
        v = v * (unsigned long)base + (unsigned long)d;
        if (v > max) return false;
    }
    *val = v;
    return true;
}

static bool parse_addr(const char *tok, uint8_t *addr) {
    unsigned long a;
    if (!tok || strlen(tok) > 2 || !parse_uint(tok, 16, 0x7f, &a)) return false;
    *addr = (uint8_t)a;
    return true;
}

static char *put_hex(char *p, const uint8_t *d, size_t n) {
    for (size_t i = 0; i < n; i++) {
        *p++ = HEX[d[i] >> 4];
        *p++ = HEX[d[i] & 15];
    }
    *p = 0;
    return p;
}

// Status text without the "OK" case: "NAK", "ERR ARB", ...
static void status_text(char *buf, size_t sz, int st, uint32_t detail) {
    switch (st) {
    case I2C_ST_NAK: snprintf(buf, sz, "NAK"); break;
    case I2C_ST_ARB: snprintf(buf, sz, "ERR ARB"); break;
    case I2C_ST_TIMEOUT: snprintf(buf, sz, "ERR TIMEOUT"); break;
    default: snprintf(buf, sz, "ERR ABRT 0x%08lx", (unsigned long)detail); break;
    }
}

static void reply_status(proto_t *p, int st, uint32_t detail) {
    char buf[32];
    status_text(buf, sizeof buf, st, detail);
    out(p, buf);
}

// ---------------------------------------------------------------- command list glue

static uint8_t g_wbuf[PROTO_MAX_DATA], g_rbuf[PROTO_MAX_DATA];
static char g_out[PROTO_LINE_MAX + 32];

static int op_write(void *ctx, uint8_t addr, const uint8_t *buf, size_t n, uint32_t *detail) {
    proto_t *p = ctx;
    return p->pf->xfer(p->pf->ctx, addr, buf, n, NULL, 0, detail);
}

static int op_write_read(void *ctx, uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r,
                         size_t rn, uint32_t *detail) {
    proto_t *p = ctx;
    return p->pf->xfer(p->pf->ctx, addr, w, wn, r, rn, detail);
}

static void op_delay(void *ctx, uint32_t ms) {
    proto_t *p = ctx;
    p->pf->delay_ms(p->pf->ctx, ms);
}

static uint32_t op_now(void *ctx) {
    proto_t *p = ctx;
    return p->pf->now_ms(p->pf->ctx);
}

static void op_report(void *ctx, const cmdlist_event_t *ev) {
    proto_t *p = ctx;
    const char *pre = p->prefix ? p->prefix : "";
    int n = snprintf(g_out, sizeof g_out, "%sS %lu ", pre, (unsigned long)ev->idx);
    char *e = g_out + n;
    if (ev->status == I2C_ST_OK) {
        if (ev->rec->op == CMDLIST_OP_DELAY) {
            snprintf(e, sizeof g_out - (size_t)n, "D %u", (unsigned)ev->rec->ms);
        } else {
            *e++ = 'O';
            *e++ = 'K';
            *e = 0;
            if (ev->rdata) {
                *e++ = ' ';
                put_hex(e, ev->rdata, ev->rec->rn);
            }
        }
    } else {
        status_text(e, sizeof g_out - (size_t)n, ev->status, ev->detail);
    }
    out(p, g_out);
}

int proto_run_list(proto_t *p, const uint8_t *list, size_t len, unsigned flags,
                   const char *prefix, cmdlist_stats_t *stats) {
    const cmdlist_ops_t ops = {op_write, op_write_read, op_delay, op_now, op_report, p};
    cmdlist_stats_t st;
    size_t err = 0;
    p->prefix = prefix;
    int rc = cmdlist_run(list, len, &ops, flags, &st, &err);
    const char *pre = prefix ? prefix : "";
    if (rc < 0)
        snprintf(g_out, sizeof g_out, "%sERR LIST %lu", pre, (unsigned long)err);
    else
        snprintf(g_out, sizeof g_out, "%sDONE %lu %lu %lu %lu", pre, (unsigned long)st.records,
                 (unsigned long)st.ok, (unsigned long)st.failed, (unsigned long)st.elapsed_ms);
    out(p, g_out);
    p->prefix = NULL;
    if (stats) *stats = st;
    return rc;
}

int proto_load_flash(proto_t *p) {
    const uint8_t *d;
    uint32_t n;
    bool autorun;
    if (!flash_store_get(&d, &n, &autorun) || n > CMDLIST_MAX_BYTES) return -1;
    memcpy(p->list, d, n);
    p->list_len = n;
    return (int)n;
}

// ---------------------------------------------------------------- commands

static void cmd_w(proto_t *p, char *rest) {
    uint8_t addr;
    if (!parse_addr(next_tok(&rest), &addr)) { out(p, "ERR ADDR"); return; }
    int n = parse_hex(rest, g_wbuf, PROTO_MAX_DATA);
    if (n == -2) { out(p, "ERR LEN"); return; }
    if (n <= 0) { out(p, "ERR DATA"); return; }
    uint32_t detail = 0;
    int st = p->pf->xfer(p->pf->ctx, addr, g_wbuf, (size_t)n, NULL, 0, &detail);
    if (st == I2C_ST_OK) out(p, "OK");
    else reply_status(p, st, detail);
}

static void cmd_r(proto_t *p, char *rest) {
    uint8_t addr;
    unsigned long nread;
    if (!parse_addr(next_tok(&rest), &addr)) { out(p, "ERR ADDR"); return; }
    if (!parse_uint(next_tok(&rest), 10, PROTO_MAX_DATA, &nread) || nread < 1) {
        out(p, "ERR LEN");
        return;
    }
    while (*rest == ' ' || *rest == '\t') rest++;
    size_t rl = strlen(rest);
    while (rl && is_space((unsigned char)rest[rl - 1])) rest[--rl] = 0;
    int nw = 0;
    if (strcmp(rest, "-") != 0) {
        nw = parse_hex(rest, g_wbuf, PROTO_MAX_DATA);
        if (nw == -2) { out(p, "ERR LEN"); return; }
        if (nw <= 0) { out(p, "ERR DATA"); return; }  // use '-' for a plain read
    }
    uint32_t detail = 0;
    int st = p->pf->xfer(p->pf->ctx, addr, g_wbuf, (size_t)nw, g_rbuf, nread, &detail);
    if (st == I2C_ST_OK) {
        memcpy(g_out, "OK ", 3);
        put_hex(g_out + 3, g_rbuf, nread);
        out(p, g_out);
    } else {
        reply_status(p, st, detail);
    }
}

static void cmd_b(proto_t *p, char *rest) {
    unsigned long khz;
    if (!parse_uint(next_tok(&rest), 10, 1000, &khz) || khz < 10) { out(p, "ERR ARG"); return; }
    uint32_t hz = p->pf->set_baud(p->pf->ctx, (uint32_t)(khz * 1000));
    snprintf(g_out, sizeof g_out, "OK %lu", (unsigned long)hz);
    out(p, g_out);
}

static void cmd_la(proto_t *p, char *rest) {
    int n = parse_hex(rest, g_wbuf, PROTO_MAX_DATA);
    if (n == -2) { out(p, "ERR LEN"); return; }
    if (n <= 0) { out(p, "ERR DATA"); return; }
    if (p->list_len + (size_t)n > CMDLIST_MAX_BYTES) { out(p, "ERR FULL"); return; }
    memcpy(p->list + p->list_len, g_wbuf, (size_t)n);
    p->list_len += (size_t)n;
    snprintf(g_out, sizeof g_out, "OK %lu", (unsigned long)p->list_len);
    out(p, g_out);
}

static void cmd_li(proto_t *p) {
    uint32_t records = 0;
    cmdlist_validate(p->list, p->list_len, &records, NULL);
    const uint8_t *fd = NULL;
    uint32_t fn = 0;
    bool autorun = false;
    if (!flash_store_get(&fd, &fn, &autorun)) fn = 0;
    snprintf(g_out, sizeof g_out, "OK %lu %lu %04X %d %lu %04X", (unsigned long)p->list_len,
             (unsigned long)records, (unsigned)crc16_ccitt(0xFFFF, p->list, p->list_len),
             autorun ? 1 : 0, (unsigned long)fn, (unsigned)crc16_ccitt(0xFFFF, fd, fn));
    out(p, g_out);
}

static void cmd_lx(proto_t *p, char *rest) {
    char *arg = next_tok(&rest);
    unsigned flags = 0;
    if (arg) {
        if (!str_ieq(arg, "STOP") || next_tok(&rest)) { out(p, "ERR ARG"); return; }
        flags |= CMDLIST_STOP_ON_ERROR;
    }
    proto_run_list(p, p->list, p->list_len, flags, NULL, NULL);
}

static void cmd_ls(proto_t *p) {
    size_t err;
    if (!cmdlist_validate(p->list, p->list_len, NULL, &err)) {
        snprintf(g_out, sizeof g_out, "ERR LIST %lu", (unsigned long)err);
        out(p, g_out);
        return;
    }
    const uint8_t *fd;
    uint32_t fn;
    bool autorun = false;
    flash_store_get(&fd, &fn, &autorun);
    if (flash_store_put(p->list, (uint32_t)p->list_len, autorun) < 0) {
        out(p, "ERR FLASH");
        return;
    }
    snprintf(g_out, sizeof g_out, "OK %lu", (unsigned long)p->list_len);
    out(p, g_out);
}

static void cmd_ll(proto_t *p) {
    int n = proto_load_flash(p);
    if (n < 0) { out(p, "ERR NOLIST"); return; }
    snprintf(g_out, sizeof g_out, "OK %d", n);
    out(p, g_out);
}

static void cmd_lauto(proto_t *p, char *rest) {
    char *arg = next_tok(&rest);
    if (!arg || (strcmp(arg, "0") && strcmp(arg, "1")) || next_tok(&rest)) {
        out(p, "ERR ARG");
        return;
    }
    const bool want = arg[0] == '1';
    const uint8_t *fd = NULL;
    uint32_t fn = 0;
    bool autorun = false;
    if (!flash_store_get(&fd, &fn, &autorun)) fn = 0;  // nothing stored: store an empty list
    if (autorun != want) {
        if (flash_store_put(fd, fn, want) < 0) { out(p, "ERR FLASH"); return; }
    }
    out(p, "OK");
}

void proto_handle_line(proto_t *p, char *line) {
    char *rest = line;
    char *cmd = next_tok(&rest);
    if (!cmd) return;
    if (p->pf->activity) p->pf->activity(p->pf->ctx);

    if (str_ieq(cmd, "P")) {
        snprintf(g_out, sizeof g_out, "PONG " VERSION_STR " %lu",
                 (unsigned long)p->pf->get_baud(p->pf->ctx));
        out(p, g_out);
    } else if (str_ieq(cmd, "W")) {
        cmd_w(p, rest);
    } else if (str_ieq(cmd, "R")) {
        cmd_r(p, rest);
    } else if (str_ieq(cmd, "B")) {
        cmd_b(p, rest);
    } else if (str_ieq(cmd, "LC")) {
        p->list_len = 0;
        out(p, "OK");
    } else if (str_ieq(cmd, "LA")) {
        cmd_la(p, rest);
    } else if (str_ieq(cmd, "LI")) {
        cmd_li(p);
    } else if (str_ieq(cmd, "LX")) {
        cmd_lx(p, rest);
    } else if (str_ieq(cmd, "LS")) {
        cmd_ls(p);
    } else if (str_ieq(cmd, "LL")) {
        cmd_ll(p);
    } else if (str_ieq(cmd, "LAUTO")) {
        cmd_lauto(p, rest);
    } else {
        out(p, "ERR CMD");
    }
}

void proto_init(proto_t *p, const proto_platform_t *pf) {
    p->pf = pf;
    p->line_len = 0;
    p->overflow = false;
    p->list_len = 0;
    p->prefix = NULL;
}

void proto_feed(proto_t *p, int c) {
    if (c == '\r' || c == '\n') {
        if (p->overflow) {
            out(p, "ERR LEN");
        } else if (p->line_len > 0) {
            p->line[p->line_len] = 0;
            proto_handle_line(p, p->line);
        }
        p->line_len = 0;
        p->overflow = false;
        return;
    }
    if (p->line_len < PROTO_LINE_MAX - 1) p->line[p->line_len++] = (char)c;
    else p->overflow = true;
}

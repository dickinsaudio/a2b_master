// fake_pico: native Linux build of the a2b-master line protocol (proto.c + cmdlist.c, the same
// sources as the firmware) served over TCP against a simulated I2C bus.
//
//   fake_pico [--port 9600] [--bind 127.0.0.1] [--log] [--no-delay] [--flash FILE]
//             [--tunnel sim:HOST:PORT] [--tunnel-addr 0x2A]
//
// Simulated bus:
//   0x38, 0x06   ACK every write, reads return zeros (EVK DSP and codec)
//   0x6A         AD2437 main node: 256-byte register file, register auto-increment.
//                VENDOR(0x02)=AD PRODUCT(0x03)=37 VERSION(0x04)=21 (read-only),
//                NODEADR(0x01): bit7 BRCST, bit5 PERI, bits3:0 node;
//                INTTYPE(0x17) reads 0x18 (discovery done) once DISCVRY(0x13) was written.
//   0x6B         BUS proxy.  PERI=0: register file of sub-node NODEADR[3:0] (nodes 0, 1;
//                BRCST writes go to all sub-nodes).  PERI=1: the peripheral at that node's
//                CHIP(0x00) address: forwarded to the tunnel simulator if CHIP == tunnel addr
//                and --tunnel is given, otherwise NAK.
//   others       NAK
//
// Tunnel framing (per transaction, TCP):
//   request  [kind 'W'|'R'][node][wlen u16 LE][rlen u16 LE][wdata]
//   response [status 0 ACK / 1 NACK][rdata rlen bytes if ACK]
#include <arpa/inet.h>
#include <errno.h>
#include <netdb.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

#include "flash_store.h"
#include "i2c_status.h"
#include "proto.h"

#define MAIN_ADDR 0x6A
#define BUS_ADDR 0x6B
#define NUM_SUB 2

static int g_log;
static int g_no_delay;
static const char *g_flash_file;
static char g_tun_host[256];
static char g_tun_port[16];
static int g_tun_enabled;
static uint8_t g_tun_addr = 0x2A;
static int g_tun_fd = -1;
static int g_client = -1;
static uint32_t g_baud = 400000;

// ---------------------------------------------------------------- flash store (RAM + file)

static uint8_t g_flash[CMDLIST_MAX_BYTES];
static uint32_t g_flash_len;
static bool g_flash_valid, g_flash_autorun;

bool flash_store_get(const uint8_t **data, uint32_t *len, bool *autorun) {
    *data = g_flash_valid ? g_flash : NULL;
    *len = g_flash_valid ? g_flash_len : 0;
    *autorun = g_flash_valid && g_flash_autorun;
    return g_flash_valid;
}

// File format: "A2BL" u8 autorun u32 len (LE) u16 crc16 (LE) data[len]
int flash_store_put(const uint8_t *data, uint32_t len, bool autorun) {
    if (len > CMDLIST_MAX_BYTES) return -1;
    if (len) memmove(g_flash, data, len);
    g_flash_len = len;
    g_flash_autorun = autorun;
    g_flash_valid = true;
    if (g_flash_file) {
        FILE *f = fopen(g_flash_file, "wb");
        if (!f) return -2;
        uint16_t crc = crc16_ccitt(0xFFFF, g_flash, len);
        uint8_t h[11] = {'A', '2', 'B', 'L', autorun ? 1 : 0, (uint8_t)len, (uint8_t)(len >> 8),
                         (uint8_t)(len >> 16), (uint8_t)(len >> 24), (uint8_t)crc,
                         (uint8_t)(crc >> 8)};
        int ok = fwrite(h, 1, sizeof h, f) == sizeof h && fwrite(g_flash, 1, len, f) == len;
        if (fclose(f) != 0 || !ok) return -2;
    }
    return 0;
}

static void flash_load_file(void) {
    FILE *f = fopen(g_flash_file, "rb");
    if (!f) return;
    uint8_t h[11];
    if (fread(h, 1, sizeof h, f) == sizeof h && !memcmp(h, "A2BL", 4)) {
        uint32_t len = h[5] | h[6] << 8 | h[7] << 16 | (uint32_t)h[8] << 24;
        uint16_t crc = (uint16_t)(h[9] | h[10] << 8);
        if (len <= CMDLIST_MAX_BYTES && fread(g_flash, 1, len, f) == len &&
            crc16_ccitt(0xFFFF, g_flash, len) == crc) {
            g_flash_len = len;
            g_flash_autorun = h[4] != 0;
            g_flash_valid = true;
        }
    }
    fclose(f);
}

// ---------------------------------------------------------------- simulated devices

typedef struct {
    uint8_t regs[256];
    uint8_t ptr;
    int is_main;
} regfile_t;

static regfile_t g_main, g_sub[NUM_SUB];
static bool g_discovery_started;

static void regfile_init(regfile_t *rf, int is_main) {
    memset(rf, 0, sizeof *rf);
    rf->is_main = is_main;
    rf->regs[0x02] = 0xAD;  // VENDOR
    rf->regs[0x03] = 0x37;  // PRODUCT
    rf->regs[0x04] = 0x21;  // VERSION
}

static void reg_write(regfile_t *rf, uint8_t reg, uint8_t v) {
    if (reg >= 0x02 && reg <= 0x04) return;  // read-only ID registers
    if (rf->is_main && reg == 0x13) g_discovery_started = true;  // DISCVRY
    rf->regs[reg] = v;
}

static uint8_t reg_read(regfile_t *rf, uint8_t reg) {
    if (rf->is_main && reg == 0x17) return g_discovery_started ? 0x18 : 0x00;  // INTTYPE
    return rf->regs[reg];
}

static int regfile_xfer(regfile_t *rf, const uint8_t *w, size_t wn, uint8_t *r, size_t rn) {
    if (wn) {
        rf->ptr = w[0];
        for (size_t i = 1; i < wn; i++) reg_write(rf, rf->ptr++, w[i]);
    }
    for (size_t i = 0; i < rn; i++) r[i] = reg_read(rf, rf->ptr++);
    return I2C_ST_OK;
}

// ---------------------------------------------------------------- tunnel forwarding

static int tun_connect(void) {
    struct addrinfo hints = {0}, *res = NULL;
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    if (getaddrinfo(g_tun_host, g_tun_port, &hints, &res) != 0) return -1;
    int fd = -1;
    for (struct addrinfo *a = res; a; a = a->ai_next) {
        fd = socket(a->ai_family, a->ai_socktype, a->ai_protocol);
        if (fd < 0) continue;
        struct timeval tv = {2, 0};  // SO_SNDTIMEO also bounds connect() on Linux
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
        if (connect(fd, a->ai_addr, a->ai_addrlen) == 0) break;
        close(fd);
        fd = -1;
    }
    freeaddrinfo(res);
    if (fd >= 0) {
        int one = 1;
        setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
    }
    return fd;
}

static int send_all(int fd, const void *buf, size_t n) {
    const uint8_t *p = buf;
    while (n) {
        ssize_t k = send(fd, p, n, MSG_NOSIGNAL);
        if (k < 0 && errno == EINTR) continue;
        if (k <= 0) return -1;
        p += k;
        n -= (size_t)k;
    }
    return 0;
}

static int recv_all(int fd, void *buf, size_t n) {
    uint8_t *p = buf;
    while (n) {
        ssize_t k = recv(fd, p, n, 0);
        if (k < 0 && errno == EINTR) continue;
        if (k <= 0) return -1;
        p += k;
        n -= (size_t)k;
    }
    return 0;
}

static int tunnel_xfer(int node, const uint8_t *w, size_t wn, uint8_t *r, size_t rn) {
    if (g_tun_fd < 0 && (g_tun_fd = tun_connect()) < 0) {
        fprintf(stderr, "fake_pico: tunnel connect %s:%s failed\n", g_tun_host, g_tun_port);
        return I2C_ST_NAK;
    }
    uint8_t hdr[6] = {rn ? 'R' : 'W', (uint8_t)node, (uint8_t)wn, (uint8_t)(wn >> 8),
                      (uint8_t)rn, (uint8_t)(rn >> 8)};
    uint8_t status;
    if (send_all(g_tun_fd, hdr, sizeof hdr) < 0 || (wn && send_all(g_tun_fd, w, wn) < 0) ||
        recv_all(g_tun_fd, &status, 1) < 0 || (status == 0 && rn && recv_all(g_tun_fd, r, rn) < 0)) {
        fprintf(stderr, "fake_pico: tunnel I/O error\n");
        close(g_tun_fd);
        g_tun_fd = -1;
        return I2C_ST_NAK;
    }
    return status == 0 ? I2C_ST_OK : I2C_ST_NAK;
}

// ---------------------------------------------------------------- bus

static int bus_xfer(uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r, size_t rn) {
    switch (addr) {
    case 0x38:
    case 0x06:
        memset(r, 0, rn);
        return I2C_ST_OK;
    case MAIN_ADDR:
        return regfile_xfer(&g_main, w, wn, r, rn);
    case BUS_ADDR: {
        const uint8_t na = g_main.regs[0x01];
        const int node = na & 0x0F;
        if (!(na & 0x20)) {
            if ((na & 0x80) && rn == 0) {  // broadcast write to every sub-node
                for (int i = 0; i < NUM_SUB; i++) regfile_xfer(&g_sub[i], w, wn, NULL, 0);
                return I2C_ST_OK;
            }
            if (node >= NUM_SUB) return I2C_ST_NAK;
            return regfile_xfer(&g_sub[node], w, wn, r, rn);
        }
        if (node >= NUM_SUB) return I2C_ST_NAK;
        if (g_tun_enabled && g_sub[node].regs[0x00] == g_tun_addr)
            return tunnel_xfer(node, w, wn, r, rn);
        return I2C_ST_NAK;
    }
    default:
        return I2C_ST_NAK;
    }
}

static void log_hex(const char *tag, const uint8_t *d, size_t n) {
    fprintf(stderr, " %s", tag);
    for (size_t i = 0; i < n; i++) fprintf(stderr, "%02X", d[i]);
}

// ---------------------------------------------------------------- platform hooks

static int pf_xfer(void *ctx, uint8_t addr, const uint8_t *w, size_t wn, uint8_t *r, size_t rn,
                   uint32_t *detail) {
    (void)ctx;
    *detail = 0;
    int st = bus_xfer(addr, w, wn, r, rn);
    if (g_log) {
        fprintf(stderr, "[i2c] %02X", addr);
        if (wn) log_hex("W:", w, wn);
        if (rn) {
            if (st == I2C_ST_OK) log_hex("R:", r, rn);
            else fprintf(stderr, " R:%zu", rn);
        }
        fprintf(stderr, " -> %s\n", st == I2C_ST_OK ? "ACK" : "NAK");
    }
    return st;
}

static uint32_t pf_set_baud(void *ctx, uint32_t hz) {
    (void)ctx;
    g_baud = hz;
    return hz;
}

static uint32_t pf_get_baud(void *ctx) {
    (void)ctx;
    return g_baud;
}

static void pf_delay_ms(void *ctx, uint32_t ms) {
    (void)ctx;
    if (g_no_delay) return;
    struct timespec ts = {(time_t)(ms / 1000), (long)(ms % 1000) * 1000000L};
    while (nanosleep(&ts, &ts) < 0 && errno == EINTR) {
    }
}

static uint32_t pf_now_ms(void *ctx) {
    (void)ctx;
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint32_t)(ts.tv_sec * 1000u + ts.tv_nsec / 1000000);
}

static void pf_out(void *ctx, const char *line) {
    (void)ctx;
    if (g_client < 0) {
        printf("%s\n", line);
        fflush(stdout);
        return;
    }
    size_t n = strlen(line);
    if (send_all(g_client, line, n) < 0 || send_all(g_client, "\r\n", 2) < 0) {
        // client gone: the main loop notices on the next recv
    }
}

static const proto_platform_t g_pf = {
    NULL, pf_xfer, pf_set_baud, pf_get_baud, pf_delay_ms, pf_now_ms, pf_out, NULL,
};

static proto_t g_proto;

// ---------------------------------------------------------------- main

static void usage(void) {
    fprintf(stderr,
            "usage: fake_pico [--port 9600] [--bind 127.0.0.1] [--log] [--no-delay]\n"
            "                 [--flash FILE] [--tunnel sim:HOST:PORT] [--tunnel-addr 0x2A]\n");
    exit(2);
}

int main(int argc, char **argv) {
    int port = 9600;
    const char *bind_addr = "127.0.0.1";
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        const char *v = i + 1 < argc ? argv[i + 1] : NULL;
        if (!strcmp(a, "--log")) {
            g_log = 1;
        } else if (!strcmp(a, "--no-delay")) {
            g_no_delay = 1;
        } else if (!strcmp(a, "--port") && v) {
            port = atoi(v);
            i++;
        } else if (!strcmp(a, "--bind") && v) {
            bind_addr = v;
            i++;
        } else if (!strcmp(a, "--flash") && v) {
            g_flash_file = v;
            i++;
        } else if (!strcmp(a, "--tunnel-addr") && v) {
            g_tun_addr = (uint8_t)strtoul(v, NULL, 0);
            i++;
        } else if (!strcmp(a, "--tunnel") && v) {
            const char *colon;
            if (strncmp(v, "sim:", 4) || !(colon = strrchr(v + 4, ':')) ||
                (size_t)(colon - (v + 4)) >= sizeof g_tun_host || strlen(colon + 1) >= sizeof g_tun_port)
                usage();
            memcpy(g_tun_host, v + 4, (size_t)(colon - (v + 4)));
            g_tun_host[colon - (v + 4)] = 0;
            strcpy(g_tun_port, colon + 1);
            g_tun_enabled = 1;
            i++;
        } else {
            usage();
        }
    }
    signal(SIGPIPE, SIG_IGN);

    regfile_init(&g_main, 1);
    for (int i = 0; i < NUM_SUB; i++) regfile_init(&g_sub[i], 0);
    if (g_flash_file) flash_load_file();
    proto_init(&g_proto, &g_pf);

    int lfd = socket(AF_INET, SOCK_STREAM, 0);
    int one = 1;
    setsockopt(lfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in sa = {0};
    sa.sin_family = AF_INET;
    sa.sin_port = htons((uint16_t)port);
    if (inet_pton(AF_INET, bind_addr, &sa.sin_addr) != 1) usage();
    if (bind(lfd, (struct sockaddr *)&sa, sizeof sa) < 0 || listen(lfd, 4) < 0) {
        perror("fake_pico: bind/listen");
        return 1;
    }
    fprintf(stderr, "fake_pico: listening on %s:%d%s%s\n", bind_addr, port,
            g_tun_enabled ? ", tunnel -> " : "", g_tun_enabled ? g_tun_host : "");

    // Autorun (as on the Pico, 500 ms after start; output goes to stdout).
    {
        const uint8_t *d;
        uint32_t n;
        bool autorun;
        if (flash_store_get(&d, &n, &autorun) && autorun && n) {
            pf_delay_ms(NULL, 500);
            proto_load_flash(&g_proto);
            proto_run_list(&g_proto, g_proto.list, g_proto.list_len, 0, "AUTO ", NULL);
        }
    }

    for (;;) {
        g_client = accept(lfd, NULL, NULL);
        if (g_client < 0) {
            if (errno == EINTR) continue;
            perror("fake_pico: accept");
            return 1;
        }
        setsockopt(g_client, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
        if (g_log) fprintf(stderr, "fake_pico: client connected\n");
        g_proto.line_len = 0;  // fresh line buffer; the RAM list survives, as on the Pico
        g_proto.overflow = false;
        char buf[4096];
        for (;;) {
            ssize_t k = recv(g_client, buf, sizeof buf, 0);
            if (k < 0 && errno == EINTR) continue;
            if (k <= 0) break;
            for (ssize_t i = 0; i < k; i++) proto_feed(&g_proto, (unsigned char)buf[i]);
        }
        close(g_client);
        g_client = -1;
        if (g_log) fprintf(stderr, "fake_pico: client disconnected\n");
    }
}

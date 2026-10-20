// Command-list parser / player (portable C).
#include "cmdlist.h"

#include "i2c_status.h"

int cmdlist_next(const uint8_t *list, size_t len, size_t *off, cmdlist_rec_t *rec) {
    size_t o = *off;
    if (o >= len) return 0;
    const uint8_t op = list[o];
    const size_t left = len - o;
    rec->op = op;
    rec->addr = 0;
    rec->wdata = NULL;
    rec->wn = rec->rn = 0;
    rec->ms = 0;
    switch (op) {
    case CMDLIST_OP_END:
        return 0;
    case CMDLIST_OP_WRITE:
        if (left < 3) return -1;
        rec->addr = list[o + 1];
        rec->wn = list[o + 2];
        if (rec->addr > 0x7f || rec->wn == 0 || left < 3u + rec->wn) return -1;
        rec->wdata = &list[o + 3];
        *off = o + 3 + rec->wn;
        return 1;
    case CMDLIST_OP_WRITE_LONG:
        if (left < 4) return -1;
        rec->addr = list[o + 1];
        rec->wn = (uint16_t)(list[o + 2] | (list[o + 3] << 8));
        if (rec->addr > 0x7f || rec->wn == 0 || rec->wn > CMDLIST_MAX_LONG_WRITE ||
            left < 4u + rec->wn)
            return -1;
        rec->wdata = &list[o + 4];
        *off = o + 4 + rec->wn;
        return 1;
    case CMDLIST_OP_WRITE_READ:
        if (left < 4) return -1;
        rec->addr = list[o + 1];
        rec->wn = list[o + 2];
        if (rec->addr > 0x7f || left < 4u + rec->wn) return -1;
        rec->wdata = &list[o + 3];
        rec->rn = list[o + 3 + rec->wn];
        if (rec->rn == 0) return -1;
        *off = o + 4 + rec->wn;
        return 1;
    case CMDLIST_OP_DELAY:
        if (left < 3) return -1;
        rec->ms = (uint16_t)(list[o + 1] | (list[o + 2] << 8));
        *off = o + 3;
        return 1;
    default:
        return -1;
    }
}

bool cmdlist_validate(const uint8_t *list, size_t len, uint32_t *records, size_t *err_off) {
    size_t off = 0;
    uint32_t n = 0;
    cmdlist_rec_t rec;
    int r;
    while ((r = cmdlist_next(list, len, &off, &rec)) > 0) n++;
    if (records) *records = n;
    if (err_off) *err_off = r < 0 ? off : len;
    return r == 0;
}

int cmdlist_run(const uint8_t *list, size_t len, const cmdlist_ops_t *ops, unsigned flags,
                cmdlist_stats_t *stats, size_t *err_off) {
    cmdlist_stats_t st = {0};
    if (!cmdlist_validate(list, len, &st.records, err_off)) {
        if (stats) *stats = st;
        return -1;
    }
    const uint32_t t0 = ops->now_ms ? ops->now_ms(ops->ctx) : 0;
    static uint8_t rbuf[255];  // static: keep the stack small on the MCU (not reentrant)
    size_t off = 0;
    cmdlist_rec_t rec;
    for (uint32_t idx = 0; cmdlist_next(list, len, &off, &rec) > 0; idx++) {
        cmdlist_event_t ev = {idx, &rec, I2C_ST_OK, 0, NULL};
        switch (rec.op) {
        case CMDLIST_OP_WRITE:
        case CMDLIST_OP_WRITE_LONG:
            ev.status = ops->write(ops->ctx, rec.addr, rec.wdata, rec.wn, &ev.detail);
            break;
        case CMDLIST_OP_WRITE_READ:
            ev.status = ops->write_read(ops->ctx, rec.addr, rec.wdata, rec.wn, rbuf, rec.rn,
                                        &ev.detail);
            if (ev.status == I2C_ST_OK) ev.rdata = rbuf;
            break;
        case CMDLIST_OP_DELAY:
            if (rec.ms) ops->delay_ms(ops->ctx, rec.ms);
            break;
        default:
            break;
        }
        st.executed++;
        if (ev.status == I2C_ST_OK) st.ok++;
        else st.failed++;
        if (ops->report) ops->report(ops->ctx, &ev);
        if (ev.status != I2C_ST_OK && (flags & CMDLIST_STOP_ON_ERROR)) {
            st.stopped = true;
            break;
        }
    }
    if (ops->now_ms) st.elapsed_ms = ops->now_ms(ops->ctx) - t0;
    if (stats) *stats = st;
    return 0;
}

uint16_t crc16_ccitt(uint16_t crc, const uint8_t *data, size_t len) {
    while (len--) {
        crc ^= (uint16_t)(*data++ << 8);
        for (int i = 0; i < 8; i++)
            crc = (crc & 0x8000) ? (uint16_t)((crc << 1) ^ 0x1021) : (uint16_t)(crc << 1);
    }
    return crc;
}

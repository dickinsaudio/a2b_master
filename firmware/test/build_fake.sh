#!/bin/sh
# Build the native fake_pico (line protocol + command-list player over TCP, simulated I2C bus).
# Usage: build_fake.sh <out_dir>
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
FW=$HERE/..
OUT=${1:?usage: build_fake.sh <out_dir>}
mkdir -p "$OUT"
${CC:-gcc} -std=c11 -D_POSIX_C_SOURCE=200809L -O2 -g -Wall -Wextra -Wshadow -Wstrict-prototypes \
    -Werror -I"$FW" "$FW/proto.c" "$FW/cmdlist.c" "$HERE/fake_pico.c" -o "$OUT/fake_pico"
echo "built $OUT/fake_pico"

#!/bin/sh
# Build the native end to end harness (native_sim).  Usage: build_native.sh [out_dir]
#
# Runs the host project's real HttpServer and WIZnet ioLibrary against the code in device/.
# Assumes this repo is a submodule at <project>/extern/a2b_master. Override with:
#   PROJECT=<project root>   SERVER=<dir with http_server.cpp/.hpp>   IOLIB=<ioLibrary Ethernet dir>
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
DEV=$HERE/..
PROJECT=${PROJECT:-$HERE/../../../..}
SERVER=${SERVER:-$PROJECT/daes67_lite}
IOLIB=${IOLIB:-$PROJECT/WIZnet-PICO-C/libraries/ioLibrary_Driver/Ethernet}
OUT=${1:-/tmp/a2b_native_sim}
for f in $SERVER/http_server.cpp $IOLIB/wizchip_conf.c; do
    [ -f $f ] || { echo "missing $f - set PROJECT, SERVER or IOLIB"; exit 1; }
done
mkdir -p $OUT/src
# Copy the server so its "log.hpp" include resolves to the native stub, not the SDK one
cp $SERVER/http_server.cpp $SERVER/http_server.hpp $OUT/src/
DEFS="-D_WIZCHIP_=W5500 -DPICO_BUILD"
INC="-I$HERE/stubs -I$OUT/src -I$DEV -I$IOLIB -I$IOLIB/W5500"
gcc -c -O1 -g $DEFS $INC $IOLIB/wizchip_conf.c -o $OUT/wizchip_conf.o
gcc -c -O1 -g $DEFS $INC $IOLIB/W5500/w5500.c -o $OUT/w5500.o
gcc -c -O1 -g -Wall $DEFS $INC $DEV/vw5500.c -o $OUT/vw5500.o
gcc -c -O1 -g -Wall $DEFS $INC $DEV/tunnel_proto.c -o $OUT/tunnel_proto.o
g++ -c -O1 -g -std=gnu++17 -Wno-format-overflow $DEFS $INC -include $HERE/stubs/log.hpp $OUT/src/http_server.cpp -o $OUT/http_server.o
g++ -c -O1 -g -std=gnu++17 -Wall $DEFS $INC $HERE/native_sim.cpp -o $OUT/native_sim.o
g++ -o $OUT/native_sim $OUT/*.o
echo "built $OUT/native_sim"

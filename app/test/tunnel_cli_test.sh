#!/bin/bash
# Scripted end-to-end tunnel test of the a2b_master binary (headless) against the native
# device harness.  Usage:
#   test/tunnel_cli_test.sh <a2b_master binary> <native_sim binary> [sim_port] [base_port] [sim args...]
# Defaults: sim port 9500, tunnel base port 8080, sim args "--byte-us 25 --nack-rate 0.02".
# APPARGS overrides the transport flags, e.g. APPARGS="-port tcp:127.0.0.1:9600 -tunnel pico"
# (then start fake_pico --tunnel sim:127.0.0.1:<sim_port> yourself; see README).
set -u
APPBIN=$1; SIMBIN=$2; SIMPORT=${3:-9500}; BASE=${4:-8080}; shift 4 2>/dev/null || shift $#
SIMARGS=${*:-"--byte-us 25 --nack-rate 0.02"}
T=$(mktemp -d)
trap 'kill $APP $SIM 2>/dev/null; [ ${KEEP:-0} = 1 ] && echo "kept $T" || rm -rf $T' EXIT
python3 -c "import sys; open(sys.argv[1],'w').write(''.join('<p>%d The quick brown fox jumps over the lazy dog over A2B.</p>\n'%i for i in range(200)))" $T/exp.html
$SIMBIN --port $SIMPORT $SIMARGS > $T/sim.log 2>&1 & SIM=$!
sleep 0.3
if ! kill -0 $SIM 2>/dev/null; then echo "native_sim failed:"; cat $T/sim.log; exit 1; fi
$APPBIN -no-web ${APPARGS:--tunnel sim:127.0.0.1:$SIMPORT} -nodes 0 -base-port $BASE -settings $T/s.json -stats-s 2 > $T/app.log 2>&1 & APP=$!
for i in $(seq 50); do curl -s -o /dev/null http://127.0.0.1:$BASE/index.html && break; sleep 0.1; done
fail=0
curl -s -o $T/big.html -w "big.html: %{size_download} B in %{time_total} s (%{speed_download} B/s)\n" http://127.0.0.1:$BASE/big.html
cmp -s $T/big.html $T/exp.html && echo "big.html byte-exact" || { echo "big.html MISMATCH"; fail=1; }
curl -s -m 20 -o $T/index.html http://127.0.0.1:$BASE/index.html
grep -q "<h1>Hello over A2B</h1>" $T/index.html && echo "index.html OK" || { echo "index.html FAIL: $(head -c 300 $T/index.html)"; fail=1; }
for i in $(seq 0 19); do printf "line %02d of the chunked response\n" $i; done > $T/chunks.exp
curl -s -o $T/chunks.txt http://127.0.0.1:$BASE/chunks.txt
cmp -s $T/chunks.txt $T/chunks.exp && echo "chunks.txt byte-exact" || { echo "chunks.txt MISMATCH"; fail=1; }
t0=$(date +%s.%N); pids=""
for i in 1 2 3 4 5 6; do curl -s -o $T/p$i.html http://127.0.0.1:$BASE/big.html & pids="$pids $!"; done
wait $pids
t1=$(date +%s.%N)
ok=0; for i in 1 2 3 4 5 6; do cmp -s $T/p$i.html $T/exp.html && ok=$((ok+1)); done
python3 -c "import sys; d=float(sys.argv[2])-float(sys.argv[1]); print('6 parallel big.html: %d/6 byte-exact, %.2f s, %.1f kB/s aggregate'%(int(sys.argv[3]),d,6*12890/d/1000))" $t0 $t1 $ok
[ $ok = 6 ] || fail=1
sleep 2.2
grep stats $T/app.log | tail -1
[ $fail = 0 ] && echo PASS || echo FAIL
exit $fail

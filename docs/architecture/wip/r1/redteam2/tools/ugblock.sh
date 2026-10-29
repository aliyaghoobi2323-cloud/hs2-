#!/bin/bash
# ugblock.sh NAME: the R1 red team's (second pass, item 6) upgrade and
# rollback block: under the shared rig lock (fair queue, after GAP s since
# this pass's previous block), each scenario of upgrade.sh inside
# hs2-src/install/tests/sandbox.sh, one after another. Results in
# /root/rred2/NAME/<scenario>/ (no key, no address outside the namespaces).
set -u
NAME=${1:?}
T=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$T/../../../../../.." && pwd)
SB=$REPO/hs2-src/install/tests/sandbox.sh
O=/root/rred2/$NAME; mkdir -p "$O"
PROD=/root/r4j/bins/hs2-prod; FIN2=/root/r4j/bins/hs2-fin2
GAP=${GAP:-150}
exec 9>/root/rred2/.serial
flock 9
while last=$(cat /root/rred2/last-block-end 2>/dev/null || echo 0); [ $(( $(date +%s) - last )) -lt "$GAP" ]; do sleep 5; done
/root/r1-quiet.take.sh r1-red2 "R1 red team 2 upgrade timing $NAME" > /dev/null || exit 1
t0=$(date +%s)
trap 'rm -f /root/r1-quiet.lock; t1=$(date +%s); echo $t1 > /root/rred2/last-block-end; echo "$NAME $(date -u -d @$t0 +%FT%TZ) $(date -u -d @$t1 +%FT%TZ) $((t1 - t0))" >> /root/rred2/locktime.log' EXIT
one() { # one LABEL FROM TO ORDER GAP [PEERWAIT]
	local l=$1; shift
	echo "== $l $(date -u +%T) $*" >> "$O/ug.log"
	timeout 1200 "$SB" "$T/upgrade.sh" "$O/$l" "$@" >> "$O/ug.log" 2>&1
	echo "== $l rc=$? $(date -u +%T)" >> "$O/ug.log"
}
one up-x-first-30 $PROD $FIN2 x-first 30
one up-x-first-90 $PROD $FIN2 x-first 90
one up-e-first-30 $PROD $FIN2 e-first 30
one up-both $PROD $FIN2 both 0
one up-x-only $PROD $FIN2 x-only 0 90
one back-x-first-30 $FIN2 $PROD x-first 30
one back-both $FIN2 $PROD both 0

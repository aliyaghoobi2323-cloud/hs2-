#!/bin/bash
# fakesd.sh — the R1 review's (part C) stand-in for systemctl inside sandbox.sh,
# where no systemd is reachable: the red team's (redteam2/tools/fakesd.sh) with
# one addition, a unit that "crashed" once after its restart:
#   $UGSD/<unit>/flap present → after the unit's restart, `show NRestarts`
#   answers 1 (systemd counted one automatic restart): what hs2-safe-apply sees
#   when the new binary died once in its first minute.
# Units hs2-uge (the edge, Iran) and hs2-ugx (the exit, Kharej), each an hs2
# process in its own network namespace. State in $UGSD/<unit>/:
#   ns      the network namespace       bin   the binary path (safe-apply's BIN)
#   cfg     the config path             pid   the running process (none: stopped)
#   log     the log file (appended)     env   extra HS2_* (space separated)
#   restarts  restarts so far (this stand-in's own count)
# restart = stop (TERM, then KILL after 2 s) and start at once, as systemd's.
set -u
UGSD=${UGSD:?}
u() { echo "$UGSD/$1"; }
alive() { local p; p=$(cat "$(u "$1")/pid" 2>/dev/null) && [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }
start() {
	local d; d=$(u "$1")
	echo "$(date +%T.%N | cut -c1-12) start $(basename "$(readlink -f "$(cat "$d/bin")")") $(sha256sum "$(cat "$d/bin")" | cut -c1-12)" >> "$d/events"
	setsid ip netns exec "$(cat "$d/ns")" env HS2_NO_TUNE=1 $(cat "$d/env" 2>/dev/null) "$(cat "$d/bin")" run -c "$(cat "$d/cfg")" >> "$d/log" 2>&1 < /dev/null &
	echo $! > "$d/pid"
}
stop() {
	local d p i; d=$(u "$1")
	p=$(cat "$d/pid" 2>/dev/null)
	[ -n "$p" ] || return 0
	kill "$p" 2>/dev/null
	for i in $(seq 1 20); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
	kill -9 "$p" 2>/dev/null
	echo "$(date +%T.%N | cut -c1-12) stop" >> "$d/events"
	: > "$d/pid"
}
cmd=${1:-}; shift || true
case "$cmd" in
show)
	# show -p PROP --value UNIT
	prop=""; unit=""
	while [ $# -gt 0 ]; do
		case "$1" in -p) prop=$2; shift 2 ;; --value) shift ;; *) unit=$1; shift ;; esac
	done
	d=$(u "$unit")
	case "$prop" in
	ExecStart) echo "{ path=$(cat "$d/bin") ; argv[]=$(cat "$d/bin") run -c $(cat "$d/cfg") ; ignore_errors=no }" ;;
	NRestarts)
		if [ -e "$d/flap" ] && [ "$(cat "$d/restarts" 2>/dev/null || echo 0)" -ge 1 ]; then echo 1; else echo 0; fi ;;
	Environment) cat "$d/env" 2>/dev/null; echo ;;
	MainPID) cat "$d/pid" 2>/dev/null || echo 0 ;;
	*) echo ;;
	esac ;;
is-active)
	if alive "$1"; then echo active; else echo inactive; exit 3; fi ;;
restart)
	d=$(u "$1")
	echo $(($(cat "$d/restarts" 2>/dev/null || echo 0) + 1)) > "$d/restarts"
	stop "$1"; start "$1"
	# a unit that "crashed" once: counted once only (its flap is spent)
	[ "$(cat "$d/restarts")" -ge 2 ] && rm -f "$d/flap"
	;;
start) alive "$1" || start "$1" ;;
stop) stop "$1" ;;
daemon-reload | reset-failed) ;;
*) echo "fakesd: $cmd not handled" >&2; exit 1 ;;
esac
exit 0

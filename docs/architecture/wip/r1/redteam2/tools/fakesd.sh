#!/bin/bash
# fakesd.sh — the R1 red team's (second pass) stand-in for systemctl inside
# sandbox.sh, where no systemd is reachable: just what hs2-safe-apply calls,
# for the units hs2-uge (the edge, Iran) and hs2-ugx (the exit, Kharej), each
# an hs2 process in its own network namespace. State in $UGSD/<unit>/:
#   ns      the network namespace       bin   the binary path (safe-apply's BIN)
#   cfg     the config path             pid   the running process (none: stopped)
#   log     the log file (appended)     env   extra HS2_* (space separated)
# restart = stop (TERM, then KILL after 2 s) and start at once, as systemd's.
# NRestarts stays 0 (systemd counts only its own automatic restarts).
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
	NRestarts) echo 0 ;;
	Environment) cat "$d/env" 2>/dev/null; echo ;;
	MainPID) cat "$d/pid" 2>/dev/null || echo 0 ;;
	*) echo ;;
	esac ;;
is-active)
	if alive "$1"; then echo active; else echo inactive; exit 3; fi ;;
restart) stop "$1"; start "$1" ;;
start) alive "$1" || start "$1" ;;
stop) stop "$1" ;;
daemon-reload | reset-failed) ;;
*) echo "fakesd: $cmd not handled" >&2; exit 1 ;;
esac
exit 0

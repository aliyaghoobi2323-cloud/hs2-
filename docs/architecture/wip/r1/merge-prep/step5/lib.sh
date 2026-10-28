# lib.sh — shared by the step-5 scripts (sourced). Each script runs one item
# of R1's method step 5 with one command and writes its result to a file.
#
# The shared rig lock (/root/r1-quiet.lock): a script does not start while
# it exists and kills its own process group the moment it appears (exit 3,
# "ABORTED" in the result file), unless STEP5_IGNORE_LOCK=1 (the one who
# runs it holds the lock himself, e.g. through r1-quiet.take.sh).
set -u
STEP5_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(git -C "$STEP5_DIR" rev-parse --show-toplevel)
SRC=$REPO/hs2-src
SHA=$(git -C "$REPO" rev-parse --short HEAD)
LOCK=${STEP5_LOCK:-/root/r1-quiet.lock}
OUTDIR=${STEP5_OUT:-$HOME/hs2-step5}
mkdir -p "$OUTDIR"
command -v go >/dev/null || export PATH=$PATH:/usr/local/go/bin
export GOFLAGS=${GOFLAGS:-}

# step5_out NAME: the result file of this run (printed at the end).
step5_out() { echo "$OUTDIR/$1-$SHA-$(date -u +%Y%m%dT%H%M%SZ).txt"; }

# step5_guard CMD...: runs CMD in a process group of its own; 3 if the rig
# lock was there or came (the group is killed), else CMD's status.
step5_guard() {
	if [ -z "${STEP5_IGNORE_LOCK:-}" ] && [ -e "$LOCK" ]; then
		echo "NOT RUN: the rig lock is held: $(cat "$LOCK" 2>/dev/null)"
		return 3
	fi
	setsid "$@" &
	local pid=$!
	while kill -0 "$pid" 2>/dev/null; do
		if [ -z "${STEP5_IGNORE_LOCK:-}" ] && [ -e "$LOCK" ]; then
			kill -TERM -- "-$pid" 2>/dev/null
			sleep 2
			kill -KILL -- "-$pid" 2>/dev/null
			echo "ABORTED: the rig lock appeared: $(cat "$LOCK" 2>/dev/null)"
			return 3
		fi
		sleep 2
	done
	wait "$pid"
}

# step5_head: the run's header lines.
step5_head() {
	echo "== $1 — $(date -u +%FT%TZ), $(git -C "$REPO" log -1 --format='%h %s')"
	echo "== host: $(uname -srm), $(go version)"
	git -C "$REPO" status --short | sed 's/^/== dirty: /'
}

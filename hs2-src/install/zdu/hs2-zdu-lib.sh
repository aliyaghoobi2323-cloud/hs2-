# hs2 ZDU (zdu350 S8) shared helper, sourced by hs2-safe-deploy.sh and hs2-zdu-upgrade.sh.
# find_execstart_dropin UNIT: the drop-in that sets the ExecStart in force: the LAST file in lexical order
# under /etc/systemd/system/UNIT.service.d/ with an "ExecStart=/..." line (prod: 40-tunnel1.conf on t1,
# 50-canary.conf on t2). Prints the path; empty when none.
find_execstart_dropin(){
  local d="${SYSTEMD_DIR:-/etc/systemd/system}/$1.service.d" f last=""
  [ -d "$d" ] || return 0
  for f in $(ls -1 "$d"/*.conf 2>/dev/null | LC_ALL=C sort); do
    grep -qE '^ExecStart=/' "$f" && last="$f"
  done
  echo "$last"
}
# has_zdu BIN: `BIN version --features` lists zdu (pre-ZDU binaries ignore the flag: "no zdu")
has_zdu(){ timeout 10 "$1" version --features 2>/dev/null | grep -qE '^features:( [A-Za-z0-9_.-]+)* zdu( |$)'; }
# root_owned_path P: P and every parent directory are owned by uid 0 and not group/other writable,
# P is a regular file (not a symlink) and executable.
root_owned_path(){
  local p="$1" d
  case "$p" in /*) ;; *) return 1;; esac
  [ -L "$p" ] && return 1
  [ -f "$p" ] && [ -x "$p" ] || return 1
  d="$p"
  while :; do
    [ "$(stat -c %u "$d")" = 0 ] || return 1
    [ $(( 0$(stat -c %a "$d") & 022 )) = 0 ] || return 1
    [ "$d" = / ] && break
    d=$(dirname "$d")
  done
}
# dur_s D: a Go duration made of whole h/m/s parts ("200s", "3m20s", "1h") or whole seconds ("200") -> seconds.
# Anything else (fractions, ms, negative) fails: the caller refuses it.
dur_s(){
  local s="$1" n=0 v u
  [[ "$s" =~ ^[0-9]+$ ]] && { echo $((10#$s)); return 0; }
  [ -n "$s" ] || return 1
  while [ -n "$s" ]; do
    [[ "$s" =~ ^([0-9]+)([hms])(.*)$ ]] || return 1
    v=$((10#${BASH_REMATCH[1]})); u=${BASH_REMATCH[2]}; s=${BASH_REMATCH[3]}
    case $u in h) n=$((n+v*3600));; m) n=$((n+v*60));; s) n=$((n+v));; esac
  done
  echo "$n"
}
# drain_override_ok DRAIN [PROBATION]: an HS2_ZDU_DRAIN override counts from SIGUSR2 and includes the probation,
# so it must be at least probation (HS2_ZDU_PROBATION, default 45 s) + 60 s. Unset/""/"0" = the HH:03 rule: ok.
# The process clamps a short one anyway (drain ends >= OK + 60 s); the scripts refuse it so nobody relies on it.
drain_override_ok(){
  local d="$1" p="${2:-}" ds ps=45
  { [ -z "$d" ] || [ "$d" = 0 ]; } && return 0
  ds=$(dur_s "$d") || return 1
  [ "$ds" = 0 ] && return 0
  if [ -n "$p" ]; then ps=$(dur_s "$p") || return 1; fi
  [ "$ds" -ge $((ps + 60)) ]
}
# env_of FILE NAME: value of NAME in a /proc/PID/environ file ("" when unset)
env_of(){ tr '\0' '\n' < "$1" 2>/dev/null | sed -n "s/^$2=//p" | tail -1; }
# dropin_env DIR NAME: value of NAME the unit's drop-ins give a cold start: Environment= lines (last wins), then
# the files of their EnvironmentFile= lines ("-" prefix = optional; a file's value overrides Environment=, as in
# systemd); "" when unset.
dropin_env(){
  local v f kv
  v=$(cat "$1"/*.conf 2>/dev/null | grep -oE "^Environment=.*\b$2=[^ \"]*" | sed "s/.*$2=//" | tail -1)
  for f in $(cat "$1"/*.conf 2>/dev/null | sed -n 's/^EnvironmentFile=-\{0,1\}//p'); do
    [ -r "$f" ] || continue
    while IFS= read -r kv; do kv=${kv#export }; kv=${kv%$'\r'}
      case "$kv" in "$2"=*) v=${kv#*=}; v=${v#[\"\']}; v=${v%[\"\']};; esac
    done < "$f"
  done
  echo "$v"
}
# storm_min V: HS2_ZDU_STORM_MIN as the process reads it (TrimSpace, base 10: "08" = 8, " 5" = 5); 1 when
# unset, not a number or > 59.
storm_min(){
  local v="$1"
  v="${v#"${v%%[![:space:]]*}"}"; v="${v%"${v##*[![:space:]]}"}"
  case "$v" in ''|*[!0-9]*) echo 1; return;; esac
  v=$((10#$v)); [ "$v" -le 59 ] || v=1
  echo "$v"
}
# unit_hs2_env UNIT: the HS2_* variables systemd would give a COLD start of UNIT now (loaded unit config:
# Environment= then EnvironmentFile= files, a later file wins), one KEY=VALUE per line, sorted.
unit_hs2_env(){
  local f; declare -A m=()
  while IFS= read -r kv; do case "$kv" in HS2_*=*) m[${kv%%=*}]=${kv#*=};; esac
  done < <(systemctl show -p Environment --value "$1" 2>/dev/null | xargs -n1 printf '%s\n' 2>/dev/null)
  for f in $(systemctl show -p EnvironmentFiles --value "$1" 2>/dev/null | grep -oE '^/[^ ]+|[[:space:]]/[^ ]+' | tr -d ' '); do
    [ -r "$f" ] || continue
    while IFS= read -r kv; do kv=${kv#export }; kv=${kv%$'\r'}; case "$kv" in HS2_*=*) v=${kv#*=}; v=${v#[\"\']}; v=${v%[\"\']}; m[${kv%%=*}]=$v;; esac
    done < "$f"
  done
  for k in "${!m[@]}"; do printf '%s=%s\n' "$k" "${m[$k]}"; done | LC_ALL=C sort
}
# proc_hs2_env ENVIRON_FILE: the HS2_* variables of a running process, sorted (hand-off internals excluded:
# HS2_HANDOVER_* is set by the old process for its child only).
proc_hs2_env(){ tr '\0' '\n' < "$1" 2>/dev/null | grep -E '^HS2_[A-Za-z0-9_]*=' | grep -vE '^HS2_HANDOVER_' | LC_ALL=C sort; }
# env_drift UNIT ENVIRON_FILE: prints "+K=V" (a cold start would set it) / "-K=V" (the process has it, a cold start
# would not) lines; empty = same. A live handover keeps the process's environment (the child inherits it), so
# a drop-in Environment change only takes effect at a cold restart.
env_drift(){ diff <(proc_hs2_env "$2") <(unit_hs2_env "$1") | sed -n 's/^> /+/p; s/^< /-/p'; }
# b430 prune_backups STATE_DIR UNIT KIND [KEEP_DIR]: remove old STATE_DIR/hs2-backup-YYYYmmdd-HHMMSS-pre-KIND-UNIT
# directories (KIND zdu: hs2-zdu-upgrade.sh, safe: hs2-safe-deploy.sh). Kept: the newest HS2_BACKUP_KEEP (10) of
# that unit and kind by the name's time stamp, every one younger than HS2_BACKUP_DAYS (7) days (by its name OR its
# mtime), KEEP_DIR (the backup just made), and every one a rollback could point to: named in last-good.UNIT.conf
# or in the unit's .out result lines, or whose copy of an ExecStart drop-in equals last-good.UNIT.conf. Only real
# directories (no symlinks) whose whole name is exactly that pattern for exactly this unit are touched (unit
# "x-foo" never matches "foo"). Prints one line per removal; never fails the caller.
prune_backups(){
  local st="$1" u="$2" kind="$3" keep="${4:-}" n="${HS2_BACKUP_KEEP:-10}" days="${HS2_BACKUP_DAYS:-7}"
  local etc="${HS2_ZDU_ETC_DIR:-/etc/hs2}" lg now p b ts i=0 age refs f
  case "$st" in /*) ;; *) return 0;; esac
  [ "$st" = / ] && return 0
  [ -d "$st" ] && [ ! -L "$st" ] || return 0
  [[ "$u" =~ ^[A-Za-z0-9:_.@-]+$ ]] || return 0
  case "$kind" in zdu|safe) ;; *) return 0;; esac
  [[ "$n" =~ ^[0-9]+$ ]] && [[ "$days" =~ ^[0-9]+$ ]] || return 0
  lg="$etc/last-good.$u.conf"; now=$(date +%s)
  refs=$(cat "$lg" "$st/hs2-zdu-upgrade.$u.out" "$st/hs2-safe-deploy.$u.out" 2>/dev/null)
  local suf="-pre-$kind-$u"
  # newest first by the name's time stamp (fixed-width, so a lexical sort is a time sort)
  while IFS= read -r b; do
    [ -n "$b" ] || continue
    p="$st/$b"
    [ -d "$p" ] && [ ! -L "$p" ] || continue
    i=$((i+1))
    [ "$i" -le "$n" ] && continue
    [ -n "$keep" ] && [ "$p" = "$keep" ] && continue
    case "$refs" in *"$b"*) continue;; esac
    ts=${b#hs2-backup-}; ts=${ts%"$suf"}
    age=$(( now - $(date -d "${ts:0:8} ${ts:9:2}:${ts:11:2}:${ts:13:2}" +%s 2>/dev/null || echo "$now") ))
    [ "$age" -lt $((days*86400)) ] && continue
    [ $(( now - $(stat -c %Y "$p" 2>/dev/null || echo "$now") )) -lt $((days*86400)) ] && continue
    if [ -f "$lg" ]; then
      for f in "$p/$u.service.d"/*.conf; do [ -f "$f" ] && cmp -s "$f" "$lg" && continue 2; done
    fi
    rm -rf -- "$p" && echo "pruned $p"
  done < <(ls -1A -- "$st" 2>/dev/null | grep -E "^hs2-backup-[0-9]{8}-[0-9]{6}-pre-$kind-" | while IFS= read -r b; do
      [ "${b:0:11}${b:26}" = "hs2-backup-$suf" ] && [ "${#b}" = $(( 26 + ${#suf} )) ] && echo "$b"; done | LC_ALL=C sort -r)
  return 0
}
# b430 has_feature BIN F: `BIN version --features` lists F
has_feature(){ timeout 10 "$1" version --features 2>/dev/null | grep -qE "^features:( [A-Za-z0-9_.-]+)* $2( |\$)"; }
# b430 zdu_window BIN NOW STORM_MIN_RAW PROBATION_RAW SLACK: the end-of-hour verdict for a handover asked for at unix
# time NOW and signalled up to SLACK s later, with the running process's HS2_ZDU_STORM_MIN / HS2_ZDU_PROBATION.
# Prints "OK ..." or the refusal ("window ..."). A running binary with feature zduwin decides itself (the check
# its SIGUSR2 handler makes); an older one gets zdu_window_sh, the same rule in seconds.
zdu_window(){
  local r
  if has_feature "$1" zduwin; then
    r=$(HS2_ZDU_STORM_MIN="$3" HS2_ZDU_PROBATION="$4" timeout 10 "$1" zdu-window "$2" "$5" 2>/dev/null | head -1)
    [ -n "$r" ] && { echo "$r"; return 0; }
  fi
  zdu_window_sh "$2" "$3" "$4" "$5"
}
# zdu_window_sh NOW STORM_MIN_RAW PROBATION_RAW SLACK: the process's rule (cmd/hs2 zduWindowLine): refused when NOW
# or NOW+SLACK is in [M-1, M+3) or the window opens within READY timeout (90 s) + probation (45 s) + 15 s + SLACK.
zdu_window_sh(){
  local now="$1" sm off ps=45 span slack="${4:-0}" m
  sm=$(storm_min "$2")
  if [ -n "$3" ]; then ps=$(dur_s "$3") || { echo "window check: HS2_ZDU_PROBATION=$3 not whole h/m/s (older binary: no zdu-window)"; return 0; }
    [ "$ps" -gt 0 ] || ps=45; fi
  span=$(( 90 + ps + 15 ))
  off=$(( ((now % 3600) - ((sm + 59) % 60) * 60 + 3600) % 3600 )); m=$(( now % 3600 / 60 ))
  if [ "$off" -lt 240 ] || [ $(( (off + slack) % 3600 )) -lt 240 ]; then
    echo "window HH:$(printf %02d $(( (sm+59)%60 )))-HH:$(printf %02d $(( (sm+2)%60 ))) UTC (minute $m)"; return 0; fi
  if [ $(( off + span + slack )) -ge 3600 ]; then
    echo "window opens in <= $((span+slack))s (minute $m): READY + probation + margin (${span}s) + ${slack}s would run into the restart storm"; return 0; fi
  echo "OK span=${span}s slack=${slack}s storm_min=$sm"
}

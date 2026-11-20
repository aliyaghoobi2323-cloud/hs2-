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

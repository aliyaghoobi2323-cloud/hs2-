"""run_x.sh = run.sh + the CWNDLOCK=N cell key (lab, r1-diag): before the
two hs2 start, both hosts' routes to the other end get "cwnd N lock", so
every link socket starts with snd_cwnd_clamp = N packets (a sender-side
cap on bytes in flight; BBR keeps its cwnd within the clamp)."""
import sys, os
d = sys.argv[1]
s = open(os.path.join(d, "run.sh")).read()

def sub(old, new):
    global s
    if s.count(old) != 1:
        sys.exit("run.sh: %d matches for %r" % (s.count(old), old[:60]))
    s = s.replace(old, new)

sub("""  local N RATE RTT LOSS MODE USERS MIX WARM DUR SETTLE AGGQ_MS POLICE_BURST_MS V6
  WARM=5; DUR=30; SETTLE=5; V6=0; AGGQ_MS=50; POLICE_BURST_MS=20""", """  local N RATE RTT LOSS MODE USERS MIX WARM DUR SETTLE AGGQ_MS POLICE_BURST_MS V6 CWNDLOCK
  WARM=5; DUR=30; SETTLE=5; V6=0; AGGQ_MS=50; POLICE_BURST_MS=20; CWNDLOCK=""")
sub("""      N|RATE|RTT|LOSS|MODE|USERS|MIX|WARM|DUR|SETTLE|AGGQ_MS|POLICE_BURST_MS|V6) printf -v "$k" '%s' "$v" ;;""",
    """      N|RATE|RTT|LOSS|MODE|USERS|MIX|WARM|DUR|SETTLE|AGGQ_MS|POLICE_BURST_MS|V6|CWNDLOCK) printf -v "$k" '%s' "$v" ;;""")
sub("""  mkconf "$w" "$N" || { why="hs2 config"; return 1; }""", """  mkconf "$w" "$N" || { why="hs2 config"; return 1; }
  if [ -n "$CWNDLOCK" ]; then
    # lab (run_x.sh): snd_cwnd_clamp = CWNDLOCK packets on every link socket
    ip netns exec $X ip route replace 10.201.0.1/32 via 10.202.0.254 dev x0 cwnd lock "$CWNDLOCK" || { why="cwnd lock exit"; return 1; }
    ip netns exec $E ip route replace 10.202.0.1/32 via 10.201.0.254 dev e0 cwnd lock "$CWNDLOCK" || { why="cwnd lock edge"; return 1; }
  fi""")
s = s.replace("# run.sh PLAN OUT: the big rig's runner (R1 task 2).", "# run_x.sh PLAN OUT: run.sh with the lab key CWNDLOCK=N (r1-diag, R1 round 2\n# step 1: route \"cwnd N lock\" before the links open). run.sh: the big rig's runner (R1 task 2).", 1)
open(os.path.join(d, "run_x.sh"), "w").write(s)
os.chmod(os.path.join(d, "run_x.sh"), 0o755)
print("run_x.sh written")

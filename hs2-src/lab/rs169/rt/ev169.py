# ev169.py W MID T0FILE TT OUT: rs169 retry-heavy events (replaces NETEM_EVENTS; sub-second, reactive).
# Production topology (TOPO146): Iran IP i (0-based) = lab paths KV4[2i], KV4[2i+1]; path 1 = IPv6 (never touched).
# Event j every 40 s from t=30: Iran IP P=j%6 dark (both directions). The moment the edge log shows the first
# "iran RESCUE(" join after T (the rescue joins go out to fresh links on other IPs), 3 more Iran IPs
# (P+1..P+3) go dark at once: even j only kharej->Iran (ACK/answer path lost: the exit SERVES the RESCUE, Iran
# never hears it -> join silent -> RETRY repeats the RESCUE on a newer join = rel66 audit H1), odd j both
# directions (join lost outright). Their links still look alive for ~1.5 s, so joins keep landing on them.
# EV_CASCADE_AT=S: cascade at the first join or at T+S, whichever first (default 6 = reactive, rotation 1;
# rotation 2 uses 1.3: the cascade IPs' answer path is already dark when the first joins go out). Back at T+15. Leaves 2 IPv4 IPs + IPv6 clean during each event.
import sys, time, re, subprocess, json, os
W, MID, T0F, TT, OUT = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), sys.argv[5]
KV4 = [0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]
LIM = "limit 12036 delay 45000us rate 1000000000bit"   # knetem(LOSS=0, RATE 1000mbit, DELAY 45ms, QUEUE 100ms)
out = open(OUT, "a", buffering=1)
def t0():
    while True:
        try: return int(open(T0F).read())
        except Exception: time.sleep(0.2)
T0 = t0()
el = lambda: time.time() - T0
def emu(k):
    a, b, *x = open("%s/emu-%d" % (W, k)).read().split()
    return a, b, " ".join(x)
def tc(dev, args):
    subprocess.run(["ip", "netns", "exec", MID, "tc", "qdisc", "change", "dev", dev, "root", "netem"] + (LIM + " " + args).split(),
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
def dark(ip, mode):
    for k in (KV4[2 * ip], KV4[2 * ip + 1]):
        a, b, x = emu(k); bh = re.sub(r"loss random \S+", "loss random 100%", x)
        tc(a, bh)                      # a = Iran-side device: kharej -> Iran direction
        if mode == "both": tc(b, bh)
def clean(ip):
    for k in (KV4[2 * ip], KV4[2 * ip + 1]):
        a, b, x = emu(k); tc(a, x); tc(b, x)
def log(**k):
    k["el"] = round(el(), 3); out.write(json.dumps(k) + "\n")
CAS = float(os.environ.get("EV_CASCADE_AT", "6"))  # rot2: 1.3 = cascade at T+1.3 s, before the first joins (~T+1.5)
j = 0
while True:
    T = 30 + 40 * j
    if T + 20 > TT - 30: break
    while el() < T: time.sleep(0.05)
    P = j % 6; C = [(P + d) % 6 for d in (1, 2, 3)]; mode = "ret" if j % 2 == 0 else "both"
    lg = W + "/ir.log"
    try: off = open(lg, "rb").seek(0, 2)
    except Exception: off = 0
    dark(P, "both"); log(ev="dark", ip=P + 1, mode="both")
    hit = None; buf = b""
    with open(lg, "rb") as f:
        f.seek(off)
        while el() < T + CAS:
            d = f.read()
            if not d and os.path.getsize(lg) < f.tell(): f.seek(0)  # the log was rotated / truncated
            if d:
                buf = (buf + d)[-4096:]
                if b" iran RESCUE(" in buf or b" iran RREC(" in buf: hit = el(); break
            else: time.sleep(0.01)
    for c in C: dark(c, mode)
    log(ev="cascade", ips=[c + 1 for c in C], mode=mode, cas=CAS, first_join_after=round(hit - T, 3) if hit else None)
    while el() < T + 15: time.sleep(0.05)
    for ip in [P] + C: clean(ip)
    log(ev="clean")
    j += 1

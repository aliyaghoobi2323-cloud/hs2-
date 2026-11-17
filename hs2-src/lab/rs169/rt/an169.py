# an169.py ARMDIR...: rs169 retry-heavy A/B summary per arm: rescue counters (last "mconn rescue" line per side,
# incl. rescue145 retry and rescue169), trace-line counts by kind (both sides), call DIED (in event windows
# [T, T+20) vs calm, by error) and CORRUPT, crowd fails, p99s, RSS max, cascade timing.
import json, re, sys, gzip, collections
def evs(A):
    E = []
    for l in open(A + "/events.jsonl"):
        d = json.loads(l)
        if d["ev"] == "dark": E.append([d["el"], d])
        if d["ev"] == "cascade": E[-1].append(d)
    return E
for A in sys.argv[1:]:
    print("==", A)
    env = open(A + "/env.txt").read(); TT = int(re.search(r"TT=(\d+)", env).group(1))
    print("  ", re.search(r"edge=\S+", env).group(0), re.search(r"HS2_MCONN_RESCUE_IDEM=\d", env).group(0), re.search(r"cpus=\S+", env).group(0))
    E = evs(A)
    print("  events:", len(E), "cascade first-join lag s:", [c.get("first_join_after") for _, _, c in E if c], "modes:", "".join(c["mode"][0] for _, _, c in E if c))
    def win(el): return "ev" if any(t <= el < t + 20 for t, *_ in E) else "calm"
    died = collections.Counter(); corrupt = 0; td = 0
    for l in open(A + "/calls.jsonl"):
        d = json.loads(l)
        if d["ev"] == "CORRUPT": corrupt += 1
        if d["ev"] == "DIED" and d.get("el") is not None:
            if d["el"] >= TT - 5: td += 1; continue
            st = "stalled>3s" if d.get("lastrx_ago", 0) > 3 else "live"  # stalled: rx idle before the death
            died[(win(d["el"]), st, d.get("err", "")[:44])] += 1
    print("  calls DIED in-run:", sum(died.values()), "(ev %d)" % sum(v for k, v in died.items() if k[0] == "ev"), "teardown:", td, "CORRUPT:", corrupt)
    for k, v in sorted(died.items()): print("     ", v, "%s %s %s" % k)
    fw = collections.Counter()
    for l in open(A + "/fails.txt"):
        try: fw[win(float(l))] += 1
        except ValueError: pass
    print("  crowd fails:", dict(fw), "total", sum(fw.values()))
    try:
        r = json.load(open(A + "/result.json")); p = r.get("probe", r)
        for k in ("crowd", "call", "game"):
            x = p.get(k, {})
            print("  %-5s p50 %s p99 %s max %s fail/lost %s" % (k, x.get("p50_ms"), x.get("p99_ms"), x.get("max_ms"), x.get("fail", x.get("lost"))))
        print("  rss max MB ir %s kh %s" % (r.get("rss_ir_mb"), r.get("rss_kh_mb")))
    except Exception as e: print("  result.json:", e)
    for s in ("ir", "kh"):
        L = [l for l in open(A + "/%s.rescue-last.txt" % s, errors="replace") if "mconn rescue: " in l]
        if not L: continue
        l = L[-1]
        for part in l.split(" | "):
            part = re.sub(r"^\S+ ", "", part.strip()) if part.startswith(("0", "1", "2")) else part.strip()
            print("   %s %s" % (s, part))
        c = collections.Counter()
        for t in gzip.open(A + "/%s.rtrace.gz" % s, "rt", errors="replace"):
            k = re.sub(r"^\S+ ", "", t.strip()); k = re.sub(r"\b[0-9a-f]{8,}\b ?", "", k)
            k = re.sub(r"\d+", "N", k); k = re.sub(r"join \S+", "join L", k); k = re.sub(r"raw1 \S+", "raw1 L", k)
            c[k[:72]] += 1
        print("   %s trace:" % s, "; ".join("%d %s" % (v, k) for k, v in c.most_common(18)))
    # per retried conn: what happened after the retry (both sides' trace by conn id)
    by = collections.defaultdict(list)
    for s in ("ir", "kh"):
        for t in gzip.open(A + "/%s.rtrace.gz" % s, "rt", errors="replace"):
            f = t.split()
            if len(f) > 3 and f[1] == "rescue-trace": by[f[2]].append((f[0], " ".join(f[3:])))
    oc = collections.Counter()
    for cid, L in by.items():
        L.sort(); txt = [x for _, x in L]
        for kind, mark in (("raw", "iran rescue retry join"), ("rrec", "iran RREC(")):
            if kind == "raw" and not any(mark in x for x in txt): continue
            if kind == "rrec" and not any("rrec join silent: retry" in x or "rrec join" in x and "retry" in x for x in txt): continue
            i = max(k for k, x in enumerate(txt) if ("retry" in x and (("rrec" in x) == (kind == "rrec"))))
            after = txt[i + 1:]
            fresh = "fresh" if kind == "raw" and any("fresh=true" in x for x in txt) else ""
            if any("refused" in x for x in after): o = "exit refused"
            elif any("again: answered" in x for x in after): o = "exit answered again"
            elif any("answer on a replaced join: taken" in x for x in after): o = "late answer taken"
            elif any("ended waiting" in x for x in after): o = "ended waiting"
            elif any("RREC late answer" in x for x in after): o = "rrec late answer " + [x for x in after if "RREC late answer" in x][0].split(": ")[-1]
            elif any("answer nak=false" in x or "iran ACK(" in x for x in after): o = "answered"
            elif any("NAK(" in x or "nak=true" in x for x in after): o = "NAK"
            elif any("start failed" in x for x in after): o = "start failed"
            else: o = "other"
            oc[(kind, fresh, o)] += 1
    print("   retried conns by outcome:", "; ".join("%d %s%s %s" % (v, k[0], "/" + k[1] if k[1] else "", k[2]) for k, v in sorted(oc.items())))

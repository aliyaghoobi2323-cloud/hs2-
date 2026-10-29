import json,sys
for p in sys.argv[1:]:
  for l in open(p):
    r=json.loads(l)
    w=r["classes"].get("web",{}).get("main")
    if not w: continue
    c=r["classes"].get("call",{})
    print(r["N"], r["MIX"], r.get("extra",""), r["bin"], r["rep"], "pages",w["pages"],"fail",w["failed"],"p50",round(w["load_p50_ms"]),"p95",round(w["load_p95_ms"]),"max",round(w["load_max_ms"]))

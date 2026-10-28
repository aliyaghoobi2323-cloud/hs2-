#!/usr/bin/env python3
"""ana_test.py: unit tests of ana.py's verdicts (T17 capacity at the socket,
lost probes and pings, the asymmetric path RATE_UP). Run: python3 ana_test.py"""
import io
import json
import os
import sys
import tempfile
import unittest
from contextlib import redirect_stdout

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ana  # noqa: E402

EFF = 1448 / 1514


def lat(p1, p99, n=100, lost=0):
    l = {"n": n - lost, "sent": n, "lost": lost, "p1": p1, "p50": p1 + 1, "p90": p99, "p99": p99, "max": p99}
    return {"all": dict(l), "main": dict(l), "after": dict(l), "before": dict(l)}


def user(uid, role, flows, dn=0.0, up=0.0, path="port", w="main"):
    return {"id": uid, "role": role, "path": path, "fam": 4, "flows": flows,
            "dn_mbit": {w: dn, "before": dn}, "up_mbit": {w: up, "before": up}}


def rec(mix, users, rate=100, extra=None, probe=None, ping=None, mode="fqcodel", n=1):
    r = {"ok": True, "MIX": mix, "RATE": rate, "MODE": mode, "N": n, "extra": extra or {},
         "users": users, "dn_sum_mbit": {"main": sum(u["dn_mbit"]["main"] for u in users if "main" in u["dn_mbit"])},
         "up_sum_mbit": {"main": sum(u["up_mbit"]["main"] for u in users if "main" in u["up_mbit"])},
         "probe": probe or lat(72.0, 80.0), "ping_tun": ping, "cpu": {}, "retrans": {}, "links": {},
         "host_busy_pct": 10}
    return r


def lights(k, start=3):
    return [user("u%d" % i, "light", "probe", dn=0.0016, up=0.0016) for i in range(start, start + k)]


class Capacity(unittest.TestCase):
    def test_framing_constant(self):
        # T17: F = (E[t] - 4) / (E[t] + 22) on obfs.NewHTTPSLengthSampler, E[t] = 991.4 B
        self.assertAlmostEqual(ana.F_REC, (991.4 - 4) / (991.4 + 22), places=4)

    def test_cap_p10(self):
        r = rec("lone16", [])
        self.assertAlmostEqual(ana.capacity(r), 100 * EFF)
        self.assertAlmostEqual(ana.cap_p10(r), 100 * EFF * 0.9743)

    def test_asymmetric(self):
        r = rec("s5", [], extra={"RATE_UP": "20"})
        self.assertAlmostEqual(ana.capacity(r, "dn"), 100 * EFF)
        self.assertAlmostEqual(ana.capacity(r, "up"), 20 * EFF)
        self.assertAlmostEqual(ana.cap_p10(r, "up"), 20 * EFF * 0.9743)

    def test_agg_and_police(self):
        r = rec("s5", [], mode="police:3+agg:10", n=2)
        self.assertAlmostEqual(ana.capacity(r), 6 * EFF)


class Verdicts(unittest.TestCase):
    def test_lone16_gate_on_cap_p10(self):
        # 90 Mbit/s at 100: 94.1% of the raw capacity (fails 0.95), 96.6% of cap_P10 (passes)
        r = rec("lone16", [user("u1", "lone", "dn", dn=90.0)] + lights(199, 2))
        m = ana.derive(r)
        self.assertAlmostEqual(m["a_raw"], 90.0 / (100 * EFF), places=4)
        self.assertAlmostEqual(m["a_ratio"], 90.0 / (100 * EFF * 0.9743), places=4)
        self.assertTrue(m["a"])
        self.assertLess(m["a_raw"], 0.95)

    def test_one_vs_16_floor(self):
        ls = lights(198)
        light = sum(u["dn_mbit"]["main"] for u in ls)
        r = rec("one_vs_16", [user("u1", "one", "dn", dn=42.0), user("u2", "sixteen", "dn", dn=49.0)] + ls)
        m = ana.derive(r)
        fl10 = (100 * EFF * 0.9743 - light) / 2
        fl = (100 * EFF - light) / 2
        self.assertAlmostEqual(m["b_ratio"], 42.0 / fl10, places=4)
        self.assertAlmostEqual(m["b_raw"], 42.0 / fl, places=4)
        self.assertTrue(m["b"])  # 0.903 of the cap_P10 floor
        self.assertAlmostEqual(m["c_ratio"], 91.0 / (100 * EFF * 0.9743), places=4)
        self.assertAlmostEqual(m["c_raw"], 91.0 / (100 * EFF), places=4)

    def test_lost_probes_fail_d(self):
        us = [user("u1", "bulk", "dn", dn=20.0), user("u2", "light", "probe")]
        ok = ana.derive(rec("s5", us, rate=25, probe=lat(72.0, 85.0, lost=1)))
        self.assertTrue(ok["d"])  # 1 of 100 lost: p99 over all sent still finite
        bad = ana.derive(rec("s5", us, rate=25, probe=lat(72.0, 85.0, lost=3)))
        self.assertFalse(bad["d"])  # 3 of 100 lost: p99 over all sent is infinite
        late = ana.derive(rec("s5", us, rate=25, probe=lat(72.0, 92.5)))
        self.assertFalse(late["d"])  # 20.5 ms over the floor

    def test_lost_pings_fail_tun(self):
        us = [user("u1", "bulk", "dn", dn=20.0), user("u2", "light", "probe")]
        m = ana.derive(rec("s5", us, rate=25, ping=lat(72.0, 75.0, lost=50)))
        self.assertFalse(m["tun"])
        m = ana.derive(rec("s5", us, rate=25, ping=lat(72.0, 81.9)))
        self.assertTrue(m["tun"])

    def test_asym_duplex_per_direction(self):
        us = [user("u%d" % i, "bulk", "dn", dn=22.5) for i in range(1, 5)]
        us.append(user("u5", "bulk", "up", up=18.0))
        us.append(user("u6", "light", "probe"))
        m = ana.derive(rec("s5", us, extra={"RATE_UP": "20"}))
        self.assertAlmostEqual(m["c_dn"], 90.0 / (100 * EFF * 0.9743), places=3)
        self.assertAlmostEqual(m["c_up"], 18.0 / (20 * EFF * 0.9743), places=3)
        self.assertAlmostEqual(m["cap_up"], 20 * EFF)

    def test_newheavy_raw_and_p10(self):
        us = [user("u1", "light", "probe"), user("u2", "exist", "dn", dn=43.0, w="after_settled"),
              user("u3", "heavy", "dn", dn=48.0, w="after_settled")]
        for u in us:
            u["dn_mbit"].setdefault("after_settled", 0.0)
            u["dn_mbit"].setdefault("main", 0.0)
            u["up_mbit"].setdefault("main", 0.0)
        r = rec("newheavy", us)
        r["dn_sum_mbit"]["after_settled"] = 91.0
        m = ana.derive(r)
        self.assertAlmostEqual(m["e_bulk_ratio"], 43.0 / (100 * EFF * 0.9743 / 2), places=3)
        self.assertAlmostEqual(m["e_bulk_raw"], 43.0 / (100 * EFF / 2), places=3)
        self.assertTrue(m["e"])


class Analyze(unittest.TestCase):
    def test_table_prints_both_ratios(self):
        r = rec("lone16", [user("u1", "lone", "dn", dn=90.0)] + lights(9, 2))
        r.update({"cell": "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=10 MIX=lone16", "bin": "old"})
        with tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False) as f:
            for _ in range(2):
                f.write(json.dumps(r) + "\n")
        out = io.StringIO()
        try:
            with redirect_stdout(out):
                ana.analyze(f.name)
        finally:
            os.unlink(f.name)
        s = out.getvalue()
        self.assertIn("cap_P10 93.18", s)
        self.assertIn("(a) lone user 97% of cap_P10 (raw 94%)", s)
        self.assertIn("a PASS (2/2)", s)


def spec_of(cell):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        p = f.name
    try:
        with redirect_stdout(io.StringIO()):
            ana.spec(cell, p)
        with open(p) as f:
            return json.load(f)
    finally:
        os.unlink(p)


CELL = "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel "


class ClassSpec(unittest.TestCase):
    def test_home(self):
        s = spec_of(CELL + "USERS=16 MIX=home SBR=1.2")
        roles = [u["role"] for u in s["users"]]
        for r, k in (("game", 1), ("gameudp", 1), ("call", 2), ("stream", 4), ("web", 4),
                     ("heavy", 2), ("uploader", 2)):
            self.assertEqual(roles.count(r), k, r)
        self.assertEqual(s["total_s"], 5 + 30 + 5)  # WARM + DUR + TAIL
        self.assertEqual(s["windows"][0], {"name": "main", "from": 5, "to": 35})
        st = [u["flows"][0] for u in s["users"] if u["role"] == "stream"]
        self.assertEqual([f["start_s"] for f in st], [5.0, 6.0, 7.0, 8.0])
        self.assertTrue(all(f["rate"] == 1.2 for f in st))
        hv = [u["flows"][0] for u in s["users"] if u["role"] == "heavy"]
        self.assertEqual(hv[0], {"kind": "dn", "n": 16, "start_s": 0})
        # no probe in the class mixes
        self.assertFalse(any(f["kind"] == "probe" for u in s["users"] for f in u["flows"]))

    def test_vs_mixes(self):
        s = spec_of(CELL + "USERS=5 MIX=gamer_vs_dl GAME=tcp DIR=up")
        self.assertEqual([u["role"] for u in s["users"]], ["game", "heavy", "game", "heavy", "game"])
        self.assertEqual(s["users"][1]["flows"][0]["kind"], "up")
        s = spec_of(CELL + "USERS=4 MIX=stream_vs_dl")
        self.assertEqual([u["flows"][0]["kind"] for u in s["users"]], ["stream", "dn", "stream", "dn"])
        self.assertEqual(s["users"][0]["flows"][0]["rate"], 5.0)  # SBR default

    def test_old_mixes_unchanged(self):
        s = spec_of(CELL + "USERS=1 MIX=lone16")
        self.assertEqual([f["kind"] for f in s["users"][0]["flows"]], ["dn", "probe"])
        s = spec_of(CELL + "USERS=8 MIX=s5")
        self.assertEqual(s["total_s"], 35)
        self.assertEqual(len(s["windows"]), 2)


def owd(p50, p99, floor, sent=1000, lost=0, late=0, offered=None):
    o = {"n": sent - lost, "sent": sent, "lost": lost, "min": floor, "p50": floor + p50, "p99": floor + p99,
         "p999": floor + p99 + 1, "max": floor + p99 + 2, "jit_p99": 1.0, "late": late, "late_ms": 100}
    if offered:
        o.update({"offered_mbit": offered, "delivered_mbit": offered * 0.99})
    return o


def rt_class(p99_up, p99_dn, floor=36.0, **kw):
    return {"flows": 2, "late_ms": 100, "floor_ms": {"up": floor, "dn": floor},
            "up": {"main": owd(1, p99_up, floor, **kw)}, "dn": {"main": owd(1, p99_dn, floor, **kw)}}


def class_rec(mix, classes, users, rate=100):
    r = rec(mix, users, rate=rate)
    r["RTT"] = 72
    r["classes"] = classes
    r["probe"] = None
    return r


class ClassVerdicts(unittest.TestCase):
    def test_game_and_call(self):
        us = [user("u1", "game", "game1@0", dn=0.1, up=0.05), user("u2", "heavy", "dn16@0", dn=90.0)]
        m = ana.derive(class_rec("gamer_vs_dl", {"game": rt_class(9.0, 4.0), "gameudp": rt_class(12.0, 3.0)}, us))
        self.assertTrue(m["v_game"])
        self.assertFalse(m["v_gameudp"])  # up p99 12 ms over the floor
        self.assertEqual(m["game_up_p99"], 9.0)
        self.assertEqual(m["game_up_p99_path"], 36.0 + 9.0 - 36.0)
        m = ana.derive(class_rec("gamer_vs_dl", {"game": rt_class(5.0, 5.0, late=2)}, us))
        self.assertFalse(m["v_game"])  # 2 late of 1000: 0.2 % > 0.1 %
        m = ana.derive(class_rec("gamer_vs_dl", {"game": rt_class(5.0, 5.0, lost=1)}, us))
        self.assertTrue(m["v_game"])  # 1 lost of 1000: 0.1 %
        c = rt_class(29.0, 31.0, offered=1.5)
        m = ana.derive(class_rec("home", {"call": c}, us))
        self.assertFalse(m["v_call"])
        self.assertAlmostEqual(m["call_dn_deliv"], 0.99)
        self.assertNotIn("c", m)  # no P10 gates on the class mixes
        self.assertAlmostEqual(m["util_dn"], (90.1) / (100 * EFF * 0.9743), places=4)

    def test_lost_p99_is_never(self):
        c = rt_class(5.0, 5.0)
        c["dn"]["main"]["p99"] = -1  # the percentile fell on a lost message
        us = [user("u1", "game", "game1@0", dn=0.1), user("u2", "heavy", "dn16@0", dn=90.0)]
        m = ana.derive(class_rec("gamer_vs_dl", {"game": c}, us))
        self.assertEqual(m["game_dn_p99"], ana.INF)
        self.assertFalse(m["v_game"])

    def stream_rec(self, sbr, rebuf=0, startup_max=1.5, heavy=4):
        us = [user("u%d" % i, "stream", "stream1@5", dn=sbr) for i in range(4)]
        us += [user("h%d" % i, "heavy", "dn16@0", dn=20.0) for i in range(heavy)]
        us.append(user("w", "web", "web1@0", dn=3.0))
        st = {"users": 4, "not_started": 0, "startup_p50_s": 1.0, "startup_max_s": startup_max,
              "rebuffers": rebuf, "rebuffer_s": 2.0 * rebuf, "rebuffer_users": min(rebuf, 4),
              "seg_mbit_mean": 3.0, "bitrate_mbit": sbr, "session_mean_s": 32}
        return class_rec("stream_vs_dl", {"stream": st}, us)

    def test_stream_fair_share(self):
        m = ana.derive(self.stream_rec(5.0))
        # cap_P10 less the browser's 3 Mbit/s, over 4 viewers and 4 heavy users
        self.assertAlmostEqual(m["stream_fair"], (100 * EFF * 0.9743 - 3.0) / 8, places=4)
        self.assertTrue(m["v_stream"])
        self.assertFalse(ana.derive(self.stream_rec(5.0, rebuf=1))["v_stream"])
        self.assertFalse(ana.derive(self.stream_rec(5.0, startup_max=2.5))["v_stream"])
        # more than the fair share: no verdict
        self.assertNotIn("v_stream", ana.derive(self.stream_rec(15.0)))

    def write(self, recs):
        f = tempfile.NamedTemporaryFile("w", suffix=".jsonl", delete=False)
        for r in recs:
            f.write(json.dumps(r) + "\n")
        f.close()
        return f.name

    def analyze(self, recs):
        p = self.write(recs)
        out = io.StringIO()
        try:
            with redirect_stdout(out):
                ana.analyze(p)
        finally:
            os.unlink(p)
        return out.getvalue()

    def test_table_and_web_vs_old(self):
        cell = CELL + "USERS=9 MIX=stream_vs_dl SBR=15"
        recs = []
        for b, p95 in (("old", 2000.0), ("old", 2100.0), ("new", 2050.0), ("new", 2100.0)):
            r = self.stream_rec(15.0)
            r["classes"]["web"] = {"main": {"pages": 10, "failed": 0, "load_p50_ms": 900.0, "load_p95_ms": p95,
                                            "load_max_ms": p95, "fb_p50_ms": 80.0, "fb_p95_ms": 120.0}}
            r["classes"]["game"] = rt_class(4.0, 3.0)
            r.update({"cell": cell, "bin": b})
            recs.append(r)
        s = self.analyze(recs)
        self.assertIn("class verdict (report only): game PASS (2/2)  stream n/a", s)
        self.assertIn("web PASS (p95 2075 [2050..2100] / old 2050 [2000..2100] ms)", s)
        self.assertIn("p95 new/old 1.012", s)

    def test_web_worse(self):
        cell = CELL + "USERS=9 MIX=stream_vs_dl SBR=15"
        recs = []
        for b, p95 in (("old", 2000.0), ("new", 2200.0)):
            r = self.stream_rec(15.0)
            r["classes"]["web"] = {"main": {"pages": 10, "failed": 0, "load_p50_ms": 900.0, "load_p95_ms": p95,
                                            "load_max_ms": p95, "fb_p50_ms": 80.0, "fb_p95_ms": 120.0}}
            r.update({"cell": cell, "bin": b})
            recs.append(r)
        s = self.analyze(recs)
        self.assertIn("web FAIL", s)

    def test_inf_median(self):
        self.assertEqual(ana.agg([11053.0, ana.INF]), (ana.INF, 11053.0, ana.INF))
        self.assertEqual(ana.agg([11053.0, 12000.0, ana.INF])[0], 12000.0)
        self.assertEqual(ana.agg([1.0, 3.0]), (2.0, 1.0, 3.0))
        cell = CELL + "USERS=9 MIX=stream_vs_dl SBR=15"
        recs = []
        for b, p95 in (("old", 2000.0), ("new", 2100.0), ("new", -1)):
            r = self.stream_rec(15.0)
            r["classes"]["web"] = {"main": {"pages": 10, "failed": 1 if p95 < 0 else 0, "load_p50_ms": 900.0,
                                            "load_p95_ms": p95, "load_max_ms": p95, "fb_p50_ms": 80.0, "fb_p95_ms": 120.0}}
            r.update({"cell": cell, "bin": b})
            recs.append(r)
        s = self.analyze(recs)
        self.assertIn("p95 new/old inf", s)
        self.assertIn("web FAIL (p95 inf [2100..inf] / old 2000 ms)", s)

    def test_standing_queue_flagged(self):
        cell = CELL + "USERS=2 MIX=gamer_vs_dl"
        us = [user("u1", "game", "game1@0", dn=0.1), user("u2", "heavy", "dn16@0", dn=90.0)]
        r = class_rec("gamer_vs_dl", {"game": rt_class(4.0, 3.0, floor=36.0)}, us)
        r["classes"]["game"]["floor_ms"]["dn"] = 136.0  # 100 ms over the path
        r["classes"]["game"]["dn"]["main"] = owd(1, 3.0, 136.0)
        r.update({"cell": cell, "bin": "old"})
        s = self.analyze([r])
        self.assertIn("game PASS (1/1; floor over the path: dn +100 ms)", s)


class KillAndFrozen(unittest.TestCase):
    """the R1 round-4 judge's kill cell (KILLQ) and frozen downloads (MIX frozen)"""

    def test_kill_summary(self):
        d = tempfile.mkdtemp()
        with open(os.path.join(d, "kill.json"), "w") as f:
            json.dump({"what": "quiet", "class": "quiet", "peer": "10.202.0.1:5", "n_quiet": 2, "n_bulk": 8,
                       "killed": 1, "t_rel": 10.02, "clock": "12:00:10.020000"}, f)
        with open(os.path.join(d, "edge.log"), "w") as f:
            f.write("12:00:01.000000 mtcp: QUIET link q0 up from 10.202.0.1 (now 1 QUIET)\n"
                    "12:00:10.030000 mtcp: QUIET link q1 down: reset (now 1 QUIET)\n"
                    "12:00:11.520000 mtcp: QUIET link q2 up from 10.202.0.1 (now 2 QUIET)\n")
        su = [{"role": "game"}, {"role": "call"}, {"role": "web"}, {"role": "heavy"}]
        cl = {"kill_s": 10, "conns": [
            {"u": 1, "k": "game", "o": 1, "c": 10.05, "x": 10.05},  # cut
            {"u": 1, "k": "game", "o": 10.3, "c": 40, "x": -1},     # its new one
            {"u": 2, "k": "call", "o": 1, "c": 40, "x": -1},        # spared
            {"u": 3, "k": "web", "o": 9.5, "c": 10.1, "x": 10.1},   # cut
            {"u": 3, "k": "web", "o": 2, "c": 3, "x": -1},          # not live
            {"u": 4, "k": "dn", "o": 1, "c": 40, "x": -1},          # moved: spared
            {"u": 4, "k": "dn", "o": 1, "c": 25, "x": 25}],         # cut later: not the kill
            "recov": [{"u": 1, "k": "game", "cut": 10.05, "back": 0.4},
                      {"u": 3, "k": "web", "cut": 10.1, "back": -1}],
            "outage": [{"u": 1, "k": "game", "dir": "dn", "gap_ms": 450, "at": 10},
                       {"u": 2, "k": "call", "dir": "dn", "gap_ms": 40, "at": 9}]}
        k = ana.kill_summary(d, cl, su)
        self.assertEqual((k["live"], k["cut"], k["users_live"], k["users_hit"]), (5, 2, 4, 2))
        self.assertEqual(k["by_kind"], {"game": [1, 1], "call": [1, 0], "web": [1, 1], "dn": [2, 0]})
        self.assertEqual(k["by_role"]["heavy"], [1, 0])
        self.assertEqual((k["recov_n"], k["never"], k["recov_p50"]), (2, 1, 0.4))
        self.assertAlmostEqual(k["edge_down_s"], 0.01, places=3)
        self.assertAlmostEqual(k["replace_s"], 1.5, places=3)
        self.assertEqual(k["outage_ms"]["game_dn"]["max"], 450)
        m = {}
        ana.kill_metrics({"kill": k}, m)
        self.assertAlmostEqual(m["kill_share"], 0.4)
        self.assertAlmostEqual(m["kill_users_share"], 0.5)
        self.assertAlmostEqual(m["kill_share_game"], 1.0)
        self.assertEqual(m["kill_out_game_dn_max"], 450)

    def test_frozen(self):
        d = tempfile.mkdtemp()
        p = os.path.join(d, "edge.log")
        with open(p, "w") as f:
            f.write("12:00:20.000000 mtcp: reset 7 connection(s) on 1 link(s) whose app had taken nothing for 4s"
                    " while the link's receive buffer was full - x\n"
                    "12:00:21.000000 mtcp: 1 user(s) held more than 1024 KiB unread on QUIET links (an app not reading): y\n")
        fl = ana.frozen_log(p)
        self.assertEqual(fl, {"wedge_reset": 7, "stopped_lines": 0, "held_users": 1})
        m = {}
        ana.kill_metrics({"frozen": {"conns": 64, "frozen": 64, "mbytes": 12.5, "edge_log": fl,
                                     "probe_gap_ms": {"max": 2400, "p50": 210, "probe_p99": 95.0}}}, m)
        self.assertEqual((m["frz_gap_max"], m["frz_wedge_reset"], m["frz_held_users"]), (2400, 7, 1))

    def test_frozen_spec_and_kill_key(self):
        d = tempfile.mkdtemp()
        p = os.path.join(d, "s.json")
        with redirect_stdout(io.StringIO()):
            ana.spec("N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=10 MIX=frozen FROZEN=7 FWAVE=4 FROZENU=2 KILLQ=12.5", p)
        s = json.load(open(p))
        self.assertEqual(s["kill_s"], 12.5)
        fz = [u for u in s["users"] if u["role"] == "frozen"]
        self.assertEqual([f["n"] for u in fz for f in u["flows"]], [4, 3])
        self.assertEqual(sum(1 for u in s["users"] if u["flows"][0]["kind"] == "probe"), 8)

if __name__ == "__main__":
    unittest.main()

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


if __name__ == "__main__":
    unittest.main()

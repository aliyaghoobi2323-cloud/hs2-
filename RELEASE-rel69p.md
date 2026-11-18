# hs2 rel69p — release notes

- Tag `v-rel69p`. Binary `hs2-linux-amd64`, sha256
  `c0e040362f76c1ccc8b9fbf6705d8ad848715d6cea9693fa4c3e0946ee9dcb63`, `hs2 version` shows `[build rel69p-rc3]`.
  These are the exact bytes the reference production tunnel 1 runs since 2026-11-18 22:46 UTC (published unchanged
  for traceability, so the stamp keeps the release-candidate name). Static build (`CGO_ENABLED=0 -trimpath -ldflags
  "-s -w -X main.buildTag=rel69p-rc3"`, go1.27.3) from a `git archive` of the rel69p source commit.
- Previous GitHub release: **rel69m** (tag `v-rel69m`, binary sha256 `5e8fc8bc…`). rel69n and rel69o ran in
  production but were never published, so these notes cover **rel69m → rel69p**. The production defaults table
  (`hs2-src/envdef/envdef.go`) grew from 111 to 132 keys.
- `install.sh`: the rel69m installer plus `auto6` in the LB menus (sha256 in `install.sh.sha256`; the root
  `install.sh` and `hs2-src/install/install.sh` are the same file).
- **Config files: no change needed.** New optional value: `"ip": "auto6"` in this side's lb list.
- The public `hs2-src/` no longer carries the lab harnesses and lab results (`hs2-src/lab/*`); only `hs2-src/lab/netsim`
  stays, because Go tests import it.

## What changed since rel69m


Wire compatibility and the changes:
Source = b356 `abda029a` (the rel69p-rc3 build commit); the lab-only commits after it add files under `hs2-src/lab/`
only. envdef 111 -> 132 keys. No capability bit. Two optional additions on the wire, both ignored by older peers:
the edge's advert carries an optional per-path serving block (`HS2_B331_SPAREPATH`; an old exit drops it, an old
edge sends none and a new exit behaves as before), and the LB report carries an optional per-path `ok` field
(`HS2_B340_CUTCLEAR`, JSON `omitempty`). Upgrade the kharej (exit) first, then Iran.

Every key below is on by default unless noted; `=0` turns it off (on the side listed).

| change | key (default) | side |
|---|---|---|
| rel69p: user connections pick from a short per-path candidate list (best 16 per steering group + threshold, stripe holders always in) instead of scanning every link under the read lock | `HS2_B327_PICKLIST` (1); tuning `HS2_B327_K` (16), `HS2_B327_SLACK` (0) | both |
| rel69p: a link on a path that is stalled as a whole is not judged stuck before 20 s (stops the stuck-drain storm after a one-direction cut) | `HS2_B337_PATHSTALL` (1) | Iran |
| rel69p: stall hysteresis — once a path stalled whole, its links stay held from stuck verdicts for 20 s while part of the path answers again | `HS2_B339_PATHHYST` (1) | Iran |
| rel69p: cut grace for drain reclaim (no user gain measured while `HS2_LB_EVAC` is off) | `HS2_B339_CUTGRACE` (**0**, `=1` on) | Iran |
| rel69p: the exit dials one spare link for a recovered PATH that is >= 2 links under its share (and whose Iran IP is under the edge's target), 1/path/30 s, 6/min | `HS2_B331_SPAREPATH` (1) | both |
| rel69p: rescue-cut per-connection backoff — the first drop is taken, each further drop on the same connection within 30 s waits 0.5 s << n (max 16 s) | `HS2_B338_RC_GAP0_MS` (500; `=0` off) | both |
| rel69p: light / call / rescue placement skips links marked by rescue-cut, using them only when no unmarked link exists | `HS2_B340_SKIPCUT` (1) | both |
| rel69p: a cut path whose raw readings are clean again (3 in a row) stays lowered but is no longer cut-marked | `HS2_B340_CUTCLEAR` (1) | both |
| rel69p: a call whose lossiest copy rides a path >= 0.15 % worse than the best free path for 10 s opens one fresh sub there and gives the old copy up once the fresh sub read data (1/call/30 s, 20/min) | `HS2_B336_CALLMOVE` (1) | Iran |
| rel69p: darkcall switches a call's primary with bytes in flight and no ack progress for max(3 x srtt, 300 ms) to a live copy on another path with recent ack progress, without waiting for RTO backoff | `HS2_B342_DCFAST` (1) | both |
| rel69p: call path log (log only) | `HS2_CALLPATH_LOG` (**0**, `=1` on) | Iran |
| rel69o: an exit sub gets no watcher goroutine; the same cleanup runs once from the sub's close or the connection's end (fewer parked goroutines in reconnect storms); `/debug/hs2sched` counters on the pprof listener | `HS2_B322_EXITNOWATCH` (1) | kharej |
| rel69o: `auto6` lb address = this server's current global IPv6, re-resolved every 10 s and on `cannot assign requested address` (hosts that get a new IPv6 at each boot); a literal source IP missing from the server is cooled 15 s .. 5 min instead of redialed in a loop | config value `"ip": "auto6"` / `auto6:IFACE`; no key | both |
| rel69o: the data-dark rescue link index is sharded (64 locks): no lock convoy in reconnect storms | no key | Iran |
| rel69n: msub.close no longer waits behind the stream's FIN; heavy connections keep the synchronous close | `HS2_B307_ASYNCCLOSE` (1), `HS2_B311_HEAVYSYNC` (1) | both |
| rel69n: per-link copy-sub counter; exit conn set and mconn table sharded into 64 locks; smux `IsClosed` is one atomic load | `HS2_B302_COPYCNT` (1); shards/atomic: no key | both |
| rel69n: TUN dup second copy kept per flow, avoids the path the first copy left, scans other paths in rank order | `HS2_B303_DUPSTICK` (1), `HS2_B308_DUPAVOID` (1), `HS2_B309_DUPSCAN` (1) | both |
| rel69n: join/stripe sub opens without the connection lock; rescue-join placement memoised and placed under the read lock | `HS2_B312_OPENNOLOCK` (1), `HS2_B313_RJMEMO` (1), `HS2_B313_RJFAST` (1) | Iran |
| lab only, not in the table | `HS2_B325_STORMPICK` (off), `HS2_B335_FASTLOWER` (off), `HS2_B337_FL_LOGONLY`, `HS2_B337_UNCUT_DEBUG`, `HS2_B336_FL_DEBUG` | — |

Installer: the rel69m installer (live load-balancing screen) plus b333: the LB menus accept `auto6` (or `auto6:IFACE`)
as this server's own address. `install.sh` and `hs2-src/install/install.sh` are the same file.

Lab results (production-like harness: crowd of light users, calls, games, ping, heavy flows, path cuts, hourly-style
reconnect storms; A/B arms run in parallel on split CPU halves, swapped between runs):
- rel69n vs rel69m: heavy throughput +2.7 % (within noise), 1 h soak equal on every metric, 0 panics.
- rel69o: storm lock wait on Iran -92 %; exit run-queue peak -16 % and ~4.5k fewer parked goroutines in storms; auto6
  follows a live IPv6 change in 0.35 s with no ping or call hit.
- rel69p pick list: storm pick CPU -25 % to -76 %; PATHSTALL removed the mass stuck-drain after a down-direction cut.
- rel69p-rc3 vs production rel69o at 2x load (8 calls + 4 games): per-stream call p99 median 1571 -> 440 ms
  (p < 0.001), streams with >= 3 s call blackouts 3-7 -> 0-2 per run, crowd failures about 3x lower; ICMP and crowd
  p99 within a few ms (replicated on a second lab host: ICMP p99 equal or better in 3/3 pairs). 1 h soak: calls 8/8
  streams better (p99 433 vs 495 ms, 1 s+ samples -47 %), ICMP/crowd/heavy/reconnect equal, 0 panics, no leak.
  Reconnect-storm worst open time and heavy download: no difference beyond run-to-run noise.

Release gate on the build commit: gofmt and `go vet ./...` clean, `go test ./...` and `go test -race ./engine`
(and -race on every other package) passed, 0 DATA RACE.

Release binary (production tunnel 1 since 2026-11-18 22:46 UTC): `hs2-linux-amd64` sha256
`c0e040362f76c1ccc8b9fbf6705d8ad848715d6cea9693fa4c3e0946ee9dcb63`, `hs2 version` shows `[build rel69p-rc3]` (the
exact production bytes are published, so the stamp is the release candidate's name). Built from a `git archive` of
`abda029a` (LF) with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69p-rc3"`,
go1.27.3, static. The public tree differs from that commit only in redacted server addresses (tests and two
comments), so a rebuild from it differs only in the Go/GNU build-id bytes.

Rollback to rel69m behaviour: turn the keys above off (`HS2_B327_PICKLIST=0 HS2_B337_PATHSTALL=0 HS2_B339_PATHHYST=0
HS2_B331_SPAREPATH=0 HS2_B338_RC_GAP0_MS=0 HS2_B340_SKIPCUT=0 HS2_B340_CUTCLEAR=0 HS2_B336_CALLMOVE=0
HS2_B342_DCFAST=0 HS2_B322_EXITNOWATCH=0 HS2_B307_ASYNCCLOSE=0 HS2_B303_DUPSTICK=0 HS2_B308_DUPAVOID=0
HS2_B309_DUPSCAN=0 HS2_B312_OPENNOLOCK=0 HS2_B313_RJMEMO=0 HS2_B313_RJFAST=0 HS2_B302_COPYCNT=0`) or run the rel69m
binary (sha256 5e8fc8bc...). The shards, the atomic smux close flag and auto6 have no key.


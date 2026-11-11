# Menu M7 — lab checklist (2026-11-11, branch int39)

Labs 2 (5.160.110.234) and 3 (5.160.110.235), real servers, real tunnels.
Driver: `hs2-src/install/tests/m7/m7drive.py` (pexpect pty; GitHub made
unreachable so the bundle's binary installs). "Fixed" = failed first, fixed in
int39, passed on the rerun with the fixed build.

| # | Item | Result | Evidence / note |
|---|------|--------|-----------------|
| 1 | Fresh install from the new menu (no binary on the server) | PASS (fixed) | first run: 2) Tunnels fell to the classic setup screens; now offers to install the binary, then the wizard (both labs) |
| 2 | Wizard: reverse l3mtcp tunnel (lab 2, Iran listens) | PASS | unit `hs2` active, `iran reverse l3mtcp 0.0.0.0:24443` |
| 3 | Wizard: direct mtcp tunnel (lab 3, Kharej listens) | PASS | unit `hs2` active, `kharej direct mtcp 0.0.0.0:24444` |
| 4 | Join with a code (both labs, both directions) | PASS (fixed) | first run: "a tunnel named hs2 already exists" at the last step; now joins as `hs2-t2` with a note; both sides `active`, links up (8 links) |
| 5 | Sections 1–9 (dashboard, tunnels, LB & IPs, security & camouflage, performance, monitoring & alerts, doctor incl. run-all, maintenance, settings) — en, simple, ASCII locale | PASS | 9/9 |
| 6 | Same sections — fa, advanced, colour off, 80 columns | PASS | 9/9 |
| 7 | Settings persist in menu.conf (lang / mode / colour / default menu) | PASS (re-run, rel40) | driver check fixed: it now reads the settings screen, toggles only the items that differ from the target, re-reads after each press, then checks menu.conf (a key never written = the menu default) and the screen of the next start; 4 rounds from no menu.conf: en/simple/on/new, fa/advanced/off/classic, en/simple/on/new (written explicitly), fa/advanced/off/new — all PASS on lab 2 (rel40 build, tunnel running) and lab 3. The first int39 run's one-shot check assumed the toggle direction (test logic, not the menu) |
| 8 | No-colour TTY | PASS (fixed) | first run: separators/notes (`hr`, `info`, `warn`) kept ANSI colour; now none; `NO_COLOR` honoured |
| 9 | Narrow 80-column TTY | PASS | menu screens fit; only doctor report prose and the perf feature table are wider (wrapped by the terminal, readable) |
| 10 | Non-root user | PASS | "Please run as root", no traceback / unbound variable |
| 11 | Two admins at once (lock) | PASS | second admin sees "another admin is editing: root (pid …, /dev/pts/0) since …" and is refused; lock free once the first leaves |
| 12 | Safe-apply rollback when not confirmed | PASS | `lb add-ip --confirm-within 40s`, not confirmed → `rollback-1 <- current`, "rolledback (not confirmed in time)", tunnel active, links up; also the health gate rolled back an apply whose peer was down |
| 13 | Confirm a change from the menu | PASS (fixed) | first run: the menu only printed `hs2 apply --confirm`; now "Keep the change? y" in place, waits for the health check → `confirmed` |
| 14 | M4 add-ip of the only IP on a dialer without bind_local_ip (via the menu) | PASS (fixed) | `bind_local_ip` 5.160.110.235, weight 100, health gate passed, confirmed, `lb show` 8/8 links |
| 15 | Upgrade from the old release (rel34 binary + old menu, tunnels running) | PASS | old binary ran both configs; new `install.sh upgrade`: both tunnels restarted, "Tunnel is UP — the other server answered" ×2 |
| 16 | Old menu still reachable after the upgrade | PASS | bare `hs2-menu` → new menu; `c` → classic; `hs2-menu --classic` → classic |
| 17 | Classic ↔ new from inside the menus (`c` / `n`) | PASS | |
| 18 | Uninstall / cleanup from the new menu (8 → 7) | PASS (fixed) | all units, tun interfaces, binary and hs2-menu removed; first run left the wizard's self-signed key in /etc/hs2/certs — now removed with its tunnel |
| 19 | Non-interactive join on the listening side without a domain | PASS (fixed) | `hs2 tunnel join CODE --yes` used cert `/etc/letsencrypt/live//fullchain.pem`; now self-signed |

Cosmetic fixes found on the way: wizard/pre-flight "Reality — coming (M5)"
(M5 is done: now points to Security & camouflage → Front of an IP); the double
default "[y/N] [n]".

**Decision:** ready — every item passes with the fixed build, so a bare
`hs2-menu` opens the new menu; the classic one stays reachable (`c`,
`--classic`, `HS2_MENU=classic`, `default_menu=classic`).

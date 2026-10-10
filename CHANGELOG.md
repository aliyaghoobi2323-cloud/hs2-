# Changelog — installer & binary hardening

This log covers a hardening pass done in ordered phases. It has three tracks:

- **Track A — installer robustness** (`install.sh`, bash only, **no binary
  change**): safe to roll out to a live server because the shared binary is
  untouched.
- **Track B — binary features** (Go; the binary is rebuilt, so both ends of a
  tunnel should run the new build).
- **Phase C — active-probe resistance** (the TLS front door and the cover page).

Where each change came from: a line-by-line audit of `install.sh` (findings
numbered **#1–#23**), a set of UX findings (**U1–U14**), and a field **probe
report** on how the tunnel port answered unauthorized connections. Every phase
was covered by tests (Go unit/integration, bash unit + pty, the release-file
check) **and** an independent adversarial review whose confirmed findings were
fixed in a follow-up commit before the phase shipped.

The published binary is static (`CGO_ENABLED=0`) and carries a build stamp
visible in `hs2 version`; see `hs2-src/BUILD.md`.

---

## Track A — installer robustness (install.sh; no binary change)

### A1 — binary verification & upgrade safety
- Fail-closed sha256 verification of the downloaded binary (re-fetch past the
  CDN once, then refuse on mismatch or a missing hash).
- The installer copies itself to `hs2-menu` and publishes `install.sh.sha256`.
- **U1:** when a good binary is already installed, setting up another tunnel
  defaults to **keeping** it (the shared binary is not silently swapped under
  running tunnels); only Upgrade replaces it deliberately.
- **U2:** per-tunnel upgrade with an explicit restart-list confirmation.

### A2 — input validation & transactional config writes
- **#3/#4:** validate MTU and the ipx protocol number.
- **#16/#17:** base-10 parsing (no accidental octal) and JSON-escaping of the
  domain/panel values that land in the config and the `hs2://` link.
- **#10:** read role keys through the binary instead of a fragile grep.
- **U5:** optional subnet prompt. **U6:** warn on bilateral-field edits and
  offer a relink.
- **A2f:** every config write is transactional — written to a temp file,
  validated with `hs2 check`, then atomically moved into place.

### A3 — real liveness & a correct CLI surface
- **U11:** the tunnel list shows real peer reachability (a cached ping), not
  just "service running".
- **#5/U12:** `verify_tunnel` multi-pings and **warns rather than bailing** when
  a tunnel is slow to come up.
- **#9:** the CLI dispatch handles `status`/`uninstall`/`version`/`cleanup` and
  rejects unknown commands cleanly.
- **U13:** a "peer gone" label when a previously-reachable peer stops answering.

### A4 — crash-safety of restore/upgrade/tuning
- **#7:** restore never crashes on an old backup. **#8:** restore guards the
  binary clean-break. **#22:** re-setup stashes and restores the live config,
  scoped to its own run.
- **#6:** tuning detects an old binary (capture-then-grep, so pipefail cannot
  mis-fire the guard). **#18:** manual tuning rolls back if the tunnel does not
  come up.
- **U14:** "uninstall all" needs a typed strong confirmation.
- **U4 (rekey + rebuild the link): NOT implemented — deferred.** An earlier note
  wrongly recorded this as done; a real-server review confirmed there is no rekey
  code. It is blocked on the config not persisting the listener's public endpoint
  + domain, without which a regenerated link could be wrong and break a live
  tunnel. Left for a dedicated change.

### A5 — robustness polish
- **#12:** literal (`grep -Fx`) matching for IPs/subnets (dots are not
  wildcards). **#15:** the systemd `StartLimit` directive adapts to systemd
  older than 230. **#19:** the live monitor cannot busy-loop with no tty.
- **#20:** backup pruning is safe on bash < 4.4 (empty-array guard).
- **#21:** DNS resolution reads only answer IPs, not the resolver's address.
- **U7:** link-pool (min/max/per-link) editing in the Tuning screen.
- **U8:** the dgtun fixed-1280 MTU is explained. **U9:** user-port default IP is
  sane on multi-IP vs single-IP servers.

---

## Track B — binary features (Go; binary rebuilt)

### B1 — build stamp in `hs2 version` (U3) + a build-safety fix
- `hs2 version` now prints a build stamp (`… [build <rev> <date>]`) read from
  the binary's embedded VCS metadata, and the installer menu shows the installed
  binary's stamp — so an operator can confirm **both ends run the same build**.
  Metadata only: a stamped binary stays wire-compatible with an unstamped peer.
- **Build-safety fix found while doing this:** `BUILD.md` did not pin
  `CGO_ENABLED=0`, yet the published binary is static. A naive rebuild on a host
  with a C compiler would silently produce a dynamically-linked binary that
  fails on old-glibc/musl servers (`GLIBC_2.xx not found`). `BUILD.md` now pins
  `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`.

### B3 — `hs2 doctor`, an on-box health check
- `hs2 doctor -c <config>` prints a read-only PASS/WARN/FAIL checklist and exits
  non-zero only on a hard failure (same contract as `hs2 check`): config
  validity, whether the tunnel is running, **endpoint reachability** (the common
  "edge can't reach exit" failure), certificate expiry, the TUN device's real
  state, kernel-tuning-vs-`/proc/sys`, and a clock reminder (link auth is
  minute-bound). Also wired into the tunnel manager as a **Diagnose** action.
  The review caught and fixed a real issue: the tuning check must not run
  `modprobe`, so it is now fully read-only and needs no root.

### B4 — fix a stale obfuscation comment (#23)
- `encap/obfs.go`'s prose described an old 12-bit/4-bit split of the ICMP echo
  sequence; the implementation (and its tests) are 8/8. Corrected the comments.
  Documentation only — proven byte-identical build, so **no binary respin**.

### B2 — multiple IPs per tunnel (U10)
- Proposed as a larger design phase; **deferred at the maintainer's request**
  (not implemented).

---

## Phase C — active-probe resistance (TLS front door + cover page)

**From a field probe report:** the tunnel port answered differently depending on
the probe — a completed TLS handshake got an nginx-looking web page, but a plain
HTTP request (or random bytes) was closed silently with zero bytes. A real
HTTPS server does not behave that way, and the page claimed `Server: nginx`
while the TLS stack is Go's — a contradiction a prober can detect.

The fix makes every unauthenticated probe see **one coherent identity: an
ordinary Go HTTPS service** (the kind Caddy/Traefik are), instead of a
contradictory hybrid.

- A plain-HTTP request on the TLS port now gets the **exact** response Go's own
  HTTPS server sends (`HTTP/1.0 400 … Client sent an HTTP request to an HTTPS
  server.`), byte-pinned to the standard library by a test — not a silent close.
- Only the five request starts Go itself recognizes are answered that way;
  everything else (a real ClientHello, or garbage) flows into the TLS layer
  unchanged and behaves exactly like a real Go TLS server.
- The cover page is no longer the nginx default welcome page (itself a honeypot
  signature). It is a plain, self-contained generic site (fictional brand, no
  external requests, served via `http.ServeContent` with real static-server
  headers) and the `Server: nginx` header is gone — the Go TLS stack no longer
  contradicts a claimed server it cannot imitate.
- Listener-side only, no wire change → **drop-in and backward compatible**: an
  old-binary client still authenticates unchanged.

**Verified end-to-end** by running the new binary as a live TLS listener and
replaying every probe from the report, comparing against the previous binary:

| probe | before | after |
|-------|--------|-------|
| plain HTTP `GET /` (no TLS) | 0 bytes, silent close | Go-native `400`, 76 bytes |
| TLS + `GET /` | `Server: nginx`, "Welcome to nginx!" | no `Server` header, generic page |
| TLS, no/ wrong SNI | full handshake, cert | unchanged (consistent) |
| random bytes / unknown HTTP method | close | close (matches a real Go server) |

**Deliberately not done:** the TLS-stack fingerprint stays Go's — TLS must be
terminated in Go for the channel-bound auth, and "a Go HTTPS server" is an
ordinary, common identity once nothing contradicts it; closing that gap fully
would need a Reality-style redesign or a real-nginx dependency, neither of which
fits the single-static-binary design. A per-connection probe-flood cap was also
deferred (the half-open hold is unchanged from before, so nothing got cheaper
to flood).

---

## Post-review fixes (independent real-server review)

An independent review ran the new binary on a live server. Most claims held;
these real issues were found and fixed:

- **🔴 Regression fixed — TCP_INFO went dark on the TLS-server side.** Phase C's
  `prefixConn` stays wrapped around the connection for its whole life, and
  `Carrier.TCPConn()` unwrapped only one level (`tls.Conn` → expected
  `*net.TCPConn`), so it hit the `prefixConn` and returned nil — silently zeroing
  kernel loss/rwnd that the autopilot uses (loss-based soft-degrade never fired;
  slow-receiver vs capped-path could not be told apart) on the server side
  (Iran in reverse, Kharej in direct). Fix: `prefixConn` now exposes `NetConn()`
  and `TCPConn()` unwraps the whole chain; a regression test covers it.
- **🟡 Phase C close is now a clean FIN.** The plain-HTTP 400 path left the rest
  of the request unread, so the socket closed with an RST; a real Go server FINs.
  It now drains the (buffered, tiny) request before closing.
- **🟠 Invisible prompts fixed.** Two prompts (`install_binary` keep/update and
  `upgrade` "Proceed?") were written with `read -rp "…" 2>/dev/null`, and the
  `2>/dev/null` (there to skip silently with no tty) also hid the prompt text,
  leaving the operator at a blank cursor. The prompt is now printed separately so
  it is visible with a tty and still skipped without one.
- **🟡 Installer polish.** A bad MTU now re-asks instead of aborting the whole
  wizard; a multi-word bilateral field ("tunnel subnet") renders as one bullet,
  not two; the config-edit temp (a key-bearing copy) is kept out of `/tmp` —
  beside the config in `/etc/hs2` (root-only 600), removed on return, and swept
  at startup if an interrupt left one.

### Known limitations / follow-ups

- **The default cover page is identical on every install** ("Oakline", one fixed
  sha), so the page itself is a shared signature a censor could hash. For the
  best cover, set `backend_addr` in the config to your own real site; a
  per-install default page is a possible future change.
- **Backups contain the certificate private key** (`privkey.pem`); they are
  root-only (600) and the newest 10 are kept (`HS2_KEEP_BACKUPS`). Treat a backup
  as secret.
- Smaller: `hs2 doctor` does not detect a cert-renewal method that needs port 80
  (standalone authenticator); the link-pool screen (U7) does not note that the
  other side has its own cap; the config editor is always nano.

## Phase E — fixes from the second real-server review

The same independent reviewer re-tested the post-review build on a live server
(and ran a full direct-mode tunnel: TLS 1.3, matching keys, no plaintext on the
wire, hot cert reload, recovery after a killed link — all sound). It confirmed
the TCP_INFO fix with strace on both sides, and found two real problems with the
post-review fixes themselves:

- **🔴 The tunnel-manager menu died after every config edit** (`tmp: unbound
  variable` on Back). The edit temp's cleanup used `trap … RETURN`; a RETURN
  trap set in a function stays installed after the function returns and fires
  again when the *caller* returns — where `$tmp` no longer exists — and
  `set -u` killed the installer. (Our earlier claim, and the reviewer's, that it
  did not leak was wrong: the test that "proved" it never returned from the
  menu.) Fix: no RETURN trap at all. Every return path already removed the temp
  explicitly; an abnormal exit (Ctrl+C, SIGTERM, a `set -e` abort) is now
  covered by the existing `on_exit` EXIT handler via `HS2_EDIT_TMP`, and a hard
  kill by the startup sweep. The tunnel itself was never affected.
- **🟡 Probes other than the five plain-HTTP starts still closed with an RST**
  (DELETE, an HTTP/2 preface, random bytes) where a real Go HTTPS server sends a
  FIN. Cause: the 5-byte pre-TLS peek read *less* of the socket than crypto/tls
  does in its first read, leaving the rest of a small probe unread at close.
  Fix: the peek and its `prefixConn` wrapper are gone. `tls.Server` reads the
  socket itself, and a plain-HTTP request is recognised exactly the way
  net/http recognises it — from `tls.RecordHeaderError` — and answered with the
  same bytes and the same close. Read sizes, and therefore FIN vs RST, are now
  Go's own by construction rather than an imitation. (This supersedes the
  post-review "drain before close" patch, which only covered the 400 path.)
  Listener-side only, no wire change.

Not changed in this phase: the default cover page is still the same on every
install (see Known limitations above — set `backend_addr` to your own site).

New tests: `hs2-src/install/tests/tm_edit_test.sh` drives the real
`tm_tunnel_menu` through a pty under the installer's real `set -euo pipefail`
(Edit → Back, a saved edit, two edits, SIGTERM while the editor is open, a
`set -e` abort mid-apply) and forbids `trap … RETURN` in install.sh; it fails on
the previous install.sh. `TestProbeCloseMatchesStdlib` sends ten probes to both
our listener and a real net/http HTTPS server and requires the same bytes and
the same FIN-vs-RST for each; it failed on the previous server.

## Phase F — certificate renewal, editor choice, link-pool clarity

The follow-ups left open after phase E (all but U4 rekey, which was dropped at
the maintainer's request as too complex for the people who use this).

### F1 — certificate renewal that cannot fail silently
- **Found while preparing this phase:** a certificate issued with the installer's
  **DNS-01** option never renewed. certbot ran `--manual` with no auth hook, and
  certbot cannot renew such a certificate unattended (someone must publish a new
  TXT record each time), yet the installer said "Renewal set: 30 days before
  expiry". Such certificates expired after 90 days. Because the client never
  verifies the server certificate (auth is the shared key), **the tunnel kept
  working** — so nobody noticed, while every probe saw an expired certificate.
- The DNS-01 option now says up front that it is renewed by hand, and setup ends
  with an honest warning and the expiry date instead of "Renewal set".
- The tunnel screen's **Certificate** line (shown whenever the tunnel is opened,
  running or not) states expiry **and** how it renews; it flags DNS-01, an
  overdue renewal, a busy HTTP-01 port, a missing renewal config or
  `autorenew = False`, and an expired certificate. It replaces a line that
  claimed "auto-renews" for every certificate.
- New per-tunnel action **c) Certificate**: test the automatic renewal (a dry
  run against Let's Encrypt's test server — changes nothing), renew now, or for
  DNS-01 renew by hand on the same lineage with every name on the certificate
  (wildcards included). It refuses, and says why, when the HTTP-01 port is taken
  and no `pre_hook` frees it.
- `hs2 doctor` gains a read-only **cert renewal** check: it reads certbot's
  renewal config for the lineage, the kernel's listening sockets and the certbot
  timer, and reports DNS-01-by-hand, a busy HTTP-01 port (respecting
  `http01_port` and `pre_hook`), an overdue renewal, a disabled or missing
  renewal, no active timer, or an own certificate. It never runs certbot. An
  expired certificate still gets this diagnosis (why it was not renewed).

### F2 — the link-pool screen says which server's values count (U7)
- Verified against the engine: the Iran side always decides the link count. In
  direct mode the Kharej side only accepts links, so its values do nothing and
  the screen now says so and writes nothing. In reverse the Kharej side dials
  and caps Iran's target at its own min/max, so the effective ceiling is the
  lower max — the screen says to raise it on the Kharej server too. The `tls`
  mode is always one link and single-session transports have no pool; for those
  the screen explains and writes nothing.

### F3 — your own editor, without leaking the key
- Edit config uses `SUDO_EDITOR`, `VISUAL` or `EDITOR` (that order, as sudoedit)
  when it names a terminal editor installed here — a simple command line such as
  `vim -u NONE` is fine; desktop editors and missing ones are skipped with a
  note — else nano as before. Editor-specific hints (nano / vi keys).
- The copy being edited holds the tunnel key, and editors write more copies
  beside it (vim's `.swp`, emacs's `~` backup and `#autosave#`). Measured: with
  such an editor the previous version left **3 key-bearing files** next to the
  config; now the edit happens in a private directory (700) that is removed
  with everything in it on every exit path (normal return, Ctrl+C, SIGTERM,
  a dropped SSH session, a `set -e` abort; the startup sweep covers a hard kill).
- An editor that exits with an error (vim's `:cq`) after a change no longer
  applies it silently: the operator is asked first.

### F4 — stale tests brought in line with the code
- `helpers_test.sh` still expected the pre-A1 "warn and install anyway" when no
  hash is published; it now asserts the fail-closed contract (exact status 0 /
  1 / 2). `migrate_test.sh` extracts the new certificate helpers it now needs.

New tests: `cert_test.sh` (real certificates in a fake certbot tree; the menu
action driven on a pty with a recording certbot stub), `tm_tune_links_test.sh`
(every carrier × role × direction), the editor cases in `tm_edit_test.sh`
(including a **real vim** driven through the pty: `:wq`, `:cq`, and kill -9
mid-edit with the session dropped), and `doctor_cert_test.go`.

### Review fixes (independent adversarial review of phase F)
The review reproduced every finding through the real menu on a pty (and checked
certbot's behaviour against a real certbot 5.8). Fixed:
- **Pre-hooks certbot runs from anywhere count.** A port-80 holder freed by an
  executable in `renewal-hooks/pre/` or a `pre-hook` in `cli.ini` (not only the
  lineage's `pre_hook`) is no longer reported as "renewal will fail" — by the
  tunnel screen or by `hs2 doctor`. And when the port is busy with no visible
  hook, **c) Certificate** still runs the dry run (the real test — a hook we
  cannot see may free the port); a real "Renew now" asks first, default No.
- **The startup sweep no longer deletes another session's live edit.** The edit
  directory is named `.hs2-edit.<pid>.<random>`; one whose installer is still
  running (a second SSH window) is left alone, and swept once it is gone.
- `hs2 status` and the daemon log no longer say "certbot renews automatically"
  for an expiring certificate (a DNS-01 one never does); they point at
  `hs2 doctor`.
- The dry run is described honestly: it does not replace the certificate, but
  certbot's pre/post hooks do run.
- With no explicit `renew_before_expiry`, the renewal threshold follows
  certbot's own rule — a third of the certificate's lifetime (half under 10
  days) — instead of a fixed 30 days that called short-lived certificates
  "overdue" while certbot was correctly waiting.
- The installer and `hs2 doctor` read odd `cert_file` paths (`//`, `/./`)
  identically, and neither resolves `..` into another lineage.
- The editor shown in the menu is the one that opens (a leading space in
  `EDITOR` no longer shows "nano" while vim opens).
- `hs2 doctor` recognises a certbot scheduled from a crontab (a pip/venv
  install) instead of reporting "nothing will run the renewal".
- The `EDITOR='*'` test now really catches glob expansion (it runs where `*`
  would match an installed editor).

### Field request — the reverse exit's log says why a link went away
On the Kharej side in reverse, a link killed by the network (`ss -K`, a reset)
was logged exactly like the autopilot shrinking the pattern: `exit slot N
retired — pattern shrinking`. The behaviour was right (above the target, a slot
whose link ends is not redialed); the words conflated two different events and
dropped the cause. The link's end reason (the first socket error, in the same
words the Iran side uses) now reaches the pool, and the log says which it was:
- `exit slot 6 retired — closed by the edge while above its target (pattern
  shrinking) (now 7)` — the edge closed the link cleanly: normally the autopilot
  retiring an idle link. (Worded no more strongly than that on purpose: Go's TLS
  cannot tell the edge's close_notify from a bare FIN — e.g. a crashed edge
  process — so the log does not claim a decision it cannot see.)
- `exit slot 6 lost (read: reset by the network or the other server) — not
  redialed, pool above target (now 7)` — a loss (an RST such as `ss -K`).
- `… lost (no data from the edge for 24s (keepalive timeout — path stalled)) …`
  — a stalled path, instead of the misleading "closed locally".
- `… (read: aborted on this server (socket killed, e.g. ss -K or a local
  firewall)) …` — when the socket is killed on the exit itself (Go's raw
  "software caused connection abort", in the same plain words as the rest).
- `exit link down (slot 6: <reason>; now 7); redial` — at target, with the cause.
Log text only — no change to when links are retired or redialed. Proven over the
real TLS + smux stack: a clean close, a bare FIN, a genuine RST and a silent
(stalled) edge each produce their own reason, and the two reverse-shrink
integration tests now count retirements under the new wording.

A second, focused review of these changes (pool behaviour confirmed unchanged,
race-clean) found only wording/diagnostic gaps, all fixed: the clean-close
wording above and the keepalive-stall reason; hooks listed exactly as certbot
lists them (dotfiles count, `~` backups do not, `directory_hooks = False` /
`no-directory-hooks` honoured); a cron line counts only if it really runs
`certbot … renew` (not a `MAILTO=`, a check script, `certbot certificates` or a
`--dry-run`), only from cron.d names cron reads, and only with a cron daemon
running; the startup sweep also clears a leftover whose PID was reused by a
newer process (e.g. after a reboot); and the installer and `hs2 doctor` parse
cert paths identically (26-path differential test).

Not done: a per-install cover page (requested again from the field test). The
default cover page is still the same on every install — see Known limitations.

Known, not changed here: `tun_ports_test.py` fails the same 11 checks on `main`
and on this branch — it still expects the wizard's pre-U5 question order; it
needs its expectations updated, separately.

## Phase G — a per-install cover page (no user action)

**From the field test:** the built-in cover ("Oakline") was byte-for-byte
identical on every install and its text is in the public source, so one known
sha256 found every hs2 server in a bulk active scan. Pointing `backend_addr` at
a real site fixes it, but most users never do, so the default is what the whole
fleet shows.

- The installer now writes a per-install `cover_seed` (128-bit, from
  `openssl rand`, **independent of the shared key** so the public page can leak
  nothing about the secret) into every builtin-cover tunnel it sets up, and
  `migrate_config` adds one to existing builtin tunnels on the next upgrade — no
  user action. A custom `backend_addr` keeps priority and ignores the seed.
- `hs2-src/cmd/hs2/cover.go` turns the seed into a page whose brand, hero copy,
  accent colour (hue + saturation + lightness), favicon glyph, layout tokens
  (radius, widths, gap, class names), service cards, long-section set and order,
  size, `Last-Modified` offset and `ETag` all vary. No two installs share a
  hash, and no single surface value clusters (measured over 2000 pages: 2000
  unique hashes, ~1000 distinct brands, 359 hues, top hue 0.6%), so the ensemble
  is not obviously generated either.
- **Stability contract:** the bytes are a pure function of (seed, year) via a
  SHA-256 counter stream (byte-stable across Go versions, unlike math/rand), so
  a server shows the same page across restarts and binary rebuilds; only the
  copyright year ticks, for everyone at once. A golden test pins the exact bytes
  for a known seed, so the generator can never change silently (which would move
  every server's page on upgrade day — a correlated fleet event).
- **No seed → the exact legacy page**, byte-for-byte (an old or hand-written
  config is undisturbed; verified it still hashes to the field-observed
  `86dfcf86…`).
- **Honest scope (and the README says so):** this defeats cheap hash/structural
  enumeration; it is NOT a disguise against a determined prober — the TLS stack
  is still Go's, and a classifier trained on several generated pages could still
  recognise the family. A real `backend_addr` is still the strongest cover.

### Field-review fixes (real-server test of the cover page)
A real-server test confirmed the whole checklist, and found two over-claims and
a quality note, all fixed:
- **ETag was a bare `sha256(body)[:16]`** — a *single-probe* fingerprint: anyone
  could fetch the page, hash it, and confirm the server used hs2's exact rule.
  It is now SALTED with the seed (stable per seed+content, so caching still
  works, but unpredictable from the body, like a real server's mtime/inode
  ETag). The seedless legacy page sets **no** ETag at all, exactly as the
  pre-per-install binary did.
- **The neutral palette was constant** — only the accent varied; the text, grey
  and dark-background hexes were the fixed Oakline values on every page. They
  are now jittered per seed within a contrast-safe band, so no base colour is a
  shared constant either. (The earlier "nothing constant remains" was too
  strong; the honest claim is: no byte-exact token is shared, a trained
  classifier still could recognise the genre.)
- **migrate_config** now edits the config on a snapshot and keeps the result
  only if the binary still validates it (rolling back otherwise), instead of an
  unchecked in-place `sed` — matching the "check, then replace" path used
  elsewhere.

New tests: `cmd/hs2/cover_test.go` (determinism + golden, 500-seed uniqueness,
seedless==legacy with the `86dfcf86…` check, structural invariants: no external
requests, no JS, inline favicon, light+dark, balanced tags, size varies; served
headers: Content-Type, ETag, 304, root-only 404) and
`install/tests/cover_seed_test.sh` (every builtin heredoc seeded, seed
independent of the key, migration adds one / leaves a custom backend alone /
never duplicates, valid JSON). Verified live over real TLS: two seeds serve two
different pages, a seedless config serves the exact legacy page.

## Phase H — a link-pool ceiling sized to the server, shown exactly on both servers

The pool's ceiling (`max_links`) was a flat 32. At 300–400 *active*
connections the autopilot wants ~50 links (one per 8 active), so 32 was the
binding limit; a flat 64 everywhere, though, would let a 1 GB VPS buffer up to
~512 MiB (8 MiB per link under a stalled reader). The ceiling now follows the
hardware, and both servers show the number that actually applies.

### H1 — auto ceiling (`max_links: 0`), re-derived at every start
- `max_links` is three-state: **`0` = auto** — from the server's RAM/cores at
  every start, using the same low/medium/high profile that sizes the kernel
  buffers (**32 / 48 / 64**, `tune.RecommendedMaxLinks`); a **number** = fixed,
  never changed automatically; **absent** = the historical fixed **32**, so a
  config written before this behaves exactly as before after the upgrade.
- A server resized up or down gets the matching ceiling on its next start (a
  VPS resize needs a reboot anyway), with nobody editing the config. An older
  binary reads `0` as its fixed 32, so a rollback still runs.
- New installs write `0`. Existing installs are never changed (the migration
  still writes a literal 32 for the two pre-adaptive lines only).
- **One behaviour change to know:** an explicit `"max_links": 0` used to mean
  "the default, 32"; it now means auto. No installer ever wrote 0 (it wrote 16
  or 32, and the menu accepted 1–1024), so only a hand-edited config, or an
  older `hs2 config set max_links 0`, is affected — on a medium/high box it now
  gets 48/64. Set a number to keep it fixed, or remove the key for the old 32.
- When a tunnel is added on a server that keeps an older hs2 binary, the new
  config still says auto, and the installer says plainly that the old binary
  runs it as a fixed 32 until hs2 is upgraded (it never shows the old binary's
  "unknown command" as hardware information).
- Startup log line: `link pool: ceiling N links — auto …` / `fixed …` /
  `the default …`. `hs2 recommend-links [--why]` prints what this server's
  hardware gives; `hs2 config set max_links auto` sets auto.

### H2 — both servers learn the other's ceiling (display only)
- **Stream (TLS) tunnels:** a one-shot `kindInfo` stream per link, opened by the
  Iran (edge) side in both directions: `[ver][n][max u16]` each way, 5 s
  deadline, a few bounded retries on a congested link. An older exit closes the
  unknown kind and the edge stops at once; an older edge never opens it.
- **Datagram (dgtun) tunnels:** two trailing bytes on the control frames that
  already flow — the exit's ceiling on `TypeLinkStats`, the edge's on
  `TypePoolCtl` (now also sent by a direct edge, every ~15 s ± 20 %; a direct
  exit has always ignored its target). **No new frame type**, so the datagram
  carrier, FEC, pacing and drop paths are untouched; older parsers read exactly
  the bytes they always read. A report older than 45 s reads as unknown.
- None of it feeds the autopilot or the pool. Each server reads the other's
  number only from links that are up now, so a value from links that are gone
  (e.g. after the other server was rolled back to an older release) is never
  shown; a ceiling lifted by a higher `min_links` is shown as exactly that.

### H3 — the exact effective ceiling, on both servers, in both directions
- Direct: the Iran server's ceiling alone (the Kharej exit accepts every link
  it dials — its `max_links` is not applied). Reverse: the lower of the two
  (the exit clamps the edge's target to its own max).
- `hs2 status` prints a `ceiling:` line on **either** server: the effective
  number, which side sets it, and both servers' own ceilings (auto/fixed/
  default, with the profile and hardware); unknown is said plainly (no link up,
  or an older hs2 on the other side).
- `hs2 doctor`: WARN when a fixed (or default) ceiling is above what the RAM
  comfortably holds, INFO when the box could use more; for auto, WARN/INFO
  when the hardware changed under a running daemon (restart applies it); a
  direct Kharej gets only the fact that its value does not apply.
- Installer → tunnel → Tuning → **Link pool**: shows `auto, now N`, accepts
  `auto`, shows the hardware's number, and prints the daemon's exact ceiling
  line (on every role, including a direct Kharej whose own values do nothing).
  Min/max are written in an order the binary's per-step validation accepts.

### Measured (real binary, real TLS on loopback, 400 connections each moving data)
| scenario | Iran `max_links` | Kharej `max_links` | links reached | both servers show |
|---|---|---|---|---|
| direct | auto (64) | 48 | **50** | 64, set by Iran (Kharej's 48 not applied) |
| direct | 32 | auto (64) | **32** | 32, set by Iran |
| reverse | auto (64) | 48 | **48** (target 50) | 48, limited by Kharej |
| reverse | 40 | auto (64) | **40** | 40, limited by Iran |
| reverse | absent | absent | **32** | 32, both — the default |
| reverse, older Kharej | auto (64) | 48 (old binary) | 48 | Iran: "does not report — older hs2" |
| reverse, older Iran | 32 (old binary) | auto (64) | 32 | Kharej: "does not report — older hs2" |

Peak RSS per process in these runs was 55–67 MiB at 32–50 links with 400
connections (no stalled reader, so the per-link 8 MiB buffer cap was never
approached). Mixed versions carried traffic normally in both directions. The
datagram path was run the same way over real UDP and TUN devices (direct,
reverse, `max_links` absent, and an older binary on either side): every case
showed the exact number on both servers, or said plainly that the older side
does not report it.

### Field-test fixes (real servers, 400 active connections)
A real-server test confirmed every claim in both directions (auto 48 on the
field servers' 1.9 GB "medium" boxes, a fixed 40 on Kharej limiting reverse to
exactly 40 TCP links, direct ignoring Kharej's cap, old configs staying at 32,
mixed versions working). One display point was fixed:
- **Reverse: the Iran side's target did not say it was capped.** With the
  Kharej exit at 40, Iran still printed `target 48` and logged
  `pattern 28 → 48 links`, while 40 links ran. The links line now reads
  `40 up / target 48, capped at 40 by the Kharej server`, and the log line adds
  `— capped at 40 by the Kharej server (its max_links), so at most 40 links
  run` (stream and dgtun). Display only — the target itself is unchanged; a
  direct edge never claims a cap (a direct exit does not clamp), and an older
  Kharej that does not report its ceiling is never assumed to cap.
- `gofmt` alignment in two files touched by this phase.
- Not field-tested (no 1 GB server at hand): a 1 GB box getting 32. It follows
  from the profile thresholds and is unit-tested (512 MB and 1 GB → low → 32);
  on any 1 GB VPS `hs2 recommend-links --why` must print `32 (low profile …)`.

### Test maintenance
- `install/tests/tun_ports_test.py` had gone stale: it predated the optional
  tunnel-subnet prompt (U5) and the `hs2 check` validation of every written
  config (A2f), so 10 installer branches stalled (on `main` too). It now
  answers the subnet prompt on the link-making side and gives the installer a
  real `hs2` binary to validate with; all installer branches pass again, and
  with root + iproute2 + ping its namespace part passes too (137 PASS: real
  traffic, direct and reverse, l3mtcp / tls / dgtun over udp, ipx and gre,
  plus the blocked-GRE case).
- `install/tests/multi_tunnel_test.py` had gone stale the same way: it never
  answered the subnet prompt (U5), the upgrade restart confirmation (U2) or
  the typed `REMOVE ALL` that now guards a whole-server uninstall (U14). Its
  pty now answers the optional subnet prompt wherever it appears, and the
  upgrade/uninstall steps answer their confirmations; 54 PASS, 0 failures
  (several tunnels side by side on two namespace "servers", upgrade,
  backup/restore, delete, uninstall).

## Phase I — each Iran user port to its own panel inbound (per-port targets)

The Iran server could open several user ports (`forward_ports: "8443,2053"`),
but the Kharej server delivered every connection to its ONE panel address
(`expose`): a panel with one inbound per port could only be served by one
tunnel per port. There was also no menu to add or remove a user port after
setup, and neither setup question said how several ports are handled.

### I1 — the routing table lives on the Kharej server
- New Kharej key **`port_map`**: comma-separated `P` (= `127.0.0.1:P`, the same
  port on the Kharej server — the default) or `P=host:port`. A user port with
  an entry goes there; every other port goes to **`expose`** (the default
  panel); with no `expose`, a port without an entry is **refused**, and the
  Kharej server logs which port (once a minute per port) and shows it in
  `hs2 status` / `hs2 doctor`.
- On every user connection the Iran server says only **which of its user
  ports** the user came in on — a number, never an address — so a compromised
  Iran server can reach only what the Kharej operator listed (never
  `127.0.0.1:22`, a database or the panel's admin port).
- A config without `port_map` behaves exactly as before.

### I2 — on the wire, with every older/newer combination working
- **Who decides:** once the Iran server knows the Kharej server routes by
  port, it says the port on every TCP connection and stream (the Kharej server
  then decides with the table it has *now* — a port mapped there a moment ago
  included); the cost is 2–3 bytes once per connection. Only the datagram tun's
  UDP, where the bytes would ride on every datagram, is tagged just when the
  Kharej server has a table at all (a `port_map`, or no default): a Kharej
  server without one gets those datagrams byte for byte as before.
- **Stream tunnels (mtcp, l3mtcp, tls):** two new stream kinds,
  `kindTCPPort`/`kindUDPPort` = `[kind][port u16]`. The per-link `kindInfo`
  exchange (Phase H) is now **v2** — `maxLinks, caps, flags, count, ports…` —
  so each side learns whether the other routes by port, which ports the Iran
  server opens (and whether it forwards UDP) and which ports the Kharej server
  has its own target for (and whether it has a default). A v1 reader takes the
  first two bytes as always. A new link takes user connections once its first
  exchange attempt is over (one round trip): answered → that answer decides;
  closed by an older exit → never tagged; no answer in time (5 s) → the link
  goes by what the pool's other links learned from the exit while it keeps
  asking (after its three quick tries, once a minute) — a slow answer never
  leaves a link without one for its whole life. What the pool learned is
  forgotten when no link is left (the exit may have restarted).
- **Datagram tun (dgtun, every encap):** untagged traffic stays on tun port
  28443 → the default panel, exactly as before. Tagged traffic goes to the new
  tun port **28444**: such a TCP connection and each such UDP datagram start
  with `[1][port u16]`. The edge **probes** 28444 (port 0 = probe; the same v2
  info message both ways) at start, every 15 s, faster while unsure — also with
  no user port, so the Kharej server learns that too. Refused (an older exit)
  or answered by something that is not an hs2 exit → untagged; silent while
  28443 answers, asked twice, and again on the next probe → *filtered* (a
  firewall on the Kharej server's tun) → untagged, and both servers say so (a
  single silence after a good answer is taken for a tun flap); an answer that
  is only late changes nothing. Until the first probe answers nothing is
  tagged: a connection waits for it while the tun is not up yet (an untagged
  dial would wait just the same; at most 10 s), but goes untagged at once when
  the tun answers without a verdict. A refused tagged TCP connection falls back
  to 28443 *before any user byte is sent*; a UDP flow is redone the right way
  when what is known changes, and a refused UDP flow only asks the probe (it
  never turns per-port routing off for the whole edge). The exit now always offers UDP on
  both tun ports, so turning UDP on needs only the Iran server (an older Kharej
  build still needs `"udp": true`). Tagged UDP flows are keyed by the edge's
  source address and replaced, not dropped, when that address is reused for
  another port; targets are resolved once, not per flow.
- **Compatibility, all verified live:** newer Iran + older Kharej → every port
  reaches its one panel (the Iran side's `hs2 status` says the Kharej server
  runs an older hs2); older Iran + newer Kharej → it does not say the port, so
  every connection reaches the default panel (the Kharej side says so if it
  persists with links up). Direct and reverse alike.
- The throughput / FEC / drop path (udpcarrier, FEC, the datagram pool) is
  **untouched**: dgtun per-port routing lives entirely in the userspace
  forwarder on top of the tun.

### I3 — where each port goes, shown and edited on both servers
- **`hs2 status`** prints a `ports:` table on either server. Iran: each user
  port → its own target on the Kharej server / the Kharej server's default
  panel / **NO target (refused)** with the fix; Kharej: each port the Iran
  server opens → its target, ports with a target that Iran does not open, and
  the default. Unknown is said plainly (tunnel not running, no link yet, or an
  older hs2 on the other side).
- **`hs2 doctor`**: WARN for an Iran user port the Kharej server has no target
  for, for a filtered dgtun tag port, and (on the Kharej server) for a local
  target nothing listens on — TCP listeners and bound UDP sockets (a UDP-only
  inbound counts), read from `/proc/net/{tcp,tcp6,udp,udp6}`; no connection is
  made.
- **`hs2 ports -c cfg [add P[=host:port] | remove P | default host:port|none |
  udp on|off]`**: the table, and this server's half to edit (Iran: user ports
  and UDP; Kharej: own targets and the default; one port per change, and a
  hand-broken `port_map` is never rewritten). Every change is validated like
  `hs2 check` and written atomically. `hs2 config set` also accepts
  `forward_ports`, `port_map`, `expose` and `udp` (normalized).
- **`hs2 check`** validates `port_map` (port, target, duplicates), accepts a
  Kharej server with `port_map` and no `expose` (mtcp/tls still need one of
  the two), warns that `port_map` does nothing on the Iran server or on an IP
  tunnel, and (dgtun) that a target on 28443/28444 would fight the forwarder.

### I4 — installer
- **Kharej setup** says the Iran server opens the ports and this server decides
  where each goes, asks the **default panel**, then *"Iran user ports with
  their OWN inbound here"* (`2053,2083` = the same port on 127.0.0.1;
  `2053=127.0.0.1:2096` for another; Enter = none). A bad entry — including a
  target the binary would refuse, such as `:80` or an unbracketed IPv6 — is
  re-asked at the prompt, never at the end of setup; skipped with a note when
  the installed binary predates it.
- **Iran setup** says, before the user-port question, that each port reaches
  the Kharej server's target for it (its default panel unless it has its own).
- **Tunnel manager → `p) Ports`** on both servers: the live table, then
  Iran — add / remove user ports (checked free on TCP — and UDP when it is on —
  and never the tunnel's own listen port), UDP on/off; Kharej — own targets
  (Enter = the same port on 127.0.0.1, several at once), remove, default panel
  (or none; Enter keeps it without a restart). Each change is applied with the
  usual restart and automatic rollback, and followed by **what — if anything —
  has to change on the other server**. Several ports at once are all-or-nothing:
  one refused entry undoes the others, and says so; a change is not made when
  the backup it would roll back to cannot be written. An older binary or an IP-tunnel
  transport is refused with the reason. The tunnel's details show its own
  targets, and the setup summary points at the screen.

### Review fixes (independent adversarial review, all confirmed and fixed)
- dgtun: one refused UDP flow (Iran UDP on, Kharej UDP off — exactly what the
  Ports screen allowed) flipped per-port routing off for the whole edge, over
  and over: TCP on mapped ports went to the default panel and the log flooded.
  A UDP error now only asks the probe, and the exit always offers UDP.
- stream: a link whose info exchange timed out was used untagged for its
  whole life (in `tls` mode, every connection). It now goes by what the pool's
  other links learned, and keeps asking.
- dgtun: a late probe answer was taken for an older exit; now it changes
  nothing. A tag port that is filtered no longer leaves connections waiting:
  nothing is tagged until a probe answers, and filtered is detected and named.
- dgtun UDP datagrams carry the tag only when the Kharej server has a table,
  so an existing config sends them identically (no 3-byte header on datagrams
  near the tun MTU). A second review pass then found that the Iran side could
  keep an old copy of the Kharej server's table after a Kharej-side Ports
  change; now TCP connections and streams always say their port and the Kharej
  server decides with its current table. Also from that pass: a connection
  made before the tun is up waits for it instead of going to the default
  panel; a tun flap is not taken for a filter; "filtered" never outlives a
  later refusal; a silent tagged port pokes the probe at once; the UDP
  forwarder on 28443 checks its target resolves before it binds; the installer
  loops are safe under `set -u` on bash older than 4.4.
- dgtun exit: a reused source address no longer drops a new flow; targets are
  resolved once. Status: an Iran server with no user ports, a filtered tag
  port and a slow answer are each said as what they are (not "older hs2");
  the refusal log is per port AND protocol. Doctor counts UDP inbounds and
  `localhost` on `::1`. `hs2 ports` refuses a list in one change and never
  rewrites a broken `port_map`. Installer: all-or-nothing multi-port changes
  with a message, no glob expansion of entries, targets validated like the
  binary.

### Test maintenance
- `tm_edit_test.sh` was intermittently red (about 1 run in 5): the edit-dir
  sweep compared two whole-second clocks, so a directory made within a second
  of its owner's start could look older than it and be swept. The sweep now
  allows 2 s before calling a PID reused (a real reuse is hours apart).
- The pty tests (`tun_ports_test.py`, `multi_tunnel_test.py`) answer the new
  Kharej questions; the multi-tunnel pty also answers optional prompts that
  come after the last listed step.

## Phase Q — up to 300 links on a strong server, and what keeps that safe

Production was pinned at the old 64-link ceiling (Iran 20 cores / 17 GB,
Kharej 12 cores / 22 GB, ~500 active of ~6,000 open connections). The auto
ceiling now follows the RAM up to 300; most of this phase is what makes
hundreds of links safe on restart, on outage, under stalled readers and on
small servers. Every change went through an independent adversarial review
(33 findings, measured where possible); the confirmed ones are fixed here.
Both servers should run this build; mixed versions keep working.

### Q1 — the ceiling
- Auto (`max_links: 0`): one link per 48 MB of RAM, at most 300, never below
  the old 32/48/64 profile; one core keeps its profile value, 2–3 cores at
  most 128. Explicit numbers are never rewritten; absent stays 32.
- dgtun uses the same auto ceiling. Its interim cap (64 over a raw
  encapsulation, 128 over udp) was lifted after its load test: 300 gre
  carriers on the shared raw socket carried 176 Mbit/s (28 with one socket per
  carrier, the older build); 300 udp carriers ran with p99 121 ms, no dropped
  connection, ~5% more CPU than 128.
- Sizing reads the cgroup's limits when lower than the host's (containers,
  units with MemoryMax/CPUQuota). Go's soft memory limit is half the RAM.
- `hs2 check` warns about `min_links` above 64. A hand-edited **Kharej**
  config with `min_links` above `max_links` is now an error for `hs2 check` /
  `hs2 config set` (it was silently accepted).

### Q2 — getting to 300 and back without storms
- One dial gate per process: ≤ 8 handshakes in flight, starts 40–160 ms
  apart (~10 links/s; 300 links in ~31 s measured). A dial checks it is still
  wanted before taking a start slot, so retiring slots never hold up the ones
  that must dial (the exit's first link after an outage was ~17 s late; now
  0.2–1.3 s, test-measured). One failed handshake no longer drops the queued
  dials — only 3 in a row, or a failure with no link up.
- Outage: the Kharej side lets one slot retry (2 s connect, every ≤2 s); the
  others wait. Its log says when the outage begins (`no link up to the edge —
  dials fail`) and when a link is back (`… back after Xs with none up`); a
  shorter outage used to show neither (failures are folded every 30 s).
- Warm start: a restart within 15 min comes back at the previous size (only
  ever raising the start size; written after a minute of uptime).
- Refill hold: after a start or a total loss a new TCP connection waits up to
  10 s for a link with fewer **open** connections than the fair share, then
  goes onto the existing links — never refused, and said in the log when that
  is above the cap, with the reason: a pool of hundreds cannot be back within
  10 s at the gate's pace (`links open at the dial pace … — expected, not a
  fault`), and only a pool well behind that pace is blamed on the path.
  Simulated with the real pool: an outage with 2,400
  connections and 300 links put 2,400 on one link before, 24 now; a
  production-shaped restart (6,000 open, 63 links) 781 before, 96 now.
- Reverse accept cap: live links only, 2×max+8; a refused link is held 5 s
  and refusals are logged once a minute with their count.
- Per-link cost: pool-control refreshes fast on 2 links, every 30 s on the
  rest, and a change goes out spread over 1.5 s; control pings stay on a fixed
  3 s cadence (a jittered version had re-enabled a download-loss false alarm);
  l3mtcp's side channel keeps quiet when both servers support it.

### Q3 — stalled readers
- A few users whose apps stop reading used to fill a link's 8 MiB receive
  buffer, stop every other connection on it and get the link killed by the
  other server's TCP after 20 s (Kharej RSS 5.5 GB in the stall test). Now the
  connections whose app took nothing for 6 s while the buffer is full are
  reset (`mtcp: reset N connection(s) on K link(s) …`); slow readers that
  still read are kept (test: released in 7.7 s, slow and fast readers kept).
- Doctor: kernel TCP memory against tcp_mem, the conntrack table, and several
  tunnels' combined worst case; the status writer logs once when kernel TCP
  memory passes its pressure mark. `tcp_tw_reuse=1` is now set.

### Q4 — the Kharej side counts its own users and traffic
- Open connections, active ones, Mbit/s and the last minute's peak on the
  Kharej server too (direct and reverse, mtcp and dgtun), in the live monitor,
  `hs2 status` and `hs2 doctor`; an older hs2 there says "counted on the Iran
  server" instead of 0.

### Q5 — dgtun at scale
- Raw encapsulations (icmp/gre/ipip/ipx): one shared dial socket per peer
  instead of one per carrier — the kernel copied every received packet to
  every carrier's socket. Measured per packet (send+receive): 300 carriers
  86–90 µs before, 11.4–11.6 µs now; one carrier unchanged. Link ids are
  unique (at 300 carriers two used to collide about half the time).
- A carrier closed on purpose is closed on the other side at once (it stayed
  "alive" 15–17 s); a restarted peer's dead carriers go as soon as a fresh one
  comes up; a scout dial runs while every carrier is silent.
- Retiring is mirrored to the other side, and flows stuck to a retiring
  carrier move after 30 s, so a shrink finishes under nonstop download; the
  reverse edge serves retired/spare carriers again when its target rises.
- Accept loops survive transient errors; the shrink sort is O(N log N); per-
  carrier log lines are folded.

### Q6 — load test, and what it found
Three network namespaces (Iran, a userspace netem middle: 400 Mbit/s, 40±5 ms,
0.05% loss with bursts, a 3 Mbit/s per-flow policer; Kharej), production
tuning, 2,400 active users (256 B echo every 500 ms), 3,000 idle, 16 bulk
downloads: 5,416 open connections. Old = the main build before this phase.
- 300 links reached in ~57 s; CPU ~48% of a core per side, RSS 400–480 MB;
  four minutes after the load the pool is at ~90 links (old: 200).
- Iran restart under load (3 runs): users served normally again after
  16.3 / 16.4 / 16.3 s (old: 71 s); 40 s outage (3 runs): 16–20 s (old: 61 s).
- Mixed versions and l3mtcp reverse run at 300 links.
- dgtun counted interactive users (~1 KB/s) as idle, and each connection
  twice (once per direction): the pool sat at 8 carriers for 100 s with echo
  p99 up to 87 s. Both fixed: 2,416 active counted, p99 121 ms at 300 udp
  carriers, no dropped connection.
- Review fixes: the reverse edge's refill hold waits only for the links the
  exit allows and ends when links stop coming; dgtun's serve notice can no
  longer be lost for good, a restarted exit's first carrier is not taken for
  spare, receive stamps are monotonic, the direct exit's flow map is pruned;
  the outage begins/ends in the Kharej log and the scout's first retry is
  capped; doctor totals several tunnels at their effective ceilings; status
  names the usable cores; the installer's visibility note comes once the
  direction and carrier are known.
- Found: Go 1.24+ opens listeners as MPTCP by
  default, and an accepted MPTCP socket ignores tcp_notsent_lowat, so a user
  whose app stops reading holds up to its whole send buffer (4–7 MB) of
  kernel memory and keeps its link busy (echo p99 8 s for the others). With
  MPTCP off the stall guard resets exactly those users and p99 is 0.5 s; under
  normal load no difference.
- A degraded link no longer takes all its users down with it after 45 s (it
  cut 60–90 active connections per link in the test). It still takes no new
  user and is replaced at once; after 45 s its connections that moved no data
  for 15 s are closed (they reconnect onto healthy links): idle ones, and on a
  link that is really stuck every one, 15 s after its data stopped — all at
  once: each close waits for the link's writer, and one after another would
  have kept most users waiting minutes. A connection whose data still passes
  stays, however little moves (an SSH session being typed in, a game's beat),
  45 s more — 90 s after the link degraded the link closes with what is
  left. (A first cut kept them up to 5 min: in the test they had p90 1.6–2.7
  s all that time, against 0.2–0.4 s a minute after a reconnect.) The
  draining link keeps them only while the
  pool can refill without its slot: it does not count against max (all links
  stay within max + an eighth), the reverse exit is now asked for the
  replacement at once (before: only after the bad link closed), and when the
  pool is at its ceiling and short of serving links the oldest draining links
  close as before. Log: `link N degraded for 45s — its connections that moved
  no data for 15s (idle or stuck) are closed now …`, once a minute `closed N
  connection(s) on degraded links that moved no data for 15s`, and at the
  ceiling `N degraded link(s) (…) closed with their M remaining
  connection(s): the pool is at its ceiling and needs their slots`.
- A link failing slowly during the stats handshake was taken for an older
  exit: `the other server does not report link stats (older hs2)` with both
  servers current (seen on stuck links in the load test, old build too). An
  exit that answered the info exchange is newer than stats, so the edge now
  just tries again.
- MPTCP off: every listener is plain TCP (see the finding above). The
  carrier, user-port and dgtun listeners set it explicitly; go.mod's `godebug
  multipathtcp=0` covers the rest and the tests.

### Q7 — stuck links (throttled, not cut)
DPI can throttle a link to a few packets a second without cutting it. Such
a link still gets a keepalive through, so it is never *suspect*, and it
moves too little for the loss rule, so it is never *degraded*. It kept
serving, and it took new users because it looked the least loaded. In the
load test, main kept 7 of 10 such links until the end, and only 79% of
echoes were answered for good.

- **The signal.** Each link's control ping travels behind its own traffic
  both ways, so the age of its oldest unanswered ping is how long its users
  wait. The control loop keeps up to 4 pings outstanding. A write that
  timed out stays queued in smux, so it no longer ends the loop. The wait
  is published on every answer.
- **The rule.** A link is *stuck* when all of these hold:
  - its ping has waited 6 s, in two samples in a row;
  - it moves less than 12 KB per 2 s, or less than half of what the links
    that answer promptly move (and under 96 KB);
  - another busy link answers in under 2 s now, to a ping sent *after*
    this link's oldest;
  - the path is not slow (below).

  A stuck link is degraded at once: no new users, and its replacement is
  asked for. Its connections that moved no data for 15 s close right away,
  and the rest get 90 s at most.
- **What it leaves alone.**
  - **A slow path.** The path or the other server is slow when two or more
    links wait 6 s while moving little, and they outnumber the busy links
    answering promptly. Moving users would not help, so nothing is drained
    and nothing is judged for loss. That holds afterwards too, for as long
    as the spell was slow all told (30 s to 2 min), because TCP backed off
    through it and resumes up to that long after.
    - Links answering in 2–6 s, heavy links waiting behind their own
      backlog, and links that never answer (an older exit) count neither
      way. A review showed that counting them as "slow" let two lossy links
      at night switch both rules off, silently.
  - **A link moving its share.** On a full path, a link whose users wait
    behind their own load while it moves its share is not stuck.
  - **A link wedged by its own users.** If its reader was parked on a full
    receive buffer, the wedge guard handles it.
  - **A suspect link.**
  - **Drain limit.** At most an eighth of the pool drains as stuck at a time.
- **Log.**
  - `link N stuck: its traffic has waited 10s for an answer while it moved
    1.8 KB in 2s (the other links answer in ~85ms) — draining`
  - then `link N stuck — its connections that moved no data for 15s are
    closed now …`
  - on a slow path, once a minute: `N of M busy links have waited 6s+ for
    an answer and only K answer promptly — the path or the other server is
    slow, not those links: none is drained`
- **Load test (reverse, ~200 links, 1,600 active + 2,000 idle users, 16
  downloads; "stuck" = 3 packets/s each way via iptables). Release build
  unless noted.**
  - **10 busy links stuck.** 9 were caught at 10.6 s; the 10th was already
    draining for loss. Answered echoes went from 76% to 100% within 60 s,
    and p99 settled at ~335 ms. Main stayed at 79% for good.
  - **10 random links stuck.** All 10 were caught, 8 of them within 13 s.
    p99 stayed at 580–650 ms; main's went to 44–69 s.
  - **Small pool (max 10), 4 links stuck.** All 4 were caught at 9–15 s,
    and answered echoes were 100% after 30 s. The build before the review
    fix took the 4 for a slow path ("4 of 8 busy links … none is drained")
    and answered 51–56% for a minute.
  - **Squeezes: no stuck verdict, drops in main's range.**

    | Squeeze | This build (active drops) | Main (active drops) |
    |---|---|---|
    | 8 Mbit/s for 60 s, two runs | 1,485 and 1,178 | 1,519 and 1,527 |
    | 12 Mbit/s for 120 s | 1,219 | 1,108 and 1,410 |

  - **Shallow queue.** The same 8 Mbit/s squeeze through a shallow 20 ms
    queue, where packets are dropped rather than queued, also gave no stuck
    verdict. But this build dropped 1,730 and 1,750 active connections, and
    main 1,537 in one run, with 4 loss verdicts 1–2 min after the squeeze
    where main had 1. This is still open and is followed on the real server
    (VALIDATION.md, V5).
  - **Outage, 40 s.** No stuck verdict, and normal service 18 s after it
    ended.
  - **Iran restart under load.** No stuck verdict, and normal service
    again after 14.3 s, as before.
  - **600 users whose apps stop reading.** No stuck verdict; the wedge
    guard reset those 600 connections, as before.
  - **20% loss on 10 links.** The loss rule drained 7 of them in this run.
    In an earlier run of this phase it drained 11, where main drained 6.
- **The loss rule waits out a slow spell too.** Before this was fixed, the
  stuck builds dropped about 8% more in the 8 Mbit/s squeeze (1,651 and
  1,662), with no stuck verdict at all. The extra drops came from the loss
  rule: it drained 4–8 links for download loss just after each squeeze,
  where main drained 1–2.
  - Why main drained fewer: a single ping write timeout ended a link's
    control channel for good. That left the loss rule without the exit's
    retransmit count for the rest of the link's life. The control channel
    now survives, and right after a squeeze it reported TCP's recovery
    retransmits.
  - The fix: no link is judged for loss during a slow spell or its recovery
    window, and the rule then needs three fresh samples.
  - The same survival makes the loss rule see lossy links it used to miss.
    In the 20%-loss test the new build drained 11 lossy links where main
    drained 6, cutting 365 active connections against 178. Those users
    reconnect onto healthy links, and p99 matched main.
  - Only links whose control channel has answered count toward the slow
    test. A pool on an older exit, whose links never answer, would
    otherwise pass for slow for good and silence the loss rule.
  - Earlier cuts of the rule drained 210, 55, then 27 links in the
    squeeze, cutting hundreds of users who would have recovered. Each was
    fixed before this one.
- **Reviews.** Two independent adversarial reviews; every confirmed finding
  is fixed and has a test that fails without its fix (more than 30
  mutations, all caught).
  - A minority of stuck links (more than a third of the busy ones) passed
    for a slow path for good.
  - A link waiting on its own load was taken for stuck.
  - The per-tick cap did not bound how many links drained at once.
  - The wait stayed stale until every outstanding pong was back.
  - A reused ping buffer rewrote queued pings.
  - An answer arriving just after an outage began counted as proof that
    the path worked.
- **Not seen from the edge.** A wedge on the exit's side (its reader parked
  by a slow target). The exit's guard frees readers that stop within
  seconds, before a verdict; one that trickles is judged like a throttled
  path.

### Q8 — the health rules at high bandwidth (and UDP flows)
Q7 was load-tested at 16 downloads. A new rig mode (`run3.py --bulkmb
--bulkspread`: downloads of a few MB that end and come back, started over the
ramp) put 150 downloads and 2,400 active + 3,000 idle users through 300
links at 150-200 Mbit/s, each link behind the rig's per-connection throttle
(3 Mbit/s, dropping what goes over it). On the release build (and on the
build before it alike) the health rules misjudged the busy links there, and
an independent review found why.

- **The loss rule read pong timing, not loss.** The download loss was the
  exit's retransmits from the last control pong over the bytes of the 2 s
  health tick. Pongs come every 3 s, so one tick in three read no loss (the
  streak reset) and the others held 3 s — or 6 s — of resends over 2 s of
  bytes. A link resending 30-90% with steady pongs was never drained; one
  resending 9% with pongs jittering behind its own queue, as on every busy
  link at peak, was (16 of 20 in 5 min). It is now judged over the window
  between two pongs, against what came in over that same window, and each
  direction keeps its own streak.
- **The loss fraction was inflated.** Its denominator was payload bytes /
  1400 while retransmits count TCP segments, so a link of small or padded
  frames read 1.1-10× lossier than it was. It is now the socket's own count
  of data segments (TCP_INFO, Linux ≥ 4.6; bytes / mss on older kernels).
- **No limit, no comparison.** Every candidate was drained in the same tick:
  a lossy path drained all 300 links at once in the review's test, and the
  pool at its ceiling then closed them with their users. Now:
  - a link that resends a lot but still moves at least half of what the
    pressed links move (those whose sender waits for the path: what a link
    gets) is left alone — its resends are what the path's rate costs, a
    throttle dropping what goes over it, and its users would get no more
    elsewhere;
  - when most busy links resend more than 12% below that rate, the path is
    lossy: none is drained (`N of M busy links resend more than 12% — the
    path is lossy, not those links: none is drained`, once a minute);
  - at most an eighth of the pool drains at a time, the lossiest first.
  - The line now says what the link moves against the others: `link N
    degraded (up-loss —, down-loss 25% of 840 segments, moving 0.4 Mbit/s
    where the busy links get 2.8, rtt 120ms) — draining`.
- **A congested path passed for stuck links.** Through a squeeze, light links
  still answer within 2 s and outnumber the heavy ones backing off, so the
  slow-path count read a minority of stuck links: a 30 s squeeze from 190
  to 60 Mbit/s drained 33 links as stuck. The path is now also slow when
  links wait and the ones that answer promptly take 4× their usual time
  (the lowest median of the last 10 minutes) and over 0.5 s: `N of M busy
  links have waited 6s+ for an answer and the K that answer promptly take
  ~1298ms, 15× their usual ~88ms — the path is congested, not those links:
  none is drained`.
- **UDP: one slow flow held up the whole port.** The Iran side's one read
  loop per UDP user port wrote each datagram to its flow's link itself, and
  opened the stream of a new flow itself: a user whose link was throttled
  or busy stalled every UDP user of that port (in a test the other user, on
  a healthy link, got 0 of 200 datagrams). Each flow now has its own queue
  (256 datagrams, 512 KB) and writer; a full queue drops, as a full UDP
  socket would. UDP forwarding is off unless it was turned on at setup.
- **Load test, release build → this build** (reverse, 300 links, 5,500
  users, 150 downloads; "loss10" = 10% random loss on a link, "stuck" = 3
  packets/s each way):

  | Scenario | Release | This build |
  |---|---|---|
  | Steady, 2.5 min: loss verdicts / connections cut | 28 / 131 (the build before it: 40 / 693) | 1 / 2 |
  | Squeeze 190 → 60 Mbit/s for 30 s: stuck / loss verdicts / cut | 33 / 39 / 1,738 | 1 / 1 / 99 (the stuck one: a link TCP had backed off, still waiting 49 s, ~30 s after the squeeze) |
  | 3 links at 10% loss: caught in 90 s / loss verdicts / cut | 1 of 3 / 41 / 722 | 3 of 3 / 6 / 322 |
  | 4 busy links stuck: caught | 2 (2 more already draining for loss) / 31 loss verdicts | 4 of 4 in 9-11 s / 1 |

  And at Q7's load (1,600 active + 2,000 idle users, 16 downloads, ~200
  links):

  | Scenario | Release | This build |
  |---|---|---|
  | 10 busy links stuck | 9 of 10 at 10.6 s | 10 of 10 at 10.6 s |
  | 40 s outage: stuck / loss verdicts after it | 0 / 6 | 0 / 0 |
  | 8 Mbit/s squeeze, 20 ms queue: active cut / loss verdicts | 1,730-1,750 / 4 | 1,706 / 0 (one link still backed off 63 s after the squeeze drained as stuck) |
  | 20% random loss on 10 random links: drained / cut / p99 | 4 closed by 120 s / 657 / 3.0-4.3 s | none (light, or moving what the busy links get) / 0 / 2.5-5.8 s |

- **From the first real-server report** (two test servers, 1-3 cores and
  2 GB each, auto ceiling 48, 550-950 real users, reverse, the release build):
  - **Stalled readers under kernel memory pressure (🔴).** Twenty downloads
    whose app was stopped for 60 s (`iperf3 -R -P 20`, `kill -STOP`), under
    250 users: the guard reset nothing (one stalled download per link never
    fills a link's buffer), the kernel's TCP memory went past its pressure
    mark (113 of 169 MB), every socket was squeezed, and the health rules
    drained 11 healthy links for loss and as stuck — 44 users cut, p50 84 →
    170 ms. Now, while either server is past the mark (the exit says so in a
    new flag of its link stats; an older edge ignores it), the guard resets
    the connections whose app took nothing for 6 s without waiting for a
    full link, and no link is judged lossy or stuck, nor for the recovery
    window after.
  - **The pool did not shrink (🟠).** Five minutes after the load, 48 links
    stayed for 137 active users: the per-link capacity estimate (~0.9
    Mbit/s) came from the few slow links still pressed at low load, sample
    after sample. It is now one entry per link — the best sustained rate
    each showed while pressed in the 30-minute window — and their median.
  - **A lossy link missed (🟠).** One of two links at 20% loss was never
    drained in 150 s: the pong-timing estimate (above), and its traffic
    dipping under the activity threshold, which reset the streak. A quiet
    sample now keeps a streak whose last bad sample is under 20 s old.
  - **Uninstall left files.** The tunnels' warm-start records and the ping
    guard's folder in `/run/hs2` are removed; the option says it keeps the
    program, and asks whether to remove `hs2` and `hs2-menu` too.
  - Also confirmed there, with real users: growth, restarts of either
    server, a 30 s outage, one and four stuck links, a path-wide 2 Mbit/s
    slowdown (no verdict) — as on the rig.
- **What the rig also showed, not changed here.**
  - A sudden crowd onto a small pool (5,500 connections in 30 s onto 8 warm
    links) leaves the first links crowded (~235 connections each) until
    their users reconnect: the refill hold covers a start or a total loss,
    not a crowd onto a pool that is already up. Their downloads are pinned
    to those links.
  - 10,000 open connections: ~0.9 GB RSS on each server (copy buffers of
    32 KB each way per connection), 74% of a core on the Iran side at ~190
    Mbit/s in the rig.
  - The review also found (not changed yet): a burst of new connections
    (over ~40 a minute per link) can hide a link's real pressure from
    placement, so 6-10% of new connections land on throttled links; the
    autopilot's probe on the reverse edge does not know the exit's ceiling.

## Phase V — tun over icmp: a cut carrier heals in a second, and a ceiling that fits icmp

A review of the datagram tun (`dgtun`) over icmp found that the autopilot sizes
it like the stream pool and the pool already heals a restart, a path outage, a
policer and random loss — but not one carrier cut while the others work, and
that its ceiling ignored what icmp is.

### V1 — a carrier whose own way through is cut moves its flows in ~1 s
- **What was wrong.** A carrier was only taken for dead when it failed a send,
  when the other server said goodbye, after 15 s of silence, or when a new
  carrier arrived (and only those silent 3 s+), or when *every* carrier was
  silent (the scout). One carrier cut alone — its icmp echo id (or its port)
  dropped by a middlebox, its state lost on the other server — kept its flows,
  and took 1/n of the new ones, until its 15 s timeout. The inner TCP backs
  off meanwhile, so its users came back after 26-38 s; with only the replies
  cut, some never did within the test. Nothing was logged.
- **Now.** Each side looks every 250 ms. The other server sends feedback on
  every carrier every 100 ms, so a carrier that has heard nothing for 1 s
  while another carrier still hears the other server is **mute**: new flows
  avoid it and its flows move at their next packet; it tells the other
  server (`closeMute`, sent twice; an older peer ignores it), whose flows
  leave it too — so a one-way cut heals on both sides. At 3 s it is closed
  and replaced. A mute carrier that hears again takes flows again
  (`closeHear`; the other server's word also expires after 4 s). Nothing is
  judged while no carrier hears the other server — then the path or the
  other server is down, and the scout (unchanged) takes over — nor right
  after the process (or its VM) was stopped for a moment.
- **Visible.** `dg: carrier N has heard nothing from the other server for
  1.2s while 5 other carrier(s) still do — its own way through is cut: new
  flows avoid it and its 10 flow(s) move to live carriers`, then `dg:
  carrier N heard nothing for 3.2s — closed; a new carrier replaces it`; on
  the other server `dg: carrier N: the other server hears nothing on it —
  what goes there is lost: its flows move to live carriers`. `hs2 status`
  flags such a carrier `M` on its `carriers` line and counts the closed ones
  (`mute:` line; `mute_closed` in the status file). A carrier that fails
  outright (a send or read error) now says so too: `dg: carrier N failed
  (…) — its flows move to live carriers`.
- **Measured** (two network namespaces, dgtun over icmp, 6 carriers, 24 TCP
  downloads; one carrier's echo id dropped silently on the way in):

  | Cut | Release: connections stalled / longest | This build |
  |---|---|---|
  | Both directions | 9 / 26-38 s, no log line | 7 / 1.5 s |
  | Kharej → Iran only | 9 / 38 s (not back by the end) | 6 / 2.0 s |
  | Iran → Kharej only | — | 3 / 1.4 s |

### V2 — tun over icmp runs at most 8 carriers
- Every carrier of a tun over icmp is one echo identifier between the same two
  IPs: they share one path and one policer (an ICMP rate limit sees their sum,
  which is why the governor caps them together), so past a handful more
  carriers add no bandwidth — only a pattern ping never makes (up to 300 echo
  ids to one host on a strong server with the auto ceiling) and feedback
  traffic (10 reports a second each way per carrier). With `max_links` 0
  (auto) or absent, a tun over icmp now runs at most **8**; a positive
  `max_links` still fixes the number (doctor and status warn above 8).
- Said everywhere the ceiling is: the start line (`link pool: ceiling 8 links
  — tun over icmp: every carrier is one echo id between the same two IPs, so
  more than 8 add no bandwidth …`), `hs2 status`, `hs2 doctor`, `hs2
  recommend-links -c <config>`, and the installer (end of setup, and Tuning →
  Link pool).
- An existing icmp tunnel without `max_links` ran the historical 32; it now
  runs 8.

### V4 — ping under load: a fair queue per carrier, interactive flows first
- **What was wrong.** Each carrier had one FIFO send queue (up to 256 packets,
  50 ms) in front of its pacer, which holds up to 20 ms more. On a busy pool
  every carrier carries some download, so a game's, a call's, a DNS or a ping
  packet waited behind up to ~70 ms of it: that was ping and jitter under
  load. A full queue also dropped the newcomer — often the interactive packet.
- **Now** (`engine/dgfq.go`): each carrier's queue is a deficit round robin
  over its flows. A *sparse* flow — nothing queued, under 256 kbit/s lately
  (a game, a call's audio, DNS, ping, a remote shell, the first packets of
  any connection) — is served before the backlog, and its packet takes a
  fast lane in the carrier's pacer (after FEC parity, ahead of the data
  queue). Downloads take turns by bytes. A full queue drops the head of the
  flow with the most queued. A flow's packet takes the fast lane only once
  everything it sent the ordinary way has left the pacer's data queue (the
  carrier counts the shards in and out of it), so a flow's packets keep their
  order however slowly that queue drains; a heavy flow paced just under the
  carrier's rate does not keep the head start (all it sends spends its rate
  budget), so it cannot starve the downloads.
  Bandwidth, pacing, FEC and the 50 ms sojourn bound are unchanged.
  `HS2_DG_FQ=0` restores the single FIFO.
- **Measured** (two network namespaces, tun over icmp, 4 carriers, a
  20 Mbit/s bottleneck each way, 8 downloads, ping through the tun):

  | | FIFO (`HS2_DG_FQ=0`) | fair queue |
  |---|---|---|
  | ping idle | 0.7 ms | 0.6 ms |
  | ping under load: p50 / p99 | 66 / 94 ms | 12 / 20 ms |
  | jitter (std dev) | 17 ms | 5 ms |
  | pings lost | 5 of 120 | 0 of 120 |
  | throughput | 16.4 Mbit/s | 16.4 Mbit/s |

  What is left (~12 ms) is the bottleneck's own queue, which the carriers'
  delay-based pacing keeps short. Over three runs the fair queue gave p50
  11-15 ms with no ping lost; the FIFO 66-144 ms, losing up to 43 of 120.

### Review fixes (independent review of V1-V4, all confirmed and fixed)
- A flow whose queue never empties (a VPN over UDP inside the tunnel, faster
  than its carrier) grew its queue's array by every packet it had sent (64 MB
  after 2M packets in the review's test): the popped part is now reclaimed.
- The fast lane's ordering guard was a fixed 100 ms; at a carrier's floor
  rate, or under a policer cap, its ordinary packets could wait longer, and a
  later packet overtook them (reproduced). It is now the queue's own count.
- Pool control no longer rides a carrier the other server hears nothing on.
- A full queue looks for the fattest flow among the queued ones only, not
  every flow seen in the last 2 s.

### From the real-server report (two test servers, Iran 3 cores / Turkey 2 cores, reverse, build 1b5591f91bee)
- The icmp ceiling: 8 on both servers, with its reason (the hardware alone
  would have allowed 79-81).
- On that path only icmp carries data: udp carriers came up and went silent
  (100% loss), so a mixed icmp+udp pool would gain nothing there.
- Fair queue vs FIFO under an 8-stream download: download 66-72 vs 47-53
  Mbit/s, ping loss 0.7% vs 4.7%, p99 140 vs 198 ms.
- One carrier cut (both ways, and Iran-only): mute in 1.0 s, flows moved at
  once, closed at 3.0 s, a new carrier ~7 s; the download never stopped.
- The cap (~60-72 Mbit/s down, ~80-86 up) was the Turkey server's CPU (2
  cores at ~97%, shared with the production tunnel), not the path: one
  carrier 47-51, eight ~60 (CPU parallelism, no policer detected). A lab CPU
  profile of the sending side puts ~30% in syscalls (one per datagram on the
  raw socket and one per packet on the tun) and ~25% in goroutine hand-offs;
  crypto is ~4%.

### V3 — guidance: the kernel's own ping replies on a dedicated server
The icmp listener keeps the server answering ordinary ping by dropping only
the kernel's replies to tunnel packets (nft/iptables). The kernel still
builds each of those replies before the rule drops it — a copy per received
tunnel packet, on the side that receives the echo requests (in reverse the
Iran server, for the download). On a server that runs only the tunnel,
`HS2_ICMP_SUPPRESS=global` (in the service's environment) stops the kernel
answering ping at all instead, which saves that work; the server then does not
answer ordinary ping.

## Phase W — less CPU per gigabyte, and carriers that share the bottleneck

The real-server report of Phase V put the datagram tun's limit on a small
server at the CPU (Turkey, 2 cores at ~97%), and a lab profile of the sending
side spent ~30% of it in system calls: one per datagram on the socket, one per
packet on the tun.

### W1 — several datagrams per system call
- `sendmmsg`/`recvmmsg` (new package `mmsg`) on the raw sockets of every
  encapsulation (dial and listen side) and on udp sockets. The pacer gathers
  the datagrams already waiting that its token bucket can pay for (at most
  16, in lane order) into one call; nothing leaves earlier than its pacing
  allows. A datagram the kernel refuses (a soft error such as EMSGSIZE) costs
  only itself. `HS2_RAW_BATCH=0` sends and receives one at a time.
- The carriers' read loop takes the frames already waiting in one pass and
  writes what they carry to the tun together.
- `HS2_PPROF=127.0.0.1:<port>` serves Go profiles, on a loopback address only.

### W2 — TCP offload on the tun
- The tun is opened with a virtio-net header and TCP segmentation offload:
  the kernel hands hs2 one TCP packet of up to 64 KB, which hs2 cuts into the
  usual MTU-sized segments with full checksums (exactly what the kernel would
  have sent one by one; the IP ID, sequence numbers and flags per segment as
  Linux does it), and finishes any checksum the kernel left partial. On the
  receiving server consecutive in-order segments of one TCP connection are
  written to the tun as one packet (the kernel's GRO rules). Nothing changes
  on the wire; an older hs2 on the other server works with it.
- `HS2_TUN_OFFLOAD=0` turns it off; a kernel that refuses it gets plain
  packets. The start line and `hs2 status` (`tun:` line) say which, with the
  packets per read and per write, malformed kernel packets dropped and merged
  packets the kernel refused (those go in one by one).
- **Measured** (two network namespaces, icmp, reverse, 2 cores a side): a
  download ~450 → ~650 Mbit/s; CPU per GB on each server 27-39% lower (agent
  round 1: 16.3 and 14.3 CPU-s/GB against 23.3 and 21.8 for the release);
  every copy intact in every mode.

### W3 — carriers share the bottleneck (rate control)
A ping-under-load check of W1/W2 behind a 30 Mbit/s bottleneck found ping
p50 from 11 to 70 ms run to run and some runs losing 10-18% of pings — with
the release as well (61 ms in one release run). A trace of the rate
controller showed why: the carriers of a pool meet at one bottleneck, and
each read the queue they all built as its own.
- **A carrier stuck in startup.** Startup ended only on a queue seen while
  the carrier used its allowance. When the users' TCP filled the bottleneck
  before the pacer did, the carrier was never "limited" and stayed in startup
  at 2.9x its delivery (75 Mbit/s allowed on a 30 Mbit/s path): unpaced, the
  queue sat in the bottleneck instead of in hs2's fair queue, and pings
  waited and were dropped there. In the simulator a source offering 1.02-1.5x
  the path (2x as well on a short path; at 80 ms round trip the 2x offer
  used its allowance and left) kept it so for good: queue at the buffer
  limit (120 ms), thousands of drops. Now startup also ends once a queue has stood for 3 reports
  (~300 ms) while the carrier carries more than ~1 Mbit/s and at least half
  what the pool's busy carriers carry (their fair share; with none, the
  active carriers' mean). A light carrier — a call, the other direction's
  ACKs — keeps startup's fast ramp for its own bulk later, unless the queue
  stays past 30 ms for eight base round trips (one to three seconds) while
  it delivers less than it sends and the busy carriers do not see that queue
  (theirs short, or under half its own: the governor publishes the queue
  they see): far deeper than the others' pacing holds and growing under it,
  so its own, on a path of its own. The governor holds its fair share and
  that queue through a second without busy carriers (a base probe slows
  them all at once), so a light carrier does not read itself as busy, or a
  shared queue as its own, for a moment. The capacity it starts
  from is what got through, not the allowance: 9.5 ms, no drops.
- **A stale peak.** Out of startup, a carrier's capacity was floored at 0.4x
  its windowed peak delivery — a window that only advances while it uses its
  allowance. A carrier that had run at 900 Mbit/s and then met a 30 Mbit/s
  bottleneck kept ~360 Mbit/s and paced nothing (seen with mixed versions:
  ping 122/475 ms, 5% lost). The floor now holds only while the carrier uses
  its allowance (it guards against a queue someone else built); otherwise
  its capacity follows what it delivers; and while a queue stands a peak
  older than 1.5 s leaves the window, so the floor cannot come back from it
  either: 120 ms and thousands of drops → 9.5 ms, none (simulator, measured
  from 2 s after the bottleneck appears); lab, 30 Mbit/s tbf added after a
  ~510 Mbit/s download: ping p99 28-59 ms, none lost (release: up to 1960
  ms and 19% lost).
- **Late carriers starved.** After startup a carrier's capacity tracked its
  own delivery, so whoever held the queue first kept it: the others read it
  as theirs and sat at ~0.3 Mbit/s, with the flows on them. Now the pool's
  governor computes the fair share (the mean rate of the carriers using
  their allowance), and while a queue of 5-20 ms stands each such carrier
  grows toward it: 0.5% of the share, or 10% of its gap below it, per
  report — never more than 3% of its own capacity; like every per-report
  step, scaled to the part of a round trip the report covers (per round
  trip on paths over 100 ms).
  With the queue term's proportional cut that settles every busy carrier on
  the same rate. Not while no queue stands: a carrier on its own slower path
  (a pool over several IPs) cannot tell its queue from the pool's, and
  growth there drove it into its buffer (100-200 ms in an earlier version
  of this rule; now 33-40 ms p95 against 12-17 ms without the rules, no
  drops, its whole path's rate).
- **Base probes together.** The carriers' base-delay probes now fall on one
  shared 4 s clock (anchored on the monotonic clock, so a wall-clock step
  cannot hold them back; at least 3/4 of a period apart, so a probe that
  started late is not followed by another), so the whole pool slows at once
  and the queue really empties; one carrier probing alone while the others
  kept the queue full measured a base with the queue in it.
- **Paths over 400 ms round trip measured at all.** An RTT sample over 8x
  the smoothed RTT is dropped as a clock step; the smoothed RTT starts at 50
  ms, so on a path over 400 ms every sample was dropped and the controller
  ran on 50 ms for good (since the controller was written; the release
  too). The first sample now counts, and so does the third outlier in a row
  (a step is one sample in flight). Simulator, a carrier on its own path at
  600 ms round trip behind a 200 ms buffer: queue p95 168-251 → 39-60 ms,
  up to 1112 drops → none.
- `HS2_FAIR_SHARE=0` turns the three off.
- **Measured.** Pool simulator (new tests, `udpcarrier/rate_pool_sim_test.go`;
  each carrier sees the round trip its reports really measured and the loss
  a full buffer caused), carriers joining a second apart: Jain's fairness
  index 0.45-0.91 → 0.99-1.00 over 30-60 s on paths of 20-120 ms round trip
  (0.93-0.98 already 10-25 s after start), 0.43-0.55 → 0.77-0.87 on 160-240
  ms; queue p95 27-38 → 17-21 ms, utilization 99% both; eight carriers (the
  icmp ceiling) on 16-100 Mbit/s: 0.97-0.99, queue p95 19-21 ms, no drops.
  Lab (icmp, 4 carriers, 8 downloads behind a 30 Mbit/s tbf each
  way): ping under load p50 17-19 ms, p99 21-34 ms, no ping lost in any run
  (release: p50 11-114 ms, p99 47-410 ms, runs losing 13-24% of pings);
  behind 8 Mbit/s with 4 downloads p50 18-28 ms, p99 27-48 ms, none lost.
- The pacer's token bucket is now capped after a timer wait as well: a timer
  that fired late on a busy server let one batch exceed its budget.

### Verification by agents (six rounds, each agent under 20 minutes)
- **Round 1** (7 agents on W1/W2): bandwidth, mixed versions, every encap
  and stress passed; the code review found the three bugs below; the
  latency agent's regression traced to the rate control (W3), present in
  the release too.
- **Round 2** (8 agents, W1-W3): high bandwidth (NEW 486-748 vs release
  334-459 Mbit/s, CPU per GB 33-38% lower, 81 copies intact), every encap
  and switch (26 runs intact), stress (5% loss: 297 vs 181 Mbit/s), many
  connections (p99 21-38 vs 38-70 ms), 30 Mbit/s latency (p99 20-30 vs
  47-410 ms, no ping lost) passed. Found and fixed since: slow convergence
  on an 8 Mbit/s path, the stale peak after a fast period, the probe clock
  on the wall clock, a slow separate path pushed into its buffer, a light
  carrier ramping slowly later (W3 above).
- **Round 3** (4 agents on those fixes): 30 Mbit/s ping p99 23-26 ms with
  no ping lost in 12 runs (release 22-52), 8 Mbit/s delivery 5.65 vs 5.32
  Mbit/s and carriers within 2x from 18 s (release 5-24x), high bandwidth
  696 vs 488 Mbit/s and 5% loss 240-252 vs 142 Mbit/s, all intact. Found and
  fixed since: the stale peak floor returning on reports that use the
  allowance, and a light carrier on its own filled path kept in startup
  (W3 above); the simulators' probe clock made deterministic.
- **Round 4** (2 agents on those fixes): lab, a 30 Mbit/s bottleneck
  appearing after a fast download, 9 runs (7 new on both sides, 2 with the
  release on the Iran side): no ping lost, p99 24.5-45.5 ms, every copy
  intact, the fast carrier's capacity down to ~25 Mbit/s within 10 s (the
  version before kept 1043 Mbit/s: 12% lost, p99 1142 ms); 8 Mbit/s with 4
  downloads, 8 runs: no ping lost, p99 25.8-46.6 ms, busy carriers within
  1.41x at 22 s (before: 1.61x). The review found that a light carrier on a
  long shared path (100-200 ms one way) left startup on the busy carriers'
  own startup queue, and its bulk later ramped at 1.6-3 Mbit/s instead of
  20-26 — hidden in the pool simulator, which fed the controllers no round
  trip. Fixed since: the deep-queue exit needs eight round trips and a
  delivery shortfall (now 24-27 Mbit/s, as with the rules off, also behind
  a 600 ms buffer); the simulator feeds real round trips; the cap on the
  fair-share step no longer shrinks with the round trip (it was 3% of
  capacity × 250 ms/τ; now 3% per report on any path, per round trip past
  100 ms) (fairness there 0.63 → 0.81-0.88).
- **Round 5** (2 agents on those fixes): lab, 5 stale-after-fast runs (no
  ping lost, p99 18.9-28.8 ms, every copy intact), 30 Mbit/s with 8
  downloads (p99 21.7-24.7 ms against 36.2 before) and 8 Mbit/s with 4
  downloads, 12 runs (no ping lost, p99 median 34.2 ms against 33.0 before):
  no regression. The review found that the wait for a light carrier's own
  deep queue counted smoothed round trips, which hold that very queue: on
  its own path behind a 0.5-2 s buffer the wait grew with the queue until
  the queue became the base delay, and the carrier stayed in startup on a
  full buffer for good (thousands of drops). Fixed since: base round trips,
  at most 3 s (left startup 3.3-3.7 s after joining, behind a 1 s buffer at
  300 ms; after round 6's busy-queue rule the simulator gives 1.0 s at 2
  Mbit/s and 3.7 s with 11 drops at 4 Mbit/s). Its second finding — fairness among 10-16 carriers on a narrow
  short path worse than before the unshrunk fair-share step — did not hold
  in a wider sweep: 4-10 carriers on 8-24 Mbit/s at 20-80 ms round trip,
  Jain 0.825 on average against 0.813 before (better or worse case by case,
  by a wide margin either way where per-carrier shares are under ~2
  Mbit/s); 12-16 carriers with 4 light ones on 12-16 Mbit/s: 0.38 against
  0.41, both collapsing, 0.22 with the rules off. The pool simulator now
  also reports a full buffer's drops as loss, as the peer does.
- **Round 6** (2 agents on those fixes): lab behind deep tbf buffers (30
  Mbit/s with 1 s, 8 Mbit/s with 1.5 s): no ping lost, the queue held at
  15-75 KB after startup, no busy carrier left in startup — the release
  kept every busy carrier in startup there (ping p50 84-117 ms); the
  standard runs unchanged. The review found the base-round-trip wait too
  short at 360-600 ms round trip on a shared path: the busy carriers' own
  startup queue outlasted it, and a light carrier delivers short of what it
  sends in a shared FIFO as well, so it left startup again (its bulk later
  1.6 Mbit/s instead of 24). A carrier alone cannot tell a shared queue
  from its own, so the pool tells it: the busy carriers' queue (above).
  Now 24-27 Mbit/s for a 1.2-2 Mbit/s light carrier at 300-400 ms round
  trip in every probe phase, as with the rules off (a 4 Mbit/s one at 300
  ms: 3.2-4.0, rules off 2.5); a light carrier on its own path still leaves (72 cases,
  0.2-2 s buffers, 30-600 ms: at most 5.5 s, queue p95 at most 62 ms, no
  drops). It also found the 400 ms RTT lockout (above).
- Pre-existing, unchanged (release the same): under a steady policer with
  no loss episodes the pool sends ~2.5x what passes and parity rises to its
  ceiling; many tiny flows behind a shared bottleneck all count as sparse,
  so the fair queue cannot single out a UDP echo among them; 16 carriers on
  an 8 Mbit/s bottleneck collapse to the floor rate (see the next item for
  the wider picture); a carrier out of startup that later gets bulk on an
  empty path re-ramps at the capacity probe's 4% per report (every 100 ms:
  ~1.5x a second; 4% per round trip on paths over 100 ms); 300 ms RTT with 26% bursty
  loss sometimes collapses utilization (4 of 30 seeds in the simulator,
  rules on or off); a carrier still in startup from a fast period sends
  its first 1-2 s unpaced when a slow bottleneck appears (a few hundred
  drops at the bottleneck, before pacing takes over); after that the
  formerly fast carrier keeps a multiple of the others' rate for 10-20 s.
- Pre-existing, found in round 5 (release the same, rules off alike): with
  8 carriers on a 16-24 Mbit/s path at 80 ms round trip, the carriers that
  join while a queue already stands take it for the base delay, the tail
  drops of the full buffer for the path's random loss, and loss
  compensation then keeps the pool on a full buffer (simulator: Jain
  0.13-0.26, queue at the 200 ms buffer, tens of thousands of drops; with
  the drops not reported as loss, 0.81-0.92 and no drops). At 16 Mbit/s and
  40 ms, 30 Mbit/s and 120 ms, or 100 Mbit/s and 80 ms the same pool is
  fine (0.97-0.99, no drops). A fix belongs in the loss compensation and is
  left for a separate change. 10-16 carriers sharing 8-16 Mbit/s collapse
  the same way.
- Known, with the rules: a flow outside the pool that holds the
  bottleneck's queue past ~100 ms squeezes every carrier (a delay-based
  controller next to a loss-based flow; the release too), and with the
  rules a light carrier as well — a 1.2 Mbit/s call got ~0.5 Mbit/s in the
  simulator, where with the rules off it stays in startup and keeps its
  rate. Behind a queue held at 60 ms a call next to busy carriers keeps its
  1.2 Mbit/s; a call alone in its pool is no light carrier (nothing busier
  to compare with) and got 0.76 Mbit/s behind 60 ms, 0.5 behind 120 ms.
  `HS2_FAIR_SHARE=0` gives the release behaviour back if this shows.

### Review fixes (agent round 1)
- A udp listener on an IPv6 address read in batches dropped every datagram
  (the batch held IPv4 addresses only): batches are now used on IPv4
  sockets only.
- A packet the kernel refused in a tun batch write lost the rest of the batch
  and none of it was counted: now only that packet is lost (a refused merged
  packet goes in segment by segment) and the count is exact.
- A transport checksum that computes to 0 is written as 0xffff, as the kernel
  does (for udp, 0 means "no checksum").

## Phase CA — optional ICMP traffic-shape camouflage (`HS2_ICMP_CAMO`)

A field report had an IP carrying the icmp tunnel filtered at the border, the
pattern — not the content — recognised. This phase hardens the icmp flow's
*timing and id* against a stateful classifier, behind an opt-in flag so the
default (and every existing user) is untouched. Set `HS2_ICMP_CAMO=1` on both
ends of an icmp tunnel. It changes only send timing and the id draw, not the
wire format, so it needs no peer agreement and is compatible with an old peer.

- **CA1 — the timing (the biggest cheap tell).** The feedback loop sent a
  control packet every 100 ms unconditionally — a sharp ~10 Hz spectral line,
  and at idle the only thing on the wire. And all carriers base-probe off one
  shared epoch every 4 s — a ~0.25 Hz dip correlated across the pool. With the
  flag: the feedback interval is jittered and an idle carrier (nothing received
  to report) falls quiet, relying on the jittered ~5 s keepalive (well inside
  the 15 s dead-link timeout) — so a near-idle tunnel stops beaconing, the case
  a low-traffic tunnel is most exposed in. The shared probe schedule is jittered
  off its fixed grid by a deterministic per-period offset — every carrier shifts
  the k-th probe alike, so the pool still backs off together (a clean min-RTT
  measurement) but not on a fixed period. Lab: no throughput/ping regression
  (icmp reverse, camo on vs off); idle packet rate roughly halved (the residual
  was a lingering test connection keeping the tunnel active). Honest limit:
  during an active download the receiver→sender direction is mostly feedback,
  so its cadence is jittered, not removed.
- **CA2 — the link id.** `uniqueLinkID` drew uniform-random 16-bit ids, so a
  tunnel showed N unrelated, stable ids to one host — which no real host sends
  to one peer. With the flag, icmp link ids come from one random pid-like base
  with small ascending steps, so they look like a host's own related ping
  processes. The id is only a demux label the peer reads back, so this is
  compatible with an old peer. Honest limit: on a host with a large `pid_max`
  the id *value* is already ~uniform, so the real id tell is count and
  persistence; this clusters them but a tunnel still holds its ids far longer
  than a ping.
- **Evaluated and deferred.** CA3 (hold packets under the field's size
  threshold) is the one discriminator the field proved, but it costs ~6-7x
  throughput and a flood of small packets is itself a new tell under heavy
  traffic, so it is left as a possible emergency low-rate mode, not built.
  CA4 (make the echo sequence a plain counter / couple the two directions so
  a reply echoes a request like a real ping) was investigated in full and not
  built: every variant trades the sequence tell for a worse one — a two-id
  scheme makes request and reply ids mismatch (real ping echoes the id);
  coupling the directions forces silencing the host's own ping and caps the
  upload direction — while the field filter keys on size and volume, not
  sequence correlation (the tunnel ran for a long time with the existing
  sequence mismatch). The honest high-value, low-risk wins were CA1 and CA2.

## Phase Y — the stage rule, re-measured against the field, and fixed where it lost

The Phase X field test (build 37e9f2f9c2e0, on the live reverse icmp tunnel)
found the opposite of the lab: under saturation the pool rules off
(`HS2_FAIR_SHARE=0`) carried 12% more per CPU-second than the default. The lab
reproduced it once the regime matched the field — asymmetric **and** starved.

- **Three lab regimes (same build, sender starved, interleaved pairs).** Under
  a hard CPU quota (symmetric: all the sender's threads stop together) the
  default wins big — 598 vs 375 Mbit/s per CPU-second, and in a profile 291 vs
  103 Mbit/s: the rules-off path wastes CPU sealing datagrams it then drops.
  Under `cpu.shares` with busy loops but hs2 given ~1.1 cores (asymmetric, not
  really starved) the two tie (~530 each). Under shares starving hs2 to ~0.4
  core (asymmetric **and** starved — the field: a co-tenant like an x-ui panel
  holds one core while the scheduler takes CPUs from hs2's threads one at a
  time) the rules off win by ~27% (615 vs 485), reproducing the field.
- **Why.** The machinery is cheap (a profile puts the fair queue, the stage
  bookkeeping and the pacer in the low single digits; the rules-off path even
  spends more on futex/select). The gap is the effect of the rate clamp on
  scheduling, not its cost: a stage-limited carrier's rate is clamped to ~2x
  what it sends, which shrinks the pacer's token bucket. When the scheduler
  puts the send goroutine aside for tens of ms, the 10 ms late credit let it
  drain only a little of the backlog in the slice it got, so it sent under its
  rate. The rules-off path leaves the rate unclamped (startup, `r` up to 23x),
  so its bucket is large and it drains the slice.
- **The fix (Z).** When the host CPU meter says the server is saturated
  (`host_saturated`, plumbed from the status loop to the data path via
  `udpcarrier.SetHostSaturated`), a stage-limited pacer with data waiting holds
  a larger catch-up — 50 ms of its rate instead of 10 ms (`pacerSatCredit`) —
  so it fills the slice the scheduler gives it. Gated on saturation and on the
  carrier being stage-limited (there the CPU, not the path, is the limit, so
  the burst is absorbed without a standing queue); on a host with room, or
  under a symmetric quota (where the meter does not read saturated), pacing is
  unchanged. Interactive packets take the fast lane, so their latency is not
  the cost.
- **Measured (lab, shares starving hs2 to ~0.4 core, 4-5 interleaved pairs
  against the rules off).** The default rose from 485 to ~555 Mbit/s per
  CPU-second median: the ~27% gap narrowed to ~14%, with ping under load
  better than the rules off (p50 31 vs 33 ms, p99 95 vs 116 ms, equal loss).
  A deeper send queue under saturation was tried too and dropped: it moved the
  number by nothing. The residual ~14% is deliberate — closing it fully means
  leaving the rate unclamped like the rules off, which floods a shared path's
  buffer when the CPU frees (Phase W8 / the simulator: hundreds of path drops),
  the behaviour the stage rule exists to prevent for a pool of real users on
  shared paths. Under the hard quota the default still wins (616 vs 378): no
  regression there.
- Two field-report log/wording fixes, source only: the FEC-at-ceiling log
  juxtaposed the governor's pool-wide loss estimate and the worst carrier's
  measured loss as if one bounded the other (it read "loss 39.2% (worst carrier
  27.5%)"), and "below its ceiling" read as if parity had dropped when it only
  meant no carrier was capped — both reworded. The `carriers` legend said
  `C=CPU-bound`; it now says `C=held by its send stage (CPU/socket)`, the
  code's actual meaning.

## Phase X — a sender short of CPU: seen, and sending more with the same CPU

### From the real-server report (build 2b2e2c8c3f86; reverse, tun over icmp)
- The Kharej server (2 cores, shared with another hs2 tunnel) was 96% busy
  and tasks waited for a core 76-87% of the time (the kernel's CPU pressure,
  PSI), while hs2 itself used 52-70% of one core. Its only CPU warning
  compares hs2's own use with 90% of every core, so it never fired, and
  nothing — status, installer, doctor — said the server was saturated.
- In that state no carrier sent 80% of its allowance: all stayed in startup
  (`S`, never `P`), the allowance stayed where startup had left it (18x what
  was sent), and the W3 rules never engaged. The release before Phase W
  does the same; the lab reproduces it with the sender's cores taken.
- The status showed a `pacer` drop counter that nothing ever incremented.

### X1 — the server's CPU and the send stage, visible (no data-path change)
- `hs2 status`'s `cpu:` line shows the whole server next to hs2: busy across
  all cores (softirq and steal when above 1%) and the share of the time
  tasks waited for a core (PSI avg10), with `SATURATED` once it stays at
  90% busy or 40% waiting for three samples; the log says once when that
  starts and once when it ends (once clear for 10 s). `net:` shows the IP
  packets the kernel discarded on output since hs2 started (Ip OutDiscards)
  — for the whole server, and on an icmp listener including the kernel's own
  echo reply to every tunnel packet, which the echo guard drops on purpose:
  shown, not alarmed on. This tunnel's own queue drops over icmp are counted
  exactly instead (X5: `send_refused`).
- `hs2 doctor` gains `server cpu`: the server measured over one second, PSI
  over 10 s and a minute, and what the running hs2 tunnels use in all; a
  warning when it is short of CPU.
- The send stage, over the last couple of seconds: what the pacers sent,
  the share of the time the carriers' writers waited for pacer room, the
  mean wait in the fair queue, and the mean socket write with the datagrams
  per write (`sending:`); the pool's fair share and the queue its busy
  carriers see (`pool:`). Each `carriers:` entry adds what it really sent
  (`s`) and the queue and smoothed round trip its rate control sees
  (`qQUEUE/SRTT`). Lab, the sender's two cores shared with two busy loops:
  `writers waited for pacer room 55% of the time`, every carrier `S` with
  `s` under half of `r`. (Writers wait as long on a full path — 74% behind
  a 30 Mbit/s bottleneck — but there the carriers are `P`, `s` close to
  `r`, with a 15 ms `q`.)
- The dead `pacer_dropped` is gone; `send_refused` counts the datagrams the
  kernel refused on a raw socket (a soft error), which the peer counts as
  path loss.
- The installer's tunnel screen shows the same CPU line and `Net:` line.
- The status file gains `host_cpu_pct`, `host_softirq_pct`,
  `host_steal_pct`, `host_cores`, `psi_cpu10`, `psi_cpu60`,
  `host_saturated`, `host_out_discards`, `send_refused`, `share_mbit`,
  `busy_queue_ms`, `send_mbit`, `send_held_pct`, `fq_wait_ms`, `write_us`
  and `per_write`.

### X3 — a lab sender short of CPU, the same every run
- `lab/cpuquota.sh`: two namespaces, reverse tun over icmp, 4 carriers,
  8 downloads; the Kharej hs2 (the sender) runs under a hard CPU quota
  (`QUOTA` cores, enforced every 10 ms) on its own two CPUs, everything else
  on two others (the receiving veth's work too). Busy loops beside it, as
  before, measured the scheduler's share, which moved with nice and the load
  (the same build gave 92 to 416 Mbit/s); a quota gives the sender the same
  CPU every run, so builds compare on Mbit/s per CPU-second. Prints the
  carriers' flags, the `sending:` line, ping under load and the kernel's
  output discards; `TRACE=1` adds a Go execution trace, `RATE=30mbit` makes
  the path the limit instead.
- Baseline (this phase's monitoring, no data-path change): 0.6 core 134-158
  Mbit/s, 234-281 Mbit/s per CPU-second, every carrier `S` and none `P` in
  every sample — the field regime; 1.2 cores 632 Mbit/s, 557 per
  CPU-second; no quota (2 cores) 696 Mbit/s, still all `S` (nothing else
  limits it), and 923 tunnel packets the kernel discarded on output. A
  starved sender is also a less efficient one: half the CPU, a quarter of
  the throughput. Behind a 30 Mbit/s path the three busy carriers are `P`
  at 9.3-10.1 Mbit/s against a 9.6 fair share, `q` 15 ms, ping p99 23 ms.

### X4 — less CPU per datagram, and no report waits on a writer
- **Sealing in place.** A data datagram was sealed into a new frame (three
  buffers, four copies of the payload: the plaintext frame, the ciphertext,
  the length-prefixed frame, then the carrier's `[seq][ciphertext]`), and
  the length prefix was cut off again. It is now built and encrypted in one
  buffer, in the carrier's wire layout (`core.Session.AppendDatagram`):
  the same bytes on the wire (a test checks them against the old path byte
  for byte, for every padding), 2552 → 948 ns and 4240 → 16 bytes
  allocated per 1300-byte datagram.
- **The FEC encoder's lock.** A carrier's writer holds the encoder's lock
  while its pacer applies backpressure, so the feedback that sets the loss
  estimate — on the carrier's receive loop — and the status that reads the
  encoder's counters — on the pool's loop — waited for the pacer too: under
  a starved sender, for most of the time. The loss estimate, the counters
  and the parity ratio are now read and set without that lock.
- Lab, sender at 0.6 core (`lab/cpuquota.sh`, 8 interleaved pairs against
  the build before): 167 → 195 Mbit/s on average (ahead in 6 of 8 pairs),
  297 → 340 Mbit/s per CPU-second, ping p99 under load 24.5 → 19.6 ms. Every
  carrier still `S`, never `P`: the controller's side of it is X6.

### X5 — icmp carriers no longer queue on one socket lock
- All carriers to one server sent on one shared raw socket, so their writes
  queued twice: on Go's per-socket write lock, then on the kernel's
  `lock_sock` (a raw socket that builds the IP header takes it for each
  send). On a sender short of CPU, a writer the scheduler put aside while
  holding them held every other carrier back (the field's profile: 16% in
  the scheduler, pacers waiting on the descriptor's lock).
- Now each icmp receive socket has a send-only `IPPROTO_RAW` socket beside
  it (dial side and listener). The carriers build the outer IPv4 header —
  from the receive socket's own TTL, TOS and DF policy, so the packet on
  the wire is the one the kernel built before (a test compares the
  kernel's header with the new one, both directions) — and send with
  `sendmmsg` without Go's lock (the kernel's raw `IP_HDRINCL` path takes no
  socket lock either). An `IPPROTO_RAW` socket receives nothing, so the
  per-socket receive cost the shared socket removed does not come back.
  A reply too big for the device without DF still goes out fragmented
  through the shared socket; a datagram the kernel refuses with a soft
  error is dropped and counted (`send_refused`), as before — and so is one
  the device's queue drops, which on a raw socket the kernel reports as
  sent: the send-only socket has `IP_RECVERR`, so `send_refused` counts
  this tunnel's own queue drops exactly. A datagram refused for a reason of
  its own (a firewall's `EPERM`, ...) goes through the shared socket, which
  sends it or reports it as before; only a failure of the socket itself
  turns it off for good, logged once, and its carriers carry on on the
  shared one. A kernel before 6.4 does not know the `IP_PROTOCOL` control
  message (policy routing by protocol): the packets go without it. TTL and
  TOS are read when the socket opens: a later change of
  `net.ipv4.ip_default_ttl`, or a route's hoplimit, is not followed. gre,
  ipip and ipx keep the old path: the kernel's default DF policy for them
  gives each packet a hashed IP id a built header cannot reproduce.
- A udp listener's batch sends drop Go's lock the same way (the kernel's
  udp send path takes no socket lock unless corked).
- `HS2_RAW_TX=0` turns both off; `HS2_RAW_BATCH=0` (one datagram per call)
  does too.
- Measured: the design's lab (busy loops beside the sender) 103 → 141
  Mbit/s median at saturation, 24% less CPU per Mbit/s; the send path alone
  on 4 idle cores 4.8 → 7.0 Gbit/s; no difference under a steady 64 Mbit/s.
  Under a hard CPU quota (`lab/cpuquota.sh`, 0.6 core, 7 interleaved
  pairs) it is neutral — 202 vs 190 Mbit/s, within the noise — with ping
  p99 under load 21.1 → 17.8 ms: a quota stops all of hs2's threads at once,
  so no thread is put aside holding the lock there. The field measurement
  compares it on and off.

### X6 — a carrier its send stage holds back (the rate control)
- **The signal.** A carrier that does not use its allowance (under 80% of
  it) while the pool's send queue for it drops packets, or its writer waits
  for room in its pacer for a quarter of the report or more, is held back
  by its send stage — the CPU, the socket — not by the path. Not under the
  pool's policer cap (its budget holds carriers back on purpose), not in a
  base probe (the rate is turned down on purpose there: a path-bound
  carrier's writer waits all the time), and only with the pool rules on.
- **What it changes.** Startup counts such a report like one that used the
  allowance, and leaves with the capacity estimate at twice what got out
  (not the allowance startup had reached: 2.9-18x what was sent); after
  startup the estimate stays within twice what gets out while the stage
  holds the carrier back; once it lets go (the allowance used, no queue),
  the estimate regrows 25% per report — per round trip on paths over 100
  ms — until a queue stands while the carrier uses its allowance (a queue
  another tunnel's burst puts on the path while this carrier is held back
  says nothing of its send stage, and no longer ends the regrowth). The
  pacer of such a carrier, with data waiting, keeps the credit of up to 10
  ms of a late wake-up instead of losing everything past 2 ms — on a busy
  server the 2 ms cap alone held a pacer to ~0.3-0.5x its rate; every other
  pacer keeps the 2 ms cap. `hs2 status` flags such a carrier `C`.
- **Measured.** Unit tests (startup exit within 7 reports and an allowance
  within 2.1x, the regrowth, nothing changed while the allowance is used,
  under the policer cap, or in a base probe; mutations of the probe rule and
  of the wait signal caught); the pacer at a 6 ms late wake-up sends 0.28 of
  its allowance with the cap and 0.95 with the credit. Simulator (a sender
  of limited CPU, Reno flows, the fair queue and the single writer): no
  carrier left in startup (all, before), the allowance at most 2.3x what
  got out (up to 6.4x), throughput the same while CPU-bound, after the CPU
  frees 0-86 path drops (0-477) and 166-168 against 174-177 Mbit/s on the
  plain scenario. Lab (`lab/cpuquota.sh`, 0.6 core, 5 interleaved pairs
  against the build before): 212 → 270 Mbit/s (ahead in every pair), 371 →
  484 Mbit/s per CPU-second, carriers `C` with an allowance 1.4-1.6x what
  they send; ping p99 under that load 21 → 29 ms — the same CPU now moves
  27% more, and the receive path waits longer for it: at the same
  throughput (157 Mbit/s) ping is no worse (p50 1.5-1.7 against 2.0-3.0
  ms). Behind a 30 Mbit/s path nothing changes (carriers `P`, `q` 15-18 ms,
  the same throughput and ping), and with `HS2_FAIR_SHARE=0` the build
  behaves as the one before.
- Cost: after the CPU frees, a carrier on a path that other traffic fills
  now and then regrows more slowly than with no rule (simulator: 66-89
  against 79-140 Mbit/s, where the rule-less carriers release their stale
  allowance into the path's buffer: up to 477 drops).

### X7 — fixes from the verification (five agents, each under 20 minutes)
- **The send-only socket (X5).** On a kernel before 6.4 (Ubuntu 22.04's
  5.15, Debian 12's 6.1) the `IP_PROTOCOL` control message is refused; when
  several carriers sent their first datagram at once, all but the first to
  notice read that refusal as a broken socket and turned it off for good
  (30 of 200 rounds in a test that makes the kernel refuse it). Each send now
  knows whether its own control data carried the message. A firewall's
  `EPERM` on one datagram turned the socket off for good too; now only a
  failure of the socket itself does. A full device queue's drops, silent on
  a raw socket, are reported (`IP_RECVERR`) and counted.
- **`net:` and the log (X1).** On an icmp listener, the kernel answers every
  tunnel packet with its own echo reply, which the echo guard drops on
  purpose — and counts each in Ip OutDiscards: the status said "tunnel
  packets lost before the wire" and the log said so every minute, for
  nothing (500 tunnel packets in, exactly +500). The count stays in the
  status as the whole server's, explained, and is no longer logged; this
  tunnel's own queue drops are in `send_refused`. The saturation line now
  clears only after 10 s below its marks (one dip used to flap it), and a
  sample that measures nothing changes nothing.
- **The send-stage rule (X6).** A base probe of a carrier that had been
  short of CPU refreshed its `C` flag and late credit every 4 s, long after
  the CPU had freed (on a path that never queues, 189 of 359 reports); the
  probe is now left out before the flag is set, a stage rate unused for 10 s
  is forgotten, and the flag goes off by itself when feedback stops. The
  pacer's late credit reached the data lane only when a batch was cut short:
  the data lane counted as idle, and the credit was cut back to 2 ms on the
  next round — 0.77 of the allowance with batches at 48 Mbit/s, none without
  batches. A pacer held back by its send stage now keeps up to 10 ms of its
  rate in its bucket while data waits (0.97 with batches, 0.95 without;
  on-time pacers never fill it, other carriers keep 2 ms).
- **The status.** `sending:`'s rate is now what the pacers wrote over the
  whole status interval (it was the last report's tenth of a second); the
  `carriers` summary above 32 carriers counts `C`.
- Docs: "30% less CPU per Mbit/s" was 30% more Mbit/s per CPU-second (23%
  less CPU per Mbit/s); the CPU-bound sign is `C`, not `S`; the socket write
  holds no shared lock on icmp any more; VALIDATION's thresholds now match
  `hs2 doctor`, the pool is fixed max first, and V13 measures Phase X.
- Checked and sound: the in-place seal byte for byte (mixed versions both
  ways in the lab), parity sizing under a concurrent `SetLoss`, every
  encapsulation direct and reverse, a two-address icmp listener, the header
  equivalence and the fragmenting fallback, `HS2_FAIR_SHARE=0` and
  `HS2_RAW_TX=0`; the installer's lines (shellcheck, both copies identical).

### X2 — documentation brought in line with the code
- The rate controller's description (`udpcarrier/rate.go`) named a
  `highQueue` hold band and 25%/6% growth that no longer exist: it now says
  what the code does (a proportional queue term around 10 ms; a capacity
  probe of 4% per report, per round trip past 100 ms; startup frozen on
  reports that do not use the allowance — until X6's rule, for a sender
  short of CPU). Phase W's numbers above corrected
  to the current simulator; README's status fields and the `carriers` line
  format; VALIDATION's rollback target, V11 and V12.

## Phase Z — the user-port path: slow readers, and a dying link

Both changes are on the stream path (`mtcp`, `l3mtcp`, `tls`) that carries
the users of `forward_ports`. The map of the code they start from is
`docs/architecture/`.

- **Z1 — an adaptive per-stream receive window (smux).** A user whose own line
  is slower than its share of the link (a phone on a weak connection pulling a
  large download) kept reading, so the wedge guard never saw it, yet with the
  fixed 2 MiB stream window each such stream held 1–2 MiB of the link's 8 MiB
  session buffer: five to eight of them on one link emptied it and every other
  user of that link crawled at their pace. smux v1.5.24 is now carried in
  `hs2-src/third_party/smux` (a `replace` in `go.mod`) with an adaptive window:
  a stream starts at 256 KiB (what a peer assumes before any update) and, at
  each window update, shrinks by the backlog its reader left unread over
  `StreamLagTarget` (128 KiB), down to 64 KiB, or doubles up to 2 MiB when the
  reader ran dry — unless the session's reader waited for buffer space
  meanwhile. The wire format is unchanged; either end works with an old peer.
  Lab (200 Mbit, RTT 100 ms, one fast user next to the rest, 2 MiB / 8 MiB):
  16 readers at 2 Mbit/s: the fast user 2.0 → 102–113 Mbit/s, interactive p99
  1.08 s → 0.13–0.14 s; 8 readers at 8 Mbit/s: 19.7 → 102–106 Mbit/s; four apps
  that never read: the link stopped (0) → 117 Mbit/s; three frozen after 8 MB
  plus 8 slow readers: 13 → 105 Mbit/s (the prototype; after the review fixes
  below the same matrix never emptied the bucket). On the real stack
  (`TestSlowReadersDoNotStallLink`, 12 slow readers): the fast user moved
  0.7–0.9 MiB in 3 s with the fixed window, 520–600 MiB with the adaptive one.
  Single-stream steady throughput within 3% of the fixed window at RTT
  50–300 ms. Honest costs: a new stream reaches the full window a round trip or
  two later (time to 1 MiB ×1.4, to 8 MiB ×1.2–1.4); and a reader that slows
  down *after* keeping up still holds what it was granted until it has read it
  — a link stalled ~7 s in that case, where with the fixed window it stayed
  stalled for the whole download. Lab knobs: `HS2_TUNE_SMUX_MINSTREAMBUF`,
  `HS2_TUNE_SMUX_LAGTARGET` (0 = the fixed window).
- **Z2 — new users keep off a link that is dying.** A link black-holed under
  load stopped moving its flows, so for the 8–14 s before the stuck or suspect
  verdict it looked the lightest of all and drew new users, who then waited
  ~20 s for it to die; and `openStream` waited for a stream's SYN without a
  limit of its own (smux's 30 s, three tries). Now a link is *lagging* while
  its traffic is seen waiting on the path right now — its oldest control ping
  unanswered for 2 s, its writer inside one socket write for 2 s, nothing heard
  for 10 s, no answer to a newly opened stream within 1 s (twice the link's
  round trip if longer), or a slow open on it within the last 10 s — and a
  lagging link takes new users only when no link that does not lag can, unless
  more than half the serving links lag (then it is the path, and the usual
  order spreads the users). Each try at opening a user stream waits 3 s: if its
  SYN has not gone out, the connection is tried on another link too and the
  first to open takes it (a stream whose SYN did go out is not doubled — the
  exit would dial the panel twice). Lab (four real links, 5 runs, 100 new
  users each): a busy link black-holed — users placed on the dead link
  10.4% (until 9–11 s) → 2.0% (all within the first 1.4 s); an idle link
  black-holed 10.6% → 2.0%; a link whose writer is stuck — longest open
  19.5 s → 3.1 s; all links healthy and busy, or all throttled — no regression
  (throttled: time to first byte p99 14.9 → 13.7 s).
- **Review fixes (independent review and re-measurement, all applied):** the
  window shrinks by the excess, not twice it (a reader at a steady 50 Mbit/s
  lost 6.7% to a starve/overshoot cycle; slow readers under a heavy upload got
  85% of their rate — both back to 100%, the stall protection unchanged); a
  window update that fails to go out is sent again at the next read (the peer
  could wait for it for good); a lag target under 4 bytes no longer pins the
  window; a lab override that leaves no room for the adaptive window says so in
  the log; during the refill hold a lagging link no longer takes a user when
  the links that do not lag are full; a try that fails starts the next link at
  once; no further link is tried once any try's stream is open; the open-answer
  stamp is taken before the header goes out; a lone serving link that lags
  yields to a healthy retiring one.
- **Still open:** users placed on a link in the first second or so after it is
  black-holed (before any signal) still wait for it to die (~20 s); a
  reader that slows down after keeping up (above).

## Verification, every phase

- Go: `go test ./...` and `go test -race ./...`.
- Bash: sourced-core unit tests + pty tests for the interactive prompts
  (including `tm_edit_test.sh`, which drives the menu under the real shell
  options).
- Probe behaviour is checked differentially against a real net/http HTTPS
  server, not against our own expectation of it.
- `hs2-src/install/tests/release_files_test.sh`: the two `install.sh` copies are
  byte-identical and the published sha256 files match their targets.
- `shellcheck -S warning` clean on `install.sh`.
- An independent adversarial review per phase; confirmed findings fixed before
  shipping.

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
  until it ends, 5 min at most. The draining link keeps them only while the
  pool can refill without its slot: it does not count against max (all links
  stay within max + an eighth), the reverse exit is now asked for the
  replacement at once (before: only after the bad link closed), and when the
  pool is at its ceiling and short of serving links the oldest draining links
  close as before. Log: `link N degraded for 45s — its connections that moved
  no data for 15s (idle or stuck) are closed now …`, once a minute `closed N
  connection(s) on degraded links that moved no data for 15s`, and at the
  ceiling `N degraded link(s) (…) closed with their M remaining
  connection(s): the pool is at its ceiling and needs their slots`.
- MPTCP off: every listener is plain TCP (see the finding above). The
  carrier, user-port and dgtun listeners set it explicitly; go.mod's `godebug
  multipathtcp=0` covers the rest and the tests.

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

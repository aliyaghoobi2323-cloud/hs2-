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

New tests: `cmd/hs2/cover_test.go` (determinism + golden, 500-seed uniqueness,
seedless==legacy with the `86dfcf86…` check, structural invariants: no external
requests, no JS, inline favicon, light+dark, balanced tags, size varies; served
headers: Content-Type, ETag, 304, root-only 404) and
`install/tests/cover_seed_test.sh` (every builtin heredoc seeded, seed
independent of the key, migration adds one / leaves a custom backend alone /
never duplicates, valid JSON). Verified live over real TLS: two seeds serve two
different pages, a seedless config serves the exact legacy page.

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

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
- **U4:** rekey + rebuild the `hs2://` link from the config (round-trip tested).

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

## Verification, every phase

- Go: `go test ./...` and `go test -race ./...`.
- Bash: sourced-core unit tests + pty tests for the interactive prompts.
- `hs2-src/install/tests/release_files_test.sh`: the two `install.sh` copies are
  byte-identical and the published sha256 files match their targets.
- `shellcheck -S warning` clean on `install.sh`.
- An independent adversarial review per phase; confirmed findings fixed before
  shipping.

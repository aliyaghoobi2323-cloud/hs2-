# Reality in the hs2 stream core — architecture and phase plan

Branch `reality35`, base `5ae865ee`. Phase 1 of 6: a pure refactor that
introduces a pluggable **front** (the TLS handshake / front door of each link)
with exactly one implementation, `tls` = today's behaviour.

## 1. Target architecture

```
users (mtcp user ports / l3mtcp hs0 tun)
  -> link pool + LB + autopilot + autowin + stripe + flap + defaults   (engine/)
  -> tunnel auth: tls-exporter bound, mutual (tlscarrier/auth.go)
  -> FRONT                                                             (tlscarrier/front.go)
  -> TCP
```

FRONT variants:

| kind      | what the censor sees | status |
|-----------|----------------------|--------|
| `tls`     | own cert/domain, uTLS Chrome ClientHello, nginx-identical cover (tlscarrier) | phase 1 (today's code, wrapped) |
| `reality` | SNI of a real site; hidden signal in the ClientHello session_id (x25519 ECDH with the listener's static key + short id); temporary cert verified by the shared secret; unauthenticated connections spliced byte-for-byte to the real target, with byte/rate limits | phase 2 (per-tunnel `front`) |
| `both`    | hybrid on one port: per-ClientHello classification (reality signal present -> reality, else the tls front) | later |

Front profiles are per IP in the `lb` section, announced by the listening side
to the dialer over the authenticated control channel (lb advert), and work for
direct and reverse, for the mtcp / l3mtcp / tls carriers (all three run
`runStream`, cmd/hs2/main.go).

An experimental standalone Reality carrier exists in `hs2-src/reality`
(single link, legacy engine, `runReality`): `clienthello.go`, `tls_signal.go`,
`auth.go`, `relay.go` hold the session_id signal, auth and splice/relay ideas
to reuse in later phases.

## 2. Map of today's link establishment (base 5ae865ee)

### 2.1 Who dials whom

| mode | dialer (TLS client) | listener (TLS server) |
|------|---------------------|-----------------------|
| direct  (`reverse:false`) | Iran edge (`mode:dial`) | kharej exit (`mode:listen`) |
| reverse (`reverse:true`)  | kharej exit | Iran edge |

All in `cmd/hs2/main.go runStream`:

* direct edge: `cfg.Dialer = engine.NewMTCPDialer(addr, sni, key, bind_local_ip)`;
  with lb on, `engine.NewLBLinkDialer(lb, per-path factory)` building one mtcp
  dialer per `LBPath` with SNI `lbSNIFor(lbCur(fc), p.Remote)`.
* reverse exit: `cfg.RevDial` / `cfg.RevDialScout` (2 s connect) /
  `cfg.RevDialPath(p, scout)` (lb), consumed by `engine/stream_kharej.go`
  reverse pool.
* direct exit listener: `listenLB` (one listener per lb IP, `ListenerSet`),
  `cfg.Server = &tlscarrier.Server{SharedKey, GetCertificate: certSelector,
  BackendAddr, BackendFor: coverBackends}`; `RunKharej` accepts and calls
  `Server.Handle` per conn.
* reverse edge listener: same `tlscarrier.Server` as `cfg.RevServer`, with
  `engine/stream_reverse.go acceptReverseLinks` (cap check, `newEdgeLink`).

### 2.2 Dial path (tlscarrier/carrier.go `DialFromTimeout`)

1. `net.Dialer{Timeout: connect}` (8 s default, 2 s scouts), optional
   `LocalAddr` = bind IP (invalid bind IP is an error, never a silent fallback).
2. Keepalive (`dialKeepAlive`, HS2_QUIET_IDLE 45 s), `tuneTCP` (dpi.go:
   TCP_USER_TIMEOUT etc.).
3. uTLS: `utls.Config{ServerName: sni, InsecureSkipVerify: true}`,
   `newClientConn(wrapBatch(raw), cfg, addr, clientHello)` (resume.go: Chrome
   133 preset `HelloFromEnv`, optional session resumption HS2_TLS_RESUME).
4. Handshake under `authTimeout`; TLS 1.3 required (`clientEKM`).
5. **No certificate verification**: both sides prove the shared key bound to
   the TLS exporter (EKM): `makeClientAuth` -> `readServerProof` (auth.go,
   minute bucket, nonce, padding HS2_AUTH_PAD).
6. Returns `*Carrier{conn: uconn}`; engine wraps it in smux (`newEdgeLink` /
   exit `newSession`).

SNI per LB path: `lbSNIFor` = the remote IP's own `sni` in the other side's lb
list, else the top-level `sni`.

### 2.3 Listen path (tlscarrier/server.go `Server.Handle`)

1. `tuneTCP`, quiet keepalive.
2. `tls.Server(wrapBatch(raw), cfg)`: `GetCertificate` = `certSelector`
   (cmd/hs2/certsel.go: per local IP and SNI, hot-reloaded `certReloader`s; lb
   stage 3 per-IP `cert_file/key_file`), ALPN http/1.1, shared ticket keys with
   Resume.
3. Handshake under `handshakeTimeout` 10 s; a plain-HTTP first record gets
   Go's / nginx's 400 (`plainHTTPReply`, HS2_COVER_NGINX), anything else
   closes like crypto/tls.
4. First read deadline `firstReadTimeout` (60 s nginx / 30 s Go), one record.
5. `parseClientAuth` OK -> replay check -> `makeServerProof` -> `onTunnel(car)`
   (the tail call). Not OK -> cover: `forward` to `BackendFor(local, sni)` or
   `BackendAddr` (builtin nginx-identical cover site per IP/sni, cover seed).

### 2.4 LB per-IP sni/cert and announcements

* Config `lb.{iran,kharej}[]: {ip, weight, sni, cert_file, key_file}`
  (cmd/hs2/lb.go `lbIP`); listening side: `lbListenIPs`, `newCertSelector`,
  `coverBackends`; dialing side: `lbPaths` / `lbPathsAdvert` -> `[]LBPath`.
* Stage 6 advert (engine/lb_advert.go): over an authenticated smux stream
  (`kindLBAdvert`, peer cap `capLBAdvert` from kindInfo). Edge (smux client)
  writes `[kind][advert]`, exit replies `[advert][report?]` (lb_report.go).
  `advert = [ver=1][n] n x ([iplen][ip][weight])`. The listening side's list
  wins for remote addresses; the dialing side keeps its own per-IP `sni`.
  Repeated every 15 s and after SIGHUP (`reloadLB`).

### 2.5 Status

`lbStatusNow` / `lbStatusLine` (cmd/hs2/lbreload.go), `LBAdvertState`
(peer list, rejected, last exchange), per-path `LBPathStat`
(engine/lb.go) in the status file; `hs2 doctor` `checkLBAdvert`.

## 3. Phase 1 (this change): the front seam

`tlscarrier/front.go`:

```go
type FrontProfile struct {
    Kind string                 // "" | "tls" | later "reality" | "both"
    Target string               // reality: real site host:port   (reserved)
    ServerNames []string        //          SNIs                  (reserved)
    PublicKey string            //          x25519 public         (reserved)
    PrivateKey string `json:"-"`//          listener only, never announced
    ShortIDs []string           //                                (reserved)
    FallbackMaxBytes, FallbackRateBps int64 // splice limits      (reserved)
}
type DialPath struct { Addr, SNI, BindIP string; Connect time.Duration; Key []byte; Profile *FrontProfile }
type Dialer   interface { DialFront(ctx, DialPath) (*Carrier, error) }
type Acceptor interface { AcceptFront(ctx, raw net.Conn, onTunnel func(*Carrier)) } // tunnel -> onTunnel; else handled as cover
TLSFront{}.DialFront == DialFromTimeout(addr, sni, key, bind, connect||8s)
(*Server).AcceptFront == Handle
DialerFor(profile) / DialPathFront(ctx, DialPath)   // tls only; others ErrFrontUnsupported
```

Wiring (all behaviour-neutral):

* `engine.KharejConfig.Server`, `IranConfig.RevServer`, `acceptReverseLinks`
  take a `tlscarrier.Acceptor` and call `AcceptFront`.
* `mtcpDialer` holds a `tlscarrier.Dialer` (+ profile) and calls `DialFront`;
  `NewMTCPDialer` = tls front; `NewMTCPDialerFront(…, profile)`;
  `FailingDialer` for a path whose front this build lacks.
* `engine.LBPath.Front` (nil = tls); `engine.LBAdvertIP.Front`.
* `cmd/hs2`: `lbIP.Front` (`"front": {...}` per IP, validated: phase 1 refuses
  anything but tls); `lbPaths`/`lbPathsAdvert` set `LBPath.Front` (announced
  front wins, else the local list's); `lbOwnAdvert` announces only
  non-default fronts; `RevDial*`/`RevDialPath`/edge lb factory go through
  `DialPathFront` / `lbPathDialer`.

### Wire compatibility of the announcement

The front extension is OPTIONAL and is never sent while every profile is the
default, so with phase 1 configs the advert bytes are exactly stage 6 (ver=1;
pinned by `TestFrontAdvertDefaultBytesUnchanged`). With a non-default front the
listening side sends ver=2 and appends at the END of its message
`[0xF5][1][len u16][JSON [{ip, front}]]` (after the exit's report). Old
parsers accept ver>=1, read n entries and the report, and never look further;
new parsers read the ext after the report (peeking the magic when there is no
report). Tests: old message <-> new parser and new message <-> old parser
(byte level), and all four old/new edge/exit pairings x direct/reverse over a
real smux stream with the base code embedded verbatim
(`engine/front_advert_test.go`).

## 4. Where later phases hook in

| phase | hook |
|-------|------|
| 2 reality dial side | new `tlscarrier.Dialer` for `kind:"reality"`: uTLS with the borrowed SNI, session_id signal (port `reality/clienthello.go`, `tls_signal.go`), verify temp cert by shared secret; then the unchanged tunnel auth on top. `DialerFor` switch. |
| 3 reality listen side | new `Acceptor`: read ClientHello, classify; authenticated -> own TLS with temp cert -> `onTunnel`; else splice to `Target` with `FallbackMaxBytes/RateBps` (port `reality/relay.go`). Listener chosen per local IP like `certSelector`. |
| 4 `both` | an `Acceptor` that peeks the ClientHello and dispatches to the tls `Server` or the reality acceptor. |
| 5 per-IP profiles end to end | `lbIP.Front` validation accepts reality/both; listener builds per-IP acceptors; `lbAnnouncedFront` announces (PrivateKey never leaves, json "-"); dialer's `LBPath.Front` from the advert; status shows the front per path (`LBPathStat`, `LBAdvertIP.String`). Old dialers ignore the ext and keep dialing tls (works with `both`). |
| 6 validation | A/A and probe batteries, active-probe of the splice, mixed-version, doctor checks. |

## 5. Phase 1 validation (lab, 2026-11-11)

Binaries built from source with the same Go: base `5ae865ee` vs `reality35`.

* Tests (lab 3): `go vet` clean; full engine suite (`-timeout 40m`) ok;
  tlscarrier, cmd/hs2, envdef ok; third_party/smux (own module) ok. New:
  `tlscarrier/front_test.go`, `engine/front_advert_test.go`,
  `cmd/hs2/front_test.go`.
* A/A (labs 1+2, `labslot.sh --all`, u21cmp.sh prod env, l3mtcp reverse,
  3000mbit/40ms/100ms queue/0.01% loss, 150 links, crowd 3000, T=25s),
  6 interleaved rotated reps per case, A = base/base, B = new/new:

| case  | metric | A mean | B mean | B-A | Welch t |
|-------|--------|-------:|-------:|----:|--------:|
| down1 | Mbps | 843.9 | 889.0 | +5.3% | +0.59 |
| down1 | call p99 ms | 188.8 | 179.3 | -5.0% | -1.53 |
| down8 | Mbps | 1492.8 | 1483.5 | -0.6% | -0.18 |
| down8 | call p99 ms | 186.3 | 197.4 | +6.0% | +0.79 |
| up8   | Mbps | 1605.7 | 1558.1 | -3.0% | -0.93 |
| up8   | call p99 ms | 195.7 | 180.9 | -7.5% | -0.82 |
| light | call p99 ms | 178.3 | 181.8 | +1.9% | +0.46 |
| light | game p99 ms | 141.7 | 138.8 | -2.0% | -0.48 |
| light | crowd p99 ms | 150.9 | 154.2 | +2.2% | +1.83 |

  No metric differs significantly (of ~30 comparisons the largest |t| are
  2.12 ping p99 down8 and 2.08 echo p99 light, in opposite directions; links
  150/150 and 0 failures in every run). The change touches only link
  establishment, not the data path.
* Mixed versions (light case, 6 reps reverse + 1 direct): C = old Iran + new
  kharej, D = new Iran + old kharej: all came up to 150/150 links and passed
  traffic with 0 failures, in reverse and in direct mode.
* Active-probe battery (`probe30-battery.py`, 20 probes incl. slow/idle) on a
  base vs refactored listener with the same config: identical results (status
  line, headers, length, close kind, timing).

## 6. Phase 2: the reality front (per tunnel)

Code: `tlscarrier/reality.go` (dial + listen + fallback + counters),
`cmd/hs2/reality_front.go` (config block, validation, wiring, `hs2 reality
keygen`, `hs2 reality check-target`). Default stays `tls`; reality only when a
tunnel has `"front": {"kind":"reality", ...}`.

### 6.1 Config

```
"front": {"kind": "reality",
  "target": "www.site.ir:443",            listener: the real site (host:port)
  "server_names": ["www.site.ir"],         SNIs accepted (listener) / sent (dialer: the path's sni if listed, else the first)
  "private_key": "...",                    listener only (hs2 reality keygen); read from the local file only
  "public_key": "...",                     dialer
  "short_ids": ["61bbbd5ea44185c5"],       hex, 0..16 digits; the dialer sends the first
  "fallback_max_bytes": 262144, "fallback_rate_bps": 65536, "fallback_max_seconds": 600,   0 = these defaults
  "force_target": false}                   accept a multi-tenant-CDN target / foreign names
```

Who needs what: the TLS listener of the links (direct: kharej `mode:listen`;
reverse: Iran `mode:dial` + `reverse`) needs `private_key` + `target`; the
dialer needs `public_key`. Carriers mtcp / l3mtcp / tls (the front is below
them). Load-time checks (`validateFront`, also in `hs2 check`): the generic
profile check; a target or server name on a multi-tenant CDN (the
`multiTenantCDN` list of doctor_deep.go: Cloudflare, Fastly, Akamai,
CloudFront, Google, Azure CDN, CDN77, Gcore, Bunny...) is refused unless
`force_target`; server names must be the target host, a subdomain or a parent
of it (IP targets skip this). `FrontProfile.PrivateKey` keeps `json:"-"`:
`fileConfig.UnmarshalJSON` copies `front.private_key` from THIS side's file
only, so an announced profile can never carry or leak one. lb per-IP reality
fronts are still refused (phase 3); an lb path without its own front uses the
tunnel's (`pathFront`).

### 6.2 Crypto

Dial (`RealityFront.DialFront`): the uTLS `clientHello` preset (Chrome 133,
same as the tls front; no session resumption), `BuildHandshakeState`, then

```
eph     = KeyShareKeys.Ecdhe (the 0x001d share; else the hybrid's X25519 part)
shared  = X25519(eph, listener static public)
authKey = HKDF-SHA256(shared, salt = client_random[0:20], info "hs2-reality-v1"), 32 B
plain   = [ver=1][0 0 0][unix time u32 BE][short_id 8]                    16 B
sid     = AES-256-GCM(authKey).Seal(nonce = client_random[20:32], plain,
          aad = the ClientHello handshake message with session_id zeroed)  32 B
```

written into the hello's 32-byte session_id (`Raw[39:71]`). Every byte of the
hello is authenticated (AAD), so a modified hello fails. Then a normal TLS 1.3
handshake with the listener; `VerifyPeerCertificate` accepts only a leaf with
extension 1.3.6.1.4.1.55738.1.1 = OCTET STRING(HMAC-SHA256(authKey,
"hs2-reality-cert" || SPKI)); TLS 1.3 CertificateVerify proves the listener
holds that key. No CA. Then the unchanged tunnel auth (`makeClientAuth` /
`readServerProof`, tls-exporter bound, mutual): a reality key without the
tunnel's shared key gets nothing.

Listen (`RealityServer.AcceptFront`): dial the target at once (5 s) while
reading the ClientHello (deadline 10 s, <= 32 KB, reassembled across records);
client bytes are forwarded to the target as they arrive. Garbage is detected
from the first bytes (record type 22, version 3, handshake type 1) and spliced
at once. For a complete hello: SNI in server_names, 32-byte session_id, X25519
share(s) (0x001d, else the X25519MLKEM768 tail) -> AEAD open -> version ->
|now - time| <= 90 s -> known short_id -> replay cache (`replayMem`, 5 min >=
the +-90 s window, keyed by the sealed session_id). Pass: the target conn is
dropped and crypto/tls (TLS 1.3 only, ALPN h2/http1.1, no tickets) runs over
the already-read bytes + the socket with a throw-away cert (a per-listener
ECDSA P-256 key; per connection a cert carrying the tag, CN/SAN = the SNI) ->
tunnel auth -> `onTunnel`. Fail: `fallback` splices the whole connection byte
for byte (the already-read bytes first), never decrypted or altered; per
direction free up to `fallback_max_bytes`, then `fallback_rate_bps`;
`fallback_max_seconds` deadline on both sockets. Target unreachable -> the
client conn just closes.

Counters (`tlscarrier.RealityStatsNow`, status file `"reality"`): `authed`,
`fallback_conns`, `fallback_bytes`, `rejected{reason}` with reasons
not_hello, read, timeout, sni, no_session_id, no_x25519, signal (AEAD),
version, time, short_id, replay, target_dial, cert, handshake, tunnel_auth,
tunnel_replay.

### 6.3 Tests

`tlscarrier/reality_test.go`: the tunnel works (both short ids); rejects with
the right reason and the target's answer for a wrong public key, wrong short
id, wrong SNI, client clock +-100 s (60 s passes), a plain Chrome hello
(random session_id); an authenticated hello captured and replayed -> `replay`;
tampered at the session_id, the random and the last extension byte ->
rejected; the bytes reach the target exactly. Probe transcripts (Chrome hellos
with right/wrong SNI, plain HTTP, a truncated record, garbage): the target
receives exactly what the prober sent and the prober exactly what the target
answered. A real TLS target: a standard TLS client gets the target's own
certificate and body through the listener. Fallback limits (400 KB with 64 KB
free at 256 KB/s = about 1.3 s) and the max duration. A tls-front dial never
authenticates on a reality listener. `cmd/hs2/reality_front_test.go`: CDN
target/name refused (unless forced), foreign server names, carrier, missing
keys per side (direct/reverse), private key read from the local file only.
Phase-1 tests adjusted (reality now has a dialer; an incomplete reality
profile is still refused; per-IP reality in lb refused).

### 6.4 Phase-3 hooks

* `lbIP.Front`: drop the "per lb IP is not supported" refusal in
  `lbSideWeights`; build one `RealityServer` per local IP (`listenLB` + a
  per-IP Acceptor, like `certSelector`).
* Announcement: `lbAnnouncedFront` already sends the profile without the
  private key; the dialer's `LBPath.Front` then carries public key, names and
  short ids; `pathFront` falls back to the tunnel's front.
* `both`: peek the hello (`parseRealityHello`) and dispatch: valid signal ->
  `RealityServer.serveTunnel`, else the tls `Server.Handle` on a prefixConn.
* Size mirroring (done in phase 2): `StartSizeMirror` (started by the
  listener in cmd/hs2) fetches the target's chain at start and every 3 h
  (`targetChainSize`: DER bytes + 5 B per extra entry) into `CertPad`; the
  throw-away cert is padded with private extension 1.3.6.1.4.1.55738.1.2 to
  within a few bytes of it (`TestRealitySizeMirror`). Still open: the
  CertificateVerify size (ECDSA P-256 vs an RSA target) and the ServerHello
  key-share group (our crypto/tls picks X25519MLKEM768 when offered) should
  follow the target's choice.
* Doctor: run `checkTarget` (`hs2 reality check-target`) from `hs2 doctor`.

### 6.5 Phase-2 validation (lab, 2026-11-11)

* Lab 3 on HEAD 02847b4b: `go vet ./...` clean; full engine suite
  (`-timeout 40m`) ok (601 s); tlscarrier, cmd/hs2, reality, envdef ok.
* Lab 1, `labslot.sh --all`, u21cmp.sh prod env, l3mtcp **reverse**,
  3000mbit/40ms/100ms queue/0.01% loss, 150 links, crowd 3000, T=20 s, 6
  interleaved rotated reps per case. Same binary in both arms: T = tls front,
  R = reality front on both sides (the Iran listener's target = a local
  `openssl s_server` in its netns). Status file of the Iran listener during R
  runs: `"reality": {"authed": 150, "fallback_conns": 0}` (every link through
  the reality handshake).

| case  | metric | T (tls) | R (reality) | R-T | Welch t |
|-------|--------|--------:|------------:|----:|--------:|
| light | call p99 ms  | 179.7 | 191.1 | +6.3% | +1.28 |
| light | game p99 ms  | 135.4 | 139.4 | +3.0% | +0.46 |
| light | crowd p99 ms | 149.0 | 150.7 | +1.1% | +0.81 |
| light | echo p99 ms  | 102.2 | 121.8 | +19.1% | +2.21 |
| down1 | Mbps         | 757.2 | 697.4 | -7.9% | -0.66 |
| down1 | ping p99 ms  | 130.2 | 109.9 | -15.6% | -2.04 |
| down8 | Mbps         | 1381.5 | 1479.8 | +7.1% | +1.69 |
| down8 | call p99 ms  | 196.5 | 189.5 | -3.6% | -0.62 |
| up8   | Mbps         | 1639.7 | 1564.7 | -4.6% | -1.18 |
| up8   | call p99 ms  | 181.3 | 192.4 | +6.1% | +0.71 |

  Links 150/150 and 0 failures in all 48 runs. Of 24 comparisons the two with
  |t| > 2 point in opposite directions (echo p99 light worse, ping p99 down1
  better): noise, as in the phase-1 A/A. The front only changes link
  establishment; the data path (crypto/tls TLS 1.3 under smux) is the same.
* Direct mode (kharej listens with the reality front, Iran dials): light and
  down8 came up 150/150 with 0 failures.
* Active-probe battery (`probe30-battery.py`, lab 2): nginx 1.24 directly vs
  an hs2 reality listener whose target is that nginx: all 19 probe classes
  (paths, keep-alive, plain HTTP on the TLS port, random bytes, bad methods,
  long headers/URI, no SNI, ALPN, partial request stall 60 s, idle 60 s)
  identical in status line, headers, length, close kind and timing.
* `hs2 reality check-target 127.0.0.1:18443 probe.test` on the lab nginx:
  TLS 1.3 / X25519 OK, ALPN, SANs (reported BAD: the lab cert has only a CN),
  chain WARN (self-signed), handshake 2 ms, `HTTP/1.1 200 OK`.

### 6.6 Known limits (for phase 3+)

* Every connection dials the target at accept (Xray-style, so a prober's
  bytes reach the target as they arrive): an authenticated link also opens
  and drops one target connection. No cap yet on concurrent fallback splices.
* A man in the middle that holds a client's hello and sends it first gets our
  ServerHello (Go's) instead of the target's before the replay cache can
  help; it cannot finish the handshake. Same limit as Xray REALITY.
* A hello that stalls part-way is spliced after 10 s (target already has the
  bytes, its own timeout applies); target bytes that arrive before the hello
  is complete are relayed only once the decision is made.

## 7. Phase 3: per-IP fronts, announcements, "both", fallback, live switch

Code: `cmd/hs2/front.go` (publicFront, lbAnnouncedFront, `frontMux`, status),
`cmd/hs2/lb.go` (`validateLBFronts`), `cmd/hs2/lbreload.go` (precedence,
reload hook), `cmd/hs2/main.go` (per-IP private keys), `tlscarrier/hybrid.go`
(`BothFront`, `BothPolicy`, `FrontCounters`, `ConnectError`),
`tlscarrier/reality.go` (hybrid dispatch, `target_down`),
`engine/lb.go` (path dialer rebuilt when its front changes).

### 7.1 Config (listening side; the dialer needs nothing)

```
"lb": {"enabled": true, "mode": "auto",
  "iran": [                                  (reverse: Iran listens)
    {"ip": "A", "weight": 40},                                   tls (absent = tls)
    {"ip": "B", "weight": 30, "front": {"kind": "reality",
        "target": "www.site.ir:443", "server_names": ["www.site.ir"],
        "private_key": "...", "short_ids": ["61bbbd5ea44185c5"]}},
    {"ip": "C", "weight": 30, "sni": "c.ours.ir", "cert_file": "...", "key_file": "...",
     "front": {"kind": "both", "target": "www.site.ir:443", "server_names": ["www.site.ir"],
        "private_key": "...", "short_ids": ["0c0c0c0c"]}}],
  "kharej": [{"ip": "K", "weight": 100}]}
```

* One `RealityServer` per listening IP with its own target, names, key,
  short ids, replay memory and counters (`frontMux`, dispatch by the local
  IP of the accepted socket; IPs without a front use the tunnel's front).
* `private_key` is read from THIS file only (`fileConfig.UnmarshalJSON`
  copies `lb.<side>[i].front.private_key`; the profile type never
  (de)serialises it).
* Validation per IP (`validateLBFronts`, also on SIGHUP): listening side needs
  `private_key` and a single-tenant target (CDN refusal, names of the target,
  `force_target`); a dialer that names a remote front explicitly needs
  `public_key` (or a copied `private_key`, from which the public one is
  derived). The tunnel-level `front` cannot be `both` (per IP only).

### 7.2 Announcement and precedence

The listening side announces per IP only `publicFront`: `kind`,
`server_names`, `short_ids`, `public_key` (derived from the private key when
not written). Never the private key, the target or the fallback limits
(`TestLBPerIPFrontProfiles` checks the whole wire advert). tls IPs announce
nothing (stage-6 bytes unchanged). Dialer: explicit remote `front` in its own
lb list > announced > tunnel `front` > tls. Old listener (no ext) -> tls; old
dialer (ignores the ext) -> tls: it works on `tls` and `both` IPs, not on a
`reality`-only IP (that answers like the target) — migrate with `both` first.
Profile change (reload on the listener or a new announcement): new links use
the new front (the engine's per-path dialer is rebuilt when `LBPath.Front`
changes); established links keep running and age out; nothing is dropped.

### 7.3 Hybrid "both" listener (one port)

Classification of a complete ClientHello: valid reality signal -> reality;
else SNI in our tls names (top-level `sni` + every per-IP `sni` of this side)
-> the tls front (`Server.Handle` over the already-read bytes, so its
nginx-identical cover applies); else -> cover: splice to the reality target
with the fallback limits. The target dial starts at accept for every
connection, as in phase 2, so the target also sees the first bytes of a tls
hello it is then dropped from (it learns our tls SNI; accepted).

New listener rule (also for `reality`): an authenticated hello is served only
if the target dial succeeded (`rejected.target_down`, conn closed). The
listener never answers while the site it imitates is unreachable; a `both`
dialer then uses tls.

### 7.4 Auto-fallback (dialer, "both")

`BothFront` + `BothPolicy` per path (`local>remote:port`): reality first; a
reality failure after the TCP connect is retried with tls in the same dial
(so a front failure never shows as path loss to the LB); a TCP connect
failure (`ConnectError`) is returned as is (path problem, no tls retry).
After N=3 reality failures in a row the path dials tls only for 10 min
(logged: `front both: ... dialing tls for 10m0s`), then tries reality again;
a reality success resets the count. A path where both fronts fail returns an
error and the LB cuts it as usual.

### 7.5 Status

Listening side: `"fronts": [{"ip", "kind", "counters": {"reality", "tls",
"cover", "rejected"}}]`; dialing side: `"front_paths": [{"path", "kind",
"both": {"mode", "cool_until", "consec_fails", "reality_ok", "reality_fail",
"tls_ok", "tls_fail", "fallbacks", "connect_fail"}}]`; plus the phase-2
process-wide `"reality"` block (now with `target_down`).

### 7.6 Phase-3 validation (2026-11-11)

Unit tests (lab 3): `tlscarrier/hybrid_test.go` — `TestHybridClassify`
(one "both" port: reality dial -> reality, tls dial with our name -> tls,
Chrome hellos for a foreign name and for the reality name without the signal
-> the target's bytes exactly, a tls client with a foreign SNI never reaches
the tunnel), `TestBothFallbackTargetDown` (target down -> `target_down` x3,
each dial still succeeds via tls, cool-down entered once, expires after 10
min), `TestBothPolicyStateMachine` (N, cool-down, reset, connect errors are
the path's, both fronts failing -> error), `TestBothProfile`.
`cmd/hs2/front_lb_test.go` — `TestLBPerIPFrontProfiles` (per-IP private key
from the file; announced = kind/names/short ids/public key only, checked on
the whole wire advert; per-IP refusal of CDN target, missing private key,
missing short ids, unknown kind, foreign names; dialer needs a key; tunnel
`both` refused), `TestLBFrontPrecedence` (explicit > announced > tunnel; no
announcement = tls; public key derived from a copied private key),
`TestFrontMuxDispatchAndReload` (dispatch by local IP, unchanged listener kept
across a reload, tls->both / both->reality), `TestReloadLBSwitchesFronts`
(the SIGHUP path). Phase-1/2 tests adjusted (`both` now has a dialer).

Lab 1 (`/opt/hs2lab/r35/p3`, one slot): reverse l3mtcp, lb auto, 3 paths
(1000mbit/40ms/0.01% each), Iran listens on A 192.168.50.1 (tls), B 51.1
(reality), C 52.1 (both), target `openssl s_server` in the Iran netns; the
kharej has NO front config (everything from the announcement); crowd 3000,
call, game, iperf down x8, T=45 s.

| run | event | heavy Mbps | conn/echo/call/game/crowd failures | crowd p99 | links end (A/B/C) | fronts |
|-----|-------|-----------:|------------------------|----------:|-------------------|--------|
| switch | t=15 s SIGHUP: C both->reality, A tls->both | 1132 | 0/0/0/0/0 | 171 ms | 58/38/57, churn 0 | after: A both reality 15, C reality 16 new links |
| tdown | t=15 s target killed | 908 | 0/0/0/0/0 | 164 ms | 58/37/57, churn 0 | C: 4 reality fails -> tls (19 tls links), B: established links keep serving |
| tdown0 | target killed at start | 954 | 0/0/0/0/0 | 2683 ms | 75/0/75 | B cooled (599 failed dials), C tls 73 |

* Status: Iran `fronts` per IP (e.g. B `reality 38, cover 2`, C `reality 36,
  tls 21, rejected 4`), kharej `front_paths` (C `mode tls, fallbacks 1`).
* The 2-3 `cover`/`sni` rejects on B at start are tls dials made before the
  announcement arrived (a reality-only IP answers them as the target). A
  `both` IP takes them over tls.
* Target down while running: established reality links are not touched
  (they do not use the target), so B keeps its links and gets no new ones; the
  LB does not cut a path whose links are healthy.
* Finding (LB, phase 4): a reality-only path that fails FAST at start (EOF
  right after the hello) is not cooled during the LB start grace
  (`startedLocked`), so ~600 dials in 30 s starve the pool growth (13 links
  for ~30 s, crowd p99 2.7 s, still 0 failures).
* Probe battery (lab 3, `battery.py` = probe30-battery with host:port
  targets, fast probes): listener IPs 127.0.0.1 tls / .2 reality / .3 both,
  target `openssl s_server`. Tunnel sni = another name: B == target 18/18,
  C == target 18/18. Tunnel sni = the probe's SNI: B == target 18/18, C ==
  the tls IP (nginx cover) 17/18; the one difference is plain HTTP on the
  port: a "both" IP splices non-TLS bytes to the target (no SNI to classify),
  so a prober that knows our tls name sees nginx for TLS and the target for
  plain HTTP. For `both`, point the tls cover at the target (or use a tls
  name that is the target's) — phase 4.

## 8. Phase 4: leftovers, fingerprints, performance (2026-11-11)

### 8.1 Fixes (code + tests)

* **Start-up storm** (`engine/lb.go`): within the LB start grace a non-primary
  path is still never cooled, but after `lbGraceFreeFails`=6 failures in a row
  it is skipped for an exponential backoff (250 ms doubling, cap 4 s;
  `LB.backoff`, kept across `remapLocked`, cleared by a success). The first 6
  failures are free so the startup race (`TestLBStartupRaceNoCooling`) is
  unchanged. `TestLBStartGraceFastFailBackoff`: a path failing on every dial
  for the whole 30 s grace (10 ms dial cadence) gets <= 25 dials (was one per
  dial: ~3000 in the test, ~600 in the lab), stays `ok`, and the pool grows on
  the other paths.
* **Validation** (`cmd/hs2/lb.go` `tlsNameOverlap`): a reality/both IP whose
  `server_names` include a name our tls front takes (top-level `sni` or any
  per-IP `sni`; case/trailing dot normalised; `*.x` covers one label below) is
  refused (`TestLBFrontTLSNameOverlap`). The warning when our tls sni does
  not resolve to that IP: done in phase 4b (8.4).
* **Lazy target dial on "both"** (`AcceptFront`): the target is dialed only
  when the first read is not a complete hello (garbage/partial: the prober's
  bytes still reach the target as they come) or a complete hello is not ours;
  our tls dials never contact the target. `reality` IPs still dial at accept
  (an authenticated hello needs the target up). `TestHybridLazyTargetAndCap`:
  3 tls dials -> 0 target dials. Timing cost measured and closed in phase 4b
  (8.4: pre-dial after 10 ms of silence).
* **Cover-splice caps**: `fallback_max_conns` (per listener IP, default 256)
  and `fallback_max_per_net` (per client /24, IPv6 /64, default 16). Over the
  cap the hello is read, then FIN (CloseWrite), the rest drained 2 s (so the
  close is never an RST), no target contact; reason `rejected.fallback_cap`.
  Rationale: a busy server that accepts and hangs up; an RST or a silent
  stall would be rarer patterns. Tested in `TestHybridLazyTargetAndCap`
  (clean EOF, slots released).
* **Fingerprint mirroring** (`tunnelTLSConfig`, `targetMirror`): the size
  mirror's probe (Chrome-like offer: X25519MLKEM768+X25519, ALPN h2/http1.1)
  now also records the target's group and ALPN; the authenticated handshake
  uses exactly that group (`CurvePreferences`) and ALPN (none if the target
  has none). The throw-away cert is EXACTLY the target chain's size, no
  jitter (fixed serial length, ECDSA signature re-signed until 71 B, padding
  nudged until equal). `TestRealityMirrorTarget`.

### 8.2 Fingerprint comparison (lab 2, nginx 1.24/OpenSSL 3.0.13)

ClientHello (tls and reality fronts vs uTLS Chrome 133; curl-impersonate not
available offline): JA4 `t13d1516h2_8daaf6152771_d8a2da3f94cd` identical,
JA3n identical (JA3 changes per connection, as Chrome's: extension order
shuffled, 400/400 distinct), GREASE, key shares (GREASE, 0x11ec 1216 B, 0x1d
32 B), ECH GREASE, record sizes 1715-1811, ALPN identical. The reality
session_id is AES-GCM output: byte chi2 249 (df 255) vs random 226; no layout
difference to the tls front. Prober without the key: byte-identical to nginx
(spliced), time to ServerHello +0.66 ms (listener->target RTT; keep the
target close).

Authenticated client vs the target, passive observer (30 conns each):

| item | before (be79c990) | after (phase 4) | nginx ECDSA 387 B chain | after, RSA 2181 B chain | nginx RSA | severity / status |
|---|---|---|---|---|---|---|
| ServerHello group / size | 0x11ec / 1210 | 0x1d / 122 | 0x1d / 122 | 0x1d / 122 | 0x1d / 122 | high -> fixed |
| EncryptedExtensions | 36 (h2) | 42 | 42 | 42 | 42 | med-high -> fixed |
| Certificate record | 441-443 jitter | 442 fixed | 417 | 2211 | 2211 | med -> fixed for real chains; floor ~411 B (lab chain smaller) |
| CertificateVerify | 95-97 | 95-97 | 95-97 | 95-97 | 281 | high for RSA targets -> fixed in 4b (281) |
| NewSessionTicket | none | none (Go would send 1x139 B inside its first flight: Go-only, kept off) | 2 (266/282) after client Finished | none | 2 | med -> fixed in 4b |
| first server flight | 1866-1869 | ~790 | 760-762 | ~2560 | 2740 | follows the rows above |
| time to first server byte | 1.40 ms | 1.36 ms | 0.49 ms | 1.95 ms | 1.30 ms | low (cert build + sign) -> OPEN |

(Phase 4b closed (1)-(3) and cut the cert build of (4) to 0.2 ms; see 8.4.)
Open at phase 4, with the plan: (1) RSA target: when the mirrored leaf is RSA, use a
throw-away RSA key of the same size (CertificateVerify rsa_pss 2048 = 256 B
sig); (2) NST: after the client's Finished, send N padded records of the
target's NST sizes (measure N/sizes in the mirror probe: read ~100 ms after
the handshake); the dialer discards them — a wire change for the dialer, so
version it; (3) cert floor: log a warning when the target chain is smaller
than our minimum; (4) pre-build certs to cut the +0.6-0.9 ms.

### 8.3 Performance A/B (lab 1, reverse l3mtcp, crowd 3000, 1% loss, 150 links)

Note: `LOSS` in the lab runner is a fraction: the phase-2 runs (6.5) also had
1% loss (measured drop 0.98%), not 0.01%. Same binary (be79c990), 6
interleaved reps x 4 cases x 2 arms, 48 runs: links 150/150, 0 failures,
churn 0; R runs: `reality.authed` 150, `fallback_conns` 0.

| case | metric | T (tls) | R (reality) | R-T | Welch t |
|---|---|--:|--:|--:|--:|
| down8 | Mbps | 1357.5 | 1420.4 | +4.6% | +1.83 |
| down8 | call p99 ms | 174.5 | 195.5 | +12.0% | +0.81 |
| down8 | crowd p99 ms | 157.6 | 157.4 | -0.1% | +0.08 |
| light | call p99 ms | 182.5 | 174.2 | -4.5% | -0.15 |
| light | crowd p99 ms | 149.8 | 151.1 | +0.8% | +0.67 |
| light | echo p99 ms | 136.1 | 139.2 | +2.3% | -0.02 |
| down1 | Mbps | 821.8 | 655.2 | -20.3% | -0.92 |
| down1 | crowd p99 ms | 149.9 | 153.2 | +2.2% | +1.38 |
| up8 | Mbps | 1719.8 | 1519.8 | -11.6% | -1.12 |
| up8 | crowd p99 ms | 159.0 | 158.8 | -0.1% | -0.20 |

No |t| > 2 in 23 comparisons (down1 single stream at 1% loss swings 294-1068
Mbps in both arms). Raw: lab 1 `/opt/hs2lab/r35/p4/res/ab4.jsonl`.

LB, 3 paths (A tls, B reality, C both; 1000mbit/40ms/1% each; crowd 3000,
call, game, iperf down x8; 45 s):

| run | heavy Mbps | failures | crowd p99 | links A/B/C | share |
|---|--:|---|--:|---|---|
| eq0 | 1185 | 0 | 173 ms | 59/36/58 | 39/24/38% |
| eq1 | 1259 | 0 | 168 ms | 57/37/56 | 38/25/37% |
| eq2 | 1146 | 0 | 167 ms | 59/35/58 | 39/23/38% |
| B worse (+80 ms, 3% corrupt) | 1040 | 0 | 1230 ms | 60/28/65 | 39/18/42% |

Findings for the LB (resolved / explained in 8.5, 8.6): (a) with equal paths the reality-only path B
settles at ~37 links instead of ~51: at ~10 s one B dial fails with
`reading server proof: i/o timeout` and auto2 lowers B to x0.17 citing
`loss 1.1%` (the same loss as A and C), and the share does not recover in
the run — the LB is biased by the front's startup failures, not by quality.
(b) when B really degrades, auto2 cuts it (x0.10, `loss 4.2%`) but only idle
links retire (37 -> 28), so crowd p99 rises to 1.23 s: light users stay
behind a bad path — against the "light users never behind heavy/bad" rule.

### 8.4 Phase 4b: front / crypto / cover leftovers (2026-11-11, branch reality35b)

Code: `tlscarrier/reality.go`, `tlscarrier/realitymirror.go`,
`tlscarrier/server.go`, `cmd/hs2/front_dns.go`; tests
`tlscarrier/phase4b_test.go`, `cmd/hs2/front_dns_test.go`.

1. **One origin on "both"** (`Server.handle` with a cover dialer,
   `RealityServer.dialCoverHTTP`): a client that completes a handshake with
   OUR certificate (our tls SNI) but does not authenticate is proxied, after
   decryption, to the reality target over TLS (the target's own name, ALPN
   http/1.1) instead of the local cover backend. Plain HTTP, other SNIs and
   our SNI now all show the target's HTTP server. `TestHybridOneOriginCover`:
   direct / other SNI / our SNI = `200 server="target-origin"`, plain HTTP on
   the port = the target's own 400. Probe battery (lab 2, nginx 1.24 target,
   18 fast probes): tunnel sni = the probe's SNI: both == target **18/18**
   (was 17/18: plain HTTP differed), reality == target 18/18; tunnel sni =
   another name: both 18/18, reality 18/18. The handshake on our SNI still
   shows our certificate (it is our name).
2. **SNI DNS warning** (`sniResolveWarnings`): every listening lb IP with a
   tls or both front whose sni (per-IP, else top-level) does not resolve to
   that IP, or does not resolve at all, is logged at load and at each reload
   (`front: WARNING ...`, in the background: load never waits on DNS) and is
   a `warn` line `front sni dns` in `hs2 doctor`. Wildcards, IP literals and
   reality-only IPs are skipped. `TestSNIResolveWarnings`.
3. **Lazy dial timing** (`TestHybridCoverTimingParity`, 30 ms
   listener->target connect, median of 7, time from the prober's hello to the
   first target byte):

   | prober | reality (dial at accept) | both, lazy only (phase 4) | both + pre-dial (4b) |
   |---|--:|--:|--:|
   | hello right after connect | 30.7 ms | 30.6 ms | 30.7 ms |
   | hello 100 ms after connect | 0.18 ms | **30.6 ms** | 0.19 ms |

   A prober that connects and waits saw one extra listener->target connect
   with the phase-4 lazy dial. Now a "both" connection with no complete hello
   after `lazyPreDial` = 10 ms dials the target anyway. Our own tls dialer
   sends its hello at once, so it still never contacts the target
   (`TestHybridLazyTargetAndCap`: 0 target dials). Immediate hellos were
   already at parity (the eager dial only overlaps the accept->hello gap).
4. **RSA leaf -> RSA key** (`useLeafKey`, `cachedKey`): the mirror probe
   records the target leaf's key type and size; the throw-away cert then
   carries an RSA (or ECDSA P-384/521) key of that size, generated once per
   process outside the handshake path. All throw-away certs are now SIGNED by
   a per-listener Ed25519 issuer key (only our dialer sees that signature,
   and it checks only the tag): the fixed 64 B signature makes the exact size
   one signing (with a cached pad hint). Cert build 1.0-1.3 ms -> 0.20-0.23
   ms (`BenchmarkRealityMakeCert`, lab 2: 1.003 signings/op, was ~4).
5. **NewSessionTicket parity** (`padRecord`, `readPadsThenProof`,
   `realityCapPad`): the mirror probe reads the target's records after its
   Finished (`recordTap`) and samples ticket sizes over 4 probes per refresh
   (OpenSSL: 266 or 282 B, independently per ticket). Right after the
   client's Finished the listener writes as many application_data records as
   the target sends tickets (TLS 1.3 hides the inner type), each sized at
   random from the samples: marker(HMAC(ekm)) || u16 n || n random bytes.
   Go's own in-flight 139 B ticket stays off. Versioned: the dialer sets bit 0
   of the signal's byte 1 (old listeners ignore bytes 1-3); the listener sends
   pads only to such a dialer, so old dialers get none and keep working
   (`TestRealityRSAKeyAndTicketPads`: new dialer `.. 281 53 {266,282}
   {266,282} proof`, old dialer `.. 281 53 proof`).
6. **Cert size floor**: a minimal form (no subject, no key usages, 64-bit
   serial) is used when the target chain is below our normal cert (P-256:
   427 B normal, 333 B minimal); gaps up to 90 B ride inside the tag's OCTET
   STRING (older dialers read only Value[2:34]), larger ones in the pad
   extension; exact to the byte (`TestRealityMinimalCert`). A target chain
   below the minimum logs `WARNING the target's certificate chain (..) is
   smaller than our smallest throw-away certificate`.

Fingerprint re-run (lab 2, nginx 1.24 / OpenSSL 3.0.13, passive relay, 30
authenticated connections each; ours = commit 034a354f):

| item | ours, EC 387 B chain | nginx EC | ours, RSA 2181 B chain | nginx RSA |
|---|---|---|---|---|
| ServerHello group / size | 0x1d / 122 | 0x1d / 122 | 0x1d / 122 | 0x1d / 122 |
| EncryptedExtensions | 42 | 42 | 42 | 42 |
| Certificate record | **417** (was 442) | 417 | 2211 | 2211 |
| CertificateVerify | 95-97 | 95-97 | **281** (was 95-97) | 281 |
| server first flight | 760-762 | 760-762 | **2740** (was ~2560) | 2740 |
| records after client Finished | **2 x {266,282}** + proof | 2 x {266,282} + HTTP | **2 x {266,282}** + proof | 2 x {266,282} |
| ticket pairs 266/266, 282/266, 266/282, 282/282 | 15/4/7/4 | 8/7/9/6 | 5/8/12/5 | 7/7/6/10 |
| client Finished -> 1st ticket | 0.16 ms | 0.25 ms | 0.14 ms | 0.23 ms |
| time to first server byte (median) | 1.38 ms | 0.63 ms | 3.16 ms | 1.43 ms |

(The EC run's 266/266 excess: the first connections came before the 4
ticket samples were in, using one probe's sizes.)

Left: (a) time to first server byte is still +0.7 ms (EC) and +1.7 ms (RSA)
over a co-located target. The cert build is no longer the cost (0.2 ms);
for RSA, Go's RSA-PSS 2048 CertificateVerify signature is ~1.3 ms slower
than OpenSSL's and cannot be precomputed (it signs the transcript). Below
WAN jitter but measurable on a quiet path; plan: profile the authenticated
path (signal check, Go handshake) and keep the target close. (b) The first
~1 s after start uses one probe's ticket sizes. (c) Pads only for dialers
of this version: an old dialer's connection still shows no tickets.

### 8.5 Phase 4c: LB front bias, light users on a cut path, start-storm (2026-11-11)

Commit `777c181d` (branch reality35c, engine only). Lab 1, same setup as 8.3
(`/opt/hs2lab/r35/c`, whole lab per run, 3 paths A tls / B reality / C both,
1000mbit/40ms/1% each, reverse l3mtcp, crowd 3000, call, game, iperf down x8).

**(a) Front bias — root cause.** Every equal-path run (old and new binary,
6/6) has exactly ONE failed dial, always on B at ~10-12 s:
`tlscarrier: reading server proof: read tcp ...: i/o timeout` (TCP connected,
the front's proof read timed out). `LB.measure` turned the tick's dial counts
into `q2Reading.fail = dfails/dials`; that tick B had 2 dials, 1 failed ->
fail 0.5, and `q2Goodness` = 1/(1 + 50*loss + queue/50 + 4*fail) fell from
~0.65 to ~0.28 next to A and C at the same 1.1% loss: r = 0.43, target r^2 =
0.19 — exactly the logged `1.00->0.19 (lower: loss 1.1%)` (the reason string
named only the loss). B then climbed 0.1 a reading, but a busy pool only
retires idle links, so B ended the 45 s runs at 35-37 links. Not retransmits,
not link age: the per-path loss reading is computed the same way for every
front. Unit test `TestJudgeEqualPathsFrontFailure` reproduces it with the
old rule (B -> <0.3) and shows 1.0 with the fix.

Fix (`lb_judgedata.go`, `HS2_LB_JUDGE_DATA`, default ON in envdef): the
judge's fail share counts only dials that failed BEFORE TCP connected
(`*net.OpError` Op "dial": refused / SYN timeout — the path itself) and only
in a tick with >= 4 dials. A front's handshake failure never reaches the
judge (it stays with the front fallback / cool-down / start back-off, which
still count every failed dial). The reason string now shows `dial fail N%`
when that term is in play.

| equal paths | links A/B/C end | crowd p99 | heavy Mbps | n |
|---|---|--:|--:|--:|
| before (8.3, be79c990) | 59/36/58, 57/37/56, 59/35/58 | 168 ms | 1185 | 3 |
| after (JUDGE_DATA=1) | 51/50/49, 51/50/49, 51/50/49 | 169 ms | 1220 | 3 |

(B's one front failure still happens in every run, ~11 s in; its cause is in
the reality front, not the LB: a lead for tlscarrier — the proof read deadline
under the start burst.)

**(b) Light users behind a cut path.** B +80 ms one-way, 3% corrupt. The
existing cut drain (`HS2_LB_CUT_DRAIN`, on) already retires B's surplus
(24-30 links holding ~1800 connections) at the cut (~12-15 s): no new
connections land there. Measured with the probe's `-warm 25s` (stats from
25 s to 60 s, i.e. AFTER the cut) the crowd is at p99 ~325 ms, not 1.2 s: the
1.2-1.5 s whole-run p99 (bbase0 1419, p4bbad0 1230; bbase1 401 — noisy) comes
from the ~12 s BEFORE the cut, when a third of the crowd is on B and the judge
still needs two bad readings.

New mechanism (`lb_evac.go`, `HS2_LB_EVAC`, default OFF): a link cut-drained
for 4 s has its LIGHT user streams (not heavy, rate < 32 KB/s, not mconn
copy-only) closed BETWEEN requests (no byte for 400 ms, re-checked right
before the close), at most 24 a link and 240 a 2 s tick over the pool, spaced
5-25 ms; after cutDrainForce (60 s) the old rule takes over. Log:
`lb evac: N closed ... (passed over: C copy-only, B busy, M mid-request)`.
What it found: ~85% of the connections on the cut links are mconn lite
second copies (copy-only — the connection already rides a good link too);
the ~200-250 real light first subs were moved in ~25 s.

| B worse, stats 25-60 s | crowd p95 | crowd p99 | call p99 | ping p99 | heavy | crowd fail | n |
|---|--:|--:|--:|--:|--:|---|--:|
| base (cut drain only) | 224 (219-226) | 328 (327-336) | 282 | 172 | 961 | 0,0,0 | 3 |
| + EVAC | 200 (197-206) | 321 (320-325) | 289 | 177 | 935 | 0,4,0,0 | 4 |
| all-tls, base | 218 | 324 | 304 | 101 | 918 | 0 | 1 |
| all-tls, + EVAC | 201 | 321 | 194 | 166 | 1120 | 0 | 1 |

(medians, ranges; EVAC n=4 = one run of the first version with the same
rule + 3 of the final one.) EVAC lowers crowd p95 by ~10% consistently (no
overlap, Welch t ~ 8, the same in the all-tls run: a general LB effect, not
reality's), p99 by ~2% (noise level), heavy unchanged within noise, and one
run had 4 crowd connections reset (a light first sub without a live copy).
Not a proven p99 gain -> default off. Plan for the real p99 cause (the
pre-cut window): evacuate on the first "lowered <= 0.25" reading too, or a
faster first verdict for a path whose loss AND rtt both jump at once.

**(c) Target down from start (phase-4 back-off).** `0:tdown` (target killed
at t=0; B reality-only fails fast with EOF, C falls back to tls):

| run | crowd p99 | heavy | failures | links A/B/C t5 / t10 / t20 | B failed dials |
|---|--:|--:|---|---|--:|
| p3 tdown0 (before back-off) | 2683 ms | 954 | 0 | 5/3/4, 6/3/5, 6/4/5 (13 links until ~30 s) | 599 |
| td0 | 179 ms | 1034 | 0 | 28/0/28, 55/0/53, 76/1/74 | 26 -> cooled 20 s |
| td1 | 167 ms | 1074 | 0 | 28/0/28, 55/0/53, 76/0/74 | 25 -> cooled 20 s |

Pool growth is no longer stalled (150 links at 20 s), crowd p99 normal.

Tests: `TestJudgeFailFrontNotLoss`, `TestJudgeEqualPathsFrontFailure`,
`TestLBConnectFailCounted`, `TestEvacLightSparesHeavyAndActive` (heavy,
call-rate, mid-request and copy-only streams untouched; per-call bound),
`TestEvacDue`, `TestEvacPerTickBound` (pool-wide bound; non-cut retiring and
serving links untouched), envdef/defaults tests. Raw: lab 1
`/opt/hs2lab/r35/c/res/*`, `an.py`.

### 8.6 Phase 4d: light users behind a path that goes bad mid-run (2026-11-11)

Lab 1 (`/opt/hs2lab/r35/d`), same setup as 8.4 (3 paths A tls / B reality / C
both, 1000mbit/40ms/1% each, reverse l3mtcp, crowd 3000, call, game, iperf
down x8, 60 s), but B degrades at **t = 15 s** (`15:bbad`: netem on both
directions changed to +80 ms and 3% corrupt mid-run), and the probe reports the
crowd per window (`-crowdwin 15,27`: 3-15 s before, 15-27 s around the
verdicts, 27-60 s after).

**What the whole-run p99 really was.** With the degradation at 15 s the first
reading after it already lowers B to the floor (`1.00→0.10 (lower: loss 2.3%,
queue 135ms)`, every run, base binary): the delay over the busy links' minimum
shows the +80 ms at once, and q2Goodness squared gives 0.10. The cut follows a
reading later and the cut drain starts. Still, after the cut the crowd's p95
DOUBLES for everyone (170 → 334 ms) and its p99 stays at ~525 ms for the rest
of the run. Cause: lite's dual connections. Before 15 s a light connection's
copy sub is placed like any light sub, so about one in nine of the crowd has
BOTH subs on B; those connections have no good copy to read, the cut drain
only stops new connections, and lite never replaces a busy second sub. The
3-15 s window (before B degrades) has its own p99 of 0.6-1.4 s: the start
burst (3000 connections opened in 2 s while the pool grows), the same in
every arm.

**Fast first verdict** (`lb_fastverdict.go`, `HS2_LB_FAST_VERDICT`, default
OFF). A reading whose raw loss is over lb2CutLoss and over 2× the best other
path + 1 point AND whose delay is over the best other path's by 40 ms, with
>= 1000 segments, while some path is not bad and after the judge's first 10 s,
sets the path's target to the floor at once (the cut rule is unchanged). It
fires in every bad run and never with equal paths, but it changes nothing
measurable: the plain rule already floors B at that reading (see above). Kept
as a guard for a degradation whose loss leads its delay; off by default.

**Rescue** (`lb_rescue.go`, `HS2_LB_RESCUE`, edge, default ON). Nothing is
closed that a user would see:
- the edge marks the links of every path the dialing side's fresh report has
  at factor <= 0.25 or cut (and nothing when every path is lowered);
- a lite connection with ONE sub on a marked link gets lite's own upgrade to
  dual even past the duplication budget (lite's own light test still
  applies: not heavy, not a call, under mDupMaxRate);
- a light dual connection with EVERY sub on marked links gives up its copy
  sub through the close handshake (`HS2_MCONN_CLOSE_HS`, default on: the exit
  stops sending on it and names its end offset, so no byte rides only there);
  a second later lite upgrades it again, and steering puts the new copy on a
  good path;
- at most 400 such actions a second over the process; a dual connection
  looks once a second while the key is on.

The 4 resets of the 8.4 EVAC run were single-sub connections closed by the
evacuation: a closed stream of a connection without a live copy is a reset
whatever the idle signal (no request boundary is visible in the bytes). The
rescue never closes a connection's last sub, so it cannot reset one.

B degrades at 15 s (n=4 each, interleaved; medians, ranges; crowd ms):

| arm | crowd p95 | crowd p99 | p99 mean±sd | 15-27 s p95/p99 | 27-60 s p95/p99 | call p99 | ping p99 | heavy Mbps | crowd fail |
|---|--:|--:|--:|--:|--:|--:|--:|--:|---|
| base (248436d1 rules) | 312 (301-317) | 556 (538-588) | 560±22 | 335 / 522 | 334 / 526 | 570 (401-5632) | 230 | 994 | 0,0,0,0 |
| + fast verdict | 291 (283-298) | 537 (511-570) | 539±26 | 318 / 512 | 319 / 516 | 693 (192-920) | 198 | 1026 | 0,0,0,0 |
| + fast verdict + rescue | **180** (167-188) | **385** (355-410) | 384±27 | 198 / 448 | **179 / 283** | 336 (190-408) | 172 | 1043 | 0,0,0,0 |
| rescue only (no fast verdict) | 185 (174-195) | 388 (378-398) | 388±14 | 214 / 455 | 182 / 286 | 321 | 348 (206-490) | 996 | 0,0 (n=2) |

Rescue: 247-293 copy subs moved off B per run, 0 single-sub upgrades past the
budget (the budget did not bind here), crowd p99 -31% over the whole run
(Welch t ~ 10), -46% after the cut, p95 back to the pre-degradation level;
no crowd failure, heavy unchanged.

Equal paths, no degradation (45 s, n=3 each, interleaved):

| arm | crowd p95 | crowd p99 | 27-45 s p95/p99 | call p99 | ping p99 | heavy | fast verdicts | crowd fail |
|---|--:|--:|--:|--:|--:|--:|--:|---|
| base | 117 (112-124) | 169 (161-176) | 112 / 158 | 202 (197-282) | 129 (119-133) | 1050 | 0 | 0,0,0 |
| + fast verdict + rescue | 118 (112-142) | 165 (160-218) | 116 / 160 | 185 (173-197) | 136 (97-152) | 1050 | 0 | 0,0,0 |

No fast verdict in any equal run. In one of the three key-on runs path C read
4.0% at ~15 s (a real transient under the 8-stream download; the plain rule
lowered it 1.00→0.19 and the edge cut it on its own 4.4% reading, recovered
20 s later) and the rescue moved 336 copy subs off it; that run is the 142 /
218 one (its 3-15 s window, before anything happened, was already the worst
start of the six: p99 1.9 s). The other two key-on runs equal the base runs
(112/160, 118/165). Medians: crowd p95 +1 ms, p99 -4 ms, call p99 -17 ms,
ping p99 +7 ms with overlapping ranges.

All-tls fronts (`PLAIN=1`), B degrades at 15 s (n=2 each):

| arm | crowd p95 | crowd p99 | 27-60 s p95/p99 | call p99 | ping p99 | heavy | crowd fail |
|---|--:|--:|--:|--:|--:|--:|---|
| base | 291 (284-297) | 498 (492-504) | 333 / 514 | 324 | 273 | 900 | 0,0 |
| + fast verdict + rescue | 172 (171-174) | 320 (307-334) | 173 / 263 | 324 | 168 | 948 | 0,0 |

The same gain with every front tls: an LB / mconn effect, not reality's.

**Defaults.** `HS2_LB_RESCUE=1` on the edge (envdef, Iran side; the exit
needs nothing new: it already reports the factors and reads CLOSE-REQ): whole-run
crowd p99 -31% with B going bad (-36% all-tls), p95 back to the
pre-degradation level, zero crowd failures in 12 key-on runs, heavy
unchanged, equal paths unchanged by median. With an older exit (no fresh
report) nothing is marked; with `HS2_MCONN_CLOSE_HS` off only the
single-sub part works. `HS2_LB_FAST_VERDICT` stays off: no measurable effect
here (the plain rule already floors such a path at the same reading); its
guards are tested and it never fired with equal paths. `HS2_LB_EVAC` stays
off (it can reset single-sub connections; the rescue covers its purpose
without closing anything).

Left open: the 3-15 s start window (crowd p99 0.4-1.9 s in every arm, before
any path is bad): 3000 connections opened in 2 s while the pool grows from a
few links — the start burst, a separate lead (8.4 (c) fixed the dial side of
it, not the placement of the first connections).

Tests: `TestFastVerdictClearDegradation`, `TestFastVerdictGuards` (small
sample, loss alone, delay alone, under 2× best + 1 pt, every path bad, start
grace; a good reading clears it), `TestRescueMarkUnmark`,
`TestRescueRateBound`, `TestSteerRescueMarks` (lowered and cut paths marked,
recovered unmarked, every path lowered: nothing), `TestMConnRescueCappedSingle`
(past the budget, copy avoids the marked link), `TestMConnRescueSparesOthers`
(unmarked link, a download: untouched), `TestMConnRescueDualAllLowered`
(copy given up through the handshake and reopened while pings run both ways:
no ping lost), `TestMConnRescueDualKeyOff`. Raw: lab 1 `/opt/hs2lab/r35/d/res`,
`an.py`; the probe's `-crowdwin` flag and the `bbad` event are in this
commit / `ir.sh`.

### 8.7 Phase 5 (2026-11-11, branch reality36): start window, rescue under a binding budget

Branch reality36 = reality35b (8.4) + reality35c (8.5, 8.6) merged; the only
conflict was this file (both added an "8.4": phase 4b stays 8.4, 4c is 8.5,
4d is 8.6). Final tree: gofmt clean, `go vet ./...`, engine suite (619 s),
tlscarrier, cmd/hs2, reality, envdef all ok, `-race` on the new tests ok
(lab 3).

**(a) Start window.** Cause of the 0.4-1.9 s crowd p99 in the first seconds
(before any path is bad): the refill hold (refill.go). At a start the exit
dials its links in batches (1, ~7, ~20, ~33 per path at 0, 2, 4, 6 s); 2700
of the 3000 crowd connections wait in the hold for a link under the
fair-share cap, the longest 1.0-2.3 s, and the first request of a waiting
connection waits with it. `HS2_REFILL_WAIT_MS=N` (edge, `refill_wait.go`,
default OFF): a connection that has waited N ms leaves the queue for the
least-loaded link (v1: any; v2: only a link under 2× the fair-share cap,
else it keeps waiting and looks again N ms later). Lab 1, equal paths
(45 s, crowd 3000, call, game, iperf down x8, `-crowdwin 5,10,15`),
interleaved; medians (ranges), ms:

| arm | n | 0-5 s crowd p95/p99 | whole crowd p95 | whole crowd p99 | 15-45 s p95 | call p99 | ping p99 | heavy Mbps | busiest link |
|---|--:|--|--:|--:|--:|--:|--:|--:|--|
| base (key off) | 8 | 438 / 1006 (534-1946) | 120 (114-129) | 165 (161-183) | 115 | 182 (175-198) | 134 | 1126 | 68-80 |
| v1 300 ms, any link | 5 | 128 / 170 (159-173) | 124 (115-131) | 164 (160-166) | 124 | 190 (167-225) | 122 | 1081 | 81-93 |
| v2 300 ms, < 2× cap | 4 | 129 / 173 (160-761) | 120 (114-138) | 162 (160-169) | 118 | 213 (185-229) | 140 | 1216 | 77-141 |

Both cut the start p99 ~6× (1006 → 170 ms; 1700-2000 connections leave the
hold at the bound, longest wait 0.3 s). Neither passes: v1 raises the whole
crowd p95 +4-5 ms and the 15-45 s p95 +9 ms (the connections that left early
stay on the first links: busiest 81-93 vs 68-80); v2 keeps the whole p95 by
median (mean +3) but call p99 rises +31 ms (185-229 vs 175-198) and one run
of four still had a 761 ms start (every link at 2× the cap, the bound did not
help). **Default stays off.** Next: place the leavers by the links' queued
bytes rather than connection counts, or let lite's dual upgrade re-place a
start-window connection's primary once the pool is up (the copy goes to a
new link anyway); also faster initial dialing at the exit (the batches above
are the real limit).

**(b) Rescue when the duplication budget binds.** A heavier crowd download
mix (replies up to 32 KB, no iperf, B bad at 15 s, n=2+2) does NOT make the
budget bind: the edge's budget counts every byte it sends and receives
(liteBud), the copies it sends are only the light users' uploads, so dup is
far under 5% (0 upgrades refused, 0 copies past the budget, 244-270 copy subs
moved by the dual part). To make it bind, a lab-only build set mLiteCapPct
to 0 (k runs, B bad at 15 s, iperf down x8, n=2 each): whole crowd p95
539/593 (rescue on) vs 560/572 (`HS2_LB_RESCUE=0`), p99 817/863 vs 834/872;
copies given past the budget: **0 and 4** over the whole run. So the
single-sub part does not reach most single-sub connections when the budget
binds (unit tests pass: `TestMConnRescueCappedSingle`). Open: trace a k run
(mconn trace on a sample) to find which guard (`rescueWantLocked`: dupOff,
mode, or the lite poll not rearmed after a refusal) stops it; until then the
rescue's measured gain is the dual part (8.6), which does not depend on the
budget.

Equal paths (b) and all-tls (c) with more reps: see the table below (fresh
binary of this tree; base = `HS2_LB_RESCUE=0`, since the rescue is a default
now).

Raw: lab 1 `/opt/hs2lab/r36/d/res` (`an2.py s|u|k|b|c`), drivers `drv*.sh`.

| scenario, arm | n | crowd p95 | crowd p99 | 27+ s p95/p99 | call p99 | ping p99 | heavy | crowd fail |
|---|--:|--:|--:|--|--:|--:|--:|--|
| (b) equal, rescue off | 4 | 120 (112-124) | 173 (161-182) | 112-116 / 158-161 | 193 (184-282) | 124 (95-133) | 1091 | 0 ×4 |
| (b) equal, fast verdict + rescue | 4 | 115 (112-142) | 163 (160-218) | 114 / 160 | 181 (173-197) | 142 (97-152) | 1105 | 0 ×4 |
| (c) all-tls, B bad at 15 s, rescue off | 4 | 290 (284-297) | 498 (492-504) | 333 / 512 | 288 (177-451) | 229 (169-311) | 951 | 0 ×4 |
| (c) all-tls, fast verdict + rescue | 4 | 176 (171-182) | 321 (307-334) | 180 / 268 | 430 (204-542) | 201 (143-391) | 1016 | 0 ×4 |

(b): unchanged by median (the one 218 ms run is the C-transient run of 8.6);
(c): crowd p95 -39%, p99 -36%, after the cut p99 -48%, no failure; call p99
is noisy in both arms (177-542) and higher by median with the rescue
(430 vs 288; one rescue-only rep 621): a lead to check with more reps
before trusting the call column here (the 3-path runs of 8.6 showed call p99
336 vs 570 the other way). Rows mix the 35d binary (2 reps per arm) and this
tree's (2 reps; base there = `HS2_LB_RESCUE=0`).

### 8.8 Phase 5b (2026-11-11, branch reality37): rescue under a binding budget found, all-tls call p99

**(a) Why the single-sub rescue barely fired (8.7 (b)).** Debug counters in a
lab k run (budget forced binding by a lab-only build, mLiteCapPct = 0; B +80
ms 3% corrupt from 15 s; crowd 3000, iperf down x8) showed the guards of
`rescueWantLocked` were never the problem: over the whole run lite's
single-sub upgrade rule (mConn.tick) ran only ~50 times, 1 of them on a
lowered link. Nearly every single-sub crowd connection is **raw**
(capMConnRaw, mconn_raw.go): its upgrade rule is the raw timer `rawTick`,
which asked only the budget and never the rescue. The 0 and 4 copies of 8.7
were connections that had been dual once and fell back to the records path.
`TestMConnRescueCappedSingle` passed because `newLitePair` is not raw.
Fix: `rawTick` asks `rescueWantLocked` like the tick does
(`TestMConnRescueCappedRaw`, fails without the fix).

With that, the rescue reached the crowd (1118-1215 copies past the budget a
run, 192-278 copy subs moved) but 20-30 crowd connections failed a run (0
without the rescue). Counters on every end while a raw upgrade was still in
its handshake: all were the second sub, closed by Iran itself 10-11 s into the
handshake. The handshake waits for the exit's raw write in flight on the first
sub (upgradeSendExit takes the raw write lock before naming its boundary) and
the first sub is on the lowered path, so it can take seconds; meanwhile Iran's
lite rule saw the second sub idle for mLiteIdle (10 s), closed it, and a sub
dying in the handshake ends the connection by design. Fix: no idle close of a
sub while the connection is handshaking (and no dual-rescue CLOSE-REQ then);
no unit test reproduces it yet: a fake-net test (the exit's raw write held
past mLiteIdle) passed without the fix alone and was flaky in the full suite,
so it was dropped; the lab rows below are the proof. Open: a reproducer.

The raw part ships behind `HS2_LB_RESCUE_RAW` (edge, **default off**): the
default budget (5% of all bytes, downloads included) did not bind in any lab
run (8.7), so the default changes nothing measurable, and the raw upgrade on a
lowered path is the one place the rescue can still cost a connection.

Lab 1, k runs (budget forced binding, lab-only build), interleaved; medians
(ranges), ms:

| arm | n | crowd p95 | crowd p99 | 15-27 s p95/p99 | 27+ s p95/p99 | call p99 | crowd fail | copies past budget |
|---|--:|--:|--:|--|--|--:|--|--|
| rescue off (`HS2_LB_RESCUE=0`) | 4 | 568 (545-614) | 877 (815-1355) | 576 / 822 | 606 / 885 | 237 (181-922) | 0 ×4 | — |
| raw rescue, before the handshake fix | 4 | 225 (223-234) | 538 (517-579) | 361 / 678 | 192 / 271 | 215 (204-909) | 30, 30, 20, 29 | 1118-1215 |
| rescue off, 2nd set | 4 | 562 (550-580) | 888 (819-948) | 574 / 831 | 607 / 869 | 396 (174-703) | 0 ×4 | — |
| raw rescue + handshake fix (`HS2_LB_RESCUE_RAW=1`) | 4 | 213 (210-217) | 532 (511-544) | 339 / 673 | 184 / 258 | 764 (181-1054) | **0 ×4** | 1118-1155 |

With the fix the failures are gone (0 in 4 runs, 1118-1155 copies past the
budget a run) and the crowd gain stays: p95 -62%, p99 -40%, after the cut p99
869 -> 258 ms. But call p99 (764 vs 396 by median) and ping p99 (311 vs 161)
are worse — the same tail cost as (b) below, larger with ~1100 copies a run.
So `HS2_LB_RESCUE_RAW` stays off until (b) is fixed.

**(b) All-tls call p99 with the rescue (8.7 (c)).** Six more interleaved reps
per arm (fresh binary of this tree, default budget; base = `HS2_LB_RESCUE=0`,
rescue arm with HS2_LB_FAST_VERDICT=1 as in 8.7):

| arm | n | crowd p95 | crowd p99 | 27+ s p95/p99 | call p99 | ping p99 | heavy | fail |
|---|--:|--:|--:|--|--:|--:|--:|--|
| rescue off | 6 | 286 (279-296) | 490 (482-493) | 330 / 505 | 207 (171-424) | 197 | 1021 | 0 ×6 |
| fast verdict + rescue | 6 | 173 (170-182) | 323 (312-344) | 173 / 260 | 367 (170-980) | 236 | 1018 | 0 ×6 |

Call p99 per run, rescue: 195, 903, 191, 170, 539, 980 (8.7: 542, 416, and
621 rescue-only); off: 171, 372, 188, 200, 215, 424 (8.7: 177, 398). The
regression is real in the tail: off never passed 424 ms in 8 runs, the
rescue went past 500 ms in 5 of 9. The crowd gain holds (p95 -40%, after the
cut p99 -48%, no failure). Not fixed here (no lab time left in this run).
Leads, in order: the rescue's burst at the lowering — ~1100 upgrades and
~300 CLOSE-REQs within 2-3 s (rescuePerSec 400) onto the good paths' links,
some of them the call's — so (1) place rescue copies away from links
carrying a call sub (the HS2_MCONN_CALL_AVOID list, reversed) and (2) lower
rescuePerSec to ~100; measure call p99 per 1 s window around the cut to
confirm the burst first.

**(c) Start window v3 (HS2_REFILL_WAIT_MS by queued bytes).** Not started:
the lab time of this run went to (a) and (b). Plan unchanged from 8.7: place
a leaver on the link with the fewest queued/unacked bytes (the smux send
queue, not the connection count), re-place a start-window connection's
primary once the pool is up via lite's dual upgrade, and dial the exit's
first batches faster; acceptance as in the brief (≥4 interleaved reps,
start p99 much lower, whole crowd p95 ≤ +2 ms, no call/ping p99 regression).

Final tree (lab 3, clean `git archive`): gofmt (changed files) clean, `go
vet ./...`, tlscarrier, cmd/hs2, reality, envdef ok, `-race` on
Rescue/MConnRaw/Defaults22 ok; full engine suite (-timeout 40m): every test
passed except the dropped flaky test above (removed after, its subset re-run
ok with -race).

Raw: lab 1 `/opt/hs2lab/r36/d/res` (`an37.py k|c`, runs `*-r*`), drivers
`go37.sh`, `go37b.sh`, `drv37.sh`.

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

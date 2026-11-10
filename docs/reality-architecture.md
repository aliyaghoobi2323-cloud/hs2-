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
| `reality` | SNI of a real site; hidden signal in the ClientHello session_id (x25519 ECDH with the listener's static key + short id); temporary cert verified by the shared secret; unauthenticated connections spliced byte-for-byte to the real target, with byte/rate limits | later |
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

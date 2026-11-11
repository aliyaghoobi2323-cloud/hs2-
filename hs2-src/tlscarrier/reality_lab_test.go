package tlscarrier

// Lab-only adversarial probes of the reality front (sec41). Skipped unless
// HS2_LAB is set; never run in CI. See docs/reality-architecture.md §10.
//
//	HS2_LAB=serve  LISTEN=:18443 TARGET=www.microsoft.com:443 SNI=www.microsoft.com PRIV=.. SID=.. KEY=hex [DUR=20m]
//	HS2_LAB=fp     ADDR=ip:port  SNI=..  [PUB=.. SID=..]   (ServerHello + record fingerprint; authed when PUB set)
//	HS2_LAB=replay ADDR=ip:port  SNI=..  PUB=.. SID=..      (a captured authed hello sent twice)

import (
	"context"
	"crypto/ecdh"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

type rawTap struct {
	net.Conn
	mu  sync.Mutex
	in  []byte
	t0  time.Time
	tfb time.Duration
}

func (t *rawTap) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	t.mu.Lock()
	if n > 0 && t.tfb == 0 {
		t.tfb = time.Since(t.t0)
	}
	t.in = append(t.in, b[:n]...)
	t.mu.Unlock()
	return n, err
}

// serverHelloFP: cipher, extension ids (with key_share group / version), and
// the record (type,len) sequence of the server's bytes.
func serverHelloFP(in []byte) (sh string, recs []string) {
	for off := 0; off+5 <= len(in); {
		typ, l := in[off], int(binary.BigEndian.Uint16(in[off+3:]))
		if off+5+l > len(in) {
			recs = append(recs, fmt.Sprintf("%d:%d(cut)", typ, l))
			break
		}
		body := in[off+5 : off+5+l]
		recs = append(recs, fmt.Sprintf("%d:%d", typ, l))
		if typ == 22 && sh == "" && len(body) > 40 && body[0] == 2 {
			p := body[4:]
			v := binary.BigEndian.Uint16(p)
			sl := int(p[34])
			q := p[35+sl:]
			cs := binary.BigEndian.Uint16(q)
			comp := q[2]
			el := int(binary.BigEndian.Uint16(q[3:]))
			e := q[5 : 5+el]
			var ex []string
			for len(e) >= 4 {
				id, ln := binary.BigEndian.Uint16(e), int(binary.BigEndian.Uint16(e[2:]))
				d := e[4 : 4+ln]
				s := fmt.Sprintf("%d", id)
				if id == 51 && ln >= 2 {
					s += fmt.Sprintf("(g=%#x,%d)", binary.BigEndian.Uint16(d), ln)
				}
				if id == 43 && ln >= 2 {
					s += fmt.Sprintf("(v=%#x)", binary.BigEndian.Uint16(d))
				}
				ex = append(ex, s)
				e = e[4+ln:]
			}
			sh = fmt.Sprintf("legacy=%#x sidlen=%d cipher=%#x comp=%d ext=%v", v, sl, cs, comp, ex)
		}
		off += 5 + l
	}
	return
}

// labAuthedHello builds an authenticated uTLS client (the dialer's exact steps).
func labAuthedHello(t *testing.T, raw net.Conn, sni, pubS, sidS string) (*utls.UConn, *[]byte) {
	pk, err := parseKey32(pubS)
	if err != nil {
		t.Fatal(err)
	}
	spub, _ := ecdh.X25519().NewPublicKey(pk)
	sid8, _ := parseShortID(sidS)
	ak := new([]byte)
	cfg := &utls.Config{ServerName: sni, InsecureSkipVerify: true}
	u := utls.UClient(raw, cfg, clientHello)
	if err := u.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	h := u.HandshakeState.Hello
	ks := u.HandshakeState.State13.KeyShareKeys
	eph := ks.Ecdhe
	if eph == nil {
		eph = ks.MlkemEcdhe
	}
	shared, _ := eph.ECDH(spub)
	*ak, _ = realityAuthKey(shared, h.Random)
	for i := 39; i < 39+32; i++ {
		h.Raw[i] = 0
	}
	sid := realityAEAD(*ak).Seal(nil, h.Random[20:32], realityPlain(time.Now(), sid8), h.Raw)
	h.SessionId = sid
	copy(h.Raw[39:], sid)
	return u, ak
}

func labFP(t *testing.T, addr, sni, pub, sid string) {
	raw, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tap := &rawTap{Conn: raw, t0: time.Now()}
	var u *utls.UConn
	if pub != "" {
		u, _ = labAuthedHello(t, tap, sni, pub, sid)
	} else {
		u = utls.UClient(tap, &utls.Config{ServerName: sni, InsecureSkipVerify: true}, clientHello)
	}
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	herr := u.Handshake()
	hs := time.Since(tap.t0)
	raw.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	b := make([]byte, 4096)
	for {
		if _, err := u.Read(b); err != nil {
			break
		}
	}
	sh, recs := serverHelloFP(tap.in)
	cs := u.ConnectionState()
	t.Logf("FP addr=%s authed=%v herr=%v tfb=%v hs=%v alpn=%q certs=%d\n  SH %s\n  RECS %v", addr, pub != "", herr, tap.tfb, hs, cs.NegotiatedProtocol, len(cs.PeerCertificates), sh, recs)
}

func TestLabReality(t *testing.T) {
	mode := os.Getenv("HS2_LAB")
	if mode == "" {
		t.Skip("lab only (HS2_LAB)")
	}
	g := os.Getenv
	switch mode {
	case "serve":
		key, _ := hex.DecodeString(g("KEY"))
		p := &FrontProfile{Kind: FrontReality, Target: g("TARGET"), ServerNames: []string{g("SNI")}, PrivateKey: g("PRIV"), ShortIDs: []string{g("SID")}}
		inner := &Server{SharedKey: key}
		rs, err := NewRealityServer(p, inner)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rs.StartSizeMirror(ctx, 0, t.Logf)
		ln, err := net.Listen("tcp", g("LISTEN"))
		if err != nil {
			t.Fatal(err)
		}
		dur, _ := time.ParseDuration(g("DUR"))
		if dur == 0 {
			dur = 20 * time.Minute
		}
		time.AfterFunc(dur, func() { ln.Close() })
		go func() {
			for range time.Tick(30 * time.Second) {
				t.Logf("stats %s", RealityStatsNow())
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				t.Logf("stats %s", RealityStatsNow())
				return
			}
			go rs.AcceptFront(ctx, c, func(cr *Carrier) {
				t.Logf("tunnel up from %s", c.RemoteAddr())
				cr.Close()
			})
		}
	case "dial": // the real dialer: reality handshake + tunnel auth, then a frame echo
		key, _ := hex.DecodeString(g("KEY"))
		p := &FrontProfile{Kind: FrontReality, ServerNames: []string{g("SNI")}, PublicKey: g("PUB"), ShortIDs: []string{g("SID")}}
		t0 := time.Now()
		c, err := RealityFront{}.DialFront(context.Background(), DialPath{Addr: g("ADDR"), SNI: g("SNI"), Key: key, Profile: p})
		if err != nil {
			t.Fatalf("DIAL failed: %v", err)
		}
		t.Logf("DIAL ok in %v (tunnel authenticated)", time.Since(t0))
		c.Close()
	case "fp":
		labFP(t, g("ADDR"), g("SNI"), g("PUB"), g("SID"))
	case "replay":
		// capture: build one authed hello, record its bytes, then send them raw twice
		var hello []byte
		{
			c1, s1 := net.Pipe()
			go func() {
				b := make([]byte, 8192)
				n, _ := s1.Read(b)
				hello = append([]byte(nil), b[:n]...)
				s1.Close()
			}()
			u, _ := labAuthedHello(t, c1, g("SNI"), g("PUB"), g("SID"))
			u.Handshake()
			c1.Close()
		}
		for i := 0; i < 3; i++ {
			raw, err := net.DialTimeout("tcp", g("ADDR"), 8*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tap := &rawTap{Conn: raw, t0: time.Now()}
			raw.Write(hello)
			raw.SetReadDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, 16384)
			for {
				if _, err := tap.Read(b); err != nil {
					break
				}
			}
			sh, recs := serverHelloFP(tap.in)
			t.Logf("REPLAY #%d tfb=%v\n  SH %s\n  RECS %v", i, tap.tfb, sh, recs)
			raw.Close()
		}
	}
}

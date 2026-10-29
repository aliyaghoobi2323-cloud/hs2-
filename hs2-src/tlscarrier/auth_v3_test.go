package tlscarrier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func testEKM() []byte {
	ekm, _ := exportEKM(fakeEKM{})
	return ekm
}

// v2ClientRecord builds a client record exactly as protocol v2 did: no
// header, the v2 label, the v2 pad range.
func v2ClientRecord(key, ekm []byte) []byte {
	nonce := bytes.Repeat([]byte{5}, authNonceLen)
	head := append(append([]byte{}, nonce...), mac(key, []byte("hs2-auth-v2"), nonce, be64(minuteBucket()), ekm)...)
	return padded(head, 280, 720)
}

// The header the client sends comes back out of parseClientAuth as sent, and
// so does the server's from readServerProof, which reads the whole proof.
func TestAuthV3RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	ekm := testEKM()
	h := AuthHdr{Ver: protoVersion, MinLevel: 1, MaxLevel: 4, Caps: 0xA1B2C3D4}
	rec, nonce := makeClientAuth(key, ekm, h)
	gotNonce, got, more, v2, ok := parseClientAuth(key, ekm, rec)
	if !ok || v2 || more != 0 || got != h || !bytes.Equal(gotNonce, nonce) {
		t.Fatalf("parse: ok=%v v2=%v more=%d hdr=%+v, want %+v", ok, v2, more, got, h)
	}
	// The first TLS read may end inside the pad: the rest is still to come.
	_, got, more, _, ok = parseClientAuth(key, ekm, rec[:authHeadLen+10])
	if !ok || got != h || more != len(rec)-authHeadLen-10 {
		t.Fatalf("partial record: ok=%v hdr=%+v more=%d", ok, got, more)
	}
	p := ProofHdr{Ver: protoVersion, Level: 3, Caps: 0x0F0F0001}
	r := bytes.NewReader(makeServerProof(key, nonce, ekm, p))
	gp, err := readServerProof(r, key, nonce, ekm)
	if err != nil || gp != p {
		t.Fatalf("proof: %+v err=%v, want %+v", gp, err, p)
	}
	if r.Len() != 0 {
		t.Fatalf("%d proof bytes left unread", r.Len())
	}
}

// The records keep exactly the lengths they had on the wire in v2 (the pad
// gives up what the header takes): client 314..754, server 138..498, both
// ends reachable.
func TestAuthV3LengthsUnchanged(t *testing.T) {
	key := bytes.Repeat([]byte{0x32}, 32)
	ekm := testEKM()
	nonce := bytes.Repeat([]byte{1}, authNonceLen)
	h := LocalAuthHdr()
	p := ProofHdr{Ver: protoVersion, Level: 1, Caps: h.Caps}
	cMin, cMax, sMin, sMax := 1<<20, 0, 1<<20, 0
	note := func() {
		rec, _ := makeClientAuth(key, ekm, h)
		proof := makeServerProof(key, nonce, ekm, p)
		cMin, cMax = min(cMin, len(rec)), max(cMax, len(rec))
		sMin, sMax = min(sMin, len(proof)), max(sMax, len(proof))
		if len(rec) < 314 || len(rec) > 754 || len(proof) < 138 || len(proof) > 498 {
			t.Fatalf("record lengths client %d, server %d", len(rec), len(proof))
		}
	}
	for i := 0; i < 2000; i++ {
		note()
	}
	// 2000 draws miss one end of the client's 441 lengths about 1% of the
	// time: draw on (bounded) until all four ends are seen, so the test
	// checks that they are reachable without being flaky.
	for i := 0; i < 50000 && (cMin != 314 || cMax != 754 || sMin != 138 || sMax != 498); i++ {
		note()
	}
	if cMin != 314 || cMax != 754 || sMin != 138 || sMax != 498 {
		t.Fatalf("lengths seen: client %d..%d, server %d..%d", cMin, cMax, sMin, sMax)
	}
}

// Every bit of both headers is under the HMAC.
func TestAuthV3HeaderUnderMAC(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	ekm := testEKM()
	rec, nonce := makeClientAuth(key, ekm, LocalAuthHdr())
	for i := 0; i < authHdrLen*8; i++ {
		bad := append([]byte(nil), rec...)
		bad[authNonceLen+authTagLen+i/8] ^= 1 << (i % 8)
		if _, _, _, v2, ok := parseClientAuth(key, ekm, bad); ok || v2 {
			t.Fatalf("client header bit %d flipped: ok=%v v2=%v", i, ok, v2)
		}
	}
	proof := makeServerProof(key, nonce, ekm, ProofHdr{Ver: protoVersion, Level: 1, Caps: 0xF})
	for i := 0; i < proofHdrLen*8; i++ {
		bad := append([]byte(nil), proof...)
		bad[authTagLen+i/8] ^= 1 << (i % 8)
		if _, err := readServerProof(bytes.NewReader(bad), key, nonce, ekm); !errors.Is(err, ErrServerProof) {
			t.Fatalf("server header bit %d flipped: err=%v", i, err)
		}
	}
}

// A v2 client is recognised (for the log line only); a record under another
// key, or random bytes, is neither v3 nor v2: a probe.
func TestAuthV3SeesV2Peer(t *testing.T) {
	key := bytes.Repeat([]byte{0x34}, 32)
	ekm := testEKM()
	if _, _, _, v2, ok := parseClientAuth(key, ekm, v2ClientRecord(key, ekm)); ok || !v2 {
		t.Fatalf("v2 record: ok=%v v2=%v, want ok=false v2=true", ok, v2)
	}
	other := bytes.Repeat([]byte{0x35}, 32)
	if _, _, _, v2, ok := parseClientAuth(key, ekm, v2ClientRecord(other, ekm)); ok || v2 {
		t.Fatalf("v2 record under another key: ok=%v v2=%v", ok, v2)
	}
	if _, _, _, v2, ok := parseClientAuth(key, ekm, bytes.Repeat([]byte{7}, 400)); ok || v2 {
		t.Fatalf("random record: ok=%v v2=%v", ok, v2)
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		cMin, cMax, sMin, sMax byte
		cCaps, sCaps           uint32
		level                  byte
		caps                   uint32
		ok                     bool
	}{
		{1, 1, 1, 1, 0xF, 0xF, 1, 0xF, true},
		{1, 2, 2, 3, 0x5, 0x6, 2, 0x4, true},
		{2, 3, 1, 2, 0xF, 0x3, 2, 0x3, true},
		{1, 4, 1, 9, 0x80000001, 0xFFFFFFFF, 4, 0x80000001, true},
		{1, 1, 2, 2, 0xF, 0xF, 0, 0, false},
		{3, 4, 1, 2, 0xF, 0xF, 0, 0, false},
		{2, 1, 1, 3, 0xF, 0xF, 0, 0, false}, // a malformed range shares nothing
	}
	for _, c := range cases {
		level, caps, ok := negotiate(AuthHdr{Ver: protoVersion, MinLevel: c.cMin, MaxLevel: c.cMax, Caps: c.cCaps}, c.sMin, c.sMax, c.sCaps)
		if ok != c.ok || ok && (level != c.level || caps != c.caps) {
			t.Errorf("client %d-%d caps %#x, server %d-%d caps %#x: level %d caps %#x ok=%v, want %d %#x %v",
				c.cMin, c.cMax, c.cCaps, c.sMin, c.sMax, c.sCaps, level, caps, ok, c.level, c.caps, c.ok)
		}
	}
}

// R1 round 2 speaks levels 1 and 2 (group streams at 2); each kill switch
// takes its core's bit out of caps, HS2_TUNE_SCHED the group's with it (it
// needs the scheduler). The link class (CapQuiet) is never a local cap. The
// exit's move hint (CapHint, R1 round 3) rides the groups: HS2_TUNE_SCHED
// and HS2_TUNE_QUIET take it out with them, HS2_TUNE_MOVE_HINT alone; so do
// they the moves of the light QUIET link (CapLight, R1 round 5), and
// HS2_TUNE_QUIET_LIGHT=0 alone.
func TestLocalAuthHdrKillSwitches(t *testing.T) {
	all := CapClass | CapSched | CapHostQ | CapSendCap | CapGroup | CapHint | CapLight
	if h := LocalAuthHdr(); h != (AuthHdr{Ver: 3, MinLevel: 1, MaxLevel: 2, Caps: all}) {
		t.Fatalf("local header %+v", h)
	}
	for env, bit := range map[string]uint32{"HS2_TUNE_CLASS": CapClass, "HS2_TUNE_SCHED": CapSched | CapGroup | CapHint | CapLight,
		"HS2_TUNE_HQG": CapHostQ, "HS2_TUNE_SENDCAP": CapSendCap, "HS2_TUNE_QUIET": CapGroup | CapHint | CapLight,
		"HS2_TUNE_MOVE_HINT": CapHint, "HS2_TUNE_QUIET_LIGHT": CapLight} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "0")
			if c := LocalAuthHdr().Caps; c != all&^bit {
				t.Fatalf("%s=0: caps %#x, want %#x", env, c, all&^bit)
			}
		})
	}
}

// The parser never reads outside first and never panics.
func FuzzParseClientAuth(f *testing.F) {
	key := bytes.Repeat([]byte{0x36}, 32)
	ekm := testEKM()
	rec, _ := makeClientAuth(key, ekm, LocalAuthHdr())
	f.Add(rec)
	f.Add(rec[:authHeadLen])
	f.Add(rec[:authHeadLen-1])
	f.Add(v2ClientRecord(key, ekm))
	f.Add(legacyRecord(key))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, first []byte) {
		first = first[:len(first):len(first)] // a read past it panics
		nonce, h, more, v2, ok := parseClientAuth(key, ekm, first)
		if ok && (v2 || more < 0 || len(first) < authHeadLen || !bytes.Equal(nonce, first[:authNonceLen]) || h.Ver != protoVersion) {
			t.Fatalf("accepted: v2=%v more=%d len=%d hdr=%+v", v2, more, len(first), h)
		}
		if !ok && (nonce != nil || more != 0) {
			t.Fatalf("refused but returned nonce=%x more=%d", nonce, more)
		}
	})
}

// --- the server's refusals, end to end ---

// rawAuthConn completes TLS with the server and returns the client conn and
// its EKM, for sending hand-made auth records.
func rawAuthConn(t *testing.T, addr string) (*utls.UConn, []byte) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}
	u := utls.UClient(raw, cfg, utls.HelloChrome_133)
	if err := u.Handshake(); err != nil {
		t.Fatal(err)
	}
	ekm, err := clientEKM(u, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return u, ekm
}

// logSink collects a Server's log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logSink) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func startLoggedServer(t *testing.T, key []byte, logs *logSink, onTunnel func(*Carrier)) (string, func()) {
	t.Helper()
	return serveWith(t, &Server{SharedKey: key, Logf: logs.logf}, onTunnel)
}

// serveWith runs srv (its cert and backend set here) on a local listener.
func serveWith(t *testing.T, srv *Server, onTunnel func(*Carrier)) (string, func()) {
	t.Helper()
	be, stopB := backend(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv.Cert, srv.BackendAddr = testCert(t, "vpn.example.com"), be
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Handle(ctx, c, onTunnel)
		}
	}()
	return ln.Addr().String(), func() { cancel(); ln.Close(); stopB() }
}

// A v2 hs2 with the right key is refused (closed, not served the website),
// logged with the exact line at most once a minute per peer, and counted.
func TestServerRefusesV2Peer(t *testing.T) {
	resetProtoStats()
	defer resetProtoStats()
	key := bytes.Repeat([]byte{0x37}, 32)
	logs := &logSink{}
	hit := make(chan struct{}, 4)
	addr, stop := startLoggedServer(t, key, logs, func(c *Carrier) { hit <- struct{}{}; c.Close() })
	defer stop()
	for i := 0; i < 2; i++ {
		u, ekm := rawAuthConn(t, addr)
		u.Write(v2ClientRecord(key, ekm))
		u.SetReadDeadline(time.Now().Add(2 * time.Second))
		out, _ := io.ReadAll(u)
		u.Close()
		if len(out) != 0 {
			t.Fatalf("a v2 peer was answered: %q", out)
		}
	}
	select {
	case <-hit:
		t.Fatal("a v2 peer got a tunnel")
	case <-time.After(100 * time.Millisecond):
	}
	var v2Lines int
	for _, l := range logs.all() {
		if l == "peer runs protocol v2; upgrade both servers" {
			v2Lines++
		}
	}
	if v2Lines != 1 {
		t.Fatalf("v2 log lines %d, want 1 (at most one a minute per peer): %q", v2Lines, logs.all())
	}
	if l := ProtocolLine(); !strings.Contains(l, "refused v2 peers: 2,") || !strings.Contains(l, "s ago)") {
		t.Fatalf("status line %q", l)
	}
}

// A peer with no level in common is refused with the exact log line; the
// client reads ErrLevel from the proof (level 0) instead of a bare EOF.
func TestServerRefusesLevel(t *testing.T) {
	resetProtoStats()
	defer resetProtoStats()
	key := bytes.Repeat([]byte{0x38}, 32)
	logs := &logSink{}
	hit := make(chan struct{}, 1)
	addr, stop := startLoggedServer(t, key, logs, func(c *Carrier) { hit <- struct{}{}; c.Close() })
	defer stop()
	u, ekm := rawAuthConn(t, addr)
	defer u.Close()
	h := AuthHdr{Ver: protoVersion, MinLevel: 3, MaxLevel: 4, Caps: 0xF}
	rec, nonce := makeClientAuth(key, ekm, h)
	u.Write(rec)
	u.SetReadDeadline(time.Now().Add(2 * time.Second))
	p, err := readServerProof(u, key, nonce, ekm)
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if err := checkProofLevel(h, p); !errors.Is(err, ErrLevel) {
		t.Fatalf("level %d accepted by a client speaking 3-4: %v", p.Level, err)
	}
	if n, _ := u.Read(make([]byte, 1)); n != 0 {
		t.Fatal("the link stayed open")
	}
	select {
	case <-hit:
		t.Fatal("a peer with no common level got a tunnel")
	case <-time.After(100 * time.Millisecond):
	}
	want := "peer speaks protocol v3 levels 3-4, this server 1-2; upgrade the older server"
	if got := logs.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("log %q, want %q", got, want)
	}
	if l := ProtocolLine(); !strings.Contains(l, "refused peers (level): 1 (last") {
		t.Fatalf("status line %q", l)
	}
}

// A good link carries the negotiated level and caps on both carriers, and the
// status line says so in the exact words of the design.
func TestCarrierNegotiatedLevel(t *testing.T) {
	resetProtoStats()
	defer resetProtoStats()
	key := bytes.Repeat([]byte{0x39}, 32)
	t.Setenv("HS2_TUNE_HQG", "0")
	got := make(chan *Carrier, 1)
	addr, stop := startLoggedServer(t, key, &logSink{}, func(c *Carrier) { got <- c })
	defer stop()
	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	want := CapClass | CapSched | CapSendCap | CapGroup | CapHint | CapLight
	if car.Level() != 2 || car.Caps() != want {
		t.Fatalf("client carrier: level %d caps %#x", car.Level(), car.Caps())
	}
	select {
	case sc := <-got:
		defer sc.Close()
		if sc.Level() != 2 || sc.Caps() != want {
			t.Fatalf("server carrier: level %d caps %#x", sc.Level(), sc.Caps())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tunnel")
	}
	if l := ProtocolLine(); l != "protocol: v3 level 2 (peer levels 1-2, caps 0x9B); refused v2 peers: 0, refused peers (level): 0" {
		t.Fatalf("status line %q", l)
	}
}

// The link class (RoleCaps) is in a link's caps exactly as both ends
// offered it: the dialer by its extra caps, the server by OfferCaps (asked
// only when the dialer offered a class); the edge offers one class, the
// exit both, so the link has the edge's; a level 1 link never has a level 2
// cap.
func TestCarrierLinkClass(t *testing.T) {
	resetProtoStats()
	defer resetProtoStats()
	key := bytes.Repeat([]byte{0x3a}, 32)
	for _, c := range []struct {
		name          string
		client, offer uint32
		class         uint32
		asked         int
	}{
		{"edge dials QUIET", CapQuiet, RoleCaps, CapQuiet, 1},
		{"edge dials BULK", CapBulk, RoleCaps, CapBulk, 1},
		{"exit dials, edge wants QUIET", RoleCaps, CapQuiet, CapQuiet, 1},
		{"exit dials, edge wants BULK", RoleCaps, CapBulk, CapBulk, 1},
		{"exit dials, edge has no classes", RoleCaps, 0, 0, 1},
		{"edge has no classes", 0, RoleCaps, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := make(chan *Carrier, 1)
			var asked atomic.Int32
			srv := &Server{SharedKey: key, OfferCaps: func() uint32 {
				asked.Add(1)
				return c.offer
			}}
			addr, stop := serveWith(t, srv, func(car *Carrier) { got <- car })
			defer stop()
			car, err := DialFromTimeoutCaps(addr, "vpn.example.com", key, "", 2*time.Second, c.client)
			if err != nil {
				t.Fatal(err)
			}
			defer car.Close()
			var sc *Carrier
			select {
			case sc = <-got:
				defer sc.Close()
			case <-time.After(2 * time.Second):
				t.Fatal("no tunnel")
			}
			if car.Caps()&RoleCaps != c.class || sc.Caps()&RoleCaps != c.class {
				t.Fatalf("client caps %#x, server caps %#x: class want %#x", car.Caps(), sc.Caps(), c.class)
			}
			if car.Caps()&CapGroup == 0 || car.Caps() != sc.Caps() {
				t.Fatalf("client caps %#x, server caps %#x", car.Caps(), sc.Caps())
			}
			if int(asked.Load()) != c.asked {
				t.Fatalf("OfferCaps asked %d times, want %d", asked.Load(), c.asked)
			}
		})
	}
	// a level 1 peer: the level 2 caps are out whatever both offered
	if lc := levelCaps(1, CapGroup|RoleCaps|CapSched); lc != CapSched {
		t.Fatalf("level 1 caps %#x, want %#x", lc, CapSched)
	}
	if _, caps, ok := negotiate(AuthHdr{Ver: protoVersion, MinLevel: 1, MaxLevel: 1, Caps: CapGroup | RoleCaps | CapSched},
		1, 2, CapGroup|RoleCaps|CapSched); !ok || caps != CapSched {
		t.Fatalf("a level 1 client: caps %#x ok %v, want %#x", caps, ok, CapSched)
	}
}

// resetProtoStats clears the process-wide protocol status between tests.
func resetProtoStats() {
	proto.mu.Lock()
	defer proto.mu.Unlock()
	proto.known, proto.level, proto.caps, proto.peerMin, proto.peerMax = false, 0, 0, 0, 0
	proto.refusedV2, proto.refusedLevel, proto.lastRefused, proto.logged = 0, 0, time.Time{}, nil
}

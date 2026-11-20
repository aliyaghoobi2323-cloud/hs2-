//go:build linux

package handoff

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const childEnv = "HS2_HANDOFF_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(childEnv) {
	case "dial": // a peer that connects and sends HELLO (auth tests)
		c, err := Dial(os.Getenv("HS2_HANDOFF_PATH"), Policy{UID: 0, Self: selfExe()}, 2*time.Second)
		if err == nil {
			c.Send(&Msg{Type: TypeHello, Hello: Hello{Proto: Proto}}, time.Second)
			c.Recv(2 * time.Second)
		}
		os.Exit(0)
	case "take":
		os.Exit(childTake())
	}
	os.Exit(m.Run())
}

func selfExe() string {
	e, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(e); err == nil {
		return r
	}
	return e
}

func testPolicy() Policy { return Policy{UID: uint32(os.Getuid()), Self: selfExe()} }

func pair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b, err := Pair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func listenerFile(t *testing.T, l net.Listener) *os.File {
	t.Helper()
	f, err := l.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// fd passing over a socketpair: a TCP listener with a connection already
// queued (its SYN was answered by the kernel before the hand-off) is
// accepted by the receiver; a UDP socket receives a datagram sent to the
// old one's address; a fake TUN fd (a pipe) reads what is written to it.
func TestFDPassSocketpair(t *testing.T) {
	a, b := pair(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	queued, err := net.Dial("tcp", ln.Addr().String()) // in the accept queue, not accepted yet
	if err != nil {
		t.Fatal(err)
	}
	defer queued.Close()
	queued.Write([]byte("early"))
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	uf, _ := uc.File()
	pr, pw, _ := os.Pipe()
	defer pw.Close()
	lf := listenerFile(t, ln)
	if err := a.Send(&Msg{Type: TypeFDs, FDs: []FD{{"user443", lf}, {"udp443", uf}, {"tun-hs0", pr}}}, time.Second); err != nil {
		t.Fatal(err)
	}
	lf.Close()
	uf.Close()
	pr.Close()
	m, err := b.Recv(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.FDs) != 3 || m.FDs[0].Name != "user443" || m.FDs[1].Name != "udp443" || m.FDs[2].Name != "tun-hs0" {
		t.Fatalf("fds %+v", m.FDs)
	}
	nl, err := net.FileListener(m.FDs[0].File)
	if err != nil {
		t.Fatal(err)
	}
	defer nl.Close()
	nl.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	c, err := nl.Accept()
	if err != nil {
		t.Fatalf("queued connection not accepted by the receiver: %v", err)
	}
	buf := make([]byte, 5)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "early" {
		t.Fatalf("queued data %q %v", buf, err)
	}
	c.Close()
	pc, err := net.FilePacketConn(m.FDs[1].File)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	s, _ := net.Dial("udp", uc.LocalAddr().String())
	s.Write([]byte("dgram"))
	s.Close()
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "dgram" {
		t.Fatalf("udp %q %v", buf[:n], err)
	}
	pw.Write([]byte("pkt01"))
	if _, err := io.ReadFull(m.FDs[2].File, buf); err != nil || string(buf) != "pkt01" {
		t.Fatalf("fake tun %q %v", buf, err)
	}
	for _, f := range m.FDs {
		f.File.Close()
	}
}

func hello(gen uint32) Hello {
	h := Hello{Proto: Proto, Features: 0xa5, InstStart: 1_763_500_000_123_456_789, InstRand: 0xdeadbeef, Gen: gen}
	for i := range h.ConfigHash {
		h.ConfigHash[i] = byte(i * 7)
	}
	return h
}

// Metadata round trip through the whole Give/Take/Listening/Ready exchange.
func TestMetadataRoundTrip(t *testing.T) {
	a, b := pair(t)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	oldH, newH := hello(4), hello(5)
	newH.InstStart++
	done := make(chan error, 1)
	var gotNew Hello
	go func() {
		var err error
		gotNew, err = a.Give(oldH, []FD{{"tun-hs0", r}}, time.Second, 2*time.Second)
		done <- err
	}()
	tk, err := b.Take(newH, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Peer != oldH {
		t.Fatalf("peer hello %+v != %+v", tk.Peer, oldH)
	}
	f := tk.File("tun-hs0")
	if f == nil || tk.File("tun-hs0") != nil {
		t.Fatal("File(name) must hand the fd out once")
	}
	f.Close()
	// both Hellos carry FeatListening (0xa5 has bit 0): LISTENING before READY
	if err := tk.Listening(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ready(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if gotNew != newH {
		t.Fatalf("old saw %+v", gotNew)
	}
	for _, typ := range []uint8{TypeReady, TypeGo} {
		bb, _ := encode(&Msg{Type: typ})
		if m, _, err := decode(bb, 0); err != nil || m.Type != typ {
			t.Fatal(typ, err)
		}
	}
	bb, _ := encode(&Msg{Type: TypeDrainStat, Drain: DrainStat{Links: 77, Users: 12345}})
	if m, _, err := decode(bb, 0); err != nil || m.Drain != (DrainStat{77, 12345}) {
		t.Fatal(m, err)
	}
}

// A protocol mismatch ends in ABORT on both sides (cold restart).
func TestProtoMismatchAborts(t *testing.T) {
	a, b := pair(t)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go func() {
		_, err := a.Give(hello(1), []FD{{"x", r}}, time.Second, time.Second)
		done <- err
	}()
	h := hello(2)
	h.Proto = Proto + 1
	_, err := b.Take(h, time.Second)
	if !errors.Is(err, ErrAborted) && !errors.Is(err, ErrProto) {
		t.Fatalf("new side: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrProto) {
		t.Fatalf("old side: %v", err)
	}
}

// Policy: uid and binary path.
func TestAuthPolicy(t *testing.T) {
	p := Policy{UID: 0, Self: "/usr/local/bin/hs2-rel69p", Allowed: []string{"/usr/local/bin/hs2-rel70"}}
	cases := []struct {
		cr Cred
		ok bool
	}{
		{Cred{PID: 10, UID: 0, Exe: "/usr/local/bin/hs2-rel69p"}, true},
		{Cred{PID: 10, UID: 0, Exe: "/usr/local/bin/hs2-rel70"}, true},
		{Cred{PID: 10, UID: 1000, Exe: "/usr/local/bin/hs2-rel69p"}, false}, // non-root
		{Cred{PID: 10, UID: 0, Exe: "/tmp/evil"}, false},
		{Cred{PID: 10, UID: 0, Exe: ""}, false},
	}
	for _, c := range cases {
		if err := p.Check(c.cr); (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrAuth)) {
			t.Fatalf("%+v: %v", c.cr, err)
		}
	}
}

// The named socket: mode 0600, peer credentials read, a peer of another uid
// refused (simulated by requiring another uid), a stale socket replaced, a
// non-socket refused, the file removed on close.
func TestListenAuth(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "hs2-test.sock")
	l, err := Listen(path, testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v %v", fi.Mode(), err)
	}
	go func() {
		c, err := Dial(path, testPolicy(), time.Second)
		if err == nil {
			c.Recv(time.Second)
			c.Close()
		}
	}()
	c, err := l.Accept(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := PeerCred(c.Unix())
	if err != nil || int(cr.PID) != os.Getpid() || cr.Exe != selfExe() {
		t.Fatalf("peercred %+v %v (pid %d exe %s)", cr, err, os.Getpid(), selfExe())
	}
	c.Close()
	// another uid required: the same peer is refused
	l.pol.UID = uint32(os.Getuid()) + 1
	go func() {
		if c, err := Dial(path, testPolicy(), time.Second); err == nil {
			c.Recv(time.Second)
			c.Close()
		}
	}()
	if _, err := l.Accept(2 * time.Second); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong uid accepted: %v", err)
	}
	// and the dialer refuses a listener of another uid / binary
	l.pol = testPolicy()
	if _, err := Dial(path, Policy{UID: uint32(os.Getuid()) + 1, Self: selfExe()}, time.Second); !errors.Is(err, ErrAuth) {
		t.Fatalf("dialer accepted a wrong-uid listener: %v", err)
	}
	if _, err := Dial(path, Policy{UID: uint32(os.Getuid()), Self: "/nonexistent"}, time.Second); !errors.Is(err, ErrAuth) {
		t.Fatalf("dialer accepted a wrong binary: %v", err)
	}
	l.l.Close() // a crash: the socket file stays
	if l2, err := Listen(path, testPolicy()); err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	} else {
		l2.Close()
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("socket file left after Close")
	}
	os.WriteFile(path, []byte("x"), 0o600)
	if _, err := Listen(path, testPolicy()); err == nil {
		t.Fatal("a regular file was replaced")
	}
	os.Chmod(dir, 0o777)
	if _, err := Listen(filepath.Join(dir, "b.sock"), testPolicy()); err == nil {
		t.Fatal("a world-writable directory was used")
	}
	os.Chmod(dir, 0o700)
}

// A real non-root peer (uid 65534) is refused by SO_PEERCRED. Root only:
// the test binary is copied where nobody can run it and the socket is
// opened to others so that the uid check, not the file mode, refuses.
func TestAuthRejectsNonRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir, err := os.MkdirTemp("", "hs2ho")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o755)
	bin := filepath.Join(dir, "t.test")
	src, _ := os.ReadFile(selfExe())
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.sock")
	l, err := Listen(path, Policy{UID: 0, Self: selfExe(), Allowed: []string{bin}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	os.Chmod(path, 0o666)
	cmd := exec.Command(bin, "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"=dial", "HS2_HANDOFF_PATH="+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	_, err = l.Accept(5 * time.Second)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("uid 65534 peer: %v", err)
	}
	t.Logf("refused: %v", err)
}

// Timeouts: a silent old process, a missing socket, a peer that never
// sends READY: each ends in an error within its timeout, and TakeOrCold
// runs the cold path.
func TestTimeoutFallback(t *testing.T) {
	_, b := pair(t)
	t0 := time.Now()
	cold := 0
	tk, terr, cerr := TakeOrCold(func() (*Taken, error) { return b.Take(hello(1), 200*time.Millisecond) }, func() error { cold++; return nil })
	if tk != nil || !errors.Is(terr, ErrTimeout) || cerr != nil || cold != 1 {
		t.Fatalf("silent peer: %v %v %v cold=%d", tk, terr, cerr, cold)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
	_, terr, _ = TakeOrCold(func() (*Taken, error) {
		c, err := Dial(filepath.Join(t.TempDir(), "none.sock"), testPolicy(), 200*time.Millisecond)
		if err != nil {
			return nil, err
		}
		return c.Take(hello(1), time.Second)
	}, func() error { cold++; return nil })
	if terr == nil || cold != 2 {
		t.Fatalf("missing socket: %v cold=%d", terr, cold)
	}
	// the old side times out waiting for READY, aborts, keeps its fds
	a2, b2 := pair(t)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go func() { _, err := a2.Give(hello(1), []FD{{"x", r}}, time.Second, 300*time.Millisecond); done <- err }()
	tk2, err := b2.Take(hello(2), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrTimeout) {
		t.Fatalf("old side: %v", err)
	}
	if _, err := tk2.Conn.Recv(time.Second); !errors.Is(err, ErrAborted) {
		t.Fatalf("new side after old timeout: %v", err)
	}
	tk2.Close()
	if _, err := r.Stat(); err != nil {
		t.Fatal("old side lost its own fd")
	}
}

// DRAINSTAT reaches the sink; EOF and silence report 0.
func TestDrainStatFollow(t *testing.T) {
	a, b := pair(t)
	var got []int
	done := make(chan error, 1)
	go func() { done <- b.FollowDrain(func(n int) { got = append(got, n) }, time.Second) }()
	for _, n := range []uint32{9, 4, 1} {
		if err := a.SendDrainStat(DrainStat{Links: n, Users: n * 10}); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()
	<-done
	if len(got) != 4 || got[0] != 9 || got[1] != 4 || got[2] != 1 || got[3] != 0 {
		t.Fatalf("sink saw %v", got)
	}
	_, d := pair(t)
	var last = -1
	t0 := time.Now()
	err := d.FollowDrain(func(n int) { last = n }, 150*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || last != 0 || time.Since(t0) > time.Second {
		t.Fatalf("silence: %v last=%d", err, last)
	}
}

// Malformed input never panics, never leaks an fd, and is refused.
func TestDecodeRejects(t *testing.T) {
	good, _ := encode(&Msg{Type: TypeDrainStat, Drain: DrainStat{1, 2}})
	bad := [][]byte{
		nil, {1}, {2, 6, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0}, // wrong version
		{1, 99, 0, 0},                        // unknown type
		{1, 6, 0, 4, 0, 0, 0, 0},             // wrong length for type
		append(append([]byte{}, good...), 0), // trailing byte
		{1, 2, 0, 3, 1, 5, 'a'},              // FDS name cut
		{1, 3, 0, 1, 0},                      // READY with payload
	}
	for _, b := range bad {
		if _, _, err := decode(b, 0); err == nil {
			t.Fatalf("% x accepted", b)
		}
	}
	if _, _, err := decode(good, 1); err == nil {
		t.Fatal("fds on DRAINSTAT accepted")
	}
	f, _ := encode(&Msg{Type: TypeFDs, FDs: []FD{{"a", os.Stdin}}})
	if _, _, err := decode(f, 2); err == nil {
		t.Fatal("fd count mismatch accepted")
	}
	// fds on a non-FDS message are closed by Recv
	a, b := pair(t)
	r, w, _ := os.Pipe()
	defer w.Close()
	rights := []FD{{"x", r}}
	bb, _ := encode(&Msg{Type: TypeGo})
	if _, _, err := a.c.WriteMsgUnix(bb, rightsOf(rights), nil); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := b.Recv(time.Second); !errors.Is(err, ErrBadMsg) {
		t.Fatalf("GO with fds: %v", err)
	}
	// the pipe's read end is now closed everywhere: a write fails with EPIPE
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("received fd leaked")
	}
}

func FuzzDecode(f *testing.F) {
	for _, m := range []*Msg{{Type: TypeHello, Hello: hello(1)}, {Type: TypeReady}, {Type: TypeGo},
		{Type: TypeAbort, Reason: "x"}, {Type: TypeDrainStat}, {Type: TypeFDs, FDs: []FD{{"tun-hs0", nil}, {"u", nil}}}} {
		b, _ := encode(m)
		f.Add(b, len(m.FDs))
	}
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		m, names, err := decode(b, n&63)
		if err != nil {
			return
		}
		if m.Type == TypeFDs && len(names) != n&63 {
			t.Fatal("names != fds")
		}
		if m.Type != TypeFDs {
			re, err := encode(m)
			if err != nil || !bytes.Equal(re, b) {
				t.Fatalf("re-encode % x != % x (%v)", re, b, err)
			}
		}
	})
}

// zdu350 review M2: GiveListening runs its hook only after LISTENING when both
// sides carry FeatListening, right after FDS otherwise; a new process that
// never sends LISTENING never triggers it.
func TestGiveListening(t *testing.T) {
	for _, feat := range []uint32{0, FeatListening} {
		a, b := pair(t)
		r, w, _ := os.Pipe()
		h := Hello{Proto: Proto, Features: feat}
		hooked := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			_, err := a.GiveListening(h, []FD{{"tun-hs0", r}}, time.Second, 2*time.Second, func() { hooked <- struct{}{} })
			done <- err
		}()
		tk, err := b.Take(h, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-hooked:
			if feat != 0 {
				t.Fatal("hook ran before LISTENING")
			}
		case <-time.After(200 * time.Millisecond):
			if feat == 0 {
				t.Fatal("no LISTENING feature: hook did not run after FDS")
			}
			if err := tk.Listening(time.Second); err != nil {
				t.Fatal(err)
			}
			<-hooked
		}
		if err := tk.Ready(time.Second); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		tk.Close()
		r.Close()
		w.Close()
	}
}

// zdu391 (lab1 S9c: "give: unexpected message: got type 7, want 3"): the new
// process's LISTENING follows the Hello it SENT in Take. Mixed Hellos (old
// side with the bit, new side's Take Hello without it, or the reverse) never
// put a LISTENING where the other side waits for READY.
func TestListeningFollowsSentHello(t *testing.T) {
	for _, c := range []struct{ give, take uint32 }{{FeatListening, 0}, {0, FeatListening}, {FeatListening, FeatListening}, {0, 0}} {
		a, b := pair(t)
		r, w, _ := os.Pipe()
		hooked := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			_, err := a.GiveListening(Hello{Proto: Proto, Features: c.give}, []FD{{"tun-hs0", r}}, time.Second, 2*time.Second, func() { hooked <- struct{}{} })
			done <- err
		}()
		tk, err := b.Take(Hello{Proto: Proto, Features: c.take}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := tk.Listening(time.Second); err != nil {
			t.Fatalf("%+v: listening: %v", c, err)
		}
		if err := tk.Ready(time.Second); err != nil {
			t.Fatalf("%+v: ready: %v", c, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%+v: give: %v", c, err)
		}
		<-hooked
		tk.Close()
		r.Close()
		w.Close()
	}
}

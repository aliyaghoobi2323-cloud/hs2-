package tlscarrier

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFrontProfileDefaults(t *testing.T) {
	var nilp *FrontProfile
	for _, p := range []*FrontProfile{nilp, {}, {Kind: "tls"}, {Kind: "TLS"}} {
		if !p.IsDefault() || p.NormKind() != FrontTLS || p.Validate() != nil {
			t.Fatalf("%+v: want the default tls front", p)
		}
		if d, err := DialerFor(p); err != nil || d != (TLSFront{}) {
			t.Fatalf("%+v: DialerFor = %v %v", p, d, err)
		}
	}
	for _, p := range []*FrontProfile{{Kind: "reality"}, {Kind: "both"}, {Kind: "tls", Target: "x:443"}} {
		if p.IsDefault() {
			t.Fatalf("%+v: not default", p)
		}
	}
	for _, k := range []string{"reality", "both", "zzz"} {
		p := &FrontProfile{Kind: k}
		if p.Validate() == nil {
			t.Fatalf("%s: phase 1 must refuse it", k)
		}
		if _, err := DialerFor(p); !errors.Is(err, ErrFrontUnsupported) {
			t.Fatalf("%s: DialerFor err %v", k, err)
		}
	}
}

// The private key never leaves the listening side.
func TestFrontProfilePrivateKeyNotMarshalled(t *testing.T) {
	js, _ := json.Marshal(&FrontProfile{Kind: "reality", PublicKey: "PUB", PrivateKey: "SECRET", ShortIDs: []string{"ab"}})
	if strings.Contains(string(js), "SECRET") || !strings.Contains(string(js), "PUB") {
		t.Fatalf("marshalled %s", js)
	}
	var p FrontProfile
	json.Unmarshal([]byte(`{"kind":"reality","private_key":"x","public_key":"y"}`), &p)
	if p.PrivateKey != "" || p.PublicKey != "y" {
		t.Fatalf("unmarshalled %+v", p)
	}
	js, _ = json.Marshal(&FrontProfile{})
	if string(js) != "{}" {
		t.Fatalf("default profile marshals to %s", js)
	}
}

// The tls front end to end: DialPathFront -> Server.AcceptFront is today's
// DialFrom -> Handle (a tunnel carrier both ways; a wrong key goes to cover).
func TestTLSFrontRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	be, stop := backend(t)
	defer stop()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	var acc Acceptor = &Server{SharedKey: key, Cert: testCert(t, "front.test"), BackendAddr: be}
	got := make(chan *Carrier, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go acc.AcceptFront(context.Background(), c, func(car *Carrier) { got <- car })
		}
	}()
	for _, connect := range []time.Duration{0, 2 * time.Second} {
		car, err := DialPathFront(context.Background(), DialPath{Addr: ln.Addr().String(), SNI: "front.test", Key: key, Connect: connect})
		if err != nil {
			t.Fatal(err)
		}
		var srv *Carrier
		select {
		case srv = <-got:
		case <-time.After(5 * time.Second):
			t.Fatal("no tunnel accepted")
		}
		if err := car.SendFrame(1, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		if ft, p, err := srv.ReadFrame(); err != nil || ft != 1 || string(p) != "hello" {
			t.Fatalf("frame %d %q %v", ft, p, err)
		}
		car.Close()
		srv.Close()
	}
	// wrong key: never a tunnel (handled as cover)
	if _, err := DialPathFront(context.Background(), DialPath{Addr: ln.Addr().String(), SNI: "front.test", Key: []byte("wrong-key-wrong-key-wrong-key-xx")}); err == nil {
		t.Fatal("a wrong key must not authenticate")
	}
	select {
	case <-got:
		t.Fatal("a wrong key reached onTunnel")
	case <-time.After(300 * time.Millisecond):
	}
	// an unsupported front fails before any byte is sent
	if _, err := DialPathFront(context.Background(), DialPath{Addr: ln.Addr().String(), Key: key, Profile: &FrontProfile{Kind: "reality"}}); !errors.Is(err, ErrFrontUnsupported) {
		t.Fatalf("reality in phase 1: %v", err)
	}
}

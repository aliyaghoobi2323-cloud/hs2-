package smux

import (
	"net"
	"testing"
	"time"
)

// answeredPair returns a client and a server session of version ver over a
// pipe.
func answeredPair(t *testing.T, ver int) (*Session, *Session) {
	t.Helper()
	c1, c2 := net.Pipe()
	config := DefaultConfig()
	config.Version = ver
	config.KeepAliveDisabled = true
	client, err := Client(c1, config)
	if err != nil {
		t.Fatal(err)
	}
	server, err := Server(c2, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// waitAnswered polls s.Answered() until it is want or d passes.
func waitAnswered(s *Stream, want bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for s.Answered() != want {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// A version 2 stream is answered by the peer's first read (its window
// update), by data, or by its FIN -- and by nothing else: a stream the peer
// has accepted but not read from stays unanswered.
func TestStreamAnswered(t *testing.T) {
	client, server := answeredPair(t, 2)
	open := func() (*Stream, *Stream) {
		t.Helper()
		cs, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		ss, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		return cs, ss
	}

	cs, ss := open()
	if waitAnswered(cs, true, 50*time.Millisecond) {
		t.Fatal("answered before the peer read anything")
	}
	var b [1]byte
	if _, err := ss.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	if !waitAnswered(cs, true, time.Second) {
		t.Fatal("the peer's first read did not answer the stream")
	}

	cs, ss = open()
	if _, err := ss.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if !waitAnswered(cs, true, time.Second) {
		t.Fatal("data from the peer did not answer the stream")
	}

	cs, ss = open()
	ss.Close()
	if !waitAnswered(cs, true, time.Second) {
		t.Fatal("the peer's FIN did not answer the stream")
	}

	// The opener's own frames never answer a stream on the accepting side's
	// behalf: the server's stream counts the client's data as an answer.
	cs, ss = open()
	if !waitAnswered(ss, true, time.Second) {
		t.Fatal("the opener's data did not answer the accepted stream")
	}
	_ = cs
}

// Version 1 sends no window updates: only data or a FIN answers.
func TestStreamAnsweredV1(t *testing.T) {
	client, server := answeredPair(t, 1)
	cs, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	cs.Write([]byte("hello"))
	ss, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	ss.Read(b[:])
	if waitAnswered(cs, true, 50*time.Millisecond) {
		t.Fatal("a version 1 read answered the stream")
	}
	ss.Write([]byte("y"))
	if !waitAnswered(cs, true, time.Second) {
		t.Fatal("data did not answer the stream")
	}
}

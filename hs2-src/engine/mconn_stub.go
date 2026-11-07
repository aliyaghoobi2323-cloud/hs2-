package engine

// TEMPORARY STUB of mConn (part A, mconn.go, replaces this file): one sub,
// the connection's raw bytes on it, no records, no second copy, no switch to
// bulk. Just enough for the integration (mconn_link.go) to run end to end.
// The names and signatures are the contract's (mconn_api.go).

import (
	"io"
	"sync"
)

type mConn struct {
	id uint64

	mu     sync.Mutex
	sub    mSub
	closed bool
	ready  chan struct{} // closed once sub is set
	die    chan struct{}
}

// newMConnIran opens the connection's (one) light sub with open.
func newMConnIran(id uint64, open mOpener) (*mConn, error) {
	sub, _, err := open(planeLight, nil)
	if err != nil {
		return nil, err
	}
	m := newMConnExit(id)
	m.attach(sub, 0)
	return m, nil
}

// newMConnExit makes an exit-side connection; its sub arrives with attach.
func newMConnExit(id uint64) *mConn {
	return &mConn{id: id, ready: make(chan struct{}), die: make(chan struct{})}
}

// attach gives the connection its sub; the stub keeps only the first and
// closes any later one (or any that arrives after Close).
func (m *mConn) attach(sub mSub, role byte) {
	m.mu.Lock()
	if m.sub != nil || m.closed {
		m.mu.Unlock()
		sub.Close()
		return
	}
	m.sub = sub
	close(m.ready)
	m.mu.Unlock()
}

// wait returns the sub once it is attached, or io.EOF once the connection
// is closed without one.
func (m *mConn) wait() (mSub, error) {
	select {
	case <-m.ready:
		return m.sub, nil
	default:
	}
	select {
	case <-m.ready:
		return m.sub, nil
	case <-m.die:
		return nil, io.EOF
	}
}

func (m *mConn) Read(p []byte) (int, error) {
	s, err := m.wait()
	if err != nil {
		return 0, err
	}
	n, err := s.Read(p)
	if err != nil && err != io.EOF {
		m.Close()
	}
	return n, err
}

func (m *mConn) Write(p []byte) (int, error) {
	s, err := m.wait()
	if err != nil {
		return 0, io.ErrClosedPipe
	}
	n, err := s.Write(p)
	if err != nil {
		m.Close()
	}
	return n, err
}

func (m *mConn) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	s := m.sub
	close(m.die)
	m.mu.Unlock()
	if s != nil {
		s.Close()
	}
	return nil
}

// GetDieCh is closed when the connection fails or ends.
func (m *mConn) GetDieCh() <-chan struct{} { return m.die }

package smux

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// recvFrame is a frame as a peer writes it on the wire (protocol version 2):
// [ver][cmd][length u16 LE][stream id u32 LE][data].
func recvFrame(cmd byte, sid uint32, data []byte) []byte {
	b := make([]byte, headerSize+len(data))
	b[0], b[1] = 2, cmd
	binary.LittleEndian.PutUint16(b[2:], uint16(len(data)))
	binary.LittleEndian.PutUint32(b[4:], sid)
	copy(b[headerSize:], data)
	return b
}

// recvSeq is a group stream's data frame payload: its sequence number, then
// its data.
func recvSeq(seq uint32, data string) []byte {
	b := make([]byte, seqLen+len(data))
	binary.BigEndian.PutUint32(b, seq)
	copy(b[seqLen:], data)
	return b
}

// FuzzSessionRecv: whatever bytes the other end sends, the session's
// receive side (recvLoop) neither panics nor misses the connection's end: frame headers, the SYN payload of hs2 wire v3 (13 bytes, a sub-stream
// role needs a group, §4.2: REDESIGN §5.10 names it a parser of peer data
// to fuzz), group streams' sequence numbers (§4.7) and window updates. The
// streams it accepts are read to their end, as the engine reads them.
func FuzzSessionRecv(f *testing.F) {
	syn := EncodeSyn(7, 1)
	grp := EncodeSynGroup(9, 2, RoleMain, 0xA1B2, 0)
	mov := EncodeSynGroup(9, 2, RoleMove, 0xA1B2, 3)
	cat := func(fs ...[]byte) []byte {
		var b []byte
		for _, x := range fs {
			b = append(b, x...)
		}
		return b
	}
	upd := make([]byte, szCmdUPD)
	binary.LittleEndian.PutUint32(upd[4:], 1<<20)
	f.Add(cat(recvFrame(cmdSYN, 3, syn[:]), recvFrame(cmdPSH, 3, []byte("hello")), recvFrame(cmdFIN, 3, nil)))
	f.Add(cat(recvFrame(cmdSYN, 5, syn[:12]), recvFrame(cmdSYN, 7, nil), recvFrame(cmdPSH, 5, []byte("x"))))
	f.Add(cat(recvFrame(cmdSYN, 9, grp[:]), recvFrame(cmdPSH, 9, recvSeq(0, "ab")), recvFrame(cmdPSH, 9, recvSeq(2, "ef")),
		recvFrame(cmdSYN, 11, mov[:]), recvFrame(cmdPSH, 11, recvSeq(3, "gh")), recvFrame(cmdPSH, 9, recvSeq(1, "cd")),
		recvFrame(cmdPSH, 9, []byte{0, 0}), recvFrame(cmdFIN, 9, nil), recvFrame(cmdFIN, 11, nil)))
	f.Add(cat(recvFrame(cmdNOP, 0, nil), recvFrame(cmdUPD, 3, upd), recvFrame(cmdSYN, 3, syn[:]), recvFrame(cmdUPD, 3, upd[:4])))
	f.Add([]byte{1, 0, 13, 0, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, in []byte) {
		a, b := net.Pipe()
		cfg := DefaultConfig()
		cfg.Version = 2
		cfg.KeepAliveDisabled = true
		s, err := Server(a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			b.SetWriteDeadline(time.Now().Add(2 * time.Second))
			b.Write(in)
			b.Close()
		}()
		accepted := make(chan struct{})
		go func() {
			defer close(accepted)
			for {
				st, err := s.AcceptStream()
				if err != nil {
					return
				}
				go func() {
					st.SetReadDeadline(time.Now().Add(2 * time.Second))
					io.Copy(io.Discard, st)
					st.Close()
				}()
			}
		}()
		// the peer's end closes the connection: the receive side must see it
		// (AcceptStream returns the read error) within the bytes' time
		select {
		case <-accepted:
		case <-s.CloseChan():
		case <-time.After(3 * time.Second):
			t.Fatalf("the receive side missed its connection's end for 3 s (%d bytes in)", len(in))
		}
		s.Close()
		<-accepted
		if n := s.BadSyn(); n > uint64(len(in)/headerSize) {
			t.Fatalf("%d SYNs refused, from %d bytes", n, len(in))
		}
	})
}

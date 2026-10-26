package smux

import (
	"encoding/binary"
	"fmt"
)

const ( // cmds
	// protocol version 1:
	cmdSYN byte = iota // stream open
	cmdFIN             // stream close, a.k.a EOF mark
	cmdPSH             // data push
	cmdNOP             // no operation

	// protocol version 2 extra commands
	// notify bytes consumed by remote peer-end
	cmdUPD
)

const (
	// data size of cmdUPD, format:
	// |4B data consumed(ACK)| 4B window size(WINDOW) |
	szCmdUPD = 8
)

const (
	// initial peer window guess, a slow-start
	initialPeerWindow = 262144
)

// SynPayloadLen is the size of the payload every SYN carries (hs2 wire v3,
// docs/architecture/REDESIGN.fa.md §4.2):
//
//	[user tag u32 BE][class/role u8][group id u32 BE][seq base u32 BE]
//
// class/role: bits 0-1 the class hint (0 unknown, 1 light, 2 bulk; a hint
// only, the receiver decides), bits 2-3 the stream's role (0: a stream on its
// own), bits 4-7 reserved. A SYN without it, or with another length, opens no
// stream: the opener gets a FIN for it (BadSyn).
const SynPayloadLen = 13

// EncodeSyn is the payload of a stream on its own: group id 0, seq base 0,
// role 0.
func EncodeSyn(userTag uint32, classHint byte) [SynPayloadLen]byte {
	var p [SynPayloadLen]byte
	binary.BigEndian.PutUint32(p[0:], userTag)
	p[4] = classHint & 0x03
	return p
}

// synValid reports whether a SYN payload of the right length can open a
// stream: a sub-stream role (bits 2-3) needs a group.
func synValid(p [SynPayloadLen]byte) bool {
	role := p[4] >> 2 & 0x03
	return role == 0 || binary.BigEndian.Uint32(p[5:]) != 0
}

const (
	sizeOfVer    = 1
	sizeOfCmd    = 1
	sizeOfLength = 2
	sizeOfSid    = 4
	headerSize   = sizeOfVer + sizeOfCmd + sizeOfSid + sizeOfLength
)

// Frame defines a packet from or to be multiplexed into a single connection
type Frame struct {
	ver  byte
	cmd  byte
	sid  uint32
	data []byte
}

func newFrame(version byte, cmd byte, sid uint32) Frame {
	return Frame{ver: version, cmd: cmd, sid: sid}
}

type rawHeader [headerSize]byte

func (h rawHeader) Version() byte {
	return h[0]
}

func (h rawHeader) Cmd() byte {
	return h[1]
}

func (h rawHeader) Length() uint16 {
	return binary.LittleEndian.Uint16(h[2:])
}

func (h rawHeader) StreamID() uint32 {
	return binary.LittleEndian.Uint32(h[4:])
}

func (h rawHeader) String() string {
	return fmt.Sprintf("Version:%d Cmd:%d StreamID:%d Length:%d",
		h.Version(), h.Cmd(), h.StreamID(), h.Length())
}

type updHeader [szCmdUPD]byte

func (h updHeader) Consumed() uint32 {
	return binary.LittleEndian.Uint32(h[:])
}
func (h updHeader) Window() uint32 {
	return binary.LittleEndian.Uint32(h[4:])
}

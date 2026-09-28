package fec

import (
	"encoding/binary"
	"errors"
	"sync"

	"github.com/klauspost/reedsolomon"
)

// HeaderLen is the size of the header in front of every shard packet.
const HeaderLen = 9

// lenPrefix is the payload length inside shard content.
const lenPrefix = 2

// MaxShards bounds k+r (GF(2^8) Reed-Solomon allows 256 in total).
const MaxShards = 255

var (
	errShort    = errors.New("fec: packet shorter than its header")
	errBadShard = errors.New("fec: malformed shard")
	errTooBig   = errors.New("fec: payload larger than the shard size")
)

type header struct {
	group uint32
	idx   int
	k, r  int
	size  int
}

func putHeader(b []byte, h header) {
	binary.BigEndian.PutUint32(b[0:4], h.group)
	b[4] = byte(h.idx)
	b[5] = byte(h.k)
	b[6] = byte(h.r)
	binary.BigEndian.PutUint16(b[7:9], uint16(h.size))
}

func parseHeader(b []byte) (header, error) {
	if len(b) < HeaderLen {
		return header{}, errShort
	}
	return header{
		group: binary.BigEndian.Uint32(b[0:4]),
		idx:   int(b[4]),
		k:     int(b[5]),
		r:     int(b[6]),
		size:  int(binary.BigEndian.Uint16(b[7:9])),
	}, nil
}

// IsParity reports whether a shard packet carries parity (k is only set on
// parity). It lets a sender queue parity behind data without parsing twice.
func IsParity(pkt []byte) bool { return len(pkt) >= HeaderLen && pkt[5] != 0 }

// codecs caches Reed-Solomon encoders per (k, r): building one inverts a
// matrix, and groups reuse a handful of shapes.
type codecs struct {
	mu sync.Mutex
	m  map[[2]int]reedsolomon.Encoder
}

func (c *codecs) get(k, r int) (reedsolomon.Encoder, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[[2]int]reedsolomon.Encoder)
	}
	key := [2]int{k, r}
	if e, ok := c.m[key]; ok {
		return e, nil
	}
	e, err := reedsolomon.New(k, r, reedsolomon.WithMaxGoroutines(1))
	if err != nil {
		return nil, err
	}
	c.m[key] = e
	return e, nil
}

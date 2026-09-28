package fec

import (
	"encoding/binary"
	"time"
)

// Decoder reassembles groups and recovers lost data shards. It is not safe
// for concurrent use; the carrier calls it from its single read loop.
type Decoder struct {
	ttl       time.Duration
	maxGroups int
	maxShard  int
	codecs    codecs

	groups map[uint32]*rxGroup
	order  []uint32 // arrival order of groups, for eviction
	stats  DecoderStats
}

// DecoderStats counts what the decoder has seen.
type DecoderStats struct {
	Data      uint64 // data shards received
	Parity    uint64 // parity shards received
	Recovered uint64 // data shards rebuilt from parity
	Lost      uint64 // data shards of expired groups that could not be rebuilt
	Dups      uint64 // data shards dropped as already delivered
	Invalid   uint64 // malformed shards
}

type rxGroup struct {
	born      time.Time
	k, r      int // 0 until a parity shard has arrived
	size      int
	shards    [][]byte // by index; data content unpadded, parity full size
	delivered [4]uint64
	nDeliv    int
	nParity   int
	done      bool // decoded or complete: only dedup from here on
}

func (g *rxGroup) isDelivered(i int) bool { return g.delivered[i/64]&(1<<(i%64)) != 0 }
func (g *rxGroup) markDelivered(i int) {
	g.delivered[i/64] |= 1 << (i % 64)
	g.nDeliv++
}

func (g *rxGroup) put(i int, b []byte) {
	for len(g.shards) <= i {
		g.shards = append(g.shards, nil)
	}
	g.shards[i] = append([]byte(nil), b...)
}

// NewDecoder builds a decoder. ttl bounds how long an incomplete group is
// kept waiting for parity; maxShard is the largest shard content accepted.
func NewDecoder(ttl time.Duration, maxShard int) *Decoder {
	if ttl <= 0 {
		ttl = 500 * time.Millisecond
	}
	return &Decoder{
		ttl:       ttl,
		maxGroups: 4096,
		maxShard:  maxShard,
		groups:    make(map[uint32]*rxGroup),
	}
}

// Stats returns a snapshot of the counters.
func (d *Decoder) Stats() DecoderStats { return d.stats }

// Decode takes one shard packet. deliver is called synchronously for the
// packet's own payload (a data shard not seen before) and for every payload
// the packet lets the decoder rebuild; it must not keep the slice.
func (d *Decoder) Decode(pkt []byte, now time.Time, deliver func(payload []byte)) error {
	h, err := parseHeader(pkt)
	if err != nil {
		d.stats.Invalid++
		return err
	}
	body := pkt[HeaderLen:]
	g := d.groups[h.group]
	if g == nil {
		if len(d.groups) >= d.maxGroups {
			d.evictOldest()
		}
		g = &rxGroup{born: now}
		d.groups[h.group] = g
		d.order = append(d.order, h.group)
	}

	if h.k == 0 { // data shard
		if h.idx >= MaxShards || (g.k != 0 && h.idx >= g.k) {
			d.stats.Invalid++
			return errBadShard
		}
		if g.isDelivered(h.idx) {
			d.stats.Dups++
			return nil
		}
		p, ok := shardPayload(body)
		if !ok || len(body) > d.maxShard || (g.size != 0 && len(body) > g.size) {
			d.stats.Invalid++
			return errBadShard
		}
		d.stats.Data++
		g.markDelivered(h.idx)
		deliver(p)
		if g.done {
			return nil
		}
		g.put(h.idx, body)
	} else { // parity shard
		if h.r < 1 || h.k+h.r > MaxShards || h.idx < h.k || h.idx >= h.k+h.r ||
			h.size < lenPrefix || h.size > d.maxShard || len(body) != h.size {
			d.stats.Invalid++
			return errBadShard
		}
		if g.k != 0 && (g.k != h.k || g.r != h.r || g.size != h.size) {
			d.stats.Invalid++
			return errBadShard
		}
		d.stats.Parity++
		if g.done {
			return nil
		}
		if g.k == 0 {
			g.k, g.r, g.size = h.k, h.r, h.size
			// A data shard that arrived before parity and is longer than the
			// group's shard size, or has an index past k, is corrupt.
			for i, s := range g.shards {
				if s != nil && (i >= g.k || len(s) > g.size) {
					g.done = true
					g.shards = nil
					d.stats.Invalid++
					return errBadShard
				}
			}
		}
		if g.shards != nil && h.idx < len(g.shards) && g.shards[h.idx] != nil {
			return nil // duplicate parity
		}
		g.put(h.idx, body)
		g.nParity++
	}
	d.tryRecover(g, deliver)
	return nil
}

// tryRecover rebuilds missing data shards once any k shards of the group are
// present.
func (d *Decoder) tryRecover(g *rxGroup, deliver func([]byte)) {
	if g.done || g.k == 0 {
		return
	}
	if g.nDeliv >= g.k { // every data shard arrived: parity not needed
		g.done = true
		g.shards = nil
		return
	}
	if g.nDeliv+g.nParity < g.k {
		return
	}
	enc, err := d.codecs.get(g.k, g.r)
	if err != nil {
		return
	}
	all := make([][]byte, g.k+g.r)
	for i := 0; i < g.k+g.r && i < len(g.shards); i++ {
		s := g.shards[i]
		if s == nil {
			continue
		}
		if len(s) < g.size { // data shard: zero-extend to the coded size
			z := make([]byte, g.size)
			copy(z, s)
			s = z
		}
		all[i] = s
	}
	if err := enc.ReconstructData(all); err != nil {
		d.stats.Invalid++
		g.done = true
		g.shards = nil
		return
	}
	for i := 0; i < g.k; i++ {
		if g.isDelivered(i) {
			continue
		}
		p, ok := shardPayload(all[i])
		g.markDelivered(i)
		if !ok {
			d.stats.Invalid++
			continue
		}
		d.stats.Recovered++
		deliver(p)
	}
	g.done = true
	g.shards = nil
}

// Expire drops groups older than the ttl and counts the data shards that
// could not be rebuilt.
func (d *Decoder) Expire(now time.Time) {
	keep := d.order[:0]
	for _, id := range d.order {
		g := d.groups[id]
		if g == nil {
			continue
		}
		if now.Sub(g.born) < d.ttl {
			keep = append(keep, id)
			continue
		}
		d.retire(id, g)
	}
	d.order = keep
}

func (d *Decoder) evictOldest() {
	for len(d.order) > 0 {
		id := d.order[0]
		d.order = d.order[1:]
		if g := d.groups[id]; g != nil {
			d.retire(id, g)
			return
		}
	}
}

func (d *Decoder) retire(id uint32, g *rxGroup) {
	if !g.done && g.k != 0 && g.nDeliv < g.k {
		d.stats.Lost += uint64(g.k - g.nDeliv)
	}
	delete(d.groups, id)
}

// shardPayload returns the payload inside shard content [len:2][payload].
func shardPayload(content []byte) ([]byte, bool) {
	if len(content) < lenPrefix {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(content))
	if lenPrefix+n > len(content) {
		return nil, false
	}
	return content[lenPrefix : lenPrefix+n], true
}

package smux

import (
	"container/heap"
	"encoding/binary"
	"sync"
	"sync/atomic"
)

// SendBatch (hs2 lk179, Config.SendBatch): the shaper hands sendLoop every
// frame already queued, up to SendBatch bytes, and sendLoop writes them with
// one conn.Write (one syscall, the TLS records of one flush) instead of one
// write — and two goroutine hand-offs — per frame.
//
// Why: in the sc179 storm profile (2000-4000 opens/s over 6000 light users
// and bulk downloads) a third of the edge's and the exit's CPU was write
// syscalls and scheduler wake-ups; smux's sendLoop wrote every 16 KiB data
// frame, every window update and every SYN/FIN with its own conn.Write, each
// handed over shaper -> sendLoop -> writer through unbuffered channels.
//
// Order: the batch is the heap's order (class, then seq/jump: SmallFirst
// still puts a small frame first). A request that arrives while a batch is
// being formed is pushed into the heap with the batch's frames, so it can
// still overtake them; only a frame that arrives while sendLoop is writing
// waits for that write, as it waited for the one frame before. The batch is
// bounded (SendBatch bytes), so a small frame waits at most that much more
// than with one frame per write. The wire bytes are the same frames in the
// same order a one-at-a-time sendLoop would write.
//
// 0: upstream (one frame per write).

// sendBatchMaxFrames bounds the frames in one batch (window updates are 8
// bytes; a byte cap alone would let hundreds through).
const sendBatchMaxFrames = 64

var sendBatchPool = sync.Pool{New: func() any { b := make([]writeRequest, 0, sendBatchMaxFrames); return &b }}

// sendBatchStat (process-wide): batches written and the frames in them, the
// proof the key acts (frames/batches > 1 when it saves writes).
var sendBatchStat struct{ batches, frames atomic.Uint64 }

// SendBatchStats returns the batches written and the frames in them.
func SendBatchStats() (batches, frames uint64) {
	return sendBatchStat.batches.Load(), sendBatchStat.frames.Load()
}

// shaperLoopBatch is shaperLoop handing sendLoopBatch batches.
func (s *Session) shaperLoopBatch() {
	var reqs shaperHeap
	var batch *[]writeRequest
	limit := s.config.SendBatch
	for {
		if batch == nil && len(reqs) > 0 {
			batch = sendBatchPool.Get().(*[]writeRequest)
			b, size := (*batch)[:0], 0
			for len(reqs) > 0 && len(b) < sendBatchMaxFrames {
				fl := headerSize + len(reqs[0].frame.data) // reqs[0]: the heap's minimum
				if len(b) > 0 && size+fl > limit {
					break
				}
				r := heap.Pop(&reqs).(writeRequest)
				if r.jump > 0 && overtook(reqs, r) {
					smallFirstOvertakes.Add(1)
				}
				b = append(b, r)
				size += fl
			}
			*batch = b
		}
		var chWrite chan *[]writeRequest
		if batch != nil {
			chWrite = s.writesB
		}
		chShaper := s.shaper
		if len(reqs) >= maxShaperSize {
			chShaper = nil
		}
		if chShaper == nil && chWrite == nil {
			panic("both channel are nil")
		}
		select {
		case <-s.die:
			return
		case r := <-chShaper:
			if batch != nil { // reshape: the new request may go first
				for _, q := range *batch {
					heap.Push(&reqs, q)
				}
				clear(*batch)
				*batch = (*batch)[:0]
				sendBatchPool.Put(batch)
				batch = nil
			}
			heap.Push(&reqs, r)
		case chWrite <- batch:
			batch = nil
		}
	}
}

// sendLoopBatch is sendLoop writing a batch with one conn.Write.
func (s *Session) sendLoopBatch() {
	buf := make([]byte, 0, s.config.SendBatch+(1<<16)+headerSize)
	var lens [sendBatchMaxFrames]int
	for {
		select {
		case <-s.die:
			return
		case bp := <-s.writesB:
			b := *bp
			buf = buf[:0]
			for i := range b {
				data := b[i].own.take(b[i].frame.data)
				var h [headerSize]byte
				h[0] = b[i].frame.ver
				h[1] = b[i].frame.cmd
				binary.LittleEndian.PutUint16(h[2:], uint16(len(data)))
				binary.LittleEndian.PutUint32(h[4:], b[i].frame.sid)
				buf = append(buf, h[:]...)
				buf = append(buf, data...)
				b[i].own.st.Store(reqReleased) // the data is ours now
				lens[i] = len(data)
			}
			s.writeBusySince.Store(nowNano())
			n, err := s.conn.Write(buf)
			s.writeBusySince.Store(0)
			sendBatchStat.batches.Add(1)
			sendBatchStat.frames.Add(uint64(len(b)))
			off := 0
			for i := range b {
				// this frame's share of n, as a one-frame write would report it
				wn := min(max(n-off-headerSize, 0), lens[i])
				var e error
				if err != nil && n < off+headerSize+lens[i] {
					e = err
				}
				off += headerSize + lens[i]
				b[i].result <- writeResult{n: wn, err: e}
				close(b[i].result)
			}
			clear(b)
			*bp = b[:0]
			sendBatchPool.Put(bp)
			if err != nil {
				s.notifyWriteError(err)
				return
			}
		}
	}
}

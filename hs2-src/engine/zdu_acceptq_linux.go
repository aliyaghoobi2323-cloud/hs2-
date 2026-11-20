//go:build linux

package engine

// ZDUAcceptQueue (review 4 low, key on, old process in probation) is the
// number of connections waiting in the kernel accept queues of this
// process's live (not dropped, not closed) listeners. In probation the old
// process has paused its accept, so only the new process drains those
// queues: a queue that never empties means the new process is alive but
// hung (cmd/hs2 zduOld.watch reverts). TCP_INFO on a listening socket
// reports the current queue length in tcpi_unacked.
func ZDUAcceptQueue() (n int) {
	for _, z := range zduAll() {
		z.mu.Lock()
		gone := z.closed || z.dropped
		z.mu.Unlock()
		if gone || z.TCPListener == nil {
			continue
		}
		rc, err := z.TCPListener.SyscallConn()
		if err != nil {
			continue
		}
		rc.Control(func(fd uintptr) {
			if ti, err := tcpInfo(int(fd)); err == nil {
				n += int(ti.Unacked)
			}
		})
	}
	return n
}

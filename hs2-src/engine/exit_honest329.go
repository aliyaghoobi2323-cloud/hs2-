package engine

// exit_honest329.go — rebal329 Fix C (exit): the rebalance's log says what
// the pool does. A slot over the edge's target is retired, not redialed
// (exitPool.release/retireIfOver), so "their replacements go to the paths
// under their share" is wrong while the pool is over the target.

// overWant reports whether the pool holds more slots than the edge's target.
func (p *exitPool) overWant() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.want > 0 && len(p.slots) > p.want
}

// setPoolOver installs the pool's over-target check (nil: unknown).
func (b *LB) setPoolOver(f func() bool) {
	b.mu.Lock()
	b.poolOver = f
	b.mu.Unlock()
}

// poolOverWant: the pool is over the edge's target (false: unknown). Not
// under b.mu (it takes the pool's lock).
func (b *LB) poolOverWant() bool {
	b.mu.Lock()
	f := b.poolOver
	b.mu.Unlock()
	return f != nil && f()
}

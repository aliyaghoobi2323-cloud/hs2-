// Package fec is the adaptive, interleaved forward error correction used by
// the UDP carrier. It is transport-agnostic: it turns payloads into shard
// packets and back and knows nothing about sockets, keys or pacing.
//
// # Why FEC, and why this shape
//
// The target path loses 26–31% of UDP packets on average, independent of
// rate, in bursts, with a jitter of ~0.02 ms. Retransmission (what TCP does)
// costs at least one round trip per loss and, at that loss rate, collapses a
// loss-based congestion controller. FEC costs bandwidth instead of time:
// each group of k data shards is followed by r parity shards, and any k of
// the k+r recover the group.
//
// # Groups and shards
//
// A data shard is sent the moment it is produced, as its own packet, at its
// natural length. The receiver delivers it at once; nothing waits for FEC.
// Only when a data shard is lost does the receiver wait — for the group's
// parity, which follows when the group closes. So FEC adds latency only to
// packets that would otherwise have been lost, never to packets that arrive.
//
// Shard content is [len:2][payload], zero-extended to the group's largest
// shard for the Reed-Solomon arithmetic. Every packet starts with a 9-byte
// header:
//
//	[group:4][index:1][k:1][r:1][shardSize:2]
//
// Data packets carry k=r=shardSize=0 (not known yet: a group closes early
// when its timer fires); parity packets carry the final values.
//
// # Interleaving over a time window
//
// Loss on this path is bursty. If a group's packets went out back to back, a
// burst would take several of them together and exceed r. Instead the
// encoder keeps several groups open and deals consecutive payloads to them
// round-robin, so neighbouring packets on the wire belong to different
// groups. The number of open groups follows the packet rate so that each
// group's packets are spread over a fixed time Window rather than a fixed
// packet count: a burst of duration B then costs every group about B/Window
// of its packets, at any rate. A group still open after Window is closed
// early, which bounds how long a lost packet waits for its parity at low
// rates. Window is the latency/robustness knob: a larger window survives
// longer bursts, but a rebuilt packet reaches the receiver up to Window late
// (the inner TCP sees that as reordering).
//
// # Adaptive redundancy
//
// The redundancy follows the loss the receiver measures (see Adapter). For a
// group of k data shards and a loss estimate p, r is the smallest parity
// count whose residual loss under a binomial model is at most TargetResidual,
// clamped to a floor (always at least one parity, to catch the first packet
// of a burst the estimate has not seen yet) and a ceiling (so overhead cannot
// run away when the path is simply broken). Picking r per group from the
// binomial tail means a short, timer-flushed group gets proportionally more
// parity than a full one, which is what the math asks for.
package fec

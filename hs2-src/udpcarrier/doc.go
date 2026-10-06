// Package udpcarrier is the UDP transport for hs2: it bolts the existing core
// (Noise IKpsk2 + ChaCha20-Poly1305 session, replay window, exporter/binding)
// and the existing fec package (adaptive, interleaved forward error correction)
// onto a net.PacketConn. It brings its own encryption, exactly like the noise
// and reality carriers, so the engine adds none.
//
// # Why a UDP carrier exists
//
// The target paths lose 26%+ of packets in bursts. TCP reads that loss as
// congestion and halves its window, so a loss-based transport spends most of
// its time throttled far below the path's real capacity. This carrier does
// not retransmit and does not treat loss as congestion: it pays bandwidth
// (FEC parity) to rebuild lost packets, and it paces sends from a
// bandwidth/RTT model (see rate.go), never from loss. The win is not raw
// speed — at 26% loss the FEC overhead is large — it is that latency and
// jitter stay flat instead of sawtoothing with a congestion controller.
//
// # The send and receive paths
//
//	send:  payload --core.AppendDatagram--> [seq|ciphertext]
//	                --fec.Encoder.Encode--> shard packets --pacer--> PacketConn
//	recv:  PacketConn --fec.Decoder.Decode--> [seq|ciphertext]
//	                --core.OpenDatagram--> payload (replay-checked)
//
// Each sealed frame is prefixed with its explicit 8-byte sequence (datagrams
// arrive out of order, so the receiver cannot infer it) and handed to the FEC
// encoder as one payload. The encoder emits the data shard immediately and the
// group's parity when the group closes; every shard packet is one UDP
// datagram. The decoder delivers each data shard the moment it arrives and
// rebuilds the ones a burst dropped from parity, so FEC adds latency only to
// packets that would otherwise have been lost.
//
// # Authentication
//
// The handshake is core's Noise IKpsk2 run over datagrams (see dial.go /
// listen.go). On top of it the two ends exchange a key-confirmation MAC keyed
// by the session Exporter and bound to the handshake transcript (see auth.go),
// which is what makes a man-in-the-middle relay fail: a MITM that completes
// two separate handshakes holds two unrelated exporters, so the confirmation
// it would have to forge cannot be produced. This mirrors the tls-exporter
// binding the TCP carriers use (TestMITMRejected).
//
// The static keys and psk are derived from the tunnel's shared secret with
// core.StaticFromSeed, so both ends agree on pinned keys from the one secret
// the installer already distributes — no second key exchange.
package udpcarrier

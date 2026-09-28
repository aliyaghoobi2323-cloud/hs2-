# fec — adaptive, interleaved Reed-Solomon

Forward error correction for a lossy datagram path. It is a standalone package:
it turns payloads into shard packets and back, and knows nothing about sockets,
keys or pacing. The design is described in `doc.go`; this file gives the
numbers behind the defaults and how to reproduce them.

## Use

```go
enc := fec.NewEncoder(fec.Config{})                    // defaults below
dec := fec.NewDecoder(time.Second, enc.Config().MaxPayload+2)
ad  := fec.NewAdapter(fec.AdapterConfig{})

// sender, per payload (emit must not keep the slice):
enc.Encode(payload, time.Now(), send)
// sender, from a timer at enc.NextDeadline():
enc.Flush(time.Now(), send)
// sender, per loss report (0..1) from the receiver:
enc.SetLoss(ad.Observe(loss, time.Now()))

// receiver, per packet (deliver must not keep the slice):
dec.Decode(pkt, time.Now(), deliver)
// receiver, periodically:
dec.Expire(time.Now())
```

A received data shard is delivered at once. Only a lost one waits, for its
group's parity, at most `Window`.

## Defaults and why

| setting | value | reason |
|---|---|---|
| `K` (data shards per group) | 32 | Larger groups need far less parity for the same residual loss: at 26% loss a 1% residual needs r=8 for k=8 (100%), r=12 for k=16 (75%), ~r=21 for k=32 (65%). At low rates `Window` closes groups early, so a large K costs nothing there. |
| `Window` | 30 ms | Under 15 ms bursts: 10 ms windows left 5–7% residual, 30–40 ms 3.5–4.5%, 80 ms barely better but rebuilt packets up to 80 ms late. |
| `TargetResidual` | 1% | Parity per group is the smallest r whose binomial residual is ≤ 1% at the loss estimate. |
| `CeilRatio` | 1.5 | Parity never exceeds 1.5·k, so overhead stays bounded when the path is simply broken. |
| Adapter rise / fall | 0.5 / 0.08 per report | With a report every 100 ms: most of a step up in two reports, ~1.2 s decay, no flapping on single noisy reports. |
| Adapter hold | 0.5 × peak for 2 s | With memoryless bursts, holding at 0.75 × peak cost ~30% more overhead for ~1% less residual loss. A moderate hold still helps on paths whose bursts cluster. |
| Adapter floor / max | 3% / 50% | Some parity even on a clean path, for the first burst. |

## Measured

From the unit tests (`go test -v ./fec`) and sweeps
(`HS2_FEC_SWEEP=1 go test -run Sweep -v ./fec`), 1200-byte payloads:

| channel | wire loss | residual after FEC | parity overhead |
|---|---|---|---|
| 26% independent loss | 25.9% | 0.12% | 69% |
| 31% independent loss | 30.9% | 0.37% | 84% |
| bursty (Gilbert-Elliott, ~29%), no interleaving | 29.1% | 3.87% | — |
| same, interleaved over 4 groups | 29.2% | 2.09% | — |
| kernel `netem loss 26% 25%` generator | 17.6% | 0.00% | 69% |

- **Overhead is large at this loss rate.** At 26% loss, about 55–60% of the wire
  rate is useful payload.
- **Bursts longer than a few ms hurt.** When a burst lasts about as long as
  `Window`, FEC alone leaves a residual of a few percent. Whatever carries the
  payloads must tolerate that.
- **`netem loss P% C%` does not produce P% loss.** Its correlation mixes each
  random draw with the previous one, which narrows the distribution: "26% with
  25% correlation" realises 17.6%. To emulate a measured loss rate with bursts,
  use a Gilbert-Elliott model (`loss gemodel`), or raise P and measure.

## Performance

4-vCPU Xeon @ 2.1 GHz, `go test -run xxx -bench . -benchmem ./fec`:

| benchmark | before | after |
|---|---|---|
| encode 1300 B, 28% loss estimate | 2493 ns, 3905 B/op | 1200 ns, 29 B/op |
| encode 1300 B, 3% loss estimate | 1232 ns, 3102 B/op | 190 ns, 28 B/op |
| decode, no loss (per 8192 packets) | 5.1 ms, 12.4 MB | 1.0 ms, 0.6 KB |
| decode, 1 in 5 lost (per 8192 packets) | 12.7 ms, 16.7 MB | 4.8 ms, 1.3 MB |

Shard buffers, parity buffers and group structs come from bounded free lists
and go back when a group closes or expires. A data packet is emitted from the
same buffer the group keeps for parity, so it is copied only once. The
remaining allocations are inside `klauspost/reedsolomon`: a small scratch
slice per group encode, and the matrix inversion per loss pattern.

## Tests

```bash
go test -race ./fec                                   # correctness
go test -run xxx -bench . -benchmem ./fec             # performance
go test -run xxx -fuzz FuzzDecode -fuzztime 30s ./fec # decoder on arbitrary input
HS2_FEC_SWEEP=1 go test -run Sweep -v ./fec           # parameter sweeps
```

# mt135 — per-path MTU: phase 1 (done) and phase 2 (design only)

## Phase 1 (this branch): visibility + LB hint

- Every link is its own TCP socket, so the kernel keeps a path MTU per link
  (ICMP "frag needed", or blackhole probing with `net.ipv4.tcp_mtu_probing=1`).
  Each side reads `pmtu` / `snd_mss` / `advmss` from TCP_INFO for what it sends.
  The figure shown is the EFFECTIVE path MTU = min(pmtu, snd_mss + 52 (IPv4) /
  72 (IPv6)): blackhole probing lowers only the MSS (lab: v6 links went to
  MSS 1024–1226 with the kernel PMTU still 1500).
- The control channel carries each side's figure to the other (control.go):
  the edge sets `ctrlMtuBit` (seq bit 62) and its pmtu in seq bits 40..55; an
  exit that sees the bit puts its pmtu / snd_mss in the upper 32 bits of the
  pong's retransmit field (the kernel counter is 32-bit). Old peers: the edge
  never sets the bit (exit answers plain), an old exit echoes the seq and leaves
  the bits 0 (edge reads "no figure"). Works in reverse and direct (the edge
  drives the control channel in both).
- Status `lb_paths[]`: `pmtu_min` plus `pmtu_down` / `pmtu_up`
  (`pmtu_min`, `pmtu_med`, `mss_med`, `pmtu_usual` = highest median of the last
  10 min, `pmtu_low` = links below it, `links`, `pmtu_drops`, `last_drop_s`).
  Down = what the Kharej side sends, on both servers.
- `hs2 lb show` / LB menu: MTU column (`↓med ↑med (min x) dropped Ns ago`);
  `hs2 lb show -status FILE` renders any status file. `hs2 status` line: `mtu …`.
- `hs2 doctor`: WARN when a direction's pmtu < 1280 or a drop happened in the
  last 10 minutes; one OK line otherwise.
- Drop event: ≥ 2 links and ≥ 25 % of the read links lowered their pmtu since
  the previous reading. Logged (`lb pmtu: path …`). `HS2_LB_PMTU_HINT=1`
  (default off, dialing side, fast auto): a path with a drop in the last 2 min
  whose tick ALSO reads loss ≥ half the cut bar or a silence/no-ack is read at
  1.5 × the cut bar. A drop alone changes nothing (shrinking is the kernel
  working as designed).

## Phase 2 (NOT implemented): inner TUN MTU policy, packet carriers only

Only matters where user packets ride the tunnel as packets — the udp and dgtun
carriers. On l3mtcp (production) the inner TUN carries almost nothing and the
mtcp streams re-segment, so phase 2 does not apply there.

1. Measure: the outer budget per path = min(pmtu over its links) − carrier
   overhead (udp: IP+UDP+hs2 header+AEAD tag+FEC header; dgtun: its frame).
   Packet carriers have no kernel PMTU per flow, so add a DF probe: a padded
   keepalive at sizes {1500, 1420, 1380, 1300, 1280} per path every few minutes
   (DF set, `IP_PMTUDISC_PROBE`), acked by the peer; budget = largest acked.
2. Policy: the TUN MTU is one value for the device, so take the minimum budget
   over the paths that carry traffic (cut paths excluded), floor 1280 for IPv6,
   hysteresis (lower at once, raise only after 10 min stable, max one raise per
   10 min). Change it with `ip link set hs0 mtu` + clamp TCP MSS on the TUN
   (`TCPMSS --clamp-mss-to-pmtu` equivalent in-process: rewrite MSS options of
   SYNs we forward) so existing flows adapt without ICMP.
3. Alternative to lowering the device MTU: per-path packet steering — packets
   bigger than a path's budget are only sent on paths that fit (or fragmented
   by the carrier with its own reassembly), so one small-MTU path does not cap
   all users. Prefer this when ≥ 1 path keeps the full budget.
4. Signals: export `tun_mtu`, per-path `pkt_budget`, `oversize_drops`; doctor
   warns when the TUN MTU > the smallest live budget.
5. Rollout: behind `HS2_TUN_MTU_AUTO` (default off), lab with a 1300 hop on one
   of three paths + blackhole, compare loss/stall of large UDP (QUIC) flows.

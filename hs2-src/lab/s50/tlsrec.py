#!/usr/bin/env python3
# tlsrec.py PCAP... — per capture: the data direction of one TCP link (most payload),
# reassembled by sequence number; TLS record sizes, wire segment sizes, retransmitted
# bytes, and how many records start per segment. (s50, docs §11.8)
import struct, sys, collections
def pkts(path):
    b = open(path, 'rb').read(); mag = b[:4]
    e = '<' if mag in (b'\xd4\xc3\xb2\xa1', b'\x4d\x3c\xb2\xa1') else '>'
    lt = struct.unpack(e + 'I', b[20:24])[0]; o = 24
    while o + 16 <= len(b):
        ts, tu, cl, ol = struct.unpack(e + 'IIII', b[o:o+16]); o += 16
        d = b[o:o+cl]; o += cl
        if lt == 113: ip = d[16:]
        elif lt == 276: ip = d[20:]
        elif lt == 1: ip = d[14:]
        else: ip = d
        if not ip or ip[0] >> 4 != 4: continue
        ihl = (ip[0] & 15) * 4; tot = struct.unpack('>H', ip[2:4])[0]
        if ip[9] != 6: continue
        t = ip[ihl:tot]; sp, dp, seq = struct.unpack('>HHI', t[:8]); doff = (t[12] >> 4) * 4
        yield ts + tu / 1e6, (ip[12:16], sp, ip[16:20], dp), seq, t[doff:]
def ana(path):
    flows = collections.defaultdict(list)
    for ts, k, seq, pl in pkts(path):
        if pl: flows[k].append((ts, seq, pl))
    if not flows: return print(path, 'no data')
    k = max(flows, key=lambda x: sum(len(p) for _, _, p in flows[x])); seg = flows[k]
    base = seg[0][1]; seen = {}; retx = 0; segsz = []
    for ts, seq, pl in seg:
        off = (seq - base) & 0xffffffff
        if off in seen: retx += len(pl); continue
        seen[off] = pl; segsz.append(len(pl))
    offs = sorted(seen); stream = bytearray(); starts = []; pos = 0
    for off in offs:
        if off < pos: continue
        if off > pos: break  # hole: stop at the first gap
        starts.append(off); stream += seen[off]; pos = off + len(seen[off])
    # sync to a record header: 0x17 0x03 0x03
    i = 0
    while i < len(stream) - 5 and not (stream[i] == 0x17 and stream[i+1] == 3 and stream[i+2] == 3): i += 1
    recs = []; rstart = []
    while i + 5 <= len(stream):
        if stream[i] not in (0x17, 0x15, 0x16, 0x14) or stream[i+1] != 3: break
        n = struct.unpack('>H', stream[i+3:i+5])[0]; recs.append(n); rstart.append(i); i += 5 + n
    dur = seg[-1][0] - seg[0][0]
    c = collections.Counter(min(r // 1024, 16) for r in recs)
    big = sum(1 for r in recs if r >= 16000)
    print(f"{path}: {len(seg)} segs {sum(segsz)/1e6:.2f} MB in {dur:.2f}s ({sum(segsz)*8/1e6/max(dur,1e-3):.0f} Mbit/s) retx {retx/1e3:.0f} kB; "
          f"seg mean {sum(segsz)/max(1,len(segsz)):.0f} B max {max(segsz)}; contiguous {len(stream)/1e6:.2f} MB; "
          f"records {len(recs)} mean {sum(recs)/max(1,len(recs)):.0f} B, >=16000 B {100*big/max(1,len(recs)):.0f} %, "
          f"KB-buckets {dict(sorted(c.items()))}")
for p in sys.argv[1:]: ana(p)

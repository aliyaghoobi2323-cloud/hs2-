# Building hs2 from source

Requires Go 1.27+.

```bash
go mod tidy      # fetches deps from the Go proxy
go build -trimpath -ldflags="-s -w" -o hs2-linux-amd64 ./cmd/hs2
```

Run tests:
```bash
go test ./...
```

## Layout
- `core/`       — crypto core (Noise handshake, frame format, replay window)
- `tlscarrier/` — standard-TLS carrier: real cert, in-stream auth, probe resistance
- `reality/`    — experimental Reality-style carrier (not used by default)
- `engine/`     — TUN, carriers, link manager, L3-over-multilink, port forwarding
- `obfs/`       — traffic shaping (length/timing)
- `tun/`        — Linux TUN device
- `cmd/hs2/`    — the binary (run, keygen, version)
- `install/`    — installer script

## Carriers (config "carrier" field)
- `l3mtcp` — L3-GRE over multi-link TLS (default)
- `mtcp`   — multi-link TLS, stream mode (TCP only)
- `tls`    — single-link TLS

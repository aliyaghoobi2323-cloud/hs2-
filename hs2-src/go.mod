module github.com/hosseintaghipoursori-alt/hs2-tunnel

go 1.27

// Plain TCP listeners, not MPTCP (the Go 1.24+ default): see listenReuseRcvBuf
// in engine/listen.go for why.
godebug multipathtcp=0

require (
	github.com/flynn/noise v1.1.0
	github.com/klauspost/reedsolomon v1.14.2
	github.com/refraction-networking/utls v1.8.0
	github.com/xtaci/smux v1.5.24
	golang.org/x/crypto v0.36.0
	golang.org/x/sys v0.31.0
)

require (
	github.com/andybalholm/brotli v1.0.6 // indirect
	github.com/klauspost/compress v1.17.4 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
)

// smux v1.5.24 with the adaptive per-stream receive window
// (Config.MinStreamBuffer, Config.StreamLagTarget; third_party/smux). The wire
// format is unchanged, so either end works with an unpatched peer.
replace github.com/xtaci/smux => ./third_party/smux

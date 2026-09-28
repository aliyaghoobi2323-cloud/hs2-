// Package tlscarrier is Option 2: a DPI-resistant carrier built on STANDARD TLS
// with a REAL certificate (Let's Encrypt for a domain you control), plus
// probe-resistance by forwarding unauthorised connections to a real backend.
//
// Why this instead of a Reality reimplementation: real Reality requires forking
// crypto/tls and byte-perfect ClientHello marshalling pinned to an exact utls
// version. We proved that path is version-fragile — a drift silently burns the
// server. Standard crypto/tls has no such fragility: it works on every Go
// version, and a probe that validates the certificate sees a genuine, valid
// Let's Encrypt cert for a real domain.
//
// How a client is told apart from a probe: NOT by touching the ClientHello
// (that is the fragile part). Instead, AFTER a completely ordinary TLS
// handshake, the client sends an authentication token as the first bytes inside
// the encrypted TLS stream. A real client knows the token; a browser or probe
// does not and instead speaks HTTP(S) to what it thinks is a website. The
// server peeks the first encrypted-app-data: if it is a valid token, it is the
// tunnel; otherwise the bytes are spliced to a real backend (a local web server
// or a reverse-proxied site), so the probe gets a real HTTPS response.
//
// Threat coverage:
//   - content fingerprint: it IS real TLS, so entropy/handshake look normal
//   - TLS-library fingerprint: client uses uTLS (Chrome), so JA3/JA4 = Chrome
//   - active probing: unauthorised -> real backend, real cert, real response
//   - traffic shape: the obfs shaper wraps SendFrame (added when wired to engine)
//
// Honest limits: against a DOMAIN-WHITELIST regime (only specific SNIs allowed)
// this needs the domain to be one that is allowed; against SNI-based blocking of
// your specific domain, you rotate the domain. Neither is Iran's current default.
package tlscarrier

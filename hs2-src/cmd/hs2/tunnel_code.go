package main

// Connection codes (menu M3): everything the OTHER server needs to create the
// matching side of a tunnel, in one copy-pasteable line.
//
//	hs2://v2.<base64url(JSON)>   this format (tunnelCode, version 2)
//	hs2://<base64 of a|b|c…>      the classic installer link (version 1) — still
//	                              accepted by `hs2 tunnel join`, never produced
//
// The JSON names its version ("v") and lists which fields are secret
// ("secrets"), so a display can mask them and a future reader knows what to
// protect. A code with a version this binary does not know is refused with an
// explanation (upgrade this side), never half-read.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const codeVersion = 2
const codePrefix = "hs2://"

// tunnelCode is written from the point of view of the tunnel, not of one
// server: Role/Direction say what the CREATOR is, the joiner takes the other
// role with the same direction.
type tunnelCode struct {
	V         int       `json:"v"`
	Name      string    `json:"name"`             // systemd unit, same on both sides
	Role      string    `json:"role"`             // creator's role: iran|kharej
	Direction string    `json:"direction"`        // direct|reverse
	Carrier   string    `json:"carrier"`          // mtcp|l3mtcp|tls|dgtun
	Encap     string    `json:"encap,omitempty"`  // dgtun: udp|icmp
	Front     string    `json:"front,omitempty"`  // tls (default)|reality
	Endpoint  string    `json:"endpoint"`         // listener host:port the dialer connects to
	Dialer    string    `json:"dialer,omitempty"` // the dialer's public IP (informational / firewall)
	SNI       string    `json:"sni,omitempty"`
	SharedKey string    `json:"shared_key"`
	MTU       int       `json:"mtu"`
	Subnet    string    `json:"subnet"` // /30 base, e.g. 10.77.12.0
	UDP       bool      `json:"udp,omitempty"`
	UserPorts string    `json:"user_ports,omitempty"` // iran's user ports (info for kharej's targets)
	Panel     string    `json:"panel,omitempty"`      // kharej's default target (info for iran)
	Targets   string    `json:"targets,omitempty"`    // clone: kharej's port_map
	Clone     bool      `json:"clone,omitempty"`      // joiner takes the SAME role (recreate this side elsewhere)
	LB        *lbConfig `json:"lb,omitempty"`
	Secrets   []string  `json:"secrets"`
	HS2       string    `json:"hs2,omitempty"` // creator's binary version (diagnostics)
}

var errCodeNewer = errors.New("code from a newer hs2")

func encodeCode(c tunnelCode) (string, error) {
	c.V = codeVersion
	c.Secrets = []string{"shared_key"}
	if err := c.check(); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return codePrefix + "v" + strconv.Itoa(codeVersion) + "." + base64.RawURLEncoding.EncodeToString(b), nil
}

func (c tunnelCode) check() error {
	if c.Role != "iran" && c.Role != "kharej" {
		return fmt.Errorf("code: role must be iran|kharej, got %q", c.Role)
	}
	if c.Direction != "direct" && c.Direction != "reverse" {
		return fmt.Errorf("code: direction must be direct|reverse, got %q", c.Direction)
	}
	if !validCarrier(c.Carrier) {
		return fmt.Errorf("code: unsupported carrier %q", c.Carrier)
	}
	if _, p, err := net.SplitHostPort(c.Endpoint); err != nil || p == "0" {
		return fmt.Errorf("code: endpoint %q is not host:port", c.Endpoint)
	}
	if len(c.SharedKey) < 32 {
		return errors.New("code: shared key missing or too short")
	}
	if ip := net.ParseIP(c.Subnet); ip == nil || ip.To4() == nil {
		return fmt.Errorf("code: subnet %q is not an IPv4 /30 base", c.Subnet)
	}
	if c.Name == "" || unitNameErr(c.Name) != nil {
		return fmt.Errorf("code: bad tunnel name %q", c.Name)
	}
	return nil
}

func validCarrier(c string) bool {
	switch c {
	case "mtcp", "l3mtcp", "tls", "dgtun":
		return true
	}
	return false
}

// decodeCode reads a v2 code, or a classic v1 link (converted). The error for
// a newer version wraps errCodeNewer so the caller can explain "upgrade".
func decodeCode(s string) (tunnelCode, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, codePrefix)
	if strings.HasPrefix(s, "v") {
		if dot := strings.IndexByte(s, '.'); dot > 1 {
			if n, err := strconv.Atoi(s[1:dot]); err == nil {
				if n > codeVersion {
					return tunnelCode{}, fmt.Errorf("%w: version %d, this binary reads up to %d — upgrade hs2 on this server (menu 8 → upgrade) and paste it again", errCodeNewer, n, codeVersion)
				}
				b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s[dot+1:], "="))
				if err != nil {
					return tunnelCode{}, fmt.Errorf("code is damaged (base64): %v — copy it again in one piece", err)
				}
				var c tunnelCode
				if err := json.Unmarshal(b, &c); err != nil {
					return tunnelCode{}, fmt.Errorf("code is damaged (json): %v", err)
				}
				if c.V != n {
					return tunnelCode{}, fmt.Errorf("code says v%d outside but v%d inside — damaged", n, c.V)
				}
				return c, c.check()
			}
		}
	}
	return decodeLegacyLink(s)
}

// decodeLegacyLink reads the installer's classic link (base64 of 13 '|'
// fields: endpoint domain shared panel carrier udp transport direction mtu
// encap proto name subnet). The listener made it, so the creator role follows
// from the direction.
func decodeLegacyLink(s string) (tunnelCode, error) {
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil || !strings.Contains(string(raw), "|") {
		return tunnelCode{}, errors.New("not an hs2 code or link (expected hs2://v2.… or a classic hs2:// link)")
	}
	f := strings.Split(strings.TrimSpace(string(raw)), "|")
	for len(f) < 13 {
		f = append(f, "")
	}
	c := tunnelCode{V: 1, Endpoint: f[0], SharedKey: f[2], Panel: f[3], Carrier: f[4], Direction: f[7], Encap: f[9], Name: f[11], Subnet: f[12], Secrets: []string{"shared_key"}}
	if f[1] != "-" {
		c.SNI = f[1]
	}
	if c.Panel == "-" {
		c.Panel = ""
	}
	c.UDP = f[5] == "true"
	c.MTU, _ = strconv.Atoi(f[8])
	if c.Carrier == "" {
		c.Carrier = "mtcp"
	}
	if c.Direction == "" {
		c.Direction = "direct"
	}
	if c.Name == "" {
		c.Name = "hs2"
	}
	if c.Subnet == "" {
		c.Subnet = "10.77.0.0"
	}
	if c.MTU == 0 {
		c.MTU = 1380
		if c.Carrier == "dgtun" {
			c.MTU = 1280
		}
	}
	c.Role = "kharej" // direct: kharej listens and made the link
	if c.Direction == "reverse" {
		c.Role = "iran"
	}
	if !validCarrier(c.Carrier) {
		return c, fmt.Errorf("classic link uses carrier %q, which the new wizard does not create — use the classic menu (bash install.sh) for it", c.Carrier)
	}
	return c, c.check()
}

// shortCode is the human-checkable fingerprint of a code (QR-free short
// form): name, roles, endpoint and an 8-char digest — read it over the phone
// to confirm both servers hold the same code; it is not enough to join.
func shortCode(c tunnelCode) string {
	h := fnv32(c.SharedKey + c.Endpoint + c.Name)
	return fmt.Sprintf("%s %s→%s %s %s %08x", c.Name, c.Role, otherRole(c.Role), c.Direction, c.Endpoint, h)
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func otherRole(r string) string {
	if r == "iran" {
		return "kharej"
	}
	return "iran"
}

// masked returns the code's JSON with every secret replaced, for display.
func (c tunnelCode) masked() string {
	b, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range c.Secrets {
		if v, ok := m[k].(string); ok && v != "" {
			m[k] = maskSecret(v)
		}
	}
	out, _ := json.MarshalIndent(m, "", "  ")
	return string(out)
}

func maskSecret(v string) string {
	if len(v) <= 8 {
		return "****"
	}
	return v[:4] + "…" + v[len(v)-4:] + " (secret)"
}

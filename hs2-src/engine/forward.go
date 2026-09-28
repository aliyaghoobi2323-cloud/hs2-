package engine

import (
	"io"
	"net"
	"time"
)

// Port forwarding turns hs2 into a drop-in Backhaul replacement: the Iran side
// opens the user-facing ports and carries each connection to the panel on the
// kharej side, over the tunnel interface. The user's TCP terminates on the
// panel end to end in effect (one splice per side), and — crucially — the
// listeners live independently of the carrier, so a carrier reconnect does not
// close user ports.
//
// Path: user -> iran:PORT -> [dial peerPanelViaTunnel] -> kharej hs0 -> panel.
// On kharej, RunPanelForwarder accepts on the tunnel side and splices to the
// real panel address. On iran, RunPortForwarders opens each port and splices to
// the kharej end of the tunnel.

// RunPortForwarders (Iran side) opens each local port and forwards new
// connections across the tunnel to the kharej forwarder, which relays to the
// panel. peerForwardAddr is the kharej tunnel IP + a fixed forward port.
func RunPortForwarders(ports []string, peerForwardAddr string, logf func(string, ...any)) error {
	return RunPortForwardersOn("", ports, peerForwardAddr, logf)
}

// RunPortForwardersOn binds the user listeners to a specific local IP (empty =
// all interfaces), so on a multi-IP Iran server users reach a chosen IP.
func RunPortForwardersOn(listenIP string, ports []string, peerForwardAddr string, logf func(string, ...any)) error {
	for _, p := range ports {
		bind := ":" + p
		if listenIP != "" {
			bind = listenIP + ":" + p
		}
		ln, err := ListenReuse(bind)
		if err != nil {
			return err
		}
		go func(ln net.Listener, port string) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go spliceToPeer(c, peerForwardAddr)
			}
		}(ln, p)
		if logf != nil {
			logf("forwarding user port %s -> panel via tunnel", p)
		}
	}
	return nil
}

func spliceToPeer(user net.Conn, peerForwardAddr string) {
	defer user.Close()
	// Dial the kharej forwarder over the tunnel. Retry briefly if the tunnel is
	// mid-reconnect, so a user connection during a blip is not dropped instantly.
	var up net.Conn
	var err error
	for i := 0; i < 20; i++ {
		up, err = net.DialTimeout("tcp", peerForwardAddr, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		return
	}
	defer up.Close()
	splice(user, up)
}

// RunPanelForwarder (Kharej side) listens on the tunnel IP:forwardPort and
// relays every accepted connection to the real panel address.
func RunPanelForwarder(tunnelBind, panelAddr string, logf func(string, ...any)) error {
	// The tunnel IP is not assigned until the TUN comes up, which happens in
	// parallel with startup. Retry the bind until the address exists, so we can
	// bind to the tunnel IP (not 0.0.0.0) and never expose this port to the
	// internet.
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ {
		ln, err = ListenReuse(tunnelBind)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				up, err := net.DialTimeout("tcp", panelAddr, 5*time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				splice(c, up)
			}(c)
		}
	}()
	if logf != nil {
		logf("panel forwarder: %s -> %s", tunnelBind, panelAddr)
	}
	return nil
}

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// copyIO copies from src to dst until EOF, working with any Reader/Writer
// (including smux streams). Used by the mtcp relay.
func copyIO(dst interface{ Write([]byte) (int, error) }, src interface {
	Read([]byte) (int, error)
}) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

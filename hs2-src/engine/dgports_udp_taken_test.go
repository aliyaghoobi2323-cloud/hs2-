package engine

import (
	"context"
	"net"
	"testing"
	"time"
)

// The datagram tun's edge (dgports.go) had the same fault as the stream
// edge (TestUDPPortTakenKeepsTheTunnel): a user port whose UDP another
// program holds made StartDgPorts return the bind error, and hs2 run exits
// on it. Its TCP must keep serving, and its UDP come once the port is free.
func TestDgPortsUDPPortTakenKeepsTCP(t *testing.T) {
	const edgeIP, exitIP = "127.0.0.61", "127.0.0.62"
	panel := namedPanel(t, "panel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &logSink{}
	if _, err := StartDgPorts(ctx, DgPortsConfig{LocalTunIP: exitIP, Default: panel, UDP: true, Log: logs.logf}); err != nil {
		t.Fatal(err)
	}
	p1 := freePortOn(t, edgeIP)
	addr := net.JoinHostPort(edgeIP, p1)
	held, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Skip(err)
	}
	defer held.Close()
	if _, err := StartDgPorts(ctx, DgPortsConfig{Edge: true, Ports: []string{p1}, UserListenIP: edgeIP, PeerTunIP: exitIP, UDP: true, Log: logs.logf}); err != nil {
		t.Fatalf("the edge did not start, and hs2 run exits on that: %v", err)
	}
	if got := tcpReaches(addr); got != "panel" {
		t.Fatalf("tcp reached %q with the port's UDP taken", got)
	}
	if logs.count("UDP is NOT forwarded", 0) == 0 {
		t.Errorf("no log line says why UDP is not forwarded on the port: %q", logs.snapshot())
	}
	held.Close()
	deadline := time.Now().Add(15 * time.Second)
	for udpReaches(addr) != "panel" {
		if time.Now().After(deadline) {
			t.Fatalf("UDP never forwarded after the port was freed: %q", logs.snapshot())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if logs.count("UDP forwarded now", 0) != 1 {
		t.Errorf("want one line saying UDP is forwarded now: %q", logs.snapshot())
	}
	if got := tcpReaches(addr); got != "panel" {
		t.Fatalf("tcp reached %q after UDP came", got)
	}
}

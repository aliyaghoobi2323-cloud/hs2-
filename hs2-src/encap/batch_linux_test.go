//go:build linux

package encap

import (
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"
)

// A burst sent with WriteBatch (one sendmmsg) arrives whole and in order
// through the batched receive paths on both sides (recvmmsg): dial → listen
// with WriteBatch / ReadBatch, and listen → dial with WriteBatchTo.
func TestRawBatchRoundTrip(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("batch")}
			srv := listenT(t, k, "127.0.0.1", opt)
			c, err := Dial(k, "127.0.0.1", opt)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			bw, ok := c.(interface{ WriteBatch([][]byte) error })
			if !ok {
				t.Fatal("dial conn has no WriteBatch")
			}
			const n = 100
			var ps [][]byte
			for i := 0; i < n; i++ {
				ps = append(ps, []byte(fmt.Sprintf("up-%03d", i)))
			}
			if err := bw.WriteBatch(ps); err != nil {
				t.Fatal(err)
			}
			br := srv.(interface {
				ReadBatch(func([]byte, net.Addr)) error
				WriteBatchTo([][]byte, net.Addr) error
			})
			var got []string
			var peer net.Addr
			srv.SetReadDeadline(time.Now().Add(3 * time.Second))
			for len(got) < n {
				if err := br.ReadBatch(func(b []byte, a net.Addr) { got = append(got, string(b)); peer = a }); err != nil {
					t.Fatalf("after %d: %v", len(got), err)
				}
			}
			for i, s := range got {
				if s != fmt.Sprintf("up-%03d", i) {
					t.Fatalf("datagram %d is %q", i, s)
				}
			}
			var rs [][]byte
			for i := 0; i < n; i++ {
				rs = append(rs, []byte(fmt.Sprintf("down-%03d", i)))
			}
			if err := br.WriteBatchTo(rs, peer); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				if b := readT(t, c, 3*time.Second); string(b) != fmt.Sprintf("down-%03d", i) {
					t.Fatalf("reply %d is %q", i, b)
				}
			}
		})
	}
}

// A datagram the kernel refuses with a soft error (EMSGSIZE: larger than the
// path MTU with DF set, as icmp requests are) is dropped alone; the rest of
// the batch still goes.
func TestRawBatchSoftErrorDropsOnlyThatDatagram(t *testing.T) {
	needRawNetns(t)
	if out, err := exec.Command("ip", "link", "set", "lo", "mtu", "1500").CombinedOutput(); err != nil {
		t.Skipf("cannot set lo's MTU: %v %s", err, out)
	}
	defer exec.Command("ip", "link", "set", "lo", "mtu", "65536").Run()
	opt := Options{Key: []byte("soft")}
	srv := listenT(t, KindICMP, "127.0.0.1", opt)
	c, err := Dial(KindICMP, "127.0.0.1", opt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := make([]byte, 3000)
	err = c.(interface{ WriteBatch([][]byte) error }).WriteBatch([][]byte{[]byte("first"), big, []byte("third")})
	if err != nil {
		t.Fatalf("a soft error failed the batch: %v", err)
	}
	for _, want := range []string{"first", "third"} {
		got, _ := readFromT(t, srv, 2*time.Second)
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

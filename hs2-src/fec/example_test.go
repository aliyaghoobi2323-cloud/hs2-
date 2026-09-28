package fec_test

import (
	"fmt"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/fec"
)

// A sender and receiver around a lossy channel. The receiver measures loss
// (here: counted directly) and reports it; the sender feeds the report through
// an Adapter into the encoder, which sizes the parity of later groups for it.
func Example() {
	enc := fec.NewEncoder(fec.Config{K: 8, Depth: 1})
	dec := fec.NewDecoder(time.Second, enc.Config().MaxPayload+2)
	adapter := fec.NewAdapter(fec.AdapterConfig{})
	now := time.Unix(0, 0)

	// The last report said 25% of packets were lost.
	enc.SetLoss(adapter.Observe(0.25, now))

	var got []string
	n := 0
	send := func(pkt []byte) {
		n++
		if n == 2 || n == 5 { // the channel drops two packets
			return
		}
		dec.Decode(pkt, now, func(payload []byte) {
			got = append(got, string(payload))
		})
	}
	for i := 0; i < 8; i++ {
		enc.Encode([]byte(fmt.Sprintf("packet %d", i)), now, send)
	}
	fmt.Println(len(got), "of 8 delivered, rebuilt:", dec.Stats().Recovered)
	// Output: 8 of 8 delivered, rebuilt: 2
}

package types

import (
	"bytes"
	"testing"
)

// FuzzDecoders checks that decoding never panics and that whatever decodes
// re-encodes to the same bytes (one encoding per value, rules §2.1).
func FuzzDecoders(f *testing.F) {
	f.Add((&Transaction{Kind: KindCall, Budget: ResourceVector{1, 2, 300, 4}, Input: []byte("x")}).Encode())
	f.Add((&Transaction{Kind: KindDeploy, Budget: ResourceVector{1, 2, 300, 4}, Code: []byte("c")}).Encode())
	f.Add((&Receipt{Status: 1, Result: []byte("r"), Events: []Event{{Payload: []byte("e")}}}).Encode())
	f.Add((&Block{Header: &Header{}, Body: &Body{}}).Encode())
	f.Fuzz(func(t *testing.T, b []byte) {
		if tx, err := DecodeTransaction(b); err == nil && !bytes.Equal(tx.Encode(), b) {
			t.Fatal("transaction re-encoding differs")
		}
		if r, err := DecodeReceipt(b); err == nil && !bytes.Equal(r.Encode(), b) {
			t.Fatal("receipt re-encoding differs")
		}
		if blk, err := DecodeBlock(b); err == nil && !bytes.Equal(blk.Encode(), b) {
			t.Fatal("block re-encoding differs")
		}
		if len(b) == GenesisConfigSize {
			if g, err := DecodeGenesisConfig(b); err == nil && !bytes.Equal(g.Encode(), b) {
				t.Fatal("genesis re-encoding differs")
			}
		}
	})
}

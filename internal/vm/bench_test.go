package vm

import (
	"encoding/binary"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
)

// BenchmarkSpin measures the interpreter on a tight arithmetic loop.
func BenchmarkSpin(b *testing.B) {
	h := newHarness(&testing.T{})
	id, r := h.deploy(samples.Spin(), make([]byte, 4))
	if r.Status != 0 {
		b.Fatalf("deploy %d", r.Status)
	}
	in := binary.LittleEndian.AppendUint32(nil, 1_000_000)
	tx := &types.Transaction{Kind: types.KindCall, Budget: bigBudget, Target: id, Input: in}
	raw := tx.Encode()
	dec, _ := types.DecodeTransaction(raw)
	b.ResetTimer()
	var used uint64
	for i := 0; i < b.N; i++ {
		r, err := ExecuteTransaction(h.env, h.guard, raw, dec)
		if err != nil || r.Status != 0 {
			b.Fatal(err, r.Status)
		}
		used = r.ResourcesUsed[types.C]
		h.env.State.DiscardJournal()
	}
	b.ReportMetric(float64(used), "C/op")
	b.ReportMetric(float64(used)*float64(b.N)/b.Elapsed().Seconds()/1e6, "MC/s")
}

// BenchmarkStorageWrites measures many small storage transactions.
func BenchmarkStorageWrites(b *testing.B) {
	h := newHarness(&testing.T{})
	id, _ := h.deploy(samples.KeyValue(), nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := make([]byte, 40)
		binary.LittleEndian.PutUint64(in, uint64(i))
		tx := &types.Transaction{Kind: types.KindCall, Budget: bigBudget, Target: id, Input: in}
		raw := tx.Encode()
		dec, _ := types.DecodeTransaction(raw)
		r, err := ExecuteTransaction(h.env, h.guard, raw, dec)
		if err != nil || r.Status != 0 {
			b.Fatal(err, r.Status)
		}
		h.env.State.DiscardJournal()
	}
	_ = hashing.Hash{}
}

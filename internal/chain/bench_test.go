package chain_test

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/types"
)

// BenchmarkReplayStorageBlocks validates blocks of 200 storage-writing
// transactions each on a fresh node.
func BenchmarkReplayStorageBlocks(b *testing.B) {
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	g := c.Genesis()
	ts := g.GenesisTimestamp
	next := func(txs [][]byte) *miner.Template {
		ts += 60
		e, tpl, err := miner.MineOne(context.Background(), c, miner.Options{Timestamp: ts}, txs)
		if err != nil || len(tpl.Skipped) > 0 {
			b.Fatalf("mine: %v %v", err, tpl.Skipped)
		}
		_ = e
		return tpl
	}
	small := types.ResourceVector{400_000, 4, 400, 200}
	tpl := next([][]byte{(&types.Transaction{Kind: types.KindDeploy,
		Budget: types.ResourceVector{10_000_000, 16, 20_000, 20_000}, Code: samples.KeyValue()}).Encode()})
	var kv hashing.Hash
	copy(kv[:], tpl.Receipts[0].Result)
	const blocks, perBlock = 10, 200
	for i := 0; i < blocks; i++ {
		txs := make([][]byte, perBlock)
		for j := range txs {
			in := make([]byte, 40)
			binary.LittleEndian.PutUint64(in, uint64(i*perBlock+j))
			txs[j] = (&types.Transaction{Kind: types.KindCall, Budget: small, Target: kv, Input: in}).Encode()
		}
		next(txs)
	}
	var raws [][]byte
	for h := uint64(1); h <= c.Tip().Height; h++ {
		e, _ := c.AtHeight(h)
		raw, _ := c.Block(e.ID)
		raws = append(raws, raw)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fresh, _ := chain.New(g, chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
		for _, raw := range raws {
			if _, err := fresh.ProcessBlock(raw); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(blocks*perBlock*b.N)/b.Elapsed().Seconds(), "tx/s")
}

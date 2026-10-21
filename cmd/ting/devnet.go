package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/types"
)

// cmdDevnet mines a sample chain exercising deployments, calls, nested calls,
// read-only calls, factories, reverts, traps and exhaustion, and writes it as
// a bundle plus a list. It checks this implementation against itself only.
func cmdDevnet(args []string) error {
	fs := flag.NewFlagSet("devnet", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	nblocks := fs.Int("blocks", 20, "number of blocks after the deployments")
	out := fs.String("out", "devnet.blocks", "bundle output file")
	listOut := fs.String("list", "devnet.list", "list output file")
	fs.Parse(args)
	g, err := gf.load()
	if err != nil {
		return err
	}
	c, err := chain.New(g, chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	if err != nil {
		return err
	}
	budget := types.ResourceVector{90_000_000, 512, 1 << 20, 1 << 18}
	ts := g.GenesisTimestamp
	var blocks [][]byte
	mineBlock := func(txs ...[]byte) (*miner.Template, error) {
		ts += uint64(g.TargetInterval)
		e, tpl, err := miner.MineOne(context.Background(), c,
			miner.Options{Timestamp: ts, Producer: []byte(fmt.Sprintf("devnet %d", len(blocks)+1))}, txs)
		if err != nil {
			return nil, err
		}
		if len(tpl.Skipped) > 0 {
			return nil, fmt.Errorf("block %d skipped transactions: %v", e.Height, tpl.Skipped)
		}
		raw, err := c.Block(e.ID)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, raw)
		return tpl, nil
	}
	deploy := func(code, input []byte) []byte {
		return (&types.Transaction{Kind: types.KindDeploy, Budget: budget, Code: code, Input: input}).Encode()
	}
	call := func(target hashing.Hash, input []byte) []byte {
		return (&types.Transaction{Kind: types.KindCall, Budget: budget, Target: target, Input: input}).Encode()
	}
	codes := [][]byte{samples.Counter(), samples.Echo(), samples.KeyValue(), samples.Proxy(false),
		samples.Proxy(true), samples.Factory(samples.Counter()), samples.Failer(), samples.Spin()}
	var txs [][]byte
	for _, code := range codes {
		txs = append(txs, deploy(code, nil))
	}
	tpl, err := mineBlock(txs...)
	if err != nil {
		return err
	}
	ids := make([]hashing.Hash, len(codes))
	for i, r := range tpl.Receipts {
		if r.Status != types.StatusSuccess {
			return fmt.Errorf("deployment %d failed with status %d", i, r.Status)
		}
		copy(ids[i][:], r.Result)
	}
	counter, echo, kv, proxy, roProxy, factory, failer, spin := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7]
	key := make([]byte, 32)
	for i := 0; i < *nblocks; i++ {
		b := byte(i)
		txs := [][]byte{
			call(counter, nil),
			call(echo, []byte{b, b, b}),
			call(kv, append(append([]byte{}, key...), b, 1, 2)),
			call(kv, key),
			call(proxy, append(append([]byte{}, counter[:]...), b)),
			call(roProxy, append(append([]byte{}, counter[:]...), b)),
			call(failer, []byte{byte(i % 5)}),
			call(spin, []byte{byte(i * 7), 1, 0, 0}),
		}
		if i%4 == 0 {
			txs = append(txs, call(factory, []byte{b}), deploy(samples.Echo(), []byte{b}))
		}
		if i%6 == 5 {
			txs = append(txs, call(kv, []byte{1}), call(hashing.Hash{b}, nil),
				deploy([]byte("not a module"), nil))
		}
		if _, err := mineBlock(txs...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(*out, seed.EncodeBundle(blocks), 0o644); err != nil {
		return err
	}
	list, _ := seed.ListFromBundle(blocks)
	f, err := os.Create(*listOut)
	if err != nil {
		return err
	}
	for _, id := range list {
		fmt.Fprintln(f, id)
	}
	if err := f.Close(); err != nil {
		return err
	}
	tip := c.Tip()
	st, ok := tip.State()
	if !ok {
		return errors.New("tip state missing")
	}
	fmt.Printf("mined %d blocks; tip %s; state root %s (%d records)\nwrote %s and %s\n",
		len(blocks), tip.ID, st.Root(), st.Len(), *out, *listOut)
	return nil
}

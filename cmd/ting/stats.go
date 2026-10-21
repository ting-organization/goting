package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/ting-organization/goting/internal/seed"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/wasm"
)

// cmdStats summarizes what a bundle's blocks exercise: receipt statuses,
// transaction kinds, and the opcodes and host imports of deployed modules.
func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	bundle := fs.String("bundle", "", "bundle file")
	fs.Parse(args)
	if *bundle == "" {
		return errors.New("-bundle is required")
	}
	b, err := os.ReadFile(*bundle)
	if err != nil {
		return err
	}
	blocks, err := seed.ParseBundle(b)
	if err != nil {
		return err
	}
	statuses := map[uint8]int{}
	kinds := map[string]int{}
	ops := map[wasm.Op]int{}
	hosts := map[string]int{}
	var modules, invalidModules, events, maxTx int
	var maxUsed types.ResourceVector
	features := map[string]int{}
	seenCode := map[string]bool{}
	for _, raw := range blocks {
		blk, err := types.DecodeBlock(raw)
		if err != nil {
			return err
		}
		for i, rb := range blk.Body.Receipts {
			r, err := types.DecodeReceipt(rb)
			if err != nil {
				return err
			}
			statuses[r.Status]++
			events += len(r.Events)
			for k := 0; k < 4; k++ {
				if r.ResourcesUsed[k] > maxUsed[k] {
					maxUsed[k] = r.ResourcesUsed[k]
				}
			}
			tx, err := types.DecodeTransaction(blk.Body.Transactions[i])
			if err != nil {
				return err
			}
			if len(blk.Body.Transactions[i]) > maxTx {
				maxTx = len(blk.Body.Transactions[i])
			}
			if tx.Kind == types.KindCall {
				kinds["call"]++
				continue
			}
			kinds["deploy"]++
			if seenCode[string(tx.Code)] {
				continue
			}
			seenCode[string(tx.Code)] = true
			m, err := wasm.Decode(tx.Code)
			if err != nil {
				invalidModules++
				continue
			}
			modules++
			for _, h := range m.Imports {
				hosts[h.String()]++
			}
			if m.HasTable {
				features["table"]++
				empty := true
				for _, e := range m.Table {
					if e.Func >= 0 {
						empty = false
					}
				}
				if empty {
					features["table with no element entries"]++
				}
				if m.TableSize == 0 {
					features["table of size 0"]++
				}
			}
			if m.HasMemory && m.MemMin == 0 {
				features["memory of 0 initial pages"]++
			}
			if len(m.Data) > 0 && m.DataSpans < uint64(len(m.Data)) {
				features["empty data segments"]++
			}
			if len(m.Globals) > 0 {
				features["globals"]++
			}
			if len(m.Data) > 0 {
				features["data segments"]++
			}
			if m.MemMin > 32 {
				features["memory above 2 MiB"]++
			}
			if m.MemMax > m.MemMin {
				features["growable memory"]++
			}
			for _, f := range m.Funcs {
				for _, in := range f.Code {
					ops[in.Op]++
				}
			}
		}
	}
	fmt.Printf("blocks %d\ntransactions: %v\nreceipt statuses:\n", len(blocks), kinds)
	var sts []int
	for s := range statuses {
		sts = append(sts, int(s))
	}
	sort.Ints(sts)
	for _, s := range sts {
		fmt.Printf("  %2d %-24s %d\n", s, types.StatusName(uint8(s)), statuses[uint8(s)])
	}
	fmt.Printf("events retained %d\nlargest transaction %d bytes\nlargest per-transaction use %v\n", events, maxTx, maxUsed)
	fmt.Printf("distinct deployed modules: %d valid, %d invalid\nmodule features: %v\nhost imports (modules importing):\n", modules, invalidModules, features)
	var hs []string
	for h := range hosts {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	for _, h := range hs {
		fmt.Printf("  %-15s %d\n", h, hosts[h])
	}
	var opList []int
	for o := range ops {
		opList = append(opList, int(o))
	}
	sort.Ints(opList)
	fmt.Printf("compiled ops used: %d distinct\n ", len(opList))
	for _, o := range opList {
		fmt.Printf(" %#x", o)
	}
	fmt.Println()
	return nil
}

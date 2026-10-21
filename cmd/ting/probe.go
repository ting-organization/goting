package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/p2p"
)

// cmdProbe sends read-only requests whose answers check this client's
// handling of a peer's answers and error answers.
func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	addr := fs.String("peer", "2.29.97.212:18442", "peer address")
	fs.Parse(args)
	g, err := gf.load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := p2p.Dial(ctx, *addr, newHello(g, false))
	if err != nil {
		return err
	}
	defer c.Close()
	show := func(name string, m p2p.Message, err error) {
		if err != nil {
			fmt.Printf("%-28s error: %v\n", name, err)
			return
		}
		fmt.Printf("%-28s %T %+v\n", name, m, summarize(m))
	}
	unknown := hashing.Hash{0xde, 0xad}
	m, err := c.Do(ctx, &p2p.HeadersRequest{Locator: []hashing.Hash{unknown}})
	show("headers, unknown locator", m, err)
	m, err = c.Do(ctx, &p2p.RawRequest{ID: unknown, Length: 10})
	show("raw, unknown block", m, err)
	m, err = c.Do(ctx, &p2p.TransactionRawRequest{ID: unknown, Length: 10})
	show("transaction_raw, unknown", m, err)
	m, err = c.Do(ctx, &p2p.InventoryRequest{})
	show("inventory", m, err)
	m, err = c.Do(ctx, &p2p.InventoryRequest{Deferred: true})
	show("inventory, deferred", m, err)
	m, err = c.Do(ctx, &p2p.PendingRequest{IDs: []hashing.Hash{unknown}})
	show("pending, unknown", m, err)
	m, err = c.Do(ctx, &p2p.AddressesRequest{})
	show("addresses", m, err)
	gen := c.Peer.Locator[len(c.Peer.Locator)-1]
	m, err = c.Do(ctx, &p2p.RawRequest{ID: c.Peer.Tip, Offset: 1 << 40, Length: 10})
	show("raw, offset past the end", m, err)
	first := hashing.MustParseHash("000000004eba8c8bd3488debecab68017d3720b9f5a0da24ab80ffce4b57d1ea")
	m, err = c.Do(ctx, &p2p.RawRequest{ID: first, Offset: 316, Length: 10})
	show("raw, offset == total", m, err)
	m, err = c.Do(ctx, &p2p.RawRequest{ID: first, Offset: 310, Length: 10})
	show("raw, past the end", m, err)
	m, err = c.Do(ctx, &p2p.HeadersRequest{Locator: []hashing.Hash{c.Peer.Tip, gen}})
	show("headers after the tip", m, err)
	// Pipelined: several requests before the first answer.
	var ws []<-chan p2p.Result
	for i := 0; i < 10; i++ {
		w, err := c.Send(&p2p.StateRequest{})
		if err != nil {
			return err
		}
		ws = append(ws, w)
	}
	for _, w := range ws {
		if _, err := c.Wait(ctx, w); err != nil {
			return err
		}
	}
	fmt.Println("pipelined 10 state requests: all answered in order")
	return nil
}

func summarize(m p2p.Message) any {
	switch v := m.(type) {
	case *p2p.Headers:
		return fmt.Sprintf("%d headers", len(v.Headers))
	case *p2p.Slice:
		return fmt.Sprintf("slice offset %d total %d bytes %d", v.Offset, v.Total, len(v.Bytes))
	case *p2p.Inventory:
		return fmt.Sprintf("%d ids, next %v", len(v.IDs), v.Next != nil)
	case *p2p.Items:
		return fmt.Sprintf("%d items", len(v.Items))
	case *p2p.Addresses:
		return v.Addrs
	}
	return m
}

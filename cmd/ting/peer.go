package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/node"
	"github.com/ting-organization/goting/internal/p2p"
	"github.com/ting-organization/goting/internal/types"
)

// Agent is this implementation's peer-protocol agent string.
const Agent = "goting/" + node.Version

func newHello(g *types.GenesisConfig, announce bool) *p2p.Hello {
	var nb [8]byte
	rand.Read(nb[:])
	return &p2p.Hello{Version: p2p.ProtocolVersion, ChainID: g.ChainID(), Agent: Agent,
		Nonce: binary.LittleEndian.Uint64(nb[:]), Announce: announce}
}

// cmdPeer queries a peer: its state, headers, a block or its addresses.
func cmdPeer(args []string) error {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	gf := addGenesisFlags(fs)
	addr := fs.String("peer", "2.29.97.212:18442", "peer address")
	what := fs.String("ask", "state", "state, headers, block or addresses")
	id := fs.String("id", "", "block id for -ask block")
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
	switch *what {
	case "state":
		s := c.Peer
		return printJSON(map[string]any{"version": s.Version, "chain_id": s.ChainID, "agent": s.Agent,
			"features": s.Features, "nonce": s.Nonce, "height": s.Height, "tip": s.Tip,
			"work": s.Work.String(), "locator": s.Locator, "pending": s.Pending, "deferred": s.Deferred})
	case "headers":
		m, err := c.Do(ctx, &p2p.HeadersRequest{Locator: []hashing.Hash{types.GenesisBlockID(g.ChainID())}})
		if err != nil {
			return err
		}
		hs := m.(*p2p.Headers)
		for i, h := range hs.Headers {
			fmt.Printf("%d %s ts=%d used=%v\n", h.Height, types.HeaderIDFromBytes(hs.Raw[i]), h.Timestamp, h.ResourcesUsed)
		}
		return nil
	case "block":
		bid, err := hashing.ParseHash(*id)
		if err != nil {
			return err
		}
		m, err := c.Do(ctx, &p2p.RawRequest{ID: bid, Length: p2p.MaxSlice})
		if err != nil {
			return err
		}
		s := m.(*p2p.Slice)
		fmt.Printf("offset %d total %d bytes %d\n", s.Offset, s.Total, len(s.Bytes))
		return nil
	case "addresses":
		m, err := c.Do(ctx, &p2p.AddressesRequest{})
		if err != nil {
			return err
		}
		for _, a := range m.(*p2p.Addresses).Addrs {
			fmt.Println(a)
		}
		return nil
	}
	return errors.New("unknown -ask")
}

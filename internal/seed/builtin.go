package seed

import (
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/types"
)

// The built-in seeds: the development and production seed blocks, from the
// reference repository's conformance/seed (MIT or Apache-2.0), compressed.
// A node imports its network's seed when its data directory is created, as
// the reference node does; their ids are the built-in seed lists
// (chain.BuiltinSeed).
//
//go:embed bundles/*.blocks.gz
var bundles embed.FS

// Builtin returns the seed blocks of g's network, or nil for a network
// without a built-in seed.
func Builtin(g *types.GenesisConfig) ([][]byte, error) {
	var name string
	switch g.ChainID() {
	case types.DevelopmentGenesis().ChainID():
		name = "development"
	case types.ProductionGenesis().ChainID():
		name = "production"
	default:
		return nil, nil
	}
	f, err := bundles.Open("bundles/" + name + ".blocks.gz")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(z)
	if err != nil {
		return nil, err
	}
	blocks, err := ParseBundle(b)
	if err != nil {
		return nil, err
	}
	ids, err := ListFromBundle(blocks)
	if err != nil {
		return nil, err
	}
	want := chain.BuiltinSeed(g)
	if len(ids) != len(want) {
		return nil, fmt.Errorf("built-in %s seed holds %d blocks, its list %d", name, len(ids), len(want))
	}
	for i := range ids {
		if !bytes.Equal(ids[i][:], want[i][:]) {
			return nil, fmt.Errorf("built-in %s seed block %d is not on its list", name, i+1)
		}
	}
	return blocks, nil
}

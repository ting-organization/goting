package chain

import (
	_ "embed"
	"strings"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// The seed lists of the built-in networks: the BlockId at every seed height
// (rules §27.1), as published with the protocol and built into nodes.

//go:embed seeds/development.txt
var developmentSeed string

//go:embed seeds/production.txt
var productionSeed string

func parseSeed(s string) []hashing.Hash {
	var out []hashing.Hash
	for _, line := range strings.Fields(s) {
		out = append(out, hashing.MustParseHash(line))
	}
	return out
}

// BuiltinSeed returns the seed list of a built-in network, or nil.
func BuiltinSeed(g *types.GenesisConfig) []hashing.Hash {
	switch g.ChainID() {
	case types.DevelopmentGenesis().ChainID():
		return parseSeed(developmentSeed)
	case types.ProductionGenesis().ChainID():
		list := parseSeed(productionSeed)
		if len(list) != types.ProductionSeedLength ||
			list[len(list)-1] != hashing.MustParseHash(types.ProductionLastSeedIDHex) {
			panic("built-in production seed list does not end at the pinned block")
		}
		return list
	}
	return nil
}

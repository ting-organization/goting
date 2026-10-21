package types

// Built-in configurations (rules §28).

const developmentGenesisHex = `
010000f15365000000003fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ffffffffffffffffffffffffffffffffffffffffffffffff3c00000000a30200c04e000000e1f50500000000000400000000
000000001000000000000000080000000000a086010000000000100000000000000000040000000000000010000000000000`

const experimentalGenesisHex = `
010000f15365000000003fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ffffffffffffffffffffffffffffffffffffffffffffffff3c00000000a302000400000000e1f50500000000000400000000
000000001000000000000000080000000000a086010000000000100000000000000000040000000000000010000000000000`

const productionGenesisHex = `
01008000d86a0000000000000000ffffffffffffffffffffffffffffffffffffffffffffffffffffffff0000000fffffffff
ffffffffffffffffffffffffffffffffffffffffffffffff3c000000c0a80000c04e00008074d21a00000000000800000000
000000000400000000000000020000000000005a620200000000400000000000000000800100000000000080010000000000`

// DevelopmentGenesis returns the development network's configuration.
func DevelopmentGenesis() *GenesisConfig {
	g, err := DecodeGenesisConfigHex(developmentGenesisHex)
	if err != nil {
		panic(err)
	}
	return g
}

// ExperimentalGenesis returns the experimental configuration (epoch length 4).
func ExperimentalGenesis() *GenesisConfig {
	g, err := DecodeGenesisConfigHex(experimentalGenesisHex)
	if err != nil {
		panic(err)
	}
	return g
}

// ProductionGenesis returns the production network's configuration
// (rules §28.3), launched 2026-10-21 00:00:00 UTC.
func ProductionGenesis() *GenesisConfig {
	g, err := DecodeGenesisConfigHex(productionGenesisHex)
	if err != nil {
		panic(err)
	}
	return g
}

// ProductionSeedLength and ProductionLastSeedID pin the production seed
// (rules §28.3). Each header names its parent, so the last seed BlockId
// determines the BlockId at every seed height.
const ProductionSeedLength = 325

// ProductionLastSeedIDHex is the BlockId at height 325.
const ProductionLastSeedIDHex = "00000000e04237d25842f99dc925e087198affc2da93817ef30dd2a3527a9b32"

// NetworkByName returns a built-in configuration.
func NetworkByName(name string) (*GenesisConfig, bool) {
	switch name {
	case "development", "dev":
		return DevelopmentGenesis(), true
	case "experimental", "exp":
		return ExperimentalGenesis(), true
	case "production", "prod":
		return ProductionGenesis(), true
	}
	return nil, false
}

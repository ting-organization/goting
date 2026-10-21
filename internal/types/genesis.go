package types

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ting-organization/goting/internal/codec"
	"github.com/ting-organization/goting/internal/hashing"
)

// GenesisConfigSize is the exact canonical size of a GenesisConfig.
const GenesisConfigSize = 150

// ProtocolVersion is the only protocol version these rules define.
const ProtocolVersion = 1

// MinDataCapacityFloor is the smallest allowed minimum D capacity (§3.2).
const MinDataCapacityFloor = 302

// Target is a 256-bit mining target, stored big-endian.
type Target [32]byte

func (t Target) String() string { return hex.EncodeToString(t[:]) }

// IsZero reports whether the target is zero.
func (t Target) IsZero() bool { return t == Target{} }

// ParseTarget decodes 64 hex characters.
func ParseTarget(s string) (Target, error) {
	var t Target
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return t, errors.New("target must be 64 hex characters")
	}
	copy(t[:], b)
	return t, nil
}

// GenesisConfig is the network's fixed parameter list (rules §3.2).
type GenesisConfig struct {
	ProtocolVersion  uint16
	GenesisTimestamp uint64
	InitialTarget    Target
	PowLimit         Target
	TargetInterval   uint32
	HalfLife         uint32
	VoteEpochLength  uint32
	InitialLimits    ResourceVector
	MinimumLimits    ResourceVector
}

// Encode returns the 150 canonical bytes.
func (g *GenesisConfig) Encode() []byte {
	w := codec.NewWriter(GenesisConfigSize)
	w.U16(g.ProtocolVersion)
	w.U64(g.GenesisTimestamp)
	w.Raw(g.InitialTarget[:])
	w.Raw(g.PowLimit[:])
	w.U32(g.TargetInterval)
	w.U32(g.HalfLife)
	w.U32(g.VoteEpochLength)
	g.InitialLimits.Encode(w)
	g.MinimumLimits.Encode(w)
	return w.Buf
}

// DecodeGenesisConfig parses exactly 150 bytes and validates them.
func DecodeGenesisConfig(b []byte) (*GenesisConfig, error) {
	if len(b) != GenesisConfigSize {
		return nil, fmt.Errorf("genesis config must be %d bytes, got %d", GenesisConfigSize, len(b))
	}
	r := codec.NewReader(b)
	g := &GenesisConfig{}
	g.ProtocolVersion = r.U16()
	g.GenesisTimestamp = r.U64()
	r.Into(g.InitialTarget[:])
	r.Into(g.PowLimit[:])
	g.TargetInterval = r.U32()
	g.HalfLife = r.U32()
	g.VoteEpochLength = r.U32()
	g.InitialLimits = DecodeResourceVector(r)
	g.MinimumLimits = DecodeResourceVector(r)
	r.End()
	if err := r.Err(); err != nil {
		return nil, err
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// DecodeGenesisConfigHex parses the hex form, ignoring whitespace.
func DecodeGenesisConfigHex(s string) (*GenesisConfig, error) {
	s = strings.Join(strings.Fields(s), "")
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return DecodeGenesisConfig(b)
}

// Validate applies the configuration rules of §3.2.
func (g *GenesisConfig) Validate() error {
	if g.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %d", g.ProtocolVersion)
	}
	for i := 0; i < 4; i++ {
		if g.InitialLimits[i] == 0 || g.MinimumLimits[i] == 0 {
			return errors.New("every capacity must be positive")
		}
		if g.MinimumLimits[i] > g.InitialLimits[i] {
			return errors.New("a minimum capacity exceeds its initial value")
		}
	}
	if g.InitialTarget.IsZero() || bytes.Compare(g.InitialTarget[:], g.PowLimit[:]) > 0 {
		return errors.New("initial target must lie between 1 and the PoW limit")
	}
	if g.TargetInterval == 0 || g.HalfLife == 0 || g.VoteEpochLength == 0 {
		return errors.New("target interval, half-life and epoch length must be positive")
	}
	if g.MinimumLimits[D] < MinDataCapacityFloor {
		return fmt.Errorf("minimum D capacity must be at least %d", MinDataCapacityFloor)
	}
	return nil
}

// ChainID is H("chain", encode(GenesisConfig)).
func (g *GenesisConfig) ChainID() hashing.Hash {
	return hashing.H(hashing.TagChain, g.Encode())
}

// GenesisBlockID is H("genesis", ChainId).
func GenesisBlockID(chainID hashing.Hash) hashing.Hash {
	return hashing.H(hashing.TagGenesis, chainID[:])
}

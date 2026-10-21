// Package consensus implements the chain-level rules: proof of work and
// work (rules §8), timestamps and the retarget (§9), and capacity voting
// (§10). Everything here depends only on the genesis configuration and data
// in the branch.
package consensus

import (
	"bytes"
	"math/big"
	"sort"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

var (
	two256   = new(big.Int).Lsh(big.NewInt(1), 256)
	big65536 = big.NewInt(65536)
	c1       = new(big.Int).SetUint64(195766423245049)
	c2       = big.NewInt(971821376)
	c3       = big.NewInt(5127)
	twoTo47  = new(big.Int).Lsh(big.NewInt(1), 47)
)

// TargetInt returns the target as an integer.
func TargetInt(t types.Target) *big.Int { return new(big.Int).SetBytes(t[:]) }

// IntTarget encodes a value in [0, 2^256) as a big-endian target.
func IntTarget(v *big.Int) types.Target {
	var t types.Target
	v.FillBytes(t[:])
	return t
}

// Work is floor(2^256 / (target + 1)) (rules §8.2).
func Work(t types.Target) *big.Int {
	d := TargetInt(t)
	d.Add(d, big.NewInt(1))
	return d.Div(new(big.Int).Set(two256), d)
}

// ValidPoW reports whether the BlockId, read big-endian, is at most the target.
func ValidPoW(id hashing.Hash, t types.Target) bool {
	return bytes.Compare(id[:], t[:]) <= 0
}

// NextTarget is the exact target of the block at height h ≥ 1 whose parent
// has the given timestamp (rules §9.3).
func NextTarget(g *types.GenesisConfig, h uint64, parentTimestamp uint64) types.Target {
	elapsed := new(big.Int).SetUint64(parentTimestamp)
	elapsed.Sub(elapsed, new(big.Int).SetUint64(g.GenesisTimestamp))
	v := Retarget(TargetInt(g.InitialTarget), TargetInt(g.PowLimit), uint64(g.TargetInterval),
		uint64(g.HalfLife), new(big.Int).SetUint64(h-1), elapsed)
	return IntTarget(v)
}

// Retarget computes clamp(floor(anchor × 2^((elapsed − interval × blocks) /
// half-life)), 1, limit) with the exponent floored in 1/65536 steps and its
// fraction approximated by the fixed cubic (rules §9.3). elapsed may be
// negative.
func Retarget(anchor, powLimit *big.Int, interval, halfLife uint64, blocks, elapsed *big.Int) *big.Int {
	expected := new(big.Int).SetUint64(interval)
	expected.Mul(expected, blocks)
	// exponent = floor(((elapsed − expected) × 65536) / half_life); big.Int
	// Div is Euclidean, which is floor division for a positive divisor.
	exponent := new(big.Int).Sub(elapsed, expected)
	exponent.Mul(exponent, big65536)
	exponent.Div(exponent, new(big.Int).SetUint64(halfLife))
	shifts := new(big.Int).Div(exponent, big65536)
	frac := new(big.Int).Sub(exponent, new(big.Int).Mul(shifts, big65536))

	// factor = ((c1·f + c2·f² + c3·f³ + 2^47) >> 48) + 65536
	f2 := new(big.Int).Mul(frac, frac)
	f3 := new(big.Int).Mul(f2, frac)
	factor := new(big.Int).Mul(c1, frac)
	factor.Add(factor, f2.Mul(f2, c2))
	factor.Add(factor, f3.Mul(f3, c3))
	factor.Add(factor, twoTo47)
	factor.Rsh(factor, 48)
	factor.Add(factor, big65536)

	e := shifts.Sub(shifts, big.NewInt(16))
	if e.Cmp(big.NewInt(256)) >= 0 {
		return new(big.Int).Set(powLimit)
	}
	if e.Cmp(big.NewInt(-274)) <= 0 {
		return big.NewInt(1)
	}
	v := new(big.Int).Mul(anchor, factor)
	if s := e.Int64(); s >= 0 {
		v.Lsh(v, uint(s))
	} else {
		v.Rsh(v, uint(-s))
	}
	if v.Sign() == 0 {
		v.SetInt64(1)
	}
	if v.Cmp(powLimit) > 0 {
		return new(big.Int).Set(powLimit)
	}
	return v
}

// MedianTimeSpan is the number of earlier timestamps the median uses.
const MedianTimeSpan = 11

// MedianTime returns the median the next block's timestamp must exceed,
// given the timestamps of the previous blocks, most recent first, including
// genesis when fewer than eleven mined blocks precede (rules §9.1).
func MedianTime(recent []uint64) uint64 {
	n := len(recent)
	if n > MedianTimeSpan {
		n = MedianTimeSpan
	}
	s := make([]uint64, n)
	copy(s, recent[:n])
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[n/2]
}

// Epoch returns floor((h − 1) / E) for a mined height h ≥ 1.
func Epoch(h uint64, epochLength uint32) uint64 { return (h - 1) / uint64(epochLength) }

// Direction is a capacity decision.
type Direction uint8

const (
	Hold     Direction = 0
	Increase Direction = 1
	Decrease Direction = 2
)

// Tally accumulates one epoch's vote work for every resource.
type Tally struct {
	W    *big.Int
	Up   [4]*big.Int
	Down [4]*big.Int
}

// NewTally returns an empty tally.
func NewTally() *Tally {
	t := &Tally{W: new(big.Int)}
	for i := 0; i < 4; i++ {
		t.Up[i] = new(big.Int)
		t.Down[i] = new(big.Int)
	}
	return t
}

// Add counts one block's votes weighted by its work.
func (t *Tally) Add(votes [4]uint8, work *big.Int) {
	t.W.Add(t.W, work)
	for i, v := range votes {
		switch v {
		case types.VoteIncrease:
			t.Up[i].Add(t.Up[i], work)
		case types.VoteDecrease:
			t.Down[i].Add(t.Down[i], work)
		}
	}
}

// Decide returns each resource's direction (rules §10.2). An epoch without
// work decides nothing; on a chain every block carries work, so this only
// matters for direct evaluation of the formula (the reference holds too).
func (t *Tally) Decide() [4]Direction {
	var d [4]Direction
	if t.W.Sign() == 0 {
		return d
	}
	two, three := big.NewInt(2), big.NewInt(3)
	w2 := new(big.Int).Mul(t.W, two)
	for i := 0; i < 4; i++ {
		u3 := new(big.Int).Mul(t.Up[i], three)
		l2 := new(big.Int).Mul(t.Down[i], two)
		switch {
		case u3.Cmp(w2) >= 0:
			d[i] = Increase
		case l2.Cmp(t.W) > 0:
			d[i] = Decrease
		}
	}
	return d
}

// ApplyDirections steps each capacity (rules §10.4).
func ApplyDirections(limits types.ResourceVector, dirs [4]Direction, minimum types.ResourceVector) types.ResourceVector {
	out := limits
	for i := 0; i < 4; i++ {
		x := limits[i]
		switch dirs[i] {
		case Increase:
			step := x / 20
			if step < 1 {
				step = 1
			}
			if x > ^uint64(0)-step {
				out[i] = ^uint64(0)
			} else {
				out[i] = x + step
			}
		case Decrease:
			step := x / 10
			if step < 1 {
				step = 1
			}
			var v uint64
			if x > step {
				v = x - step
			}
			if v < minimum[i] {
				v = minimum[i]
			}
			out[i] = v
		}
	}
	return out
}

// VoteState is what a block needs to know about capacity: its active
// capacity and the decisions of the three epochs before its own, which the
// next epochs will apply.
type VoteState struct {
	Limits types.ResourceVector
	// Pending[k] is the decision of epoch e−3+k, where e is the block's epoch.
	Pending [3][4]Direction
}

// GenesisVoteState is the state at height 0.
func GenesisVoteState(g *types.GenesisConfig) VoteState {
	return VoteState{Limits: g.InitialLimits}
}

// NextVoteState derives the vote state of the child at height h of a parent
// with state parent. When h starts a new epoch, closedEpoch must return the
// tally of the epoch that just ended (the parent's), and is otherwise unused.
func NextVoteState(g *types.GenesisConfig, parent VoteState, h uint64, closedEpoch func() *Tally) VoteState {
	if h == 1 {
		return GenesisVoteState(g)
	}
	if Epoch(h, g.VoteEpochLength) == Epoch(h-1, g.VoteEpochLength) {
		return parent
	}
	// The parent closed epoch e; the child opens e+1, whose limit applies
	// the decision of epoch (e+1)−3 = Pending[1] of the parent.
	e1 := Epoch(h, g.VoteEpochLength)
	decided := closedEpoch().Decide()
	next := VoteState{
		Pending: [3][4]Direction{parent.Pending[1], parent.Pending[2], decided},
	}
	if e1 >= 3 {
		next.Limits = ApplyDirections(parent.Limits, next.Pending[0], g.MinimumLimits)
	} else {
		next.Limits = g.InitialLimits
	}
	return next
}

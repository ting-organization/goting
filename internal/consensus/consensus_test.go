package consensus

import (
	"math/big"
	"testing"

	"github.com/ting-organization/goting/internal/types"
)

func TestWorkChecks(t *testing.T) {
	one := IntTarget(big.NewInt(1))
	if Work(one).Cmp(new(big.Int).Lsh(big.NewInt(1), 255)) != 0 {
		t.Fatal("work at target 1")
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	if Work(IntTarget(max)).Cmp(big.NewInt(1)) != 0 {
		t.Fatal("work at 2^256-1")
	}
}

func TestTargetSchedule(t *testing.T) {
	g := types.DevelopmentGenesis()
	init := TargetInt(g.InitialTarget)
	// Height 1: parent is genesis, target is the initial target.
	if NextTarget(g, 1, g.GenesisTimestamp) != g.InitialTarget {
		t.Fatal("first target")
	}
	// On schedule: parent of height h at genesis + 60(h-1).
	if NextTarget(g, 10, g.GenesisTimestamp+60*9) != g.InitialTarget {
		t.Fatal("on schedule")
	}
	// One half-life behind: doubles (capped by the PoW limit of 2^256-1:
	// the dev initial target is 2^254-1, so doubling stays below it).
	ts := g.GenesisTimestamp + 60*9 + uint64(g.HalfLife)
	got := TargetInt(NextTarget(g, 10, ts))
	// factor at frac 0 is exactly 65536: target = init × 2.
	if got.Cmp(new(big.Int).Lsh(init, 1)) != 0 {
		t.Fatalf("one half-life behind: %x", got)
	}
	// One half-life ahead: halves.
	got = TargetInt(NextTarget(g, 10+uint64(g.HalfLife)/60, g.GenesisTimestamp+60*9))
	if got.Cmp(new(big.Int).Rsh(init, 1)) != 0 {
		t.Fatalf("one half-life ahead: %x", got)
	}
	// Far behind saturates at the PoW limit; far ahead at 1.
	if NextTarget(g, 2, g.GenesisTimestamp+uint64(g.HalfLife)*300) != g.PowLimit {
		t.Fatal("saturate high")
	}
	if TargetInt(NextTarget(g, 1<<40, g.GenesisTimestamp+1)).Cmp(big.NewInt(1)) != 0 {
		t.Fatal("saturate low")
	}
	// Half a half-life behind: factor approximates sqrt(2) in 16.48 fixed point.
	ts = g.GenesisTimestamp + 60*9 + uint64(g.HalfLife)/2
	got = TargetInt(NextTarget(g, 10, ts))
	// frac = 32768: factor = ((195766423245049·32768 + 971821376·32768² +
	// 5127·32768³ + 2^47) >> 48) + 65536.
	f := big.NewInt(32768)
	fac := new(big.Int).Mul(big.NewInt(195766423245049), f)
	fac.Add(fac, new(big.Int).Mul(big.NewInt(971821376), new(big.Int).Mul(f, f)))
	fac.Add(fac, new(big.Int).Mul(big.NewInt(5127), new(big.Int).Mul(f, new(big.Int).Mul(f, f))))
	fac.Add(fac, new(big.Int).Lsh(big.NewInt(1), 47))
	fac.Rsh(fac, 48)
	fac.Add(fac, big.NewInt(65536))
	want := new(big.Int).Mul(init, fac)
	want.Rsh(want, 16)
	if got.Cmp(want) != 0 {
		t.Fatalf("half half-life: got %x want %x", got, want)
	}
	if fac.Int64() < 92660 || fac.Int64() > 92700 { // 65536·√2 ≈ 92681.9; the cubic is an approximation
		t.Fatalf("factor %d", fac.Int64())
	}
	// Negative exponent with a fraction: floor toward −∞. One second ahead
	// of schedule at h = 10.
	got = TargetInt(NextTarget(g, 10, g.GenesisTimestamp+60*9-1))
	// exponent = floor(-65536/172800) = -1; shifts = -1; frac = 65535.
	f = big.NewInt(65535)
	fac = new(big.Int).Mul(big.NewInt(195766423245049), f)
	fac.Add(fac, new(big.Int).Mul(big.NewInt(971821376), new(big.Int).Mul(f, f)))
	fac.Add(fac, new(big.Int).Mul(big.NewInt(5127), new(big.Int).Mul(f, new(big.Int).Mul(f, f))))
	fac.Add(fac, new(big.Int).Lsh(big.NewInt(1), 47))
	fac.Rsh(fac, 48)
	fac.Add(fac, big.NewInt(65536))
	want = new(big.Int).Mul(init, fac)
	want.Rsh(want, 17)
	if got.Cmp(want) != 0 {
		t.Fatalf("one second ahead: got %x want %x", got, want)
	}
}

func TestMedian(t *testing.T) {
	if MedianTime([]uint64{5}) != 5 {
		t.Fatal("one sample")
	}
	// Even count uses the upper middle.
	if MedianTime([]uint64{1, 9}) != 9 {
		t.Fatal("two samples")
	}
	if MedianTime([]uint64{3, 1, 2}) != 2 {
		t.Fatal("three")
	}
	// Only the first eleven count.
	if MedianTime([]uint64{1, 1, 1, 1, 1, 1, 9, 9, 9, 9, 9, 100, 100, 100}) != 1 {
		t.Fatal("eleven")
	}
}

func TestVotingThresholds(t *testing.T) {
	w := big.NewInt(3)
	tl := NewTally()
	tl.Add([4]uint8{1, 2, 0, 1}, w)
	tl.Add([4]uint8{1, 2, 2, 0}, w)
	tl.Add([4]uint8{0, 0, 2, 0}, w)
	d := tl.Decide()
	// C: exactly two-thirds increase → increase. S: two-thirds decrease → decrease.
	// D: two-thirds decrease → decrease. G: one-third increase → hold.
	if d != [4]Direction{Increase, Decrease, Decrease, Hold} {
		t.Fatalf("directions %v", d)
	}
	tl = NewTally()
	tl.Add([4]uint8{2, 0, 0, 0}, w)
	tl.Add([4]uint8{0, 0, 0, 0}, w)
	if tl.Decide()[0] != Hold {
		t.Fatal("exactly one-half decrease must hold")
	}
	min := types.ResourceVector{10, 10, 10, 10}
	got := ApplyDirections(types.ResourceVector{100, 100, 11, ^uint64(0)}, [4]Direction{Increase, Decrease, Decrease, Increase}, min)
	if got != (types.ResourceVector{105, 90, 10, ^uint64(0)}) {
		t.Fatalf("apply %v", got)
	}
	got = ApplyDirections(types.ResourceVector{19, 9, 0, 0}, [4]Direction{Increase, Decrease, Hold, Hold}, types.ResourceVector{1, 1, 0, 0})
	if got[0] != 20 || got[1] != 8 {
		t.Fatalf("minimum step 1: %v", got)
	}
}

func TestVoteSchedule(t *testing.T) {
	g := types.ExperimentalGenesis() // epoch length 4
	vs := GenesisVoteState(g)
	all := func(v uint8) func() *Tally {
		return func() *Tally {
			t := NewTally()
			for i := 0; i < 4; i++ {
				t.Add([4]uint8{v, v, v, v}, big.NewInt(1))
			}
			return t
		}
	}
	// Everyone votes increase in epoch 0 (heights 1-4); the decision takes
	// effect at epoch 3 (heights 13-16).
	limits := map[uint64]types.ResourceVector{}
	for h := uint64(1); h <= 20; h++ {
		vote := uint8(0)
		if Epoch(h-1+1, g.VoteEpochLength) == 1 { // closing epoch 0
			vote = 1
		}
		vs = NextVoteState(g, vs, h, all(vote))
		limits[h] = vs.Limits
	}
	if limits[12] != g.InitialLimits {
		t.Fatalf("limits before effect %v", limits[12])
	}
	want := ApplyDirections(g.InitialLimits, [4]Direction{1, 1, 1, 1}, g.MinimumLimits)
	if limits[13] != want || limits[16] != want {
		t.Fatalf("limits at epoch 3: %v want %v", limits[13], want)
	}
	if limits[17] != want {
		t.Fatalf("epoch 4 holds: %v", limits[17])
	}
}

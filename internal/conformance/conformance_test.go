package conformance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ting-organization/goting/internal/types"
)

// referenceDir finds the reference repository's conformance directory:
// $TING_REFERENCE/conformance, or ../ting-reference/conformance next to
// this repository. The tests skip when neither exists.
func referenceDir(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if d := os.Getenv("TING_REFERENCE"); d != "" {
		candidates = append(candidates, filepath.Join(d, "conformance"))
	}
	candidates = append(candidates, filepath.Join("..", "..", "..", "ting-reference", "conformance"))
	for _, d := range candidates {
		if st, err := os.Stat(filepath.Join(d, "vectors")); err == nil && st.IsDir() {
			return d
		}
	}
	t.Skip("reference conformance material not found; set TING_REFERENCE")
	return ""
}

func TestVectors(t *testing.T) {
	r := &Runner{Dir: referenceDir(t)}
	results, err := r.Vectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 140 {
		t.Fatalf("only %d vectors found", len(results))
	}
	for _, res := range results {
		if !res.Pass {
			t.Errorf("%s/%s: %s", res.Group, res.Name, res.Detail)
		}
	}
}

func TestVotingChain(t *testing.T) {
	r := &Runner{Dir: referenceDir(t)}
	res, err := r.VotingChain()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass {
		t.Fatal(res.Detail)
	}
}

func TestSeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("seed replay in -short mode")
	}
	r := &Runner{Dir: referenceDir(t)}
	for name, g := range map[string]*types.GenesisConfig{
		"production": types.ProductionGenesis(), "development": types.DevelopmentGenesis()} {
		res, err := r.Seed(name, g)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Pass {
			t.Fatalf("%s: %s", name, res.Detail)
		}
	}
}

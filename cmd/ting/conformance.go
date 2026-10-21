package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/ting-organization/goting/internal/conformance"
	"github.com/ting-organization/goting/internal/types"
)

// cmdConformance runs the reference repository's conformance material.
func cmdConformance(args []string) error {
	fs := flag.NewFlagSet("conformance", flag.ExitOnError)
	dir := fs.String("dir", "../ting-reference/conformance", "the reference repository's conformance directory")
	verbose := fs.Bool("v", false, "print passing vectors too")
	fs.Parse(args)
	r := &conformance.Runner{Dir: *dir}
	results, err := r.Vectors()
	if err != nil {
		return err
	}
	vc, err := r.VotingChain()
	if err != nil {
		return err
	}
	results = append(results, vc)
	for _, s := range []struct {
		name string
		g    *types.GenesisConfig
	}{{"production", types.ProductionGenesis()}, {"development", types.DevelopmentGenesis()}} {
		res, err := r.Seed(s.name, s.g)
		if err != nil {
			return err
		}
		results = append(results, res)
	}
	failed := 0
	perGroup := map[string][2]int{}
	for _, res := range results {
		c := perGroup[res.Group]
		c[1]++
		if res.Pass {
			c[0]++
		} else {
			failed++
		}
		perGroup[res.Group] = c
		if !res.Pass || *verbose {
			status := "PASS"
			if !res.Pass {
				status = "FAIL"
			}
			fmt.Printf("%s %s/%s: %s\n", status, res.Group, res.Name, res.Detail)
		}
	}
	var groups []string
	for g := range perGroup {
		groups = append(groups, g)
	}
	sortStrings(groups)
	for _, g := range groups {
		fmt.Printf("%-15s %d/%d\n", g, perGroup[g][0], perGroup[g][1])
	}
	fmt.Printf("total: %d of %d passed (%s)\n", len(results)-failed, len(results), filepath.Clean(*dir))
	if failed > 0 {
		return errors.New("conformance failures")
	}
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

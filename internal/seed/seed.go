// Package seed reads seed bundles and lists and replays them (rules §27).
package seed

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// ParseBundle splits a bundle: for each block, u32 length then its bytes,
// with no trailing bytes.
func ParseBundle(b []byte) ([][]byte, error) {
	var out [][]byte
	for off := 0; off < len(b); {
		if len(b)-off < 4 {
			return nil, fmt.Errorf("bundle: truncated length at offset %d", off)
		}
		n := int(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if n > len(b)-off {
			return nil, fmt.Errorf("bundle: block %d runs past the end", len(out))
		}
		out = append(out, b[off:off+n:off+n])
		off += n
	}
	return out, nil
}

// EncodeBundle frames blocks as a bundle.
func EncodeBundle(blocks [][]byte) []byte {
	var out []byte
	for _, b := range blocks {
		out = binary.LittleEndian.AppendUint32(out, uint32(len(b)))
		out = append(out, b...)
	}
	return out
}

// ParseList reads a seed list: either a JSON array of hex BlockIds (or of
// objects with an "id"/"block_id"/"blockId" field), or whitespace-separated
// hex BlockIds. Entry i is the BlockId at height i+1.
func ParseList(b []byte) ([]hashing.Hash, error) {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		return parseJSONList([]byte(s))
	}
	var out []hashing.Hash
	for _, f := range strings.Fields(s) {
		h, err := hashing.ParseHash(strings.TrimPrefix(f, "0x"))
		if err != nil {
			return nil, fmt.Errorf("seed list entry %d: %w", len(out)+1, err)
		}
		out = append(out, h)
	}
	return out, nil
}

func parseJSONList(b []byte) ([]hashing.Hash, error) {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if obj, ok := raw.(map[string]any); ok {
		for _, k := range []string{"blocks", "seed", "list", "ids", "block_ids"} {
			if v, ok := obj[k]; ok {
				raw = v
				break
			}
		}
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, errors.New("seed list: expected a JSON array")
	}
	out := make([]hashing.Hash, 0, len(arr))
	for i, v := range arr {
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case map[string]any:
			for _, k := range []string{"id", "block_id", "blockId", "hash"} {
				if str, ok := x[k].(string); ok {
					s = str
					break
				}
			}
		}
		h, err := hashing.ParseHash(strings.TrimPrefix(s, "0x"))
		if err != nil {
			return nil, fmt.Errorf("seed list entry %d: %w", i+1, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// ListFromBundle derives the BlockIds of a bundle's blocks. It is for
// tooling: a node's seed list must come from its own configuration.
func ListFromBundle(blocks [][]byte) ([]hashing.Hash, error) {
	out := make([]hashing.Hash, len(blocks))
	for i, b := range blocks {
		if len(b) < types.HeaderSize {
			return nil, fmt.Errorf("block %d shorter than a header", i)
		}
		out[i] = types.HeaderIDFromBytes(b[:types.HeaderSize])
	}
	return out, nil
}

// Report summarizes a replay.
type Report struct {
	Blocks       int
	Transactions int
	Tip          *chain.Entry
	Elapsed      time.Duration
}

// Replay validates the bundle's blocks in order on c (rules §27.2). It stops
// at the first block that fails, reporting its height and the reason; a
// receipt difference names the transaction and field.
func Replay(c *chain.Chain, blocks [][]byte, progress func(h uint64, e *chain.Entry)) (*Report, error) {
	start := time.Now()
	rep := &Report{}
	for i, b := range blocks {
		e, err := c.ProcessBlock(b)
		if err != nil && !errors.Is(err, chain.ErrKnown) {
			return rep, fmt.Errorf("seed block %d (height %d): %w", i, i+1, err)
		}
		rep.Blocks++
		rep.Transactions += e.TxCount
		rep.Tip = e
		if progress != nil {
			progress(e.Height, e)
		}
	}
	if len(blocks) != c.SeedLen() && c.SeedLen() > 0 {
		return rep, fmt.Errorf("bundle has %d blocks but the seed list has %d", len(blocks), c.SeedLen())
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

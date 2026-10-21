package chain

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/exec"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
)

// ErrInvalid marks a block that breaks a consensus rule. Any other error from
// validation is a local failure and says nothing about the block.
var ErrInvalid = errors.New("invalid block")

// InvalidError explains why a block is invalid.
type InvalidError struct {
	Height uint64
	ID     hashing.Hash
	Reason string
	// Header reports a header rule, checked before the body.
	Header bool
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("invalid block %s at height %d: %s", e.ID, e.Height, e.Reason)
}

func (e *InvalidError) Unwrap() error { return ErrInvalid }

// BodyMismatchError reports a body that does not match its header's
// commitments. It proves nothing about the header: the same header with its
// real body may be valid, so the BlockId is not remembered as invalid.
type BodyMismatchError struct{ InvalidError }

func (e *BodyMismatchError) Error() string {
	return fmt.Sprintf("body does not match header %s at height %d: %s", e.ID, e.Height, e.Reason)
}

func (e *BodyMismatchError) Unwrap() error { return ErrInvalid }

func mismatch(h *types.Header, id hashing.Hash, format string, args ...any) error {
	return &BodyMismatchError{InvalidError{Height: h.Height, ID: id, Reason: fmt.Sprintf(format, args...)}}
}

func invalid(h *types.Header, id hashing.Hash, format string, args ...any) error {
	var height uint64
	if h != nil {
		height = h.Height
	}
	return &InvalidError{Height: height, ID: id, Reason: fmt.Sprintf(format, args...)}
}

// ReceiptMismatch reports the first transaction whose derived receipt
// differs from the one the block carries.
type ReceiptMismatch struct {
	Height uint64
	Index  int
	Field  string
	Got    string // derived by this node
	Want   string // carried by the block
}

func (e *ReceiptMismatch) Error() string {
	return fmt.Sprintf("receipt mismatch at height %d, transaction %d, field %s: derived %s, block has %s",
		e.Height, e.Index, e.Field, e.Got, e.Want)
}

func (e *ReceiptMismatch) Unwrap() error { return ErrInvalid }

// medianTime returns the median timestamp the child of parent must exceed.
func (c *Chain) medianTime(parent *Entry) uint64 {
	var ts [consensus.MedianTimeSpan]uint64
	n := 0
	for e := parent; e != nil && n < consensus.MedianTimeSpan; e = e.Parent {
		ts[n] = e.Timestamp
		n++
	}
	return consensus.MedianTime(ts[:n])
}

// voteStateFor derives the vote state of a child of parent at height h.
func (c *Chain) voteStateFor(parent *Entry, h uint64) consensus.VoteState {
	return consensus.NextVoteState(c.genesis, parent.Vote, h, func() *consensus.Tally {
		t := consensus.NewTally()
		ep := consensus.Epoch(parent.Height, c.genesis.VoteEpochLength)
		for e := parent; e != nil && e.Height > 0 && consensus.Epoch(e.Height, c.genesis.VoteEpochLength) == ep; e = e.Parent {
			t.Add(e.Votes, e.BlockWork)
		}
		return t
	})
}

// checkHeaderAlone applies the header rules that need no parent: version,
// chain, votes, the target's range and proof of work against the declared
// target. A block failing them is refused before it can wait for a parent.
func (c *Chain) checkHeaderAlone(h *types.Header, id hashing.Hash) error {
	if h.ProtocolVersion != types.ProtocolVersion {
		return invalid(h, id, "protocol version %d", h.ProtocolVersion)
	}
	if h.ChainID != c.chainID {
		return invalid(h, id, "wrong chain id")
	}
	for i, v := range h.Votes {
		if v > types.VoteDecrease {
			return invalid(h, id, "vote %d has value %d", i, v)
		}
	}
	if h.Target.IsZero() || bytes.Compare(h.Target[:], c.genesis.PowLimit[:]) > 0 {
		return invalid(h, id, "target outside 1..pow limit")
	}
	if !consensus.ValidPoW(id, h.Target) {
		return invalid(h, id, "proof of work above target")
	}
	return nil
}

// ancestry is what validating a header needs of its parent's chain.
type ancestry struct {
	height    uint64
	id        hashing.Hash
	timestamp uint64
	median    uint64              // the child's timestamp must exceed it
	vote      consensus.VoteState // the child's vote state, active limits included
}

// ancestryOf derives a child's ancestry from an indexed parent.
func (c *Chain) ancestryOf(parent *Entry) ancestry {
	return ancestry{height: parent.Height, id: parent.ID, timestamp: parent.Timestamp,
		median: c.medianTime(parent), vote: c.voteStateFor(parent, parent.Height+1)}
}

// checkHeader applies every header rule that depends on the parent
// (rules §4.2, §7.1 items 1–4, §26.7).
func (c *Chain) checkHeader(parent *Entry, h *types.Header, id hashing.Hash) (consensus.VoteState, error) {
	if parent.Height == ^uint64(0) {
		return consensus.VoteState{}, invalid(h, id, "height %d does not follow parent height %d", h.Height, parent.Height)
	}
	a := c.ancestryOf(parent)
	return a.vote, c.checkHeaderAt(a, h, id)
}

// checkHeaderAt checks a header against an ancestry.
func (c *Chain) checkHeaderAt(a ancestry, h *types.Header, id hashing.Hash) error {
	if h.Height >= 1 && h.Height <= uint64(len(c.seed)) && c.seed[h.Height-1] != id {
		return invalid(h, id, "not the seed block listed at height %d", h.Height)
	}
	if h.ProtocolVersion != types.ProtocolVersion {
		return invalid(h, id, "protocol version %d", h.ProtocolVersion)
	}
	if h.ChainID != c.chainID {
		return invalid(h, id, "wrong chain id")
	}
	if a.height == ^uint64(0) || h.Height != a.height+1 {
		return invalid(h, id, "height %d does not follow parent height %d", h.Height, a.height)
	}
	if h.ParentID != a.id {
		return invalid(h, id, "parent id mismatch")
	}
	if h.Timestamp <= a.median {
		return invalid(h, id, "timestamp %d not above median %d", h.Timestamp, a.median)
	}
	if want := consensus.NextTarget(c.genesis, h.Height, a.timestamp); h.Target != want {
		return invalid(h, id, "target %s, expected %s", h.Target, want)
	}
	for i, v := range h.Votes {
		if v > types.VoteDecrease {
			return invalid(h, id, "vote %d has value %d", i, v)
		}
	}
	if !consensus.ValidPoW(id, h.Target) {
		return invalid(h, id, "proof of work above target")
	}
	if !h.ResourcesUsed.LessEq(a.vote.Limits) {
		return invalid(h, id, "declared resources %v exceed capacity %v", h.ResourcesUsed, a.vote.Limits)
	}
	return nil
}

// executeBody checks the body against the header and executes it from the
// parent's state (rules §7.1 items 5–9).
func (c *Chain) executeBody(parentState state.Tree, h *types.Header, id hashing.Hash, raw []byte, body *types.Body, limits types.ResourceVector) (*exec.Result, error) {
	if uint64(len(raw)) > limits[types.D] {
		// Every published byte is charged as D, so no block this large is
		// valid; the body is not even decoded.
		return nil, mismatch(h, id, "block of %d bytes exceeds D capacity %d", len(raw), limits[types.D])
	}
	if types.ProducerHash(body.ProducerData) != h.ProducerHash {
		return nil, mismatch(h, id, "producer hash mismatch")
	}
	if commit.TransactionsRoot(body.Transactions) != h.TransactionsRoot {
		return nil, mismatch(h, id, "transactions root mismatch")
	}
	// The carried receipts must be the ones the header commits to before a
	// difference from the derived ones can prove the block invalid.
	if commit.ReceiptsRoot(body.Receipts) != h.ReceiptsRoot {
		return nil, mismatch(h, id, "carried receipts do not match the receipts root")
	}
	txs, err := decodeEnvelopes(body.Transactions)
	if err != nil {
		return nil, invalid(h, id, "%v", err)
	}
	for i, rb := range body.Receipts {
		if _, err := types.DecodeReceipt(rb); err != nil {
			return nil, invalid(h, id, "receipt %d: %v", i, err)
		}
	}
	// Execution stops at the first debit past the totals the header
	// declares (they are within capacity), so a block that under-declares
	// costs no more than it claims.
	guard := limits
	for i := 0; i < 4; i++ {
		guard[i] = min(guard[i], h.ResourcesUsed[i])
	}
	ctx := &vm.BlockContext{
		ChainID:   c.chainID,
		Height:    h.Height,
		Timestamp: h.Timestamp,
		ParentID:  h.ParentID,
		Producer:  body.ProducerData,
	}
	b, err := exec.NewBuilder(parentState, ctx, guard, c.codes)
	if err != nil {
		return nil, invalid(h, id, "%v", err)
	}
	for i, raw := range body.Transactions {
		r, err := b.Add(raw, txs[i])
		if err != nil {
			if errors.Is(err, exec.ErrCapacity) {
				return nil, invalid(h, id, "execution passes the declared resources %v or capacity %v: %v",
					h.ResourcesUsed, limits, err)
			}
			return nil, fmt.Errorf("executing transaction %d at height %d: %w", i, h.Height, err)
		}
		if got := b.LastReceiptBytes(); !bytes.Equal(got, body.Receipts[i]) {
			return nil, diffReceipt(h.Height, i, r, got, body.Receipts[i])
		}
	}
	res, err := b.Finish()
	if err != nil {
		return nil, err
	}
	if res.Used != h.ResourcesUsed {
		return nil, invalid(h, id, "resources used %v, header declares %v", res.Used, h.ResourcesUsed)
	}
	if root := commit.ReceiptsRoot(res.ReceiptBytes); root != h.ReceiptsRoot {
		return nil, invalid(h, id, "receipts root mismatch")
	}
	if root := res.State.Root(); root != h.StateRoot {
		return nil, invalid(h, id, "state root %s, header declares %s", root, h.StateRoot)
	}
	return res, nil
}

// decodeEnvelopes applies the static envelope rules to every transaction.
func decodeEnvelopes(raws [][]byte) ([]*types.Transaction, error) {
	txs := make([]*types.Transaction, len(raws))
	for i, raw := range raws {
		tx, err := types.DecodeTransaction(raw)
		if err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		txs[i] = tx
	}
	return txs, nil
}

// diffReceipt names the first field in which a derived receipt differs.
func diffReceipt(height uint64, index int, got *types.Receipt, gotBytes, want []byte) error {
	e := &ReceiptMismatch{Height: height, Index: index, Field: "encoding",
		Got: fmt.Sprintf("%x", gotBytes), Want: fmt.Sprintf("%x", want)}
	w, err := types.DecodeReceipt(want)
	if err != nil {
		e.Field = "encoding (block receipt does not decode: " + err.Error() + ")"
		return e
	}
	switch {
	case w.TransactionID != got.TransactionID:
		e.Field, e.Got, e.Want = "transaction_id", got.TransactionID.String(), w.TransactionID.String()
	case w.Status != got.Status:
		e.Field = "status"
		e.Got = fmt.Sprintf("%d (%s)", got.Status, types.StatusName(got.Status))
		e.Want = fmt.Sprintf("%d (%s)", w.Status, types.StatusName(w.Status))
	case w.ResourcesUsed != got.ResourcesUsed:
		e.Field, e.Got, e.Want = "resources_used", got.ResourcesUsed.String(), w.ResourcesUsed.String()
	case !bytes.Equal(w.Result, got.Result):
		e.Field, e.Got, e.Want = "result", fmt.Sprintf("%x", got.Result), fmt.Sprintf("%x", w.Result)
	case len(w.Events) != len(got.Events):
		e.Field, e.Got, e.Want = "event_count", fmt.Sprint(len(got.Events)), fmt.Sprint(len(w.Events))
	default:
		for i := range w.Events {
			if w.Events[i].Contract != got.Events[i].Contract || !bytes.Equal(w.Events[i].Payload, got.Events[i].Payload) {
				e.Field = fmt.Sprintf("events[%d]", i)
				e.Got = fmt.Sprintf("%s:%x", got.Events[i].Contract, got.Events[i].Payload)
				e.Want = fmt.Sprintf("%s:%x", w.Events[i].Contract, w.Events[i].Payload)
				break
			}
		}
	}
	return e
}

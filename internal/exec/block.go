// Package exec executes a block's transactions: the state transition
// Execute(parent_state, stable_block_context, ordered_transactions) of
// rules §7.3, with the block-capacity guard of §14.1 and §21.6.
//
// The same Builder serves validators, which require every transaction to
// fit, and miners, which leave out a transaction the block cannot hold.
package exec

import (
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
)

// ErrCapacity reports that a block's totals would exceed its active capacity.
var ErrCapacity = errors.New("block exceeds its active capacity")

// CapacityError is ErrCapacity for one transaction, with what it had used
// when execution stopped.
type CapacityError struct {
	Index int
	Used  types.ResourceVector
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("%v: transaction %d", ErrCapacity, e.Index)
}

func (e *CapacityError) Unwrap() error { return ErrCapacity }

// BlockFull exposes the execution-level error for errors.As.
func (e *CapacityError) As(target any) bool {
	if t, ok := target.(**vm.BlockFullError); ok {
		*t = &vm.BlockFullError{Used: e.Used}
		return true
	}
	return false
}

// Builder executes transactions one after another on top of a parent state.
type Builder struct {
	env      *vm.Env
	limits   types.ResourceVector
	used     types.ResourceVector
	receipts []*types.Receipt
	encoded  [][]byte
	txs      [][]byte

	// For templates: every attempt, and the compute of attempts the block
	// did not keep, bounded by discardLimit.
	attempts     []Attempt
	discarded    uint64
	discardLimit uint64
}

// Attempt records one transaction a template tried.
type Attempt struct {
	ID   hashing.Hash
	Used types.ResourceVector
	// Fit reports that the block kept it; Alone that the block held no
	// other transaction when it was tried.
	Fit, Alone bool
}

// LimitDiscarded bounds the compute a template may throw away on attempts
// it does not keep. The attempt that reaches the bound still finishes.
func (b *Builder) LimitDiscarded(c uint64) { b.discardLimit = c }

// Spent reports that the discard allowance is used up.
func (b *Builder) Spent() bool { return b.discardLimit > 0 && b.discarded >= b.discardLimit }

// Attempts lists every transaction Try ran, in order.
func (b *Builder) Attempts() []Attempt { return b.attempts }

// Try is Add for templates: it returns the receipt when the block keeps the
// transaction, or nil when the block cannot hold it (or the discard
// allowance is spent, in which case nothing runs).
func (b *Builder) Try(raw []byte, tx *types.Transaction) (*types.Receipt, error) {
	if b.Spent() {
		return nil, nil
	}
	alone := len(b.receipts) == 0
	id := types.TransactionID(b.env.Block.ChainID, raw)
	r, err := b.Add(raw, tx)
	if err != nil {
		var full *vm.BlockFullError
		if !errors.As(err, &full) {
			return nil, err
		}
		b.discarded += full.Used[types.C]
		b.attempts = append(b.attempts, Attempt{ID: id, Used: full.Used, Alone: alone})
		return nil, nil
	}
	b.attempts = append(b.attempts, Attempt{ID: id, Used: r.ResourcesUsed, Fit: true, Alone: alone})
	return r, nil
}

// NewBuilder starts a block with the given context, active capacity and
// producer data length. It fails when the block's own bytes do not fit.
func NewBuilder(parent state.Tree, ctx *vm.BlockContext, limits types.ResourceVector, codes *vm.CodeCache) (*Builder, error) {
	b := &Builder{
		env:    &vm.Env{Block: ctx, State: state.NewOverlay(parent), Codes: codes},
		limits: limits,
	}
	b.used[types.D] = uint64(types.BlockOverhead) + uint64(len(ctx.Producer))
	if b.used[types.D] > limits[types.D] {
		return nil, fmt.Errorf("%w: block data %d exceeds D capacity %d", ErrCapacity, b.used[types.D], limits[types.D])
	}
	return b, nil
}

// Used returns the block totals so far.
func (b *Builder) Used() types.ResourceVector { return b.used }

// Count returns the number of transactions added.
func (b *Builder) Count() int { return len(b.receipts) }

// Add executes one transaction. raw must be its canonical encoding and tx its
// decoded, statically valid envelope. When the block cannot hold what the
// transaction consumes, Add returns an error wrapping ErrCapacity and leaves
// the builder as it was.
func (b *Builder) Add(raw []byte, tx *types.Transaction) (*types.Receipt, error) {
	var guard types.ResourceVector
	for i := 0; i < 4; i++ {
		guard[i] = b.limits[i] - b.used[i]
	}
	r, err := vm.ExecuteTransaction(b.env, guard, raw, tx)
	if err != nil {
		var full *vm.BlockFullError
		if errors.As(err, &full) {
			return nil, &CapacityError{Index: len(b.receipts), Used: full.Used}
		}
		return nil, err
	}
	b.env.State.DiscardJournal()
	for i := 0; i < 4; i++ {
		b.used[i] += r.ResourcesUsed[i] // fits: execution never passes the guard
	}
	b.receipts = append(b.receipts, r)
	b.encoded = append(b.encoded, r.Encode())
	b.txs = append(b.txs, raw)
	return r, nil
}

// Result is a finished block execution.
type Result struct {
	Receipts     []*types.Receipt
	ReceiptBytes [][]byte
	Transactions [][]byte
	Used         types.ResourceVector
	State        state.Tree
}

// Finish commits the block's writes and returns the outcome.
func (b *Builder) Finish() *Result {
	return &Result{
		Receipts:     b.receipts,
		ReceiptBytes: b.encoded,
		Transactions: b.txs,
		Used:         b.used,
		State:        b.env.State.Commit(),
	}
}

// LastReceiptBytes returns the encoding of the most recent receipt.
func (b *Builder) LastReceiptBytes() []byte { return b.encoded[len(b.encoded)-1] }

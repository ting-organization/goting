// Package chain keeps the block tree, validates blocks against the rules,
// and follows the valid branch with the most accumulated work.
//
// It works headers first, as the reference node does (wiki: node/sync):
// a header is checked against its indexed parent and indexed before any
// body; a body is stored once it matches its header's commitments; and a
// branch is executed only when the node moves its tip to it, by the
// fork-choice rule. A branch with little work never costs execution, and a
// chain far below the tip's work is refused unless a pre-sync shows it
// reaches the threshold.
//
// Every executed block's state is an immutable state.Tree that shares
// unchanged subtrees with its parent's, so recent states are kept on their
// entries and switching between them is cheap. Older states are dropped,
// except periodic checkpoints on the canonical chain, and re-derived from
// stored blocks when needed.
package chain

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ting-organization/goting/internal/commit"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/exec"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
)

// Status of a block-tree entry.
type Status uint8

const (
	StatusHeader  Status = iota // the header is valid; the body is not stored
	StatusStored                // the body is stored and matches the header; not executed
	StatusValid                 // executed and valid
	StatusInvalid               // breaks a rule, or descends from a block that does
)

func (s Status) String() string {
	return [...]string{"header", "stored", "valid", "invalid"}[s]
}

// Entry is a node of the block tree.
type Entry struct {
	ID        hashing.Hash
	Header    *types.Header // nil for the genesis anchor
	Parent    *Entry
	Height    uint64
	Timestamp uint64
	Target    types.Target
	Votes     [4]uint8
	BlockWork *big.Int
	ChainWork *big.Int
	// Vote is the block's vote state; Vote.Limits its active capacity.
	Vote    consensus.VoteState
	Status  Status
	TxCount int
	Size    int

	children []*Entry
	complete bool        // has data, as do all ancestors down to an executed one
	state    *state.Tree // after this block; nil when not retained
	// changes are the block's net record writes, kept with a database while
	// the block is above the base: what flushing and rebasing replay.
	changes []state.Change
}

// HasData reports whether the block's body is stored.
func (e *Entry) HasData() bool { return e.Status == StatusStored || e.Status == StatusValid }

// State returns the retained state after this block, if any.
func (e *Entry) State() (state.Tree, bool) {
	if e.state == nil {
		return state.Tree{}, false
	}
	return *e.state, true
}

// Config tunes local, non-consensus behavior.
type Config struct {
	// RetainDepth is how many recent blocks keep their state in memory.
	RetainDepth uint64
	// CheckpointInterval keeps the state of every Nth canonical block.
	CheckpointInterval uint64
	// CodeCacheBytes bounds the compiled-code cache.
	CodeCacheBytes int64
	// MarksPath persists the operator's invalid marks (marks.json); empty
	// keeps them in memory only.
	MarksPath string
	// Clock returns the time for the clock checks; time.Now when nil.
	Clock func() time.Time
	// State keeps the state in a database (see stored.go). Without one the
	// whole state is in memory.
	State *db.DB
	// UndoWindow is how many blocks below the database's base keep undo
	// data.
	UndoWindow uint64
	// Fatal is told of a failure of the node's own storage, after which the
	// chain processes no more blocks.
	Fatal func(error)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{RetainDepth: 128, CheckpointInterval: 1000, CodeCacheBytes: 256 << 20, UndoWindow: DefaultUndoWindow}
}

// Event reports a tip change to subscribers.
type Event struct {
	NewTip    *Entry
	Connected []*Entry // blocks that became canonical, oldest first
	Removed   []*Entry // blocks that stopped being canonical, newest first
}

// Local policy (wiki: node/sync, node/storage).
const (
	// ClockSlack is how far ahead of the clock a block may be admitted.
	ClockSlack = 7200
	// ThresholdBlocks: a header chain needs work within this many of the
	// tip's blocks of the tip's work to be indexed without a pre-sync.
	ThresholdBlocks = 1440
)

// Errors that are policy or timing, not proof of invalid data.
var (
	ErrKnown         = errors.New("block already known")
	ErrUnknownParent = errors.New("unknown parent")
	ErrBehind        = errors.New("branch too far behind the tip")
	ErrAhead         = errors.New("timestamp ahead of the clock")
	// ErrOrphan is the former name of ErrUnknownParent.
	ErrOrphan = ErrUnknownParent
)

// SeedConformanceError reports that this node rejects a block the seed list
// pins. The rules make that the implementation's fault, not the block's: the
// block is not remembered as invalid, and a node should stop (rules §27.1).
type SeedConformanceError struct {
	Height uint64
	ID     hashing.Hash
	Err    error
}

func (e *SeedConformanceError) Error() string {
	return fmt.Sprintf("CONFORMANCE FAULT: this node rejects the seed block %s listed at height %d: %v",
		e.ID, e.Height, e.Err)
}

// SeedConformanceError deliberately has no Unwrap: the block is not
// invalid, so errors.Is(err, ErrInvalid) must not hold. The underlying rule
// failure (for example a *ReceiptMismatch) is in Err.

type flight struct {
	peer  string
	since time.Time
}

// Chain is the block tree of one network.
type Chain struct {
	genesis   *types.GenesisConfig
	chainID   hashing.Hash
	genesisID hashing.Hash
	seed      []hashing.Hash
	codes     *vm.CodeCache
	store     BlockStore
	cfg       Config

	processMu sync.Mutex // serializes block processing
	lastErr   error      // why connect last failed, and for which block
	lastErrID hashing.Hash

	mu        sync.RWMutex // guards everything below
	entries   map[hashing.Hash]*Entry
	invalid   map[hashing.Hash]string // rejected ids, indexed or not
	gen       *Entry
	tip       *Entry
	canonical []*Entry // by height
	heads     map[*Entry]struct{}
	best      *Entry   // the header with the most work that is not invalid
	bestPath  []*Entry // genesis .. best, by height
	frontier  int      // lowest height on bestPath without data
	inFlight  map[hashing.Hash]flight
	expired   map[hashing.Hash]flight
	presync   map[string]*presyncState
	retained  map[*Entry]struct{}
	subs      map[chan Event]struct{}
	hooks     []func(Event)
	// delivered names the peer each stored block not yet validated came
	// from; offenders are peers whose blocks then failed (wiki: node/sync).
	delivered map[hashing.Hash]string
	offenders map[string]struct{}
	marks     map[hashing.Hash]struct{} // the operator's invalid marks

	failedWork *big.Int // the most work of a branch that failed here
	reorg      *Reorganization

	base   *Entry // the block whose state the database holds; nil without one
	broken error  // a storage failure: nothing more is processed
	cursor Cursor // the block files' position up to which the database lists blocks
	// rewinds counts rewinds of the base: a state derived across one is
	// not kept.
	rewinds uint64
}

// NoSeed, passed as a seed list, makes a private chain with no seed heights
// even for a built-in network: for tests and local development chains.
var NoSeed = []hashing.Hash{}

// New creates a chain at its genesis anchor. seed lists the BlockIds at
// heights 1..N; it must come from the node's own configuration, never from a
// peer. A nil seed uses the network's built-in list, if any.
func New(g *types.GenesisConfig, seed []hashing.Hash, store BlockStore, cfg Config) (*Chain, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		store = NewMemStore()
	}
	if seed == nil {
		seed = BuiltinSeed(g)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	chainID := g.ChainID()
	gid := types.GenesisBlockID(chainID)
	empty := state.Empty
	gen := &Entry{
		ID:        gid,
		Timestamp: g.GenesisTimestamp,
		Target:    g.InitialTarget,
		BlockWork: new(big.Int),
		ChainWork: new(big.Int),
		Vote:      consensus.GenesisVoteState(g),
		Status:    StatusValid,
		complete:  true,
		state:     &empty,
	}
	c := &Chain{
		genesis:    g,
		chainID:    chainID,
		genesisID:  gid,
		seed:       seed,
		codes:      vm.NewCodeCache(cfg.CodeCacheBytes),
		store:      store,
		cfg:        cfg,
		entries:    map[hashing.Hash]*Entry{gid: gen},
		invalid:    make(map[hashing.Hash]string),
		gen:        gen,
		tip:        gen,
		canonical:  []*Entry{gen},
		heads:      map[*Entry]struct{}{gen: {}},
		best:       gen,
		bestPath:   []*Entry{gen},
		frontier:   1,
		inFlight:   map[hashing.Hash]flight{},
		expired:    map[hashing.Hash]flight{},
		presync:    map[string]*presyncState{},
		delivered:  map[hashing.Hash]string{},
		failedWork: new(big.Int),
		offenders:  map[string]struct{}{},
		retained:   map[*Entry]struct{}{gen: {}},
		subs:       map[chan Event]struct{}{},
	}
	if err := c.loadMarks(); err != nil {
		return nil, err
	}
	if cfg.State != nil {
		c.base = gen
		if c.cfg.UndoWindow == 0 {
			c.cfg.UndoWindow = DefaultUndoWindow
		}
	}
	return c, nil
}

// Genesis returns the network's configuration.
func (c *Chain) Genesis() *types.GenesisConfig { return c.genesis }

// ChainID returns the network's ChainId.
func (c *Chain) ChainID() hashing.Hash { return c.chainID }

// GenesisID returns the GenesisBlockId.
func (c *Chain) GenesisID() hashing.Hash { return c.genesisID }

// Codes returns the shared compiled-code cache.
func (c *Chain) Codes() *vm.CodeCache { return c.codes }

// Store returns the block store.
func (c *Chain) Store() BlockStore { return c.store }

// SeedLen returns the number of seed heights.
func (c *Chain) SeedLen() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.seed)
}

// Seed returns the pinned seed list.
func (c *Chain) Seed() []hashing.Hash {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.seed
}

// SetSeed pins the seed list after construction. It fails when a canonical
// block already contradicts the list.
func (c *Chain) SetSeed(list []hashing.Hash) error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for h := uint64(1); h < uint64(len(c.canonical)) && h <= uint64(len(list)); h++ {
		if c.canonical[h].ID != list[h-1] {
			return fmt.Errorf("canonical block at height %d is not the seed block", h)
		}
	}
	c.seed = append([]hashing.Hash(nil), list...)
	return nil
}

// Tip returns the canonical head.
func (c *Chain) Tip() *Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip
}

// BestHeader returns the indexed header with the most work that is not
// invalid; blocks are fetched along its branch.
func (c *Chain) BestHeader() *Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.best
}

// Get returns an entry by BlockId.
func (c *Chain) Get(id hashing.Hash) (*Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[id]
	return e, ok
}

// AtHeight returns the canonical entry at a height.
func (c *Chain) AtHeight(h uint64) (*Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h >= uint64(len(c.canonical)) {
		return nil, false
	}
	return c.canonical[h], true
}

// IsCanonical reports whether e is on the canonical chain.
func (c *Chain) IsCanonical(e *Entry) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isCanonical(e)
}

func (c *Chain) isCanonical(e *Entry) bool {
	return e.Height < uint64(len(c.canonical)) && c.canonical[e.Height] == e
}

// InvalidReason returns why a block was rejected, if it was.
func (c *Chain) InvalidReason(id hashing.Hash) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.invalid[id]
	return r, ok
}

// Subscribe returns a channel of tip changes and a function that ends the
// subscription. Slow subscribers miss events; state that must follow every
// change uses OnTip.
func (c *Chain) Subscribe() (<-chan Event, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan Event, 64)
	c.subs[ch] = struct{}{}
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

// OnTip registers fn to run for every tip change, synchronously and in
// order, before the call that changed the tip returns. fn must not process
// blocks.
func (c *Chain) OnTip(fn func(Event)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, fn)
}

// NextContext returns what a block extending parent must carry: its height,
// minimum timestamp (exclusive), target and active capacity.
func (c *Chain) NextContext(parent *Entry) (height uint64, median uint64, target types.Target, limits types.ResourceVector) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	height = parent.Height + 1
	median = c.medianTime(parent)
	target = consensus.NextTarget(c.genesis, height, parent.Timestamp)
	limits = c.voteStateFor(parent, height).Limits
	return
}

func (c *Chain) now() uint64 { return uint64(c.cfg.Clock().Unix()) }

// threshold is the work a header chain needs to be indexed: within
// ThresholdBlocks blocks of the tip's work. Called with c.mu held.
func (c *Chain) threshold() *big.Int {
	block := consensus.Work(c.tip.Target)
	window := new(big.Int).Mul(block, big.NewInt(ThresholdBlocks))
	if c.tip.ChainWork.Cmp(window) <= 0 {
		return new(big.Int)
	}
	return new(big.Int).Sub(c.tip.ChainWork, window)
}

// maxInvalid bounds the remembered rejections; past it the memory is
// cleared, which only costs re-checking a block that is sent again.
const maxInvalid = 100000

// remember records an id as invalid. Called with c.mu held.
func (c *Chain) remember(id hashing.Hash, err error) {
	if len(c.invalid) >= maxInvalid {
		c.invalid = make(map[hashing.Hash]string)
	}
	c.invalid[id] = err.Error()
}

func (c *Chain) reject(id hashing.Hash, err error) error {
	c.mu.Lock()
	c.remember(id, err)
	c.mu.Unlock()
	return err
}

// listedAs reports whether the seed lists exactly this block.
func (c *Chain) listedAs(h *types.Header, id hashing.Hash) bool {
	return h.Height >= 1 && h.Height <= uint64(len(c.seed)) && c.seed[h.Height-1] == id
}

// AcceptHeader checks a header against its indexed parent and indexes it.
// It returns ErrUnknownParent, ErrAhead, ErrBehind, a SeedConformanceError,
// or an error wrapping ErrInvalid.
func (c *Chain) AcceptHeader(h *types.Header) error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	id := h.ID()
	if _, ok := c.Get(id); ok {
		return nil
	}
	_, err := c.acceptHeader(h, id, false, false)
	return err
}

// acceptHeader indexes a header. recovered skips the clock and threshold
// checks for blocks this node stored before; noThreshold skips the threshold
// only, for a pre-synced chain. Called with processMu held.
func (c *Chain) acceptHeader(h *types.Header, id hashing.Hash, recovered, noThreshold bool) (*Entry, error) {
	c.mu.RLock()
	reason, bad := c.invalid[id]
	_, marked := c.marks[id]
	c.mu.RUnlock()
	if marked {
		return nil, ErrMarked
	}
	if bad && !c.listedAs(h, id) {
		return nil, &InvalidError{Height: h.Height, ID: id, Reason: "previously rejected: " + reason, Header: true}
	}
	if err := c.checkHeaderAlone(h, id); err != nil {
		return nil, c.fail(h, id, err)
	}
	c.mu.RLock()
	parent, ok := c.entries[h.ParentID]
	c.mu.RUnlock()
	if !ok {
		return nil, ErrUnknownParent
	}
	if parent.Status == StatusInvalid {
		c.mu.RLock()
		byOperator := c.byOperator(parent.ID)
		c.mu.RUnlock()
		if byOperator {
			return nil, ErrMarked
		}
		// A valid header on a failed branch: its work counts toward the
		// warning that this node may follow other rules.
		if _, err := c.checkHeader(parent, h, id); err == nil {
			c.mu.Lock()
			c.failedBranch(new(big.Int).Add(parent.ChainWork, consensus.Work(h.Target)))
			c.mu.Unlock()
		}
		return nil, c.fail(h, id, invalid(h, id, "its parent is invalid"))
	}
	vs, err := c.checkHeader(parent, h, id)
	if err != nil {
		return nil, c.fail(h, id, err)
	}
	if !recovered && h.Timestamp > c.now()+ClockSlack {
		return nil, ErrAhead
	}
	work := consensus.Work(h.Target)
	chainWork := new(big.Int).Add(parent.ChainWork, work)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !recovered && !noThreshold && chainWork.Cmp(c.threshold()) < 0 {
		return nil, ErrBehind
	}
	if e, ok := c.entries[id]; ok {
		return e, nil
	}
	e := &Entry{
		ID: id, Header: h, Parent: parent, Height: h.Height, Timestamp: h.Timestamp, Target: h.Target,
		Votes: h.Votes, BlockWork: work, ChainWork: chainWork, Vote: vs, Status: StatusHeader,
	}
	c.entries[id] = e
	parent.children = append(parent.children, e)
	if better(e, c.best) {
		c.setBest(e)
	}
	return e, nil
}

// fail classifies a header rule failure: a listed seed block is the node's
// own fault; any other invalid header is remembered. Called without c.mu.
func (c *Chain) fail(h *types.Header, id hashing.Hash, err error) error {
	var ie *InvalidError
	if errors.As(err, &ie) {
		ie.Header = true
	}
	if c.listedAs(h, id) && errors.Is(err, ErrInvalid) {
		return &SeedConformanceError{Height: h.Height, ID: id, Err: err}
	}
	if errors.Is(err, ErrInvalid) {
		return c.reject(id, err)
	}
	return err
}

// better orders headers by work, then by id.
func better(a, b *Entry) bool {
	if d := a.ChainWork.Cmp(b.ChainWork); d != 0 {
		return d > 0
	}
	return bytes.Compare(a.ID[:], b.ID[:]) > 0
}

// setBest makes e the best header and keeps the path to it and the download
// frontier. Called with c.mu held.
func (c *Chain) setBest(e *Entry) {
	c.best = e
	var path []*Entry
	n := e
	for n.Height >= uint64(len(c.bestPath)) || c.bestPath[n.Height] != n {
		path = append(path, n)
		n = n.Parent
	}
	c.bestPath = c.bestPath[:n.Height+1]
	for i := len(path) - 1; i >= 0; i-- {
		c.bestPath = append(c.bestPath, path[i])
	}
	if int(n.Height)+1 < c.frontier {
		c.frontier = int(n.Height) + 1
	}
	c.advanceFrontier()
}

func (c *Chain) advanceFrontier() {
	for c.frontier < len(c.bestPath) && c.bestPath[c.frontier].HasData() {
		c.frontier++
	}
}

// recomputeBest scans for the best header after invalidation. Called with
// c.mu held.
func (c *Chain) recomputeBest() {
	best := c.gen
	for _, e := range c.entries {
		if e.Status != StatusInvalid && better(e, best) {
			best = e
		}
	}
	c.bestPath = c.bestPath[:1]
	c.frontier = 1
	c.setBest(best)
}

// ProcessBlock accepts a block and moves the tip to the best branch it can
// connect. It returns the block's entry. Errors wrapping ErrInvalid mean the
// block breaks a rule; ErrUnknownParent, ErrAhead and ErrBehind are policy.
// A block on a branch with less work is stored without being executed.
func (c *Chain) ProcessBlock(raw []byte) (*Entry, error) {
	return c.ProcessBlockFrom(raw, "")
}

// maxDelivered bounds the blocks whose deliverers are remembered.
const maxDelivered = 1 << 16

// Offenders returns, and forgets, the peers whose delivered blocks failed
// validation since the last call.
func (c *Chain) Offenders() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.offenders))
	for p := range c.offenders {
		out = append(out, p)
	}
	clear(c.offenders)
	return out
}

// ProcessBlockFrom is ProcessBlock for a block a peer delivered: should the
// block fail validation later, as its branch is activated, the peer becomes
// an offender.
func (c *Chain) ProcessBlockFrom(raw []byte, peer string) (*Entry, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.broken != nil {
		return nil, c.broken
	}
	e, known, err := c.acceptBlock(raw, false)
	if err != nil {
		return e, err
	}
	if peer != "" && !known {
		c.mu.Lock()
		if e.Status == StatusStored && len(c.delivered) < maxDelivered {
			c.delivered[e.ID] = peer
		}
		c.mu.Unlock()
	}
	c.lastErr = nil
	if err := c.activate(); err != nil {
		return e, err
	}
	c.mu.RLock()
	status := e.Status
	reason := c.invalid[e.ID]
	c.mu.RUnlock()
	if status == StatusInvalid {
		var why error = &InvalidError{Height: e.Height, ID: e.ID, Reason: reason}
		if c.lastErr != nil && c.lastErrID == e.ID {
			why = c.lastErr
		}
		if c.listedAs(e.Header, e.ID) {
			return e, &SeedConformanceError{Height: e.Height, ID: e.ID, Err: why}
		}
		return e, why
	}
	if known {
		return e, ErrKnown
	}
	return e, nil
}

// acceptBlock indexes a block's header if needed and stores its body once
// it matches the header. Called with processMu held.
func (c *Chain) acceptBlock(raw []byte, recovered bool) (*Entry, bool, error) {
	if len(raw) < types.HeaderSize {
		return nil, false, &InvalidError{Reason: "truncated header"}
	}
	id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
	h, _ := types.DecodeHeader(raw[:types.HeaderSize])
	c.mu.RLock()
	e, indexed := c.entries[id]
	c.mu.RUnlock()
	switch {
	case indexed && e.Status == StatusInvalid:
		c.mu.RLock()
		byOperator := c.byOperator(id)
		c.mu.RUnlock()
		if byOperator {
			return e, false, ErrMarked
		}
		reason, _ := c.InvalidReason(id)
		return e, false, &InvalidError{Height: h.Height, ID: id, Reason: "it failed validation: " + reason}
	case indexed && e.HasData():
		return e, true, nil
	case indexed:
		if !recovered && h.Timestamp > c.now()+ClockSlack {
			return nil, false, ErrAhead
		}
	default:
		var err error
		if e, err = c.acceptHeader(h, id, recovered, false); err != nil {
			return nil, false, err
		}
	}
	if uint64(len(raw)) > e.Vote.Limits[types.D] {
		return e, false, mismatch(h, id, "block of %d bytes exceeds D capacity %d", len(raw), e.Vote.Limits[types.D])
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return e, false, mismatch(h, id, "%v", err)
	}
	if types.ProducerHash(body.ProducerData) != h.ProducerHash ||
		commit.TransactionsRoot(body.Transactions) != h.TransactionsRoot ||
		commit.ReceiptsRoot(body.Receipts) != h.ReceiptsRoot {
		return e, false, mismatch(h, id, "body commitment mismatch")
	}
	if err := c.store.Put(id, raw); err != nil {
		return e, false, fmt.Errorf("storing block: %w", err)
	}
	c.mu.Lock()
	if e.Status == StatusHeader {
		e.Status = StatusStored
	}
	e.TxCount = len(body.Transactions)
	e.Size = len(raw)
	delete(c.inFlight, id)
	c.markComplete(e)
	c.advanceFrontier()
	c.mu.Unlock()
	return e, false, nil
}

// markComplete records that e has data; it and every descendant with data
// become complete when its parent is. Called with c.mu held.
func (c *Chain) markComplete(e *Entry) {
	if e.Parent == nil || !e.Parent.complete || e.Status == StatusInvalid {
		return
	}
	stack := []*Entry{e}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.complete || !n.HasData() || n.Status == StatusInvalid {
			continue
		}
		n.complete = true
		delete(c.heads, n.Parent)
		c.heads[n] = struct{}{}
		for _, ch := range n.children {
			if ch.HasData() && ch.Status != StatusInvalid {
				stack = append(stack, ch)
			}
		}
	}
}

// markInvalid marks e and its descendants invalid and returns them. Called
// with c.mu held.
func (c *Chain) markInvalid(e *Entry, err error) []*Entry {
	c.remember(e.ID, err)
	var marked []*Entry
	stack := []*Entry{e}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n != e {
			c.remember(n.ID, fmt.Errorf("descends from invalid block %s", e.ID))
		}
		n.Status = StatusInvalid
		marked = append(marked, n)
		n.complete = false
		n.state = nil
		delete(c.retained, n)
		delete(c.heads, n)
		delete(c.inFlight, n.ID)
		stack = append(stack, n.children...)
	}
	// The parent is a head again unless another valid child continues it.
	p := e.Parent
	if p.complete && p.Status != StatusInvalid {
		alone := true
		for _, ch := range p.children {
			if ch.complete && ch.Status != StatusInvalid {
				alone = false
			}
		}
		if alone {
			c.heads[p] = struct{}{}
		}
	}
	if c.best.Status == StatusInvalid {
		c.recomputeBest()
	}
	return marked
}

// overtakes is the fork-choice margin (rules §8.2, reference policy): a
// branch that does not grow the start tip must exceed its work by more than
// half the candidate head's own block work.
func overtakes(head *Entry, startWork *big.Int) bool {
	margin := new(big.Int).Rsh(head.BlockWork, 1)
	margin.Add(margin, startWork)
	return head.ChainWork.Cmp(margin) > 0
}

// isAncestor reports whether a is an ancestor of (or equal to) b.
func isAncestor(a, b *Entry) bool {
	for b != nil && b.Height > a.Height {
		b = b.Parent
	}
	return b == a
}

// choose picks the branch to move toward: of the complete heads, most work
// first, the first that is the start tip, grows it with more work, or clears
// the margin against it. Called with c.mu held.
func (c *Chain) choose(start *Entry, startWork *big.Int) *Entry {
	heads := make([]*Entry, 0, len(c.heads))
	for h := range c.heads {
		heads = append(heads, h)
	}
	sort.Slice(heads, func(i, j int) bool { return better(heads[i], heads[j]) })
	for _, h := range heads {
		if h == start || overtakes(h, startWork) || (h.ChainWork.Cmp(startWork) > 0 && isAncestor(start, h)) {
			return h
		}
	}
	return nil
}

// activate moves the tip toward the best branch it can connect, executing
// the branch's blocks from the fork; a block that fails is marked invalid
// with its descendants, and the choice is made again. Called with processMu
// held.
func (c *Chain) activate() error {
	c.mu.RLock()
	start := c.tip
	startWork := new(big.Int).Set(start.ChainWork)
	c.mu.RUnlock()
	for {
		c.mu.RLock()
		head := c.choose(start, startWork)
		tip := c.tip
		c.mu.RUnlock()
		if head == nil || head == tip {
			return c.maybeFlush()
		}
		valid, err := c.connect(head)
		if err != nil {
			return err
		}
		if valid {
			c.publish(c.setTip(head))
		}
	}
}

// connect executes the blocks between the executed part of the tree and
// head. It reports whether head is valid; on a failure it marks the failing
// block invalid and returns false.
func (c *Chain) connect(head *Entry) (bool, error) {
	c.mu.RLock()
	var branch []*Entry
	for n := head; n.Status != StatusValid; n = n.Parent {
		branch = append(branch, n)
	}
	c.mu.RUnlock()
	if len(branch) == 0 {
		return true, nil
	}
	fork := branch[len(branch)-1].Parent
	st, err := c.stateAt(fork)
	if errors.Is(err, ErrBelowBase) {
		// The branch forks below the database's base: rewind it first.
		c.mu.RLock()
		to := commonAncestor(fork, c.base)
		c.mu.RUnlock()
		if err := c.rewind(to); err != nil {
			return false, c.storageFail(err)
		}
		st, err = c.stateAt(fork)
	}
	if err != nil {
		return false, err
	}
	for i := len(branch) - 1; i >= 0; i-- {
		b := branch[i]
		raw, err := c.store.Get(b.ID)
		if err != nil {
			return false, fmt.Errorf("reading stored block %s: %w", b.ID, err)
		}
		body, err := types.DecodeBody(raw[types.HeaderSize:])
		if err != nil {
			return false, fmt.Errorf("stored block %s: %w", b.ID, err)
		}
		res, err := c.executeBody(st, b.Header, b.ID, raw, body, b.Vote.Limits)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				return false, err
			}
			if c.listedAs(b.Header, b.ID) {
				// The seed's blocks are canonical: the fault is this node's.
				return false, &SeedConformanceError{Height: b.Height, ID: b.ID, Err: err}
			}
			c.mu.Lock()
			// Whoever delivered a block the failure invalidates sent
			// invalid data; the most work on it may warrant a warning.
			for _, n := range c.markInvalid(b, err) {
				if peer, ok := c.delivered[n.ID]; ok {
					c.offenders[peer] = struct{}{}
					delete(c.delivered, n.ID)
				}
				c.failedBranch(n.ChainWork)
			}
			c.mu.Unlock()
			c.lastErr, c.lastErrID = err, b.ID
			return false, nil
		}
		st = res.State
		s := st
		c.mu.Lock()
		b.Status = StatusValid
		b.state = &s
		if c.cfg.State != nil {
			b.changes = res.Changes
		}
		c.retained[b] = struct{}{}
		delete(c.delivered, b.ID)
		c.mu.Unlock()
	}
	return true, nil
}

// setTip makes e the canonical head and returns the event.
func (c *Chain) setTip(e *Entry) *Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	ev := &Event{NewTip: e}
	var connected []*Entry
	n := e
	for n.Height >= uint64(len(c.canonical)) || c.canonical[n.Height] != n {
		connected = append(connected, n)
		n = n.Parent
	}
	fork := n
	for h := uint64(len(c.canonical)) - 1; h > fork.Height; h-- {
		ev.Removed = append(ev.Removed, c.canonical[h])
	}
	c.canonical = c.canonical[:fork.Height+1]
	for i := len(connected) - 1; i >= 0; i-- {
		c.canonical = append(c.canonical, connected[i])
		ev.Connected = append(ev.Connected, connected[i])
	}
	c.tip = e
	c.pruneStates()
	if len(ev.Removed) > 0 {
		c.reorg = &Reorganization{Replaced: uint64(len(ev.Removed)), ForkHeight: fork.Height,
			Height: e.Height, Tip: e.ID, Time: uint64(c.now()), Deep: len(ev.Removed) >= DeepReorg}
	}
	return ev
}

// publish runs the hooks and notifies subscribers of a tip change.
func (c *Chain) publish(ev *Event) {
	c.mu.RLock()
	hooks := c.hooks
	subs := make([]chan Event, 0, len(c.subs))
	for ch := range c.subs {
		subs = append(subs, ch)
	}
	c.mu.RUnlock()
	for _, fn := range hooks {
		fn(*ev)
	}
	for _, ch := range subs {
		select {
		case ch <- *ev:
		default:
		}
	}
}

// pruneStates drops states far below the tip, keeping canonical
// checkpoints. Called with c.mu held.
func (c *Chain) pruneStates() {
	if c.cfg.State != nil {
		return // the states above the base stay until it moves (stored.go)
	}
	if c.cfg.RetainDepth == 0 || c.tip.Height <= c.cfg.RetainDepth {
		return
	}
	floor := c.tip.Height - c.cfg.RetainDepth
	for e := range c.retained {
		if e.Height >= floor || e == c.gen {
			continue
		}
		if c.isCanonical(e) && c.cfg.CheckpointInterval > 0 && e.Height%c.cfg.CheckpointInterval == 0 {
			continue
		}
		e.state = nil
		delete(c.retained, e)
	}
}

// StateAt returns the state after an executed block, re-deriving it if it
// is no longer retained.
func (c *Chain) StateAt(e *Entry) (state.Tree, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	return c.stateAt(e)
}

// stateAt re-executes stored blocks from the nearest ancestor that still
// has a state. Called with processMu held.
func (c *Chain) stateAt(e *Entry) (state.Tree, error) {
	c.mu.RLock()
	if e.Status != StatusValid {
		c.mu.RUnlock()
		return state.Tree{}, fmt.Errorf("block %s is not executed", e.ID)
	}
	if e.state != nil {
		s := *e.state
		c.mu.RUnlock()
		return s, nil
	}
	var path []*Entry
	n := e
	rewinds := c.rewinds
	for n.state == nil {
		if c.base != nil && n.Height <= c.base.Height {
			c.mu.RUnlock()
			return state.Tree{}, ErrBelowBase
		}
		path = append(path, n)
		n = n.Parent
	}
	st := *n.state
	c.mu.RUnlock()
	var changes []state.Change
	for i := len(path) - 1; i >= 0; i-- {
		b := path[i]
		raw, err := c.store.Get(b.ID)
		if err != nil {
			return state.Tree{}, fmt.Errorf("re-deriving state at height %d: %w", b.Height, err)
		}
		body, err := types.DecodeBody(raw[types.HeaderSize:])
		if err != nil {
			return state.Tree{}, fmt.Errorf("stored block %s: %w", b.ID, err)
		}
		res, err := c.executeBody(st, b.Header, b.ID, raw, body, b.Vote.Limits)
		if err != nil {
			return state.Tree{}, fmt.Errorf("re-executing stored block %s: %w", b.ID, err)
		}
		st, changes = res.State, res.Changes
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rewinds != rewinds || e.Status != StatusValid {
		return st, nil // the base moved back meanwhile: not kept
	}
	s := st
	e.state = &s
	if c.cfg.State != nil {
		e.changes = changes
	}
	c.retained[e] = struct{}{}
	return st, nil
}

// Receipts decodes a stored block's receipts.
func (c *Chain) Receipts(e *Entry) ([]*types.Receipt, error) {
	raw, err := c.store.Get(e.ID)
	if err != nil {
		return nil, err
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return nil, err
	}
	out := make([]*types.Receipt, len(body.Receipts))
	for i, rb := range body.Receipts {
		r, err := types.DecodeReceipt(rb)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// Block returns a stored block's bytes.
func (c *Chain) Block(id hashing.Hash) ([]byte, error) { return c.store.Get(id) }

// NewBuilder starts a block on top of parent for mining.
func (c *Chain) NewBuilder(parent *Entry, ctx *vm.BlockContext, limits types.ResourceVector) (*exec.Builder, error) {
	st, err := c.StateAt(parent)
	if err != nil {
		return nil, err
	}
	return exec.NewBuilder(st, ctx, limits, c.codes)
}

// Load re-reads every block in the store, as after a restart: each header
// is checked again and each body against its header; the best branch is
// then executed. Cached data never stands in for execution.
func (c *Chain) Load() (int, error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if c.cfg.State != nil {
		if err := c.restoreRows(); err != nil {
			return 0, err
		}
	}
	n, err := c.indexStored()
	if err != nil {
		return n, err
	}
	if c.cfg.State != nil {
		if err := c.loadBase(); err != nil {
			return n, err
		}
	}
	return n, c.activate()
}

// indexStored indexes the stored blocks the index does not hold. A block
// stored before its parent, as bodies arrive out of order, is tried again
// once the others are in, for as long as that stores more. Called with
// processMu held.
func (c *Chain) indexStored() (int, error) {
	n := 0
	var waiting [][]byte
	err := c.store.Each(func(id hashing.Hash, raw []byte) error {
		c.mu.RLock()
		e, indexed := c.entries[id]
		done := indexed && (e.HasData() || e.Status == StatusInvalid)
		c.mu.RUnlock()
		if done {
			return nil
		}
		_, _, err := c.acceptBlock(raw, true)
		switch {
		case err == nil:
			n++
		case errors.Is(err, ErrUnknownParent):
			waiting = append(waiting, raw)
		default:
			// Invalid, marked, or no longer acceptable: not part of any
			// branch. A block that is needed is fetched again.
		}
		return nil
	})
	if err != nil {
		return n, err
	}
	for progress := true; progress && len(waiting) > 0; {
		progress = false
		var left [][]byte
		for _, raw := range waiting {
			_, _, err := c.acceptBlock(raw, true)
			switch {
			case err == nil:
				n++
				progress = true
			case errors.Is(err, ErrUnknownParent):
				left = append(left, raw)
			}
		}
		waiting = left
	}
	return n, nil
}

// HeadersAfter returns up to max canonical headers following the highest
// locator entry on the canonical chain, or ErrNoCommonAncestor.
func (c *Chain) HeadersAfter(locator []hashing.Hash, max int) ([][]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	best := -1
	for _, id := range locator {
		e, ok := c.entries[id]
		if !ok || !c.isCanonical(e) {
			continue
		}
		if int(e.Height) > best {
			best = int(e.Height)
		}
	}
	if best < 0 {
		return nil, ErrNoCommonAncestor
	}
	var out [][]byte
	for h := best + 1; h < len(c.canonical) && len(out) < max; h++ {
		out = append(out, c.canonical[h].Header.Encode())
	}
	return out, nil
}

// ErrNoCommonAncestor answers a locator that names no canonical block.
var ErrNoCommonAncestor = errors.New("no common ancestor")

// SideTips returns up to n stored, complete heads that are not canonical,
// most work first.
func (c *Chain) SideTips(n int) []*Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*Entry
	for e := range c.heads {
		if !c.isCanonical(e) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return better(out[i], out[j]) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Locator lists BlockIds of the best header's branch from its tip back: the
// ten most recent, then at gaps that double each time, up to 23 in all;
// then up to eight stored side branches, most work first; then genesis.
func (c *Chain) Locator() []hashing.Hash {
	c.mu.RLock()
	var out []hashing.Hash
	h := int64(len(c.bestPath) - 1)
	step := int64(1)
	for h > 0 && len(out) < 23 {
		out = append(out, c.bestPath[h].ID)
		if len(out) >= 10 {
			step *= 2
		}
		h -= step
	}
	c.mu.RUnlock()
	for _, e := range c.SideTips(8) {
		out = append(out, e.ID)
	}
	return append(out, c.genesisID)
}

// ValidateNext validates raw as the block after parent, as a conformance
// vector states it, without adding it to the tree. Unlike ProcessBlock it
// never looks the parent up by the header's parent_id: a header naming
// another parent is invalid here.
func (c *Chain) ValidateNext(parent *Entry, raw []byte) error {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	if len(raw) < types.HeaderSize {
		return &InvalidError{Reason: fmt.Sprintf("block of %d bytes is shorter than a header", len(raw))}
	}
	id := types.HeaderIDFromBytes(raw[:types.HeaderSize])
	h, _ := types.DecodeHeader(raw[:types.HeaderSize])
	vs, err := c.checkHeader(parent, h, id)
	if err != nil {
		return err
	}
	if uint64(len(raw)) > vs.Limits[types.D] {
		return invalid(h, id, "block of %d bytes exceeds D capacity %d", len(raw), vs.Limits[types.D])
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return invalid(h, id, "%v", err)
	}
	st, err := c.stateAt(parent)
	if err != nil {
		return err
	}
	_, err = c.executeBody(st, h, id, raw, body, vs.Limits)
	return err
}

// CheckHeader checks the header a peer pushes a block under, without
// indexing it: whether the block is stored already, and otherwise the data
// capacity that bounds the block's size.
func (c *Chain) CheckHeader(h *types.Header) (known bool, max uint64, err error) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	id := h.ID()
	c.mu.RLock()
	e, ok := c.entries[id]
	marked := c.byOperator(id) || (!ok && c.byOperatorParent(h.ParentID))
	c.mu.RUnlock()
	if marked {
		return false, 0, ErrMarked
	}
	if ok && e.HasData() {
		return true, 0, nil
	}
	if ok && e.Status == StatusInvalid {
		reason, _ := c.InvalidReason(id)
		return false, 0, &InvalidError{Height: h.Height, ID: id, Reason: "it failed validation: " + reason, Header: true}
	}
	if ok {
		if h.Timestamp > c.now()+ClockSlack {
			return false, 0, ErrAhead
		}
		return false, e.Vote.Limits[types.D], nil
	}
	if err := c.checkHeaderAlone(h, id); err != nil {
		return false, 0, c.fail(h, id, err)
	}
	c.mu.RLock()
	parent, ok := c.entries[h.ParentID]
	c.mu.RUnlock()
	if !ok {
		return false, 0, ErrUnknownParent
	}
	if parent.Status == StatusInvalid {
		return false, 0, c.fail(h, id, invalid(h, id, "its parent is invalid"))
	}
	vs, err := c.checkHeader(parent, h, id)
	if err != nil {
		return false, 0, c.fail(h, id, err)
	}
	if h.Timestamp > c.now()+ClockSlack {
		return false, 0, ErrAhead
	}
	return false, vs.Limits[types.D], nil
}

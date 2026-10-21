package node

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/db"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/miner"
	"github.com/ting-organization/goting/internal/types"
)

// Config is a node's local configuration.
type Config struct {
	// Name is the operator's name for the node, shown in its status.
	Name     string
	Mine     bool
	Producer []byte
	Votes    [4]uint8
	Workers  int
	// Interval is the least time between two blocks this node mines, by
	// wall clock (the reference's miner spacing; 0 mines as fast as the
	// target allows).
	Interval time.Duration
	// Pool bounds: transactions and MiB (the reference's defaults when 0).
	PoolTransactions int
	PoolMiB          int
	// DeferredPath persists the deferred set (deferred.json); empty: none.
	DeferredPath string
	// SnapshotPath, when set, receives a state snapshot every SnapshotEvery
	// blocks, taken SnapshotDepth blocks below the tip.
	SnapshotPath  string
	SnapshotEvery uint64
	SnapshotDepth uint64
}

// Location is one occurrence of a transaction in the canonical chain.
type Location struct {
	Height uint64       `json:"height"`
	Block  hashing.Hash `json:"block"`
	Index  int          `json:"index"`
	Status uint8        `json:"status"`
}

// Node runs a chain with a transaction pool, a deferred set and an optional
// miner.
type Node struct {
	Chain *chain.Chain
	cfg   Config
	log   *slog.Logger
	tx    *txPool

	mu     sync.RWMutex
	occ    map[hashing.Hash][]Location   // canonical occurrences by TransactionId
	events map[hashing.Hash][]EventPlace // canonical events by emitting program
	// With a database, the indexes above hold only the blocks above its
	// base, which it lists itself; indexed records what each block added.
	indexed map[hashing.Hash]indexedBlock
	pruned  uint64 // the base height up to which the indexes are pruned
	// started is when the node started, for its status.
	started time.Time

	tipChanged chan struct{}
	snapMu     sync.Mutex
	peers      *Peers

	// Templates kept for solutions; guarded by tx.mu.
	templates []*miner.Template

	// The miner's settings and the built-in miner, if running.
	minerMu  sync.Mutex
	producer []byte
	votes    [4]uint8
	running  *builtinMiner
	runCtx   atomic.Pointer[context.Context]

	// Fatal, when set, is called with faults that must stop the node.
	Fatal func(error)
}

// New creates a node around a chain and follows its tip changes.
func New(c *chain.Chain, cfg Config, log *slog.Logger) *Node {
	if log == nil {
		log = slog.Default()
	}
	n := &Node{Chain: c, cfg: cfg, log: log, tx: newTxPool(cfg.PoolTransactions, cfg.PoolMiB, cfg.DeferredPath),
		occ: make(map[hashing.Hash][]Location), events: make(map[hashing.Hash][]EventPlace),
		indexed:    map[hashing.Hash]indexedBlock{},
		tipChanged: make(chan struct{}, 1), started: time.Now(), producer: cfg.Producer, votes: cfg.Votes}
	c.OnTip(n.published)
	return n
}

// IndexCanonical indexes every canonical block, as after loading, then
// restores the deferred set.
func (n *Node) IndexCanonical() {
	tip := n.Chain.Tip()
	from := uint64(1)
	if b := n.Chain.Base(); b != nil {
		from, n.pruned = b.Height+1, b.Height
	}
	for h := from; h <= tip.Height; h++ {
		if e, ok := n.Chain.AtHeight(h); ok {
			n.index(e, true)
		}
	}
	n.loadDeferred()
}

type indexedBlock struct {
	height    uint64
	txs       []hashing.Hash
	contracts []hashing.Hash
}

// pruneIndex drops from the in-memory indexes the blocks the database now
// lists: those at or below its base.
func (n *Node) pruneIndex() {
	b := n.Chain.Base()
	if b == nil || b.Height <= n.pruned {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, ib := range n.indexed {
		if ib.height <= b.Height {
			n.unindexLocked(id, ib)
		}
	}
	n.pruned = b.Height
}

func (n *Node) unindexLocked(block hashing.Hash, ib indexedBlock) {
	for _, c := range ib.contracts {
		kept := n.events[c][:0]
		for _, p := range n.events[c] {
			if p.Block != block {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(n.events, c)
		} else {
			n.events[c] = kept
		}
	}
	for _, id := range ib.txs {
		locs := n.occ[id][:0]
		for _, l := range n.occ[id] {
			if l.Block != block {
				locs = append(locs, l)
			}
		}
		if len(locs) == 0 {
			delete(n.occ, id)
		} else {
			n.occ[id] = locs
		}
	}
	delete(n.indexed, block)
}

// EventPlace is one canonical event of a program: its block, the
// transaction's place in the block, the event's among that transaction's
// events, and its topic, the first 32 bytes of its payload, if it has one.
type EventPlace struct {
	Height   uint64
	Block    hashing.Hash
	Tx       int
	Event    int
	Topic    hashing.Hash
	HasTopic bool
}

// txIDs returns a block's TransactionIds, receipt statuses and events.
func (n *Node) txIDs(e *chain.Entry) ([]hashing.Hash, []uint8, map[hashing.Hash][]EventPlace) {
	raw, err := n.Chain.Block(e.ID)
	if err != nil {
		return nil, nil, nil
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return nil, nil, nil
	}
	ids := make([]hashing.Hash, len(body.Transactions))
	sts := make([]uint8, len(body.Transactions))
	events := map[hashing.Hash][]EventPlace{}
	for i, t := range body.Transactions {
		ids[i] = types.TransactionID(n.Chain.ChainID(), t)
		r, err := types.DecodeReceipt(body.Receipts[i])
		if err != nil {
			continue
		}
		sts[i] = r.Status
		for j, ev := range r.Events {
			p := EventPlace{Height: e.Height, Block: e.ID, Tx: i, Event: j}
			if len(ev.Payload) >= 32 {
				p.Topic, p.HasTopic = hashing.Hash(ev.Payload[:32]), true
			}
			events[ev.Contract] = append(events[ev.Contract], p)
		}
	}
	return ids, sts, events
}

func (n *Node) index(e *chain.Entry, add bool) ([]hashing.Hash, []uint8) {
	ids, sts, events := n.txIDs(e)
	n.mu.Lock()
	defer n.mu.Unlock()
	if !add {
		if ib, ok := n.indexed[e.ID]; ok {
			n.unindexLocked(e.ID, ib)
		}
		return ids, sts
	}
	if _, ok := n.indexed[e.ID]; ok {
		return ids, sts
	}
	ib := indexedBlock{height: e.Height, txs: ids}
	for c, places := range events {
		n.events[c] = append(n.events[c], places...)
		ib.contracts = append(ib.contracts, c)
	}
	for i, id := range ids {
		n.occ[id] = append(n.occ[id], Location{Height: e.Height, Block: e.ID, Index: i, Status: sts[i]})
	}
	n.indexed[e.ID] = ib
	return ids, sts
}

// EventsOf returns the canonical blocks, from height from on, holding
// events of a program, or of one of its topics: at most limit blocks, each
// with the events' places, and whether more follow. With a database, the
// blocks it lists come from it.
func (n *Node) EventsOf(contract hashing.Hash, topic *hashing.Hash, from uint64, limit int) ([][]EventPlace, bool) {
	var groups [][]EventPlace
	add := func(p EventPlace) {
		if len(groups) > 0 && groups[len(groups)-1][0].Block == p.Block {
			groups[len(groups)-1] = append(groups[len(groups)-1], p)
			return
		}
		groups = append(groups, []EventPlace{p})
	}
	n.mu.RLock()
	all := n.events[contract]
	i := sort.Search(len(all), func(i int) bool { return all[i].Height >= from })
	for ; i < len(all) && len(groups) <= limit; i++ {
		if p := all[i]; topic == nil || (p.HasTopic && p.Topic == *topic) {
			add(p)
		}
	}
	n.mu.RUnlock()
	mem := groups
	if d := n.Chain.Database(); d != nil {
		inMemory := map[hashing.Hash]bool{}
		for _, g := range mem {
			inMemory[g[0].Block] = true
		}
		groups = nil
		err := d.Events(contract, topic, from, func(r db.EventRow) bool {
			if inMemory[r.Block] || !n.canonical(r.Height, r.Block) {
				return true
			}
			p := EventPlace{Height: r.Height, Block: r.Block, Tx: int(r.Tx), Event: int(r.Event)}
			if r.Topic != nil {
				p.Topic, p.HasTopic = *r.Topic, true
			}
			add(p)
			return len(groups) <= limit
		})
		if err != nil {
			n.log.Error("event index", "err", err)
		}
		groups = append(groups, mem...)
		sort.SliceStable(groups, func(i, j int) bool { return groups[i][0].Height < groups[j][0].Height })
	}
	if len(groups) > limit {
		return groups[:limit], true
	}
	return groups, false
}

// canonical reports whether the block at height is the canonical one.
func (n *Node) canonical(height uint64, block hashing.Hash) bool {
	e, ok := n.Chain.AtHeight(height)
	return ok && e.ID == block
}

// PeerInfo lists peer connections.
func (n *Node) PeerInfo() []PeerInfo {
	if n.peers == nil {
		return []PeerInfo{}
	}
	return n.peers.Info()
}

// recentlyIncluded reports whether one of the last RelayRecent canonical
// blocks included the transaction successfully, or one of the last
// RelayEcho included it at all.
func (n *Node) recentlyIncluded(id hashing.Hash) bool {
	tip := n.Chain.Tip().Height
	for _, l := range n.Occurrences(id) {
		if l.Height+RelayRecent > tip && l.Status == types.StatusSuccess {
			return true
		}
		if l.Height+RelayEcho > tip {
			return true
		}
	}
	return false
}

// succeededAbove reports a successful canonical inclusion above height.
func (n *Node) succeededAbove(id hashing.Hash, height uint64) bool {
	for _, l := range n.Occurrences(id) {
		if l.Height > height && l.Status == types.StatusSuccess {
			return true
		}
	}
	return false
}

// Occurrences returns where a transaction appears in the canonical chain,
// lowest first.
func (n *Node) Occurrences(id hashing.Hash) []Location {
	n.mu.RLock()
	out := append([]Location(nil), n.occ[id]...)
	n.mu.RUnlock()
	if d := n.Chain.Database(); d != nil {
		rows, err := d.Occurrences(id)
		if err != nil {
			n.log.Error("occurrence index", "err", err)
		}
		seen := map[hashing.Hash]bool{}
		for _, l := range out {
			seen[l.Block] = true
		}
		for _, r := range rows {
			if !seen[r.Block] && n.canonical(r.Height, r.Block) {
				out = append(out, Location{Height: r.Height, Block: r.Block, Index: int(r.Index), Status: r.Status})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Height != out[j].Height {
				return out[i].Height < out[j].Height
			}
			return out[i].Index < out[j].Index
		})
	}
	return out
}

// SubmitTransaction offers a transaction as the node's own.
func (n *Node) SubmitTransaction(raw []byte) (hashing.Hash, error) {
	r, err := n.Submit(raw, nil, netip.Addr{}, false)
	if err != nil {
		return hashing.Hash{}, err
	}
	return r.ID, nil
}

// PoolLen returns the number of pooled transactions.
func (n *Node) PoolLen() int {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	return len(n.tx.items)
}

// PoolBytes returns the pooled transactions' size.
func (n *Node) PoolBytes() int {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	return n.tx.bytes
}

// DeferredLen returns the number of deferred transactions.
func (n *Node) DeferredLen() int {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	return len(n.tx.deferred)
}

// HasTx reports whether a transaction is pooled or deferred.
func (n *Node) HasTx(id hashing.Hash) bool {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	_, a := n.tx.byID[id]
	_, b := n.tx.deferred[id]
	return a || b
}

// GetTx returns a pooled or deferred transaction.
func (n *Node) GetTx(id hashing.Hash) (raw []byte, deferred *Deferred, pooled bool) {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	if d, ok := n.tx.deferred[id]; ok {
		c := *d
		deferred = &c
		raw = d.Raw
	}
	if it, ok := n.tx.byID[id]; ok {
		raw, pooled = it.Raw, true
	}
	return raw, deferred, pooled
}

// TxIDs returns ids in id order after after, at most limit, and whether more
// follow: pooled ones leaving out deferred entries, or the deferred set.
func (n *Node) TxIDs(deferred bool, after *hashing.Hash, limit int) ([]hashing.Hash, bool) {
	n.tx.mu.Lock()
	var all []hashing.Hash
	if deferred {
		for id := range n.tx.deferred {
			all = append(all, id)
		}
	} else {
		for id := range n.tx.byID {
			if _, d := n.tx.deferred[id]; !d {
				all = append(all, id)
			}
		}
	}
	n.tx.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return lessID(all[i], all[j]) })
	start := 0
	if after != nil {
		start = sort.Search(len(all), func(i int) bool { return lessID(*after, all[i]) })
	}
	end := min(start+limit, len(all))
	return all[start:end], end < len(all)
}

func lessID(a, b hashing.Hash) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// PooledItems lists the pool in arrival order (for the API).
func (n *Node) PooledItems(limit int) []*Pooled {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	out := make([]*Pooled, 0, min(limit, len(n.tx.items)))
	for _, it := range n.tx.items {
		if len(out) == limit {
			break
		}
		c := *it
		out = append(out, &c)
	}
	return out
}

// DeferredItems lists the deferred set.
func (n *Node) DeferredItems() []*Deferred {
	n.tx.mu.Lock()
	defer n.tx.mu.Unlock()
	out := make([]*Deferred, 0, len(n.tx.deferred))
	for _, d := range n.tx.deferred {
		c := *d
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return lessID(out[i].ID, out[j].ID) })
	return out
}

// SubmitBlock validates and connects a block. When this node rejects a block
// the seed list pins, the fault is its own (rules §27.1): it reports the
// fault and stops.
func (n *Node) SubmitBlock(raw []byte) (*chain.Entry, error) {
	return n.SubmitBlockFrom(raw, "")
}

// SubmitBlockFrom is SubmitBlock for a block a peer pushed.
func (n *Node) SubmitBlockFrom(raw []byte, peer string) (*chain.Entry, error) {
	e, err := n.Chain.ProcessBlockFrom(raw, peer)
	var sc *chain.SeedConformanceError
	if errors.As(err, &sc) {
		n.log.Error("stopping: seed conformance fault", "err", sc)
		if n.Fatal != nil {
			n.Fatal(sc)
		}
	}
	return e, err
}

// Run keeps the pool moving, logs tips and mines when configured, until
// ctx ends.
func (n *Node) Run(ctx context.Context) {
	events, unsubscribe := n.Chain.Subscribe()
	defer unsubscribe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				n.flushDeferred()
				return
			case ev := <-events:
				n.onTip(ev)
			case <-t.C:
				if err := n.tick(); err != nil {
					n.log.Error("pool tick", "err", err)
				}
			}
		}
	}()
	n.runCtx.Store(&ctx)
	if n.cfg.Mine {
		n.StartMiner(n.cfg.Workers, n.cfg.Interval)
	}
	<-ctx.Done()
	n.StopMiner()
	wg.Wait()
}

// onTip logs a tip change and schedules snapshots; the index and pool
// already followed it synchronously.
func (n *Node) onTip(ev chain.Event) {
	if n.cfg.SnapshotPath != "" && n.cfg.SnapshotEvery > 0 && ev.NewTip.Height%n.cfg.SnapshotEvery == 0 {
		go n.snapshot()
	}
	txs := 0
	for _, e := range ev.Connected {
		txs += e.TxCount
	}
	level := slog.LevelDebug
	if txs > 0 || len(ev.Removed) > 0 || ev.NewTip.Height%1000 == 0 {
		level = slog.LevelInfo
	}
	n.log.Log(context.Background(), level, "tip", "height", ev.NewTip.Height, "id", ev.NewTip.ID.String(),
		"txs", txs, "reorg", len(ev.Removed) > 0)
	select {
	case n.tipChanged <- struct{}{}:
	default:
	}
}

func (n *Node) snapshot() {
	if !n.snapMu.TryLock() {
		return // one is being written; the next interval will catch up
	}
	defer n.snapMu.Unlock()
	info, ok, err := n.Chain.WriteSnapshot(n.cfg.SnapshotPath, n.cfg.SnapshotDepth)
	if err != nil {
		n.log.Error("snapshot", "err", err)
		return
	}
	if ok {
		n.log.Info("snapshot written", "height", info.Height, "records", info.Count, "root", info.Root.String())
	}
}

// Template builds a candidate block on the tip: the pool's transactions in
// first-in turns, at most about one block's compute thrown away on those
// the block does not keep; a transaction that does not fit even an empty
// block leaves the pool and the deferred set. explicit, when set, names the
// transactions to include, in order. The template is kept for Solution.
func (n *Node) Template(explicit []hashing.Hash) (*miner.Template, error) {
	if err := n.tick(); err != nil {
		return nil, err
	}
	parent := n.Chain.Tip()
	b, ctx, err := n.candidate(parent)
	if err != nil {
		return nil, err
	}
	_, _, target, limits := n.Chain.NextContext(parent)
	p := n.tx
	current := now()
	if explicit != nil {
		for _, id := range explicit {
			p.mu.Lock()
			it, ok := p.byID[id]
			p.mu.Unlock()
			if !ok {
				return nil, errors.New("transaction " + id.String() + " is not in the pool")
			}
			r, err := b.Try(it.Raw, it.Tx)
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, errors.New("transaction " + id.String() + " does not fit the block")
			}
		}
	} else {
		b.LimitDiscarded(limits[types.C])
		p.mu.Lock()
		p.turn++
		turn := p.turn
		inPool := map[netip.Addr]bool{}
		for _, it := range p.items {
			inPool[it.Source] = true
		}
		for s := range p.sources {
			if !inPool[s] {
				delete(p.sources, s)
			}
		}
		pool := append([]*Pooled(nil), p.items...)
		order := turns(pool, p.sources)
		p.mu.Unlock()
		for _, at := range order {
			if b.Spent() {
				break
			}
			p.mu.Lock()
			p.sources[pool[at].Source] = turn
			p.mu.Unlock()
			if _, err := b.Try(pool[at].Raw, pool[at].Tx); err != nil {
				return nil, err
			}
		}
	}
	tpl, err := miner.Assemble(n.Chain.ChainID(), ctx, target, n.Votes(), b)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	removed := false
	for _, a := range b.Attempts() {
		if !a.Fit && a.Alone {
			p.remove(a.ID)
			if _, ok := p.deferred[a.ID]; ok {
				delete(p.deferred, a.ID)
				p.markDirty(current)
			}
			removed = true
		} else if it, ok := p.byID[a.ID]; ok {
			it.Tried = current
		}
	}
	n.templates = append(n.templates, tpl)
	if len(n.templates) > templatesKept {
		n.templates = n.templates[len(n.templates)-templatesKept:]
	}
	p.mu.Unlock()
	if removed {
		p.notify()
	}
	return tpl, nil
}

// TemplateID is the id of a template's nonce-less header.
func TemplateID(t *miner.Template) hashing.Hash {
	h := *t.Header
	h.Nonce = types.Nonce{}
	return h.ID()
}

// ErrExpiredTemplate answers a solution for a template no longer kept.
var ErrExpiredTemplate = errors.New("unknown or expired template")

// Solution sets a nonce on a kept template and imports the block.
func (n *Node) Solution(id hashing.Hash, nonce types.Nonce) (*chain.Entry, error) {
	n.tx.mu.Lock()
	var tpl *miner.Template
	for _, t := range n.templates {
		if TemplateID(t) == id {
			tpl = t
		}
	}
	n.tx.mu.Unlock()
	if tpl == nil {
		return nil, ErrExpiredTemplate
	}
	h := *tpl.Header
	h.Nonce = nonce
	if !consensus.ValidPoW(h.ID(), h.Target) {
		return nil, errors.New("solution does not meet the target")
	}
	blk := &types.Block{Header: &h, Body: tpl.Body}
	return n.SubmitBlock(blk.Encode())
}

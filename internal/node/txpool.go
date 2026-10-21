package node

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/exec"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
	"github.com/ting-organization/goting/internal/vm"
)

// The reference node's transaction policy (wiki: node/mining). None of it is
// consensus: it decides what this node offers its miner and its peers.
const (
	// PoolAge is how long a transaction stays pooled without being included.
	PoolAge = 86_400
	// DeferredMax bounds the deferred set, DeferredPerSource the entries
	// counting against one relaying address.
	DeferredMax       = 4096
	DeferredPerSource = 256
	// DeferredHorizon is how far ahead not_before may lie; DeferredGrace how
	// long an entry stays after its time without being included.
	DeferredHorizon = 366 * 86_400
	DeferredGrace   = 30 * 86_400
	deferredRetry   = 30
	deferredPass    = 2
	deferredSave    = 5
	// RelayRecent and RelayEcho: a relayed transaction is refused when one
	// of the last RelayRecent canonical blocks included it successfully, or
	// one of the last RelayEcho included it at all.
	RelayRecent   = 1440
	RelayEcho     = 10
	templatesKept = 8
	// Defaults of the reference configuration.
	DefaultPoolTransactions = 1024
	DefaultPoolMiB          = 16
)

// Refusals, with the reference node's texts.
var (
	ErrPoolFull     = errors.New("mempool is full")
	ErrDeferredFull = errors.New("deferred set is full")
	ErrHorizon      = errors.New("not_before is more than a year ahead")
	ErrTooLarge     = errors.New("transaction larger than the data capacity")
	ErrIncluded     = errors.New("transaction already included")
)

const doesNotFit = "does not fit a block"

// Pooled is a transaction waiting for a block.
type Pooled struct {
	ID      hashing.Hash
	Raw     []byte
	Tx      *types.Transaction
	Arrived uint64
	// Source is the address it counts against: the relaying peer or the
	// public REST client. Invalid for the node's own submissions.
	Source netip.Addr
	// Tried is when a template last ran it, 0 if none has.
	Tried uint64
}

// Deferred is a transaction held until not_before, then admitted to the pool
// once it executes successfully on the tip. It stays in the set until a
// canonical block above Since includes it successfully, or it expires.
type Deferred struct {
	ID          hashing.Hash
	Raw         []byte
	Tx          *types.Transaction
	NotBefore   uint64
	Expires     uint64
	Since       uint64
	LastAttempt uint64
	LastTip     hashing.Hash
	LastResult  string
	Admitted    bool
	Source      netip.Addr
}

type takenSet struct {
	height uint64
	items  []*Deferred
}

// txPool holds the pool and the deferred set. Its lock is never held while
// a transaction executes: execution needs the chain's processing lock, which
// the chain holds while it tells the pool about new blocks.
type txPool struct {
	mu       sync.Mutex
	items    []*Pooled // arrival order
	byID     map[hashing.Hash]*Pooled
	bytes    int
	deferred map[hashing.Hash]*Deferred
	taken    map[hashing.Hash]*takenSet // by the block that included them

	maxCount int
	maxMiB   int

	// FirstIn selection state.
	turn    uint64
	sources map[netip.Addr]uint64

	deferredNext uint64
	deferredTip  hashing.Hash
	dirtySince   uint64
	path         string // deferred.json, or empty

	changed func()
}

func newTxPool(maxCount, maxMiB int, path string) *txPool {
	if maxCount <= 0 {
		maxCount = DefaultPoolTransactions
	}
	if maxMiB <= 0 {
		maxMiB = DefaultPoolMiB
	}
	return &txPool{byID: map[hashing.Hash]*Pooled{}, deferred: map[hashing.Hash]*Deferred{},
		taken: map[hashing.Hash]*takenSet{}, sources: map[netip.Addr]uint64{},
		maxCount: maxCount, maxMiB: maxMiB, path: path}
}

func now() uint64 { return uint64(time.Now().Unix()) }

func (p *txPool) notify() {
	if p.changed != nil {
		p.changed()
	}
}

func (p *txPool) push(it *Pooled) {
	p.items = append(p.items, it)
	p.byID[it.ID] = it
	p.bytes += len(it.Raw)
}

func (p *txPool) removeAt(i int) {
	it := p.items[i]
	p.items = append(p.items[:i], p.items[i+1:]...)
	delete(p.byID, it.ID)
	p.bytes -= len(it.Raw)
}

func (p *txPool) remove(id hashing.Hash) {
	if _, ok := p.byID[id]; !ok {
		return
	}
	for i, it := range p.items {
		if it.ID == id {
			p.removeAt(i)
			return
		}
	}
}

func (p *txPool) retain(keep func(*Pooled) bool) {
	out := p.items[:0]
	p.bytes = 0
	for _, it := range p.items {
		if keep(it) {
			out = append(out, it)
			p.bytes += len(it.Raw)
		} else {
			delete(p.byID, it.ID)
		}
	}
	for i := len(out); i < len(p.items); i++ {
		p.items[i] = nil
	}
	p.items = out
}

// heldBytes is the most bytes the pool holds, and the deferred set as well:
// the configured MiB, or four blocks of data at the next block's capacity
// when that is more.
func (p *txPool) heldBytes(dataCapacity uint64) int {
	configured := uint64(p.maxMiB) << 20
	if four := dataCapacity * 4; four > configured {
		return int(min(four, 1<<62))
	}
	return int(configured)
}

func (p *txPool) fits(n, most int) bool {
	return len(p.items) < p.maxCount && p.bytes+n <= most
}

// fairShare picks the transaction that gives way in a full pool: the most
// recent of the address holding the most, if that address holds more than
// the newcomer's. The node's own transactions never give way.
func fairShare(pool []*Pooled, incoming netip.Addr) int {
	held := map[netip.Addr]int{}
	for _, it := range pool {
		if it.Source.IsValid() {
			held[it.Source]++
		}
	}
	if len(held) == 0 {
		return -1
	}
	addrs := make([]netip.Addr, 0, len(held))
	for a := range held {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
	var largest netip.Addr
	count := -1
	for _, a := range addrs { // the last of equals, as the reference's max_by_key
		if held[a] >= count {
			largest, count = a, held[a]
		}
	}
	own := 0
	if incoming.IsValid() {
		own = held[incoming]
	}
	if count <= own {
		return -1
	}
	for i := len(pool) - 1; i >= 0; i-- {
		if pool[i].Source == largest {
			return i
		}
	}
	return -1
}

// makeRoom makes room for n bytes from source, evicting by fair share.
func (p *txPool) makeRoom(n int, source netip.Addr, most int) bool {
	if n > most {
		return false
	}
	for !p.fits(n, most) {
		at := fairShare(p.items, source)
		if at < 0 {
			return false
		}
		p.removeAt(at)
	}
	return true
}

// turns orders the pool for a template: transactions no template ran first,
// then those run longest ago; among equals the first of each address before
// the second of any, the addresses tried longest ago first, then arrival.
func turns(pool []*Pooled, sources map[netip.Addr]uint64) []int {
	type key struct {
		tried uint64
		rank  int
		last  uint64
		at    int
	}
	seen := map[netip.Addr]int{}
	keys := make([]key, len(pool))
	for i, it := range pool {
		seen[it.Source]++
		keys[i] = key{it.Tried, seen[it.Source], sources[it.Source], i}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.tried != b.tried {
			return a.tried < b.tried
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.last != b.last {
			return a.last < b.last
		}
		return a.at < b.at
	})
	out := make([]int, len(keys))
	for i, k := range keys {
		out[i] = k.at
	}
	return out
}

// SubmitResult is the answer to a submission.
type SubmitResult struct {
	ID        hashing.Hash `json:"id"`
	Known     bool         `json:"known"`
	Deferred  *bool        `json:"deferred,omitempty"`
	NotBefore *uint64      `json:"not_before,omitempty"`
	Expires   *uint64      `json:"expires,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// NextLimits is the active capacity of the block after the tip.
func (n *Node) NextLimits() types.ResourceVector { return n.nextLimits() }

// nextLimits is the active capacity of the block after the tip.
func (n *Node) nextLimits() types.ResourceVector {
	_, _, _, limits := n.Chain.NextContext(n.Chain.Tip())
	return limits
}

// Submit offers a transaction: to the pool, or with notBefore to the
// deferred set. source is the relaying peer's or public client's address
// (invalid for the node's own); relayed submissions are refused when a
// recent block included them.
func (n *Node) Submit(raw []byte, notBefore *uint64, source netip.Addr, relayed bool) (*SubmitResult, error) {
	limits := n.nextLimits()
	if uint64(len(raw)) > limits[types.D] {
		return nil, ErrTooLarge
	}
	tx, err := types.DecodeTransaction(raw)
	if err != nil {
		return nil, err
	}
	id := types.TransactionID(n.Chain.ChainID(), raw)
	p := n.tx
	p.mu.Lock()
	_, pooled := p.byID[id]
	p.mu.Unlock()
	if relayed && !pooled && n.recentlyIncluded(id) {
		return nil, ErrIncluded
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	most := p.heldBytes(limits[types.D])
	if d, ok := p.deferred[id]; ok {
		return &SubmitResult{ID: id, Known: true, Deferred: ptr(true), NotBefore: ptr(d.NotBefore), Expires: ptr(d.Expires)}, nil
	}
	if _, ok := p.byID[id]; ok {
		r := &SubmitResult{ID: id, Known: true}
		if notBefore != nil {
			r.Deferred = ptr(false)
		}
		return r, nil
	}
	if notBefore != nil {
		return p.submitDeferred(id, raw, tx, *notBefore, source, uint64(n.Chain.Tip().Height), most)
	}
	if !p.makeRoom(len(raw), source, most) {
		return nil, ErrPoolFull
	}
	p.push(&Pooled{ID: id, Raw: append([]byte(nil), raw...), Tx: tx, Arrived: now(), Source: source})
	defer p.notify()
	return &SubmitResult{ID: id}, nil
}

func (p *txPool) deferredBytes() int {
	total := 0
	for _, d := range p.deferred {
		total += len(d.Raw)
	}
	return total
}

func (p *txPool) submitDeferred(id hashing.Hash, raw []byte, tx *types.Transaction, notBefore uint64,
	source netip.Addr, height uint64, most int) (*SubmitResult, error) {
	current := now()
	if notBefore > current+DeferredHorizon {
		return nil, ErrHorizon
	}
	if source.IsValid() {
		count := 0
		for _, d := range p.deferred {
			if d.Source == source {
				count++
			}
		}
		if count >= DeferredPerSource {
			return nil, ErrDeferredFull
		}
	}
	// A full set takes an entry due earlier in place of the relayed entry
	// due latest.
	for len(p.deferred) >= DeferredMax || p.deferredBytes()+len(raw) > most {
		var latest *Deferred
		for _, d := range p.deferred {
			if d.Source.IsValid() && (latest == nil || d.NotBefore > latest.NotBefore ||
				(d.NotBefore == latest.NotBefore && bytes.Compare(d.ID[:], latest.ID[:]) > 0)) {
				latest = d
			}
		}
		if latest == nil || latest.NotBefore <= notBefore {
			return nil, ErrDeferredFull
		}
		delete(p.deferred, latest.ID)
	}
	expires := max(notBefore, current) + DeferredGrace
	p.deferred[id] = &Deferred{ID: id, Raw: append([]byte(nil), raw...), Tx: tx, NotBefore: notBefore,
		Expires: expires, Since: height, Source: source}
	p.markDirty(current)
	p.notify()
	return &SubmitResult{ID: id, Deferred: ptr(true), NotBefore: ptr(notBefore), Expires: ptr(expires)}, nil
}

func (p *txPool) markDirty(t uint64) {
	if p.dirtySince == 0 {
		p.dirtySince = t
	}
}

// statusName is a receipt status as the reference's REST and trials name it.
func statusName(s uint8) string {
	names := []string{"Success", "Revert", "Trap", "Compute", "Access", "Data", "Growth", "Structure",
		"Missing", "InvalidModule", "Exists", "ReadOnly"}
	if int(s) < len(names) {
		return names[s]
	}
	return fmt.Sprintf("Status(%d)", s)
}

// trial runs a transaction alone in a candidate on the tip: whether it fits
// a block and succeeds, or why not, and the compute it ran.
func (n *Node) trial(raw []byte, tx *types.Transaction) (string, bool, uint64, error) {
	tip := n.Chain.Tip()
	b, _, err := n.candidate(tip)
	if err != nil {
		return "", false, 0, err
	}
	r, err := b.Try(raw, tx)
	if err != nil {
		return "", false, 0, err
	}
	var compute uint64
	if as := b.Attempts(); len(as) > 0 {
		compute = as[0].Used[types.C]
	}
	if r == nil {
		return doesNotFit, false, compute, nil
	}
	if r.Status != types.StatusSuccess {
		return statusName(r.Status), false, compute, nil
	}
	return "", true, compute, nil
}

// candidate starts a block on parent with this node's producer data and
// votes, timestamped max(now, median + 1).
func (n *Node) candidate(parent *chain.Entry) (*exec.Builder, *vm.BlockContext, error) {
	height, median, _, limits := n.Chain.NextContext(parent)
	ts := max(now(), median+1)
	ctx := &vm.BlockContext{ChainID: n.Chain.ChainID(), Height: height, Timestamp: ts,
		ParentID: parent.ID, Producer: n.Producer()}
	b, err := n.Chain.NewBuilder(parent, ctx, limits)
	return b, ctx, err
}

// tick keeps the pool and the deferred set moving, with bounded work: aged
// transactions leave; expired deferred entries leave; due ones run on the
// tip, least recently tried first, within about one block's compute every
// two seconds and on every new tip; those that succeed enter the pool and
// those that do not fit even an empty block leave.
func (n *Node) tick() error {
	p := n.tx
	current := now()
	tip := n.Chain.Tip()
	limits := n.nextLimits()
	most := p.heldBytes(limits[types.D])

	p.mu.Lock()
	before := len(p.items)
	p.retain(func(it *Pooled) bool { return it.Arrived+PoolAge >= current })
	changed := len(p.items) != before
	var due []*Deferred
	if len(p.deferred) > 0 && (current >= p.deferredNext || p.deferredTip != tip.ID) {
		p.deferredNext = current + deferredPass
		p.deferredTip = tip.ID
		for id, d := range p.deferred {
			if d.Expires < current {
				delete(p.deferred, id)
				p.markDirty(current)
				changed = true
			}
		}
		for id, d := range p.deferred {
			if _, pooled := p.byID[id]; pooled || d.NotBefore > current {
				continue
			}
			if d.LastTip != tip.ID || d.LastAttempt+deferredRetry <= current {
				due = append(due, d)
			}
		}
		sort.Slice(due, func(i, j int) bool {
			if due[i].LastAttempt != due[j].LastAttempt {
				return due[i].LastAttempt < due[j].LastAttempt
			}
			return bytes.Compare(due[i].ID[:], due[j].ID[:]) < 0
		})
	}
	p.mu.Unlock()

	allowance := limits[types.C]
	for _, d := range due {
		if allowance == 0 {
			break
		}
		why, ok, compute, err := n.trial(d.Raw, d.Tx)
		if err != nil {
			return err
		}
		if compute >= allowance {
			allowance = 0
		} else {
			allowance -= compute
		}
		p.mu.Lock()
		if p.deferred[d.ID] != d {
			p.mu.Unlock()
			continue
		}
		if why == doesNotFit {
			delete(p.deferred, d.ID)
			p.markDirty(current)
			p.mu.Unlock()
			changed = true
			continue
		}
		if ok {
			if _, pooled := p.byID[d.ID]; !pooled && p.makeRoom(len(d.Raw), d.Source, most) {
				p.push(&Pooled{ID: d.ID, Raw: d.Raw, Tx: d.Tx, Arrived: current, Source: d.Source})
				changed = true
			}
		}
		d.LastAttempt = current
		d.LastTip = tip.ID
		d.LastResult = why
		p.mu.Unlock()
	}
	p.mu.Lock()
	for id, d := range p.deferred {
		_, d.Admitted = p.byID[id]
	}
	var save []byte
	if p.dirtySince != 0 && p.path != "" && current >= p.dirtySince+deferredSave {
		save = p.encodeDeferred()
		p.dirtySince = 0
	}
	p.mu.Unlock()
	if save != nil {
		if err := writeAtomic(p.path, save); err != nil {
			n.log.Error("saving the deferred set", "err", err)
		}
	}
	if changed {
		p.notify()
	}
	return nil
}

// published follows the canonical chain: what newly connected blocks
// include leaves the pool, and leaves the deferred set when included
// successfully above the height it was deferred at; entries a disconnected
// block had taken come back; other transactions of disconnected blocks are
// offered again as the node's own. Runs synchronously for every tip change.
func (n *Node) published(ev chain.Event) {
	n.pruneIndex()
	for _, e := range ev.Removed {
		n.index(e, false)
	}
	included := map[hashing.Hash]uint64{}
	type success struct {
		height uint64
		block  hashing.Hash
	}
	succeeded := map[hashing.Hash]success{}
	for _, e := range ev.Connected {
		ids, sts := n.index(e, true)
		for i, id := range ids {
			if e.Height > included[id] {
				included[id] = e.Height
			}
			if sts[i] == types.StatusSuccess {
				succeeded[id] = success{e.Height, e.ID}
			}
		}
	}
	var requeue [][]byte
	for _, e := range ev.Removed {
		raw, err := n.Chain.Block(e.ID)
		if err != nil {
			continue
		}
		if body, err := types.DecodeBody(raw[types.HeaderSize:]); err == nil {
			requeue = append(requeue, body.Transactions...)
		}
	}
	p := n.tx
	current := now()
	p.mu.Lock()
	n.templates = nil
	p.retain(func(it *Pooled) bool { _, inc := included[it.ID]; return !inc })
	for id, d := range p.deferred {
		if s, ok := succeeded[id]; ok && s.height > d.Since {
			delete(p.deferred, id)
			t := p.taken[s.block]
			if t == nil {
				t = &takenSet{height: s.height}
				p.taken[s.block] = t
			}
			t.items = append(t.items, d)
			p.markDirty(current)
		}
	}
	restored := map[hashing.Hash]bool{}
	for _, e := range ev.Removed {
		if t := p.taken[e.ID]; t != nil {
			for _, d := range t.items {
				if d.Expires >= current {
					p.deferred[d.ID] = d
					restored[d.ID] = true
					p.markDirty(current)
				}
			}
			delete(p.taken, e.ID)
		}
	}
	tipHeight := ev.NewTip.Height
	for id, t := range p.taken {
		if t.height+RelayRecent <= tipHeight {
			delete(p.taken, id)
		}
	}
	p.mu.Unlock()
	for _, raw := range requeue {
		id := types.TransactionID(n.Chain.ChainID(), raw)
		if _, inc := included[id]; inc || restored[id] {
			continue
		}
		n.Submit(raw, nil, netip.Addr{}, false)
	}
	p.mu.Lock()
	for id, d := range p.deferred {
		_, d.Admitted = p.byID[id]
	}
	p.mu.Unlock()
	p.notify()
}

// deferred.json, as the reference writes it.
type deferredFile struct {
	Version int             `json:"version"`
	Items   []deferredEntry `json:"items"`
}

type deferredEntry struct {
	Hex       string  `json:"hex"`
	NotBefore uint64  `json:"not_before"`
	Expires   uint64  `json:"expires"`
	Since     uint64  `json:"since"`
	Source    *string `json:"source"`
}

func (p *txPool) encodeDeferred() []byte {
	f := deferredFile{Version: 1, Items: []deferredEntry{}}
	ids := make([]hashing.Hash, 0, len(p.deferred))
	for id := range p.deferred {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	for _, id := range ids {
		d := p.deferred[id]
		e := deferredEntry{Hex: hex.EncodeToString(d.Raw), NotBefore: d.NotBefore, Expires: d.Expires, Since: d.Since}
		if d.Source.IsValid() {
			e.Source = ptr(d.Source.String())
		}
		f.Items = append(f.Items, e)
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	return b
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadDeferred restores the deferred set, dropping entries that expired, no
// longer decode, exceed the limits, or a canonical block above their height
// included successfully. A file that does not parse is set aside.
func (n *Node) loadDeferred() {
	p := n.tx
	if p.path == "" {
		return
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var f deferredFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		os.Rename(p.path, p.path+".corrupt")
		n.log.Warn("deferred set unreadable; set aside", "path", p.path)
		return
	}
	current := now()
	limits := n.nextLimits()
	most := p.heldBytes(limits[types.D])
	p.mu.Lock()
	defer p.mu.Unlock()
	perSource := map[netip.Addr]int{}
	total := 0
	for _, e := range f.Items {
		raw, err := hex.DecodeString(e.Hex)
		if err != nil || e.Expires < current || len(p.deferred) >= DeferredMax || total+len(raw) > most {
			continue
		}
		tx, err := types.DecodeTransaction(raw)
		if err != nil {
			continue
		}
		var src netip.Addr
		if e.Source != nil {
			src, _ = netip.ParseAddr(*e.Source)
			if perSource[src] >= DeferredPerSource {
				continue
			}
			perSource[src]++
		}
		id := types.TransactionID(n.Chain.ChainID(), raw)
		if n.succeededAbove(id, e.Since) {
			continue
		}
		p.deferred[id] = &Deferred{ID: id, Raw: raw, Tx: tx, NotBefore: e.NotBefore, Expires: e.Expires,
			Since: e.Since, Source: src}
		total += len(raw)
	}
}

// flushDeferred writes the deferred set now, as on shutdown.
func (n *Node) flushDeferred() {
	p := n.tx
	if p.path == "" {
		return
	}
	p.mu.Lock()
	dirty := p.dirtySince != 0
	b := p.encodeDeferred()
	p.dirtySince = 0
	p.mu.Unlock()
	if dirty {
		if err := writeAtomic(p.path, b); err != nil {
			n.log.Error("saving the deferred set", "err", err)
		}
	}
}

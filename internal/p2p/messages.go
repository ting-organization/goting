package p2p

import (
	"fmt"
	"math/big"
	"net/netip"
	"unicode/utf8"

	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/types"
)

// Request tags.
const (
	TagHello          = 0
	TagState          = 1
	TagHeaders        = 2
	TagRaw            = 3
	TagImport         = 4
	TagTransactionRaw = 5
	TagSubmit         = 6
	TagInventory      = 7
	TagPending        = 8
	TagDeferred       = 9
	TagAddresses      = 10
)

// Answer tags.
const (
	AnsState     = 0
	AnsHeaders   = 1
	AnsSlice     = 2
	AnsReceived  = 3
	AnsTaken     = 4
	AnsInventory = 5
	AnsItems     = 6
	AnsAddresses = 7
)

// ProtocolVersion is the peer protocol's version.
const ProtocolVersion = 1

// Message is a request or an answer.
type Message interface {
	kind() byte
	tag() byte
	put(e *enc)
}

// Encode returns a message's frame payload.
func Encode(m Message) []byte {
	e := &enc{b: []byte{m.kind(), m.tag()}}
	m.put(e)
	return e.b
}

// Hello opens a connection.
type Hello struct {
	Version  uint32
	ChainID  hashing.Hash
	Agent    string
	Features []string
	Nonce    uint64
	Announce bool
	Listen   *uint16
}

func (*Hello) kind() byte { return KindRequest }
func (*Hello) tag() byte  { return TagHello }
func (m *Hello) put(e *enc) {
	e.u32(m.Version)
	e.id(m.ChainID)
	e.name(m.Agent)
	e.u16(uint16(len(m.Features)))
	for _, f := range m.Features {
		e.name(f)
	}
	e.u64(m.Nonce)
	e.flag(m.Announce)
	e.flag(m.Listen != nil)
	if m.Listen != nil {
		e.u16(*m.Listen)
	}
}

// StateRequest asks for the node's state.
type StateRequest struct{}

func (*StateRequest) kind() byte { return KindRequest }
func (*StateRequest) tag() byte  { return TagState }
func (*StateRequest) put(*enc)   {}

// HeadersRequest asks for canonical headers after the locator.
type HeadersRequest struct{ Locator []hashing.Hash }

func (*HeadersRequest) kind() byte   { return KindRequest }
func (*HeadersRequest) tag() byte    { return TagHeaders }
func (m *HeadersRequest) put(e *enc) { e.ids(m.Locator) }

// RawRequest asks for a slice of a block.
type RawRequest struct {
	ID     hashing.Hash
	Offset uint64
	Length uint32
}

func (*RawRequest) kind() byte { return KindRequest }
func (*RawRequest) tag() byte  { return TagRaw }
func (m *RawRequest) put(e *enc) {
	e.id(m.ID)
	e.u64(m.Offset)
	e.u32(m.Length)
}

// TransactionRawRequest asks for a slice of a pooled transaction.
type TransactionRawRequest RawRequest

func (*TransactionRawRequest) kind() byte   { return KindRequest }
func (*TransactionRawRequest) tag() byte    { return TagTransactionRaw }
func (m *TransactionRawRequest) put(e *enc) { (*RawRequest)(m).put(e) }

// Import pushes a block, whole or a slice of it.
type Import struct {
	Offset uint64
	Total  uint64
	Bytes  []byte
}

func (*Import) kind() byte { return KindRequest }
func (*Import) tag() byte  { return TagImport }
func (m *Import) put(e *enc) {
	e.u64(m.Offset)
	e.u64(m.Total)
	e.bytes(m.Bytes)
}

// Submit pushes a transaction, whole or a slice of it.
type Submit struct {
	Offset    uint64
	Total     uint64
	Bytes     []byte
	NotBefore *uint64
}

func (*Submit) kind() byte { return KindRequest }
func (*Submit) tag() byte  { return TagSubmit }
func (m *Submit) put(e *enc) {
	e.u64(m.Offset)
	e.u64(m.Total)
	e.bytes(m.Bytes)
	e.optU64(m.NotBefore)
}

// InventoryRequest pages through pooled or deferred transaction ids.
type InventoryRequest struct {
	Deferred bool
	After    *hashing.Hash
}

func (*InventoryRequest) kind() byte { return KindRequest }
func (*InventoryRequest) tag() byte  { return TagInventory }
func (m *InventoryRequest) put(e *enc) {
	e.flag(m.Deferred)
	e.optID(m.After)
}

// PendingRequest asks for pooled transactions; DeferredRequest for deferred ones.
type PendingRequest struct{ IDs []hashing.Hash }

func (*PendingRequest) kind() byte   { return KindRequest }
func (*PendingRequest) tag() byte    { return TagPending }
func (m *PendingRequest) put(e *enc) { e.ids(m.IDs) }

type DeferredRequest struct{ IDs []hashing.Hash }

func (*DeferredRequest) kind() byte   { return KindRequest }
func (*DeferredRequest) tag() byte    { return TagDeferred }
func (m *DeferredRequest) put(e *enc) { e.ids(m.IDs) }

// AddressesRequest asks for peer addresses.
type AddressesRequest struct{}

func (*AddressesRequest) kind() byte { return KindRequest }
func (*AddressesRequest) tag() byte  { return TagAddresses }
func (*AddressesRequest) put(*enc)   {}

// State describes a node.
type State struct {
	Version  uint32
	ChainID  hashing.Hash
	Agent    string
	Features []string
	Nonce    uint64
	Height   uint64
	Tip      hashing.Hash
	Work     *big.Int
	Locator  []hashing.Hash
	Pending  []hashing.Hash
	Deferred []hashing.Hash
}

func (*State) kind() byte { return KindAnswer }
func (*State) tag() byte  { return AnsState }
func (m *State) put(e *enc) {
	e.u32(m.Version)
	e.id(m.ChainID)
	e.name(m.Agent)
	e.u16(uint16(len(m.Features)))
	for _, f := range m.Features {
		e.name(f)
	}
	e.u64(m.Nonce)
	e.u64(m.Height)
	e.id(m.Tip)
	e.work(m.Work)
	e.ids(m.Locator)
	e.ids(m.Pending)
	e.ids(m.Deferred)
}

// Headers carries canonical headers.
type Headers struct {
	Headers []*types.Header
	Raw     [][]byte // each header's 294 bytes
}

func (*Headers) kind() byte { return KindAnswer }
func (*Headers) tag() byte  { return AnsHeaders }
func (m *Headers) put(e *enc) {
	e.u16(uint16(len(m.Headers)))
	for _, h := range m.Headers {
		e.b = append(e.b, h.Encode()...)
	}
}

// Slice is part of a block or transaction.
type Slice struct {
	Offset uint64
	Total  uint64
	Bytes  []byte
}

func (*Slice) kind() byte { return KindAnswer }
func (*Slice) tag() byte  { return AnsSlice }
func (m *Slice) put(e *enc) {
	e.u64(m.Offset)
	e.u64(m.Total)
	e.bytes(m.Bytes)
}

// Received acknowledges a slice of a push.
type Received struct{ Bytes uint64 }

func (*Received) kind() byte   { return KindAnswer }
func (*Received) tag() byte    { return AnsReceived }
func (m *Received) put(e *enc) { e.u64(m.Bytes) }

// Taken acknowledges a complete push.
type Taken struct {
	ID    hashing.Hash
	Known bool
}

func (*Taken) kind() byte { return KindAnswer }
func (*Taken) tag() byte  { return AnsTaken }
func (m *Taken) put(e *enc) {
	e.id(m.ID)
	e.flag(m.Known)
}

// Inventory is a page of transaction ids.
type Inventory struct {
	IDs  []hashing.Hash
	Next *hashing.Hash
}

func (*Inventory) kind() byte { return KindAnswer }
func (*Inventory) tag() byte  { return AnsInventory }
func (m *Inventory) put(e *enc) {
	e.ids(m.IDs)
	e.optID(m.Next)
}

// Item is a pooled or deferred transaction, whole or by size.
type Item struct {
	ID        hashing.Hash
	NotBefore *uint64
	Tx        []byte // set when carried whole
	Size      uint64 // set when listed by size
	BySize    bool
}

// Items carries transactions.
type Items struct{ Items []Item }

func (*Items) kind() byte { return KindAnswer }
func (*Items) tag() byte  { return AnsItems }
func (m *Items) put(e *enc) {
	e.u16(uint16(len(m.Items)))
	for _, it := range m.Items {
		e.id(it.ID)
		e.optU64(it.NotBefore)
		if it.BySize {
			e.u8(1)
			e.u64(it.Size)
		} else {
			e.u8(0)
			e.bytes(it.Tx)
		}
	}
}

// Addresses lists peers.
type Addresses struct{ Addrs []netip.AddrPort }

func (*Addresses) kind() byte { return KindAnswer }
func (*Addresses) tag() byte  { return AnsAddresses }
func (m *Addresses) put(e *enc) {
	e.u16(uint16(len(m.Addrs)))
	for _, a := range m.Addrs {
		e.address(a)
	}
}

// ErrorAnswer is an error answer.
type ErrorAnswer struct{ Text string }

func (e *ErrorAnswer) Error() string { return "peer: " + e.Text }

// EncodeError returns an error answer's payload, cut at a character
// boundary to 1,024 bytes.
func EncodeError(text string) []byte {
	b := []byte(text)
	if len(b) > MaxErrorText {
		cut := MaxErrorText
		for cut > 0 && !utf8.Valid(b[:cut]) {
			cut--
		}
		b = b[:cut]
	}
	e := &enc{b: []byte{KindError}}
	e.name(string(b))
	return e.b
}

// Announcement tells of a changed tip or pool.
type Announcement struct {
	Tip      hashing.Hash
	Height   uint64
	Work     *big.Int
	Pending  uint64
	Deferred uint64
}

// EncodeAnnouncement returns an announcement's payload.
func EncodeAnnouncement(a *Announcement) []byte {
	e := &enc{b: []byte{KindAnnouncement}}
	e.id(a.Tip)
	e.u64(a.Height)
	e.work(a.Work)
	e.u64(a.Pending)
	e.u64(a.Deferred)
	return e.b
}

func decodeNames(d *dec) []string {
	n := d.count(0, MaxFeatures)
	var out []string
	for i := 0; i < n && d.err == nil; i++ {
		out = append(out, d.name(MaxFeature))
	}
	return out
}

// DecodeRequest parses a request frame.
func DecodeRequest(p []byte) (Message, error) {
	d := &dec{b: p}
	if d.u8() != KindRequest {
		return nil, fmt.Errorf("%w: not a request", ErrNotMessage)
	}
	var m Message
	switch d.u8() {
	case TagHello:
		h := &Hello{}
		h.Version = d.u32()
		h.ChainID = d.id()
		h.Agent = d.name(MaxAgent)
		h.Features = decodeNames(d)
		h.Nonce = d.u64()
		h.Announce = d.flag()
		if d.flag() {
			v := d.u16()
			h.Listen = &v
		}
		m = h
	case TagState:
		m = &StateRequest{}
	case TagHeaders:
		m = &HeadersRequest{Locator: d.ids(1, MaxLocator)}
	case TagRaw:
		r := &RawRequest{ID: d.id(), Offset: d.u64(), Length: d.u32()}
		if d.err == nil && (r.Length < 1 || r.Length > MaxSlice) {
			d.fail("raw length %d", r.Length)
		}
		m = r
	case TagImport:
		m = &Import{Offset: d.u64(), Total: d.u64(), Bytes: d.bytes(MaxSlice)}
	case TagTransactionRaw:
		r := &TransactionRawRequest{ID: d.id(), Offset: d.u64(), Length: d.u32()}
		if d.err == nil && (r.Length < 1 || r.Length > MaxSlice) {
			d.fail("transaction_raw length %d", r.Length)
		}
		m = r
	case TagSubmit:
		m = &Submit{Offset: d.u64(), Total: d.u64(), Bytes: d.bytes(MaxSlice), NotBefore: d.optU64()}
	case TagInventory:
		m = &InventoryRequest{Deferred: d.flag(), After: d.optID()}
	case TagPending:
		m = &PendingRequest{IDs: d.ids(1, MaxItems)}
	case TagDeferred:
		m = &DeferredRequest{IDs: d.ids(1, MaxItems)}
	case TagAddresses:
		m = &AddressesRequest{}
	default:
		d.fail("unknown request tag")
	}
	d.end()
	if d.err != nil {
		return nil, d.err
	}
	return m, nil
}

// DecodeAnswer parses an answer or error-answer frame. An error answer is
// returned as *ErrorAnswer in the error position.
func DecodeAnswer(p []byte) (Message, error) {
	d := &dec{b: p}
	switch d.u8() {
	case KindAnswer:
	case KindError:
		t := d.text(MaxErrorText)
		d.end()
		if d.err != nil {
			return nil, d.err
		}
		return nil, &ErrorAnswer{Text: t}
	default:
		return nil, fmt.Errorf("%w: not an answer", ErrNotMessage)
	}
	var m Message
	switch d.u8() {
	case AnsState:
		s := &State{}
		s.Version = d.u32()
		s.ChainID = d.id()
		s.Agent = d.name(MaxAgent)
		s.Features = decodeNames(d)
		s.Nonce = d.u64()
		s.Height = d.u64()
		s.Tip = d.id()
		s.Work = d.work()
		s.Locator = d.ids(0, MaxLocator)
		s.Pending = d.ids(0, MaxPoolIDs)
		s.Deferred = d.ids(0, MaxPoolIDs)
		m = s
	case AnsHeaders:
		n := d.count(0, MaxHeaders)
		h := &Headers{}
		if d.err == nil && n*types.HeaderSize > len(d.b)-d.off {
			d.fail("headers truncated")
		}
		for i := 0; i < n && d.err == nil; i++ {
			raw := d.take(types.HeaderSize)
			hd, _ := types.DecodeHeader(raw)
			h.Headers = append(h.Headers, hd)
			h.Raw = append(h.Raw, raw)
		}
		m = h
	case AnsSlice:
		m = &Slice{Offset: d.u64(), Total: d.u64(), Bytes: d.bytes(MaxSlice)}
	case AnsReceived:
		m = &Received{Bytes: d.u64()}
	case AnsTaken:
		m = &Taken{ID: d.id(), Known: d.flag()}
	case AnsInventory:
		m = &Inventory{IDs: d.ids(0, MaxInventory), Next: d.optID()}
	case AnsItems:
		n := d.count(0, MaxItems)
		it := &Items{}
		for i := 0; i < n && d.err == nil; i++ {
			x := Item{ID: d.id(), NotBefore: d.optU64()}
			switch d.u8() {
			case 0:
				x.Tx = d.bytes(MaxFrame)
			case 1:
				x.BySize = true
				x.Size = d.u64()
			default:
				d.fail("item form")
			}
			it.Items = append(it.Items, x)
		}
		m = it
	case AnsAddresses:
		n := d.count(0, MaxAddresses)
		a := &Addresses{}
		for i := 0; i < n && d.err == nil; i++ {
			a.Addrs = append(a.Addrs, d.address())
		}
		m = a
	default:
		d.fail("unknown answer tag")
	}
	d.end()
	if d.err != nil {
		return nil, d.err
	}
	return m, nil
}

// DecodeAnnouncement parses an announcement frame.
func DecodeAnnouncement(p []byte) (*Announcement, error) {
	d := &dec{b: p}
	if d.u8() != KindAnnouncement {
		return nil, fmt.Errorf("%w: not an announcement", ErrNotMessage)
	}
	a := &Announcement{Tip: d.id(), Height: d.u64(), Work: d.work(), Pending: d.u64(), Deferred: d.u64()}
	d.end()
	if d.err != nil {
		return nil, d.err
	}
	return a, nil
}

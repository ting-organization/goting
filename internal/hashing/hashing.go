// Package hashing implements Ting's domain-separated hash (rules §2.4) and the
// fixed 32-byte Hash type used for every identifier and commitment.
package hashing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"sync"
)

// Hash is an opaque 32-byte string: a hash or an identifier.
type Hash [32]byte

// Zero is the all-zero hash.
var Zero Hash

func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports whether every byte is zero.
func (h Hash) IsZero() bool { return h == Zero }

// MarshalText encodes the hash as lowercase hex.
func (h Hash) MarshalText() ([]byte, error) {
	out := make([]byte, 64)
	hex.Encode(out, h[:])
	return out, nil
}

// UnmarshalText decodes 64 hex characters.
func (h *Hash) UnmarshalText(b []byte) error {
	v, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// ParseHash decodes a 64-character hex string.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 64 {
		return h, errors.New("hash must be 64 hex characters")
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, err
	}
	return h, nil
}

// MustParseHash is ParseHash for constants.
func MustParseHash(s string) Hash {
	h, err := ParseHash(s)
	if err != nil {
		panic(err)
	}
	return h
}

// Consensus tags (rules §2.4).
const (
	TagChain            = "chain"
	TagGenesis          = "genesis"
	TagBlock            = "block"
	TagProducer         = "producer"
	TagTransaction      = "transaction"
	TagContract         = "contract"
	TagCode             = "code"
	TagStateKey         = "state-key"
	TagStateLeaf        = "state-leaf"
	TagStateNode        = "state-node"
	TagStateEmpty       = "state-empty"
	TagTransactionsRoot = "transactions"
	TagReceiptsRoot     = "receipts"
)

var hasherPool = sync.Pool{New: func() any { return sha256.New() }}

// Hasher incrementally builds H(tag, x) for x made of several parts.
type Hasher struct {
	h hash.Hash
}

// New starts H(tag, ...): SHA256("Ting/" || u16(len(tag)) || tag || ...).
func New(tag string) Hasher {
	h := hasherPool.Get().(hash.Hash)
	h.Reset()
	var pre [7]byte
	copy(pre[:5], "Ting/")
	binary.LittleEndian.PutUint16(pre[5:], uint16(len(tag)))
	h.Write(pre[:])
	h.Write([]byte(tag))
	return Hasher{h: h}
}

// Write appends raw bytes.
func (x Hasher) Write(b []byte) { x.h.Write(b) }

// WriteU8 appends one byte.
func (x Hasher) WriteU8(v uint8) { x.h.Write([]byte{v}) }

// WriteU16 appends a little-endian u16.
func (x Hasher) WriteU16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	x.h.Write(b[:])
}

// WriteU32 appends a little-endian u32.
func (x Hasher) WriteU32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	x.h.Write(b[:])
}

// WriteU64 appends a little-endian u64.
func (x Hasher) WriteU64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	x.h.Write(b[:])
}

// WriteBytes appends Bytes(b) = u32(len(b)) || b.
func (x Hasher) WriteBytes(b []byte) {
	x.WriteU32(uint32(len(b)))
	x.h.Write(b)
}

// Sum finishes the hash and releases the hasher.
func (x Hasher) Sum() Hash {
	var out Hash
	x.h.Sum(out[:0])
	hasherPool.Put(x.h)
	return out
}

// H computes the domain-separated hash of tag and the concatenation of parts.
func H(tag string, parts ...[]byte) Hash {
	x := New(tag)
	for _, p := range parts {
		x.Write(p)
	}
	return x.Sum()
}

// Precomputed constants.
var (
	// EmptyStateRoot is H("state-empty", "").
	EmptyStateRoot = H(TagStateEmpty)
)

// ShaPrice is the C price of a SHA-256 the runtime computes on a program's
// behalf over a domain-separated message of m bytes (rules §15.5):
// 100 + 900 × ceil((m + 9) / 64). Saturates at the u64 maximum.
func ShaPrice(m uint64) uint64 {
	if m > (^uint64(0))-63-9 {
		return ^uint64(0)
	}
	blocks := (m + 9 + 63) / 64
	if blocks > ((^uint64(0))-100)/900 {
		return ^uint64(0)
	}
	return 100 + 900*blocks
}

// Domain header length: "Ting/" plus the u16 tag length.
const domainHeader = 7

// RecordKeyPrice is the price of hashing a record key (m = 81): 1,900 C.
var RecordKeyPrice = ShaPrice(domainHeader + uint64(len(TagStateKey)) + 1 + 32 + 32)

// ContractIDPrice is the price of deriving a ContractId (m = 87): 1,900 C.
var ContractIDPrice = ShaPrice(domainHeader + uint64(len(TagContract)) + 32 + 32 + 8)

// LeafPrice is the price of hashing an n-byte value into its leaf (m = n + 53).
func LeafPrice(n uint64) uint64 {
	if n > (^uint64(0))-53 {
		return ^uint64(0)
	}
	return ShaPrice(n + domainHeader + uint64(len(TagStateLeaf)) + 32 + 4)
}

// CodeHashPrice is the price of hashing n code bytes into a CodeHash (m = n + 11).
func CodeHashPrice(n uint64) uint64 {
	if n > (^uint64(0))-11 {
		return ^uint64(0)
	}
	return ShaPrice(n + domainHeader + uint64(len(TagCode)))
}

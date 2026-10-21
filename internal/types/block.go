package types

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/codec"
	"github.com/ting-organization/goting/internal/hashing"
)

// HeaderSize is the fixed size of every header (rules §4.1).
const HeaderSize = 294

// BlockOverhead is the D a block consumes outside every transaction budget,
// before its producer data: header, producer-data length prefix and the
// transaction count (rules §12.4).
const BlockOverhead = HeaderSize + 4 + 4

// MinTxReceiptPair is the smallest number of body bytes one transaction and
// its receipt can occupy, framing included (rules §26.7).
const MinTxReceiptPair = 4 + 42 + 4 + 73

// Votes for a resource (rules §10.1).
const (
	VoteHold     = 0
	VoteIncrease = 1
	VoteDecrease = 2
)

// Nonce is the u128 nonce, little-endian.
type Nonce struct {
	Lo, Hi uint64
}

// Header is a block header (rules §4.1).
type Header struct {
	ProtocolVersion  uint16
	ChainID          hashing.Hash
	Height           uint64
	ParentID         hashing.Hash
	Timestamp        uint64
	Target           Target
	TransactionsRoot hashing.Hash
	ReceiptsRoot     hashing.Hash
	StateRoot        hashing.Hash
	ResourcesUsed    ResourceVector
	Votes            [4]uint8
	ProducerHash     hashing.Hash
	Nonce            Nonce
}

// Encode returns the 294 canonical bytes.
func (h *Header) Encode() []byte {
	var buf [HeaderSize]byte
	h.EncodeTo(buf[:])
	return buf[:]
}

// EncodeTo writes the 294 canonical bytes into dst, which must be large enough.
func (h *Header) EncodeTo(dst []byte) {
	_ = dst[HeaderSize-1]
	binary.LittleEndian.PutUint16(dst[0:], h.ProtocolVersion)
	copy(dst[2:34], h.ChainID[:])
	binary.LittleEndian.PutUint64(dst[34:], h.Height)
	copy(dst[42:74], h.ParentID[:])
	binary.LittleEndian.PutUint64(dst[74:], h.Timestamp)
	copy(dst[82:114], h.Target[:])
	copy(dst[114:146], h.TransactionsRoot[:])
	copy(dst[146:178], h.ReceiptsRoot[:])
	copy(dst[178:210], h.StateRoot[:])
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(dst[210+8*i:], h.ResourcesUsed[i])
	}
	copy(dst[242:246], h.Votes[:])
	copy(dst[246:278], h.ProducerHash[:])
	binary.LittleEndian.PutUint64(dst[278:], h.Nonce.Lo)
	binary.LittleEndian.PutUint64(dst[286:], h.Nonce.Hi)
}

// NonceOffset is the byte offset of the nonce inside an encoded header.
const NonceOffset = 278

// DecodeHeader parses exactly 294 bytes. It checks framing only; validity
// against the chain is the consensus package's job.
func DecodeHeader(b []byte) (*Header, error) {
	if len(b) != HeaderSize {
		return nil, fmt.Errorf("header must be %d bytes, got %d", HeaderSize, len(b))
	}
	h := &Header{}
	h.ProtocolVersion = binary.LittleEndian.Uint16(b[0:])
	copy(h.ChainID[:], b[2:34])
	h.Height = binary.LittleEndian.Uint64(b[34:])
	copy(h.ParentID[:], b[42:74])
	h.Timestamp = binary.LittleEndian.Uint64(b[74:])
	copy(h.Target[:], b[82:114])
	copy(h.TransactionsRoot[:], b[114:146])
	copy(h.ReceiptsRoot[:], b[146:178])
	copy(h.StateRoot[:], b[178:210])
	for i := 0; i < 4; i++ {
		h.ResourcesUsed[i] = binary.LittleEndian.Uint64(b[210+8*i:])
	}
	copy(h.Votes[:], b[242:246])
	copy(h.ProducerHash[:], b[246:278])
	h.Nonce.Lo = binary.LittleEndian.Uint64(b[278:])
	h.Nonce.Hi = binary.LittleEndian.Uint64(b[286:])
	return h, nil
}

// ID is the BlockId, H("block", encode(Header)).
func (h *Header) ID() hashing.Hash {
	var buf [HeaderSize]byte
	h.EncodeTo(buf[:])
	return hashing.H(hashing.TagBlock, buf[:])
}

// HeaderIDFromBytes hashes already-encoded header bytes.
func HeaderIDFromBytes(b []byte) hashing.Hash { return hashing.H(hashing.TagBlock, b) }

// ProducerHash is H("producer", producer_data).
func ProducerHash(producer []byte) hashing.Hash { return hashing.H(hashing.TagProducer, producer) }

// Body is a block body (rules §4.3). Transactions and receipts are kept as
// their raw canonical encodings, exactly as framed in the body.
type Body struct {
	ProducerData []byte
	Transactions [][]byte
	Receipts     [][]byte
}

// EncodedLen returns the size of the body's canonical encoding.
func (b *Body) EncodedLen() int {
	n := 4 + len(b.ProducerData) + 4
	for _, t := range b.Transactions {
		n += 4 + len(t)
	}
	for _, r := range b.Receipts {
		n += 4 + len(r)
	}
	return n
}

// Encode appends the body's canonical encoding.
func (b *Body) Encode(w *codec.Writer) {
	w.Bytes(b.ProducerData)
	w.U32(uint32(len(b.Transactions)))
	for _, t := range b.Transactions {
		w.Bytes(t)
	}
	for _, r := range b.Receipts {
		w.Bytes(r)
	}
}

// ErrBodyFraming reports a body that is not canonically framed.
var ErrBodyFraming = errors.New("body framing invalid")

// DecodeBody parses a body. The returned slices alias b.
func DecodeBody(b []byte) (*Body, error) {
	r := codec.NewReader(b)
	body := &Body{}
	body.ProducerData = r.Bytes()
	n := r.U32()
	if r.Err() != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyFraming, r.Err())
	}
	// Each transaction/receipt pair occupies at least 123 bytes; check the
	// count against the remaining bytes before allocating.
	if uint64(n)*MinTxReceiptPair > uint64(r.Remaining()) {
		return nil, fmt.Errorf("%w: transaction count %d exceeds body size", ErrBodyFraming, n)
	}
	body.Transactions = make([][]byte, n)
	body.Receipts = make([][]byte, n)
	for i := range body.Transactions {
		body.Transactions[i] = r.Bytes()
	}
	for i := range body.Receipts {
		body.Receipts[i] = r.Bytes()
	}
	r.End()
	if r.Err() != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyFraming, r.Err())
	}
	return body, nil
}

// Block is a header and a body.
type Block struct {
	Header *Header
	Body   *Body
}

// Encode returns the block's canonical bytes: header then body.
func (b *Block) Encode() []byte {
	w := codec.NewWriter(HeaderSize + b.Body.EncodedLen())
	w.Raw(b.Header.Encode())
	b.Body.Encode(w)
	return w.Buf
}

// DecodeBlock parses a block's canonical bytes.
func DecodeBlock(b []byte) (*Block, error) {
	if len(b) < HeaderSize {
		return nil, fmt.Errorf("block shorter than a header: %d bytes", len(b))
	}
	h, err := DecodeHeader(b[:HeaderSize])
	if err != nil {
		return nil, err
	}
	body, err := DecodeBody(b[HeaderSize:])
	if err != nil {
		return nil, err
	}
	return &Block{Header: h, Body: body}, nil
}

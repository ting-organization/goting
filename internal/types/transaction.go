package types

import (
	"errors"
	"fmt"

	"github.com/ting-organization/goting/internal/codec"
	"github.com/ting-organization/goting/internal/hashing"
)

// Transaction kinds (rules §4.4).
const (
	KindCall   = 0
	KindDeploy = 1
)

// EnvelopeVersion is the only accepted envelope version.
const EnvelopeVersion = 1

// DataBaseOverhead is added to a transaction's encoded length to form its
// data base: the two body length prefixes and the receipt's fixed fields.
const DataBaseOverhead = 4 + 4 + ReceiptFixedSize

// MaxDataBudget is the ceiling of a transaction's declared D (rules §4.4).
const MaxDataBudget = 1<<32 - 1

// Transaction is a Call or a Deploy envelope.
type Transaction struct {
	Kind   uint8
	Budget ResourceVector
	// Call
	Target hashing.Hash
	Input  []byte
	// Deploy
	Code []byte
	// Input doubles as the deploy's initial input.
}

// Encode returns the canonical encoding.
func (t *Transaction) Encode() []byte {
	n := 2 + 32 + 4 + len(t.Input)
	if t.Kind == KindCall {
		n += 32
	} else {
		n += 4 + len(t.Code)
	}
	w := codec.NewWriter(n)
	w.U8(EnvelopeVersion)
	w.U8(t.Kind)
	t.Budget.Encode(w)
	if t.Kind == KindCall {
		w.Raw(t.Target[:])
		w.Bytes(t.Input)
	} else {
		w.Bytes(t.Code)
		w.Bytes(t.Input)
	}
	return w.Buf
}

// Envelope errors.
var (
	ErrEnvelope   = errors.New("invalid transaction envelope")
	ErrDataBudget = errors.New("transaction data budget outside its floor and ceiling")
)

// DecodeTransaction parses an envelope and applies its static rules,
// including the D floor and ceiling. The returned slices alias b.
func DecodeTransaction(b []byte) (*Transaction, error) {
	r := codec.NewReader(b)
	if v := r.U8(); r.Err() == nil && v != EnvelopeVersion {
		return nil, fmt.Errorf("%w: version %d", ErrEnvelope, v)
	}
	t := &Transaction{}
	t.Kind = r.U8()
	if r.Err() == nil && t.Kind != KindCall && t.Kind != KindDeploy {
		return nil, fmt.Errorf("%w: kind %d", ErrEnvelope, t.Kind)
	}
	t.Budget = DecodeResourceVector(r)
	if t.Kind == KindCall {
		r.Into(t.Target[:])
		t.Input = r.Bytes()
	} else {
		t.Code = r.Bytes()
		t.Input = r.Bytes()
	}
	r.End()
	if r.Err() != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvelope, r.Err())
	}
	base := DataBase(len(b))
	if t.Budget[D] < base || t.Budget[D] > MaxDataBudget {
		return nil, fmt.Errorf("%w: D budget %d, data base %d", ErrDataBudget, t.Budget[D], base)
	}
	return t, nil
}

// DataBase is len(encode(Transaction)) + 81 (rules §12.3).
func DataBase(encodedLen int) uint64 { return uint64(encodedLen) + DataBaseOverhead }

// TransactionID is H("transaction", ChainId || encode(Transaction)).
func TransactionID(chainID hashing.Hash, encoded []byte) hashing.Hash {
	x := hashing.New(hashing.TagTransaction)
	x.Write(chainID[:])
	x.Write(encoded)
	return x.Sum()
}

// ContractID is H("contract", ChainId || TransactionId || u64(ordinal)).
func ContractID(chainID, txID hashing.Hash, ordinal uint64) hashing.Hash {
	x := hashing.New(hashing.TagContract)
	x.Write(chainID[:])
	x.Write(txID[:])
	x.WriteU64(ordinal)
	return x.Sum()
}

// CodeHash is H("code", code).
func CodeHash(code []byte) hashing.Hash { return hashing.H(hashing.TagCode, code) }

// Record namespaces (rules §5.3).
const (
	NamespaceManifest = 0
	NamespaceCode     = 1
	NamespaceStorage  = 2
)

// RecordKey is H("state-key", u8(namespace) || ContractId || logical_key).
func RecordKey(namespace uint8, contract hashing.Hash, logical *[32]byte) hashing.Hash {
	x := hashing.New(hashing.TagStateKey)
	x.WriteU8(namespace)
	x.Write(contract[:])
	x.Write(logical[:])
	return x.Sum()
}

var zeroLogical [32]byte

// ManifestKey is the key of a contract's manifest record.
func ManifestKey(contract hashing.Hash) hashing.Hash {
	return RecordKey(NamespaceManifest, contract, &zeroLogical)
}

// CodeKey is the key of a contract's code record.
func CodeKey(contract hashing.Hash) hashing.Hash {
	return RecordKey(NamespaceCode, contract, &zeroLogical)
}

// StorageKey is the key of a contract's storage record.
func StorageKey(contract hashing.Hash, logical *[32]byte) hashing.Hash {
	return RecordKey(NamespaceStorage, contract, logical)
}

// ManifestSize is the exact size of a manifest (rules §5.3).
const ManifestSize = 44

// Manifest is CodeHash || u32(code_length) || u64(code_price).
type Manifest struct {
	CodeHash   hashing.Hash
	CodeLength uint32
	CodePrice  uint64
}

// Encode returns the 44 manifest bytes.
func (m *Manifest) Encode() []byte {
	w := codec.NewWriter(ManifestSize)
	w.Raw(m.CodeHash[:])
	w.U32(m.CodeLength)
	w.U64(m.CodePrice)
	return w.Buf
}

// DecodeManifest parses 44 bytes.
func DecodeManifest(b []byte) (Manifest, error) {
	var m Manifest
	if len(b) != ManifestSize {
		return m, fmt.Errorf("manifest must be %d bytes, got %d", ManifestSize, len(b))
	}
	r := codec.NewReader(b)
	r.Into(m.CodeHash[:])
	m.CodeLength = r.U32()
	m.CodePrice = r.U64()
	return m, r.Err()
}

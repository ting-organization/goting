package types

import (
	"fmt"

	"github.com/ting-organization/goting/internal/codec"
	"github.com/ting-organization/goting/internal/hashing"
)

// ReceiptFixedSize is a receipt's size without result bytes and events.
const ReceiptFixedSize = 32 + 1 + 32 + 4 + 4

// EventFixedSize is an event's size without its payload.
const EventFixedSize = 32 + 4

// Receipt statuses (rules §4.5).
const (
	StatusSuccess         = 0
	StatusRevert          = 1
	StatusTrap            = 2
	StatusExhaustedC      = 3
	StatusExhaustedS      = 4
	StatusExhaustedD      = 5
	StatusExhaustedG      = 6
	StatusOrdinalOverflow = 7
	StatusTargetAbsent    = 8
	StatusModuleInvalid   = 9
	StatusIDExists        = 10
	StatusReadOnly        = 11

	// CallError is returned to a calling program when no child ran. It is
	// never a receipt status.
	CallError = 128
)

// StatusAPIName names a status as the reference node's API does.
func StatusAPIName(s uint8) string {
	names := [...]string{"Success", "Revert", "Trap", "Compute", "Access", "Data", "Growth", "Structure",
		"Missing", "InvalidModule", "Exists", "ReadOnly"}
	if int(s) < len(names) {
		return names[s]
	}
	return "Unknown"
}

// StatusName names a status for diagnostics.
func StatusName(s uint8) string {
	switch s {
	case StatusSuccess:
		return "success"
	case StatusRevert:
		return "revert"
	case StatusTrap:
		return "trap"
	case StatusExhaustedC:
		return "compute exhausted"
	case StatusExhaustedS:
		return "state access exhausted"
	case StatusExhaustedD:
		return "data exhausted"
	case StatusExhaustedG:
		return "growth exhausted"
	case StatusOrdinalOverflow:
		return "ordinal overflow"
	case StatusTargetAbsent:
		return "target absent"
	case StatusModuleInvalid:
		return "module invalid"
	case StatusIDExists:
		return "identifier exists"
	case StatusReadOnly:
		return "read-only violation"
	case CallError:
		return "call error"
	}
	return fmt.Sprintf("status(%d)", s)
}

// Event is an emitting ContractId plus program-defined bytes.
type Event struct {
	Contract hashing.Hash
	Payload  []byte
}

// Receipt records one transaction's outcome.
type Receipt struct {
	TransactionID hashing.Hash
	Status        uint8
	ResourcesUsed ResourceVector
	Result        []byte
	Events        []Event
}

// EncodedLen returns the size of the canonical encoding.
func (r *Receipt) EncodedLen() int {
	n := ReceiptFixedSize + len(r.Result)
	for i := range r.Events {
		n += EventFixedSize + len(r.Events[i].Payload)
	}
	return n
}

// Encode returns the canonical encoding.
func (r *Receipt) Encode() []byte {
	w := codec.NewWriter(r.EncodedLen())
	w.Raw(r.TransactionID[:])
	w.U8(r.Status)
	r.ResourcesUsed.Encode(w)
	w.Bytes(r.Result)
	w.U32(uint32(len(r.Events)))
	for i := range r.Events {
		w.Raw(r.Events[i].Contract[:])
		w.Bytes(r.Events[i].Payload)
	}
	return w.Buf
}

// DecodeReceipt parses a receipt's canonical encoding. Validators never trust
// supplied receipts; this exists for tools and APIs.
func DecodeReceipt(b []byte) (*Receipt, error) {
	rd := codec.NewReader(b)
	r := &Receipt{}
	rd.Into(r.TransactionID[:])
	r.Status = rd.U8()
	r.ResourcesUsed = DecodeResourceVector(rd)
	r.Result = rd.Bytes()
	n := rd.U32()
	if rd.Err() != nil {
		return nil, rd.Err()
	}
	if r.Status > StatusReadOnly {
		return nil, fmt.Errorf("invalid receipt status %d", r.Status)
	}
	if r.Status != StatusSuccess && n != 0 {
		return nil, fmt.Errorf("a failed receipt carries %d events", n)
	}
	if uint64(n)*EventFixedSize > uint64(rd.Remaining()) {
		return nil, fmt.Errorf("event count %d exceeds receipt size", n)
	}
	if n > 0 {
		r.Events = make([]Event, n)
	}
	for i := range r.Events {
		rd.Into(r.Events[i].Contract[:])
		r.Events[i].Payload = rd.Bytes()
	}
	rd.End()
	if rd.Err() != nil {
		return nil, rd.Err()
	}
	return r, nil
}

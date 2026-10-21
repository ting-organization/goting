package p2p

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
)

// FuzzMessages checks that decoding a peer's message never panics and that
// whatever decodes re-encodes to the same bytes.
func FuzzMessages(f *testing.F) {
	f.Add(Encode(&HeadersRequest{Locator: []hashing.Hash{{1}, {2}}}))
	f.Add(Encode(&RawRequest{ID: hashing.Hash{3}, Offset: 5, Length: 7}))
	f.Add(EncodeError("block not found"))
	f.Add(EncodeAnnouncement(&Announcement{Height: 3, Work: big.NewInt(70)}))
	f.Add(Encode(&Received{Bytes: 9}))
	f.Fuzz(func(t *testing.T, b []byte) {
		if m, err := DecodeRequest(b); err == nil && !bytes.Equal(Encode(m), b) {
			t.Fatalf("request %T re-encoding differs", m)
		}
		if m, err := DecodeAnswer(b); err == nil && !bytes.Equal(Encode(m), b) {
			t.Fatalf("answer %T re-encoding differs", m)
		}
		if a, err := DecodeAnnouncement(b); err == nil && !bytes.Equal(EncodeAnnouncement(a), b) {
			t.Fatal("announcement re-encoding differs")
		}
	})
}

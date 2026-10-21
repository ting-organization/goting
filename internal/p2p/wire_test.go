package p2p

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ting-organization/goting/internal/hashing"
)

func unhex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic(err)
	}
	return b
}

// The example frames of the peer protocol document.
func TestExampleFrames(t *testing.T) {
	chain := hashing.MustParseHash("15d6549cfacd816f835c632b4c67308d1c63e642f4cbf4049368430670705107")
	port := uint16(18442)
	hello := &Hello{Version: 1, ChainID: chain, Agent: "ting-node/0.1.0", Nonce: 0x0123456789abcdef,
		Announce: true, Listen: &port}
	var buf bytes.Buffer
	WriteFrame(&buf, Encode(hello))
	want := unhex(`45000000 01 00 01000000
15d6549cfacd816f835c632b4c67308d1c63e642f4cbf4049368430670705107
0f00 74696e672d6e6f64652f302e312e30 0000
efcdab8967452301 01 01 0a48`)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("hello frame\n%x\n%x", buf.Bytes(), want)
	}
	m, err := DecodeRequest(want[4:])
	if err != nil || m.(*Hello).Agent != "ting-node/0.1.0" || *m.(*Hello).Listen != 18442 {
		t.Fatalf("decode hello: %v", err)
	}

	gen := hashing.MustParseHash("24b76a52f3825b921373b6c6a41ced2a74de7dbdb34015669c10594996887459")
	buf.Reset()
	WriteFrame(&buf, Encode(&HeadersRequest{Locator: []hashing.Hash{gen}}))
	want = unhex(`24000000 01 02 0100
24b76a52f3825b921373b6c6a41ced2a74de7dbdb34015669c10594996887459`)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("headers frame %x", buf.Bytes())
	}

	b1 := hashing.MustParseHash("000000004eba8c8bd3488debecab68017d3720b9f5a0da24ab80ffce4b57d1ea")
	buf.Reset()
	WriteFrame(&buf, Encode(&RawRequest{ID: b1, Offset: 0, Length: 1 << 20}))
	want = unhex(`2e000000 01 03
000000004eba8c8bd3488debecab68017d3720b9f5a0da24ab80ffce4b57d1ea
0000000000000000 00001000`)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("raw frame %x", buf.Bytes())
	}
}

func TestStrictDecoding(t *testing.T) {
	// A flag of 2, trailing bytes and a work with a leading zero are no message.
	bad := [][]byte{
		{KindRequest, TagInventory, 2, 0},
		{KindRequest, TagState, 0},
		{KindAnnouncement},
		{KindRequest, 99},
	}
	for _, b := range bad {
		if _, err := DecodeRequest(b); err == nil {
			if _, err := DecodeAnnouncement(b); err == nil {
				t.Fatalf("accepted %x", b)
			}
		}
	}
	e := &enc{b: []byte{KindAnnouncement}}
	e.id(hashing.Hash{})
	e.u64(1)
	e.u8(2)
	e.b = append(e.b, 0, 1) // leading zero
	e.u64(0)
	e.u64(0)
	if _, err := DecodeAnnouncement(e.b); err == nil {
		t.Fatal("non-minimal work accepted")
	}
	// Error text cut at a character boundary.
	long := strings.Repeat("é", 600) // 1,200 bytes
	p := EncodeError(long)
	_, err := DecodeAnswer(p)
	ea, ok := err.(*ErrorAnswer)
	if !ok || len(ea.Text) > MaxErrorText || !strings.HasPrefix(long, ea.Text) {
		t.Fatalf("error text: %v", err)
	}
}

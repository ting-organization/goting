package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/samples"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

func getJSON(t *testing.T, url string, want int) map[string]any {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	if r.StatusCode != want {
		t.Fatalf("GET %s: %d %v", url, r.StatusCode, out)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("protective headers missing")
	}
	return out
}

func postJSON(t *testing.T, url, body string, want int) map[string]any {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	if r.StatusCode != want {
		t.Fatalf("POST %s: %d %v", url, r.StatusCode, out)
	}
	return out
}

// A node mines what the REST API submits, and the API reads it back in
// the reference node's shapes.
func TestRESTAPI(t *testing.T) {
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(c, Config{Name: "test", Mine: true, Producer: []byte("t"), Workers: 2, Interval: 50 * time.Millisecond}, log)
	srv := httptest.NewServer(n.API(APIOptions{}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	u := srv.URL

	deploy := (&types.Transaction{Kind: types.KindDeploy, Budget: types.ResourceVector{50_000_000, 64, 10_000, 10_000},
		Code: samples.Counter()}).Encode()
	id := types.TransactionID(c.ChainID(), deploy)
	sub := postJSON(t, u+"/v1/transactions", `{"hex":"`+hex.EncodeToString(deploy)+`"}`, 200)
	if sub["id"] != id.String() || sub["known"] != false {
		t.Fatalf("admission %v", sub)
	}
	// Strict bodies and lowercase hex.
	postJSON(t, u+"/v1/transactions", `{"hex":"00","extra":1}`, 422)
	postJSON(t, u+"/v1/transactions", `{"hex":"`+strings.ToUpper(hex.EncodeToString(deploy))+`"}`, 400)
	if r, _ := http.Post(u+"/v1/transactions", "text/plain", strings.NewReader("{}")); r.StatusCode != 415 {
		t.Fatalf("content type: %d", r.StatusCode)
	}

	var occ map[string]any
	waitFor(t, "the deployment to be mined", func() bool { return len(n.Occurrences(id)) == 1 })
	occ = getJSON(t, u+"/v1/transactions/"+id.String(), 200)
	list := occ["occurrences"].([]any)
	rc := list[0].(map[string]any)["receipt"].(map[string]any)
	if rc["status_name"] != "Success" || occ["pending"] != false || occ["has_more"] != false {
		t.Fatalf("lookup %v", occ)
	}
	contract := hashing.MustParseHash(rc["result_hex"].(string))
	height := uint64(list[0].(map[string]any)["height"].(float64))

	// The block, by height and id, and its raw bytes.
	b := getJSON(t, u+"/v1/blocks/"+itoa(int(height)), 200)
	if b["canonical"] != true || b["producer_hex"] != "74" || b["limits"] == nil || len(b["nonce"].(string)) == 0 {
		t.Fatalf("block %v", b)
	}
	getJSON(t, u+"/v1/blocks/"+b["id"].(string), 200)
	raw := getJSON(t, u+"/v1/blocks/"+b["id"].(string)+"/raw?offset=1&length=10", 200)
	if raw["offset"] != 1.0 || len(raw["hex"].(string)) != 20 {
		t.Fatalf("raw %v", raw)
	}
	getJSON(t, u+"/v1/blocks/0", 400)
	getJSON(t, u+"/v1/blocks/99999", 404)
	getJSON(t, u+"/v1/blocks?limit=101", 400)
	getJSON(t, u+"/v1/blocks?bogus=1", 400)
	page := getJSON(t, u+"/v1/blocks?from=1&limit=2", 200)
	if len(page["items"].([]any)) != 2 || page["next"] != 3.0 {
		t.Fatalf("page %v", page)
	}

	// Calls emit events; the program's events page skips other blocks.
	call := (&types.Transaction{Kind: types.KindCall, Budget: types.ResourceVector{5_000_000, 64, 10_000, 10_000},
		Target: contract}).Encode()
	cid := types.TransactionID(c.ChainID(), call)
	postJSON(t, u+"/v1/transactions", `{"hex":"`+hex.EncodeToString(call)+`"}`, 200)
	waitFor(t, "the call to be mined", func() bool { return len(n.Occurrences(cid)) == 1 })
	ev := getJSON(t, u+"/v1/events?contract="+contract.String(), 200)
	items := ev["items"].([]any)
	// The deployment's initializer ran the program too.
	if len(items) != 2 || ev["next"].(float64) <= float64(height) {
		t.Fatalf("events %v", ev)
	}
	e0 := items[1].(map[string]any)["events"].([]any)[0].(map[string]any)
	if e0["contract"] != contract.String() || e0["transaction"] != cid.String() {
		t.Fatalf("event %v", e0)
	}
	getJSON(t, u+"/v1/events?topic="+contract.String(), 400)

	// Contract, storage with a proof that checks, and a batch.
	ct := getJSON(t, u+"/v1/contracts/"+contract.String(), 200)
	if ct["code_bytes"].(float64) != float64(len(samples.Counter())) {
		t.Fatalf("contract %v", ct)
	}
	var zero [32]byte
	st := getJSON(t, u+"/v1/contracts/"+contract.String()+"/storage/"+hex.EncodeToString(zero[:])+"?proof=true", 200)
	if st["value_hex"] != "0200000000000000" {
		t.Fatalf("storage %v", st)
	}
	pr := st["proof"].(map[string]any)
	var sibs []hashing.Hash
	for _, s := range pr["siblings"].([]any) {
		sibs = append(sibs, hashing.MustParseHash(s.(string)))
	}
	value, _ := hex.DecodeString(st["value_hex"].(string))
	if pr["end"] != "present" || !state.VerifyProof(hashing.MustParseHash(st["state_root"].(string)),
		types.StorageKey(contract, &zero), value, true, state.Proof{End: state.EndPresent, Siblings: sibs}) {
		t.Fatalf("proof %v", pr)
	}
	absent := getJSON(t, u+"/v1/contracts/"+contract.String()+"/storage/"+strings.Repeat("ab", 32), 200)
	if absent["value_hex"] != nil {
		t.Fatal("absent record has a value")
	}
	batch := postJSON(t, u+"/v1/storage", `{"reads":[{"contract":"`+contract.String()+`","key":"`+
		hex.EncodeToString(zero[:])+`"}],"proof":true}`, 200)
	if len(batch["values"].([]any)) != 1 || batch["others"] == nil {
		t.Fatalf("batch %v", batch)
	}
	postJSON(t, u+"/v1/storage", `{"reads":[]}`, 400)
	getJSON(t, u+"/v1/contracts/"+strings.Repeat("00", 32), 404)

	// Status, votes, mempool, health and the fallback.
	s := getJSON(t, u+"/v1/status", 200)
	if s["name"] != "test" || s["network"] != "development" || s["resource_order"] == nil || s["stored_blocks"].(float64) < 2 {
		t.Fatalf("status %v", s)
	}
	v := getJSON(t, u+"/v1/votes", 200)
	if len(v["capacities"].([]any)) != 3 {
		t.Fatalf("votes %v", v)
	}
	getJSON(t, u+"/v1/mempool", 200)
	getJSON(t, u+"/health", 200)
	if out := getJSON(t, u+"/nowhere", 404); out["error"] != "endpoint not found" {
		t.Fatalf("fallback %v", out)
	}
	if n.PoolLen() != 0 {
		t.Fatal("mempool not cleared")
	}
	_ = bytes.NewReader
}

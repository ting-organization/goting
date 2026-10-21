package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/consensus"
	"github.com/ting-organization/goting/internal/hashing"
	"github.com/ting-organization/goting/internal/state"
	"github.com/ting-organization/goting/internal/types"
)

// The REST API follows the reference node's (wiki: start/explore, the
// node's openapi.json): JSON under /v1 and /health, lowercase hex, resource
// vectors in C, S, D, G order, pages of at most 100 entries that stop
// before 16 MiB. It is a node interface, not part of the protocol.
const (
	// DefaultAPIAddr is the reference node's REST address.
	DefaultAPIAddr = "127.0.0.1:18441"
	// apiSlotsPerCore bounds requests in progress, per processor core.
	apiSlotsPerCore = 32
	apiDeadline     = 35 * time.Second
	// frameMax bounds request bodies other than submissions and the JSON of
	// one answer.
	frameMax  = 16 << 20
	answerMax = frameMax - 1024
	sliceMax  = 1 << 20
	pageMax   = 100
	pageSize  = 20
	batchMax  = 1024
	// Version is this node's version in its status.
	Version = "0.3.0"
)

// APIOptions configures the REST API.
type APIOptions struct {
	// CORS lets browser applications from any origin use the API.
	CORS bool
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

// API returns the REST API's handler.
func (n *Node) API(opts APIOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", n.apiHealth)
	mux.HandleFunc("GET /v1/status", n.apiStatus)
	mux.HandleFunc("GET /v1/metrics", n.apiStatus)
	mux.HandleFunc("GET /v1/blocks", n.apiBlocks)
	mux.HandleFunc("GET /v1/blocks/{id}", n.apiBlock)
	mux.HandleFunc("GET /v1/blocks/{id}/raw", n.apiRaw)
	mux.HandleFunc("GET /v1/transactions/{id}", n.apiTransaction)
	mux.HandleFunc("POST /v1/transactions", n.apiSubmit)
	mux.HandleFunc("GET /v1/mempool", n.apiMempool)
	mux.HandleFunc("GET /v1/votes", n.apiVotes)
	mux.HandleFunc("GET /v1/events", n.apiEvents)
	mux.HandleFunc("GET /v1/contracts/{id}", n.apiContract)
	mux.HandleFunc("GET /v1/contracts/{id}/storage/{key}", n.apiStorage)
	mux.HandleFunc("POST /v1/storage", n.apiStorageBatch)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
	})
	slots := make(chan struct{}, apiSlotsPerCore*runtime.NumCPU())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if opts.CORS {
			h.Set("Access-Control-Allow-Origin", "*")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "*")
				h.Set("Access-Control-Allow-Headers", "*")
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "HTTP service busy"})
			return
		}
		// A submission also has the time its body may take to arrive, as a
		// peer has for a transaction of that size.
		deadline := apiDeadline
		if r.Method == http.MethodPost {
			deadline += chain.TransferTime(n.submissionLimit()) - chain.TransferTime(0)
		}
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()
		tw := &timeoutWriter{w: w, h: make(http.Header)}
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.Body = http.MaxBytesReader(tw, r.Body, frameMax)
			mux.ServeHTTP(tw, r.WithContext(ctx))
		}()
		select {
		case <-done:
			tw.flush()
		case <-ctx.Done():
			tw.abandon()
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "HTTP deadline exceeded"})
		}
	})
}

// timeoutWriter buffers a response so that a request past its deadline can
// be answered with an error instead.
type timeoutWriter struct {
	w    http.ResponseWriter
	h    http.Header
	buf  bytes.Buffer
	code int
	mu   chanMutex
	gone bool
}

type chanMutex struct{ ch chan struct{} }

func (m *chanMutex) lock() {
	if m.ch == nil {
		m.ch = make(chan struct{}, 1)
	}
	m.ch <- struct{}{}
}
func (m *chanMutex) unlock() { <-m.ch }

func (t *timeoutWriter) Header() http.Header { return t.h }
func (t *timeoutWriter) WriteHeader(code int) {
	if t.code == 0 {
		t.code = code
	}
}
func (t *timeoutWriter) Write(b []byte) (int, error) {
	t.mu.lock()
	defer t.mu.unlock()
	if t.gone {
		return 0, http.ErrHandlerTimeout
	}
	if t.code == 0 {
		t.code = http.StatusOK
	}
	return t.buf.Write(b)
}
func (t *timeoutWriter) abandon() {
	t.mu.lock()
	t.gone = true
	t.mu.unlock()
}
func (t *timeoutWriter) flush() {
	for k, v := range t.h {
		t.w.Header()[k] = v
	}
	if t.code == 0 {
		t.code = http.StatusOK
	}
	t.w.WriteHeader(t.code)
	t.w.Write(t.buf.Bytes())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// respond answers with v, or with err as the reference maps node errors:
// what the client asked for is 400 or 404; what the node cannot do right
// now is 503, or 504 when it ran out of time.
func respond(w http.ResponseWriter, v any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.code, map[string]string{"error": ae.msg})
		return
	}
	msg := err.Error()
	code := http.StatusBadRequest
	switch {
	case strings.Contains(msg, "not found"):
		code = http.StatusNotFound
	case strings.Contains(msg, "timed out"):
		code = http.StatusGatewayTimeout
	case strings.Contains(msg, "queue"), strings.Contains(msg, "busy"), strings.Contains(msg, "is full"),
		strings.Contains(msg, "restart required"), strings.Contains(msg, "stopped"), strings.Contains(msg, "storage"):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

// query reads a request's query parameters, refusing any not listed and any
// given twice, as the reference's strict query decoding does.
func query(r *http.Request, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	for k, vs := range r.URL.Query() {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return nil, &apiError{http.StatusBadRequest, fmt.Sprintf("Failed to deserialize query string: unknown field `%s`", k)}
		}
		if len(vs) != 1 {
			return nil, &apiError{http.StatusBadRequest, fmt.Sprintf("Failed to deserialize query string: duplicate field `%s`", k)}
		}
		out[k] = vs[0]
	}
	return out, nil
}

// u64Query reads an optional non-negative integer parameter.
func u64Query(q map[string]string, key string) (*uint64, error) {
	s, ok := q[key]
	if !ok {
		return nil, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, fmt.Sprintf("Failed to deserialize query string: %s: invalid digit found in string", key)}
	}
	return &v, nil
}

// unhex decodes lowercase hex of at most max bytes.
func unhex(s string, max uint64) ([]byte, error) {
	if len(s)%2 != 0 || uint64(len(s)/2) > max {
		return nil, errors.New("invalid hex length")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, errors.New("hex must be lowercase")
		}
	}
	return hex.DecodeString(s)
}

func hashArg(s string) (hashing.Hash, error) {
	b, err := unhex(s, 32)
	if err != nil {
		return hashing.Hash{}, err
	}
	if len(b) != 32 {
		return hashing.Hash{}, errors.New("expected 32-byte hash")
	}
	return hashing.Hash(b), nil
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// blockRef resolves a decimal height on the canonical chain or the id of a
// stored block.
func (n *Node) blockRef(text string) (*chain.Entry, error) {
	if len(text) != 64 && text != "" && strings.Trim(text, "0123456789") == "" {
		h, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return nil, errors.New("block not found")
		}
		if h == 0 {
			return nil, errors.New("genesis has no ordinary block body")
		}
		e, ok := n.Chain.AtHeight(h)
		if !ok {
			return nil, errors.New("block not found")
		}
		return e, nil
	}
	id, err := hashArg(text)
	if err != nil {
		return nil, err
	}
	e, ok := n.Chain.Get(id)
	if !ok || !n.Chain.HasBlock(id) {
		return nil, errors.New("block not found")
	}
	return e, nil
}

// targetHex is a target as 32 big-endian bytes.
func targetHex(t types.Target) string { return hexOf(t[:]) }

func nonceString(nc types.Nonce) string {
	v := new(big.Int).SetUint64(nc.Hi)
	v.Lsh(v, 64)
	v.Or(v, new(big.Int).SetUint64(nc.Lo))
	return v.String()
}

func receiptJSON(r *types.Receipt) map[string]any {
	events := make([]map[string]any, len(r.Events))
	for i, e := range r.Events {
		events[i] = map[string]any{"contract": e.Contract, "payload_hex": hexOf(e.Payload)}
	}
	return map[string]any{"transaction": r.TransactionID, "status": r.Status,
		"status_name": types.StatusAPIName(r.Status), "used": r.ResourcesUsed,
		"result_hex": hexOf(r.Result), "events": events}
}

func transactionSummary(tx *types.Transaction, encoded int) map[string]any {
	out := map[string]any{"kind": "call", "target": nil, "input_bytes": len(tx.Input), "code_bytes": nil,
		"budget": tx.Budget, "encoded_bytes": encoded}
	if tx.Kind == types.KindDeploy {
		out["kind"], out["code_bytes"] = "deploy", len(tx.Code)
	} else {
		out["target"] = tx.Target
	}
	return out
}

// inlineHex is a transaction's hex, or null beyond a slice: a canonical one
// is then read from its block's slices.
func inlineHex(raw []byte) any {
	if len(raw) > sliceMax {
		return nil
	}
	return hexOf(raw)
}

func (n *Node) blockJSON(e *chain.Entry) (map[string]any, error) {
	raw, err := n.Chain.Block(e.ID)
	if err != nil {
		return nil, errors.New("block not found")
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return nil, fmt.Errorf("stored block: %v", err)
	}
	h := e.Header
	txs := make([]hashing.Hash, len(body.Transactions))
	for i, t := range body.Transactions {
		txs[i] = types.TransactionID(n.Chain.ChainID(), t)
	}
	receipts := make([]map[string]any, len(body.Receipts))
	for i, rb := range body.Receipts {
		r, err := types.DecodeReceipt(rb)
		if err != nil {
			return nil, fmt.Errorf("stored receipt: %v", err)
		}
		receipts[i] = receiptJSON(r)
	}
	var limits any
	if e.Status == chain.StatusValid {
		limits = e.Vote.Limits
	}
	return map[string]any{"id": e.ID, "height": h.Height, "parent": h.ParentID, "timestamp": h.Timestamp,
		"target": targetHex(h.Target), "work": e.ChainWork.String(), "canonical": n.Chain.IsCanonical(e),
		"state_root": h.StateRoot, "transactions_root": h.TransactionsRoot, "receipts_root": h.ReceiptsRoot,
		"used": h.ResourcesUsed, "limits": limits, "votes": h.Votes, "producer_hex": hexOf(body.ProducerData),
		"producer_hash": h.ProducerHash, "nonce": nonceString(h.Nonce), "bytes": len(raw),
		"transactions": txs, "receipts": receipts}, nil
}

// pushBounded adds an item to a page unless the page would pass the answer
// limit; the first item alone may not.
func pushBounded(items *[]any, item any, size *int) (bool, error) {
	b, err := json.Marshal(item)
	if err != nil {
		return false, err
	}
	if *size+len(b)+1 > frameMax-4096 {
		if len(*items) == 0 {
			return false, errors.New("object exceeds JSON response limit; fetch canonical raw block instead")
		}
		return false, nil
	}
	*size += len(b) + 1
	*items = append(*items, item)
	return true, nil
}

func pageParams(q map[string]string, defFrom uint64) (uint64, uint64, error) {
	from, err := u64Query(q, "from")
	if err != nil {
		return 0, 0, err
	}
	limit, err := u64Query(q, "limit")
	if err != nil {
		return 0, 0, err
	}
	f, l := defFrom, uint64(pageSize)
	if from != nil {
		f = *from
	}
	if limit != nil {
		l = *limit
	}
	if l > pageMax {
		return 0, 0, errors.New("pagination limit is 100")
	}
	return f, l, nil
}

// Status is the node's status document, with the reference's fields.
func (n *Node) Status() map[string]any {
	c := n.Chain
	tip := c.Tip()
	headers, stored, best := c.Counts()
	var root hashing.Hash
	records := 0
	if st, err := c.StateAt(tip); err == nil {
		root, records = st.Root(), st.Len()
	}
	marked := []string{}
	for _, id := range c.Marks() {
		marked = append(marked, id.String())
	}
	warnings := []string{}
	if w := c.ForkWarning(); w != "" {
		warnings = append(warnings, w)
	}
	var reorg any
	if r := c.LastReorganization(); r != nil {
		reorg = r
	}
	pending, bytes := n.PoolLen(), n.PoolBytes()
	return map[string]any{"name": n.cfg.Name, "version": Version, "network": n.network(),
		"chain_id": c.ChainID(), "genesis": c.GenesisID(), "height": tip.Height, "tip": tip.ID,
		"work": tip.ChainWork.String(), "state_root": root, "limits": n.nextLimits(),
		"resource_order": []string{"C", "S", "D", "G"}, "mempool_count": pending, "mempool_bytes": bytes,
		"deferred_count": n.DeferredLen(), "producer_hex": hexOf(n.Producer()),
		"stored_blocks": stored, "best_height": best, "state_records": records,
		"uptime_seconds": uint64(time.Since(n.started).Seconds()), "undo_pruned": 0, "settled_height": 0,
		"held_headers": headers, "replaying": false, "marked": marked, "reorganization": reorg,
		"warnings": warnings}
}

func (n *Node) network() string {
	switch n.Chain.ChainID() {
	case types.ProductionGenesis().ChainID():
		return "production"
	case types.DevelopmentGenesis().ChainID():
		return "development"
	}
	return "other"
}

func (n *Node) apiHealth(w http.ResponseWriter, r *http.Request) {
	tip := n.Chain.Tip()
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "height": tip.Height, "tip": tip.ID})
}

func (n *Node) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, n.Status())
}

func (n *Node) apiBlock(w http.ResponseWriter, r *http.Request) {
	e, err := n.blockRef(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	v, err := n.blockJSON(e)
	respond(w, v, err)
}

func (n *Node) apiBlocks(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "from", "limit")
	if err != nil {
		respond(w, nil, err)
		return
	}
	from, limit, err := pageParams(q, 1)
	if err != nil {
		respond(w, nil, err)
		return
	}
	from = max(from, 1)
	items, size := []any{}, 0
	tip := n.Chain.Tip()
	for h := from; h < from+limit; h++ {
		e, ok := n.Chain.AtHeight(h)
		if !ok {
			break
		}
		v, err := n.blockJSON(e)
		if err != nil {
			respond(w, nil, err)
			return
		}
		more, err := pushBounded(&items, v, &size)
		if err != nil {
			respond(w, nil, err)
			return
		}
		if !more {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "from": from, "next": from + uint64(len(items)),
		"tip": tip.ID, "height": tip.Height})
}

// apiRaw serves a block this node executed and found valid, whole or in
// slices of at most 1 MiB; a block beyond about 8 MiB only in slices.
func (n *Node) apiRaw(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "offset", "length")
	if err != nil {
		respond(w, nil, err)
		return
	}
	e, err := n.blockRef(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if e.Status != chain.StatusValid {
		respond(w, nil, errors.New("block not found"))
		return
	}
	raw, err := n.Chain.Block(e.ID)
	if err != nil {
		respond(w, nil, errors.New("block not found"))
		return
	}
	v, err := slice(raw, q, "block")
	respond(w, v, err)
}

func slice(b []byte, q map[string]string, what string) (any, error) {
	off, err := u64Query(q, "offset")
	if err != nil {
		return nil, err
	}
	length, err := u64Query(q, "length")
	if err != nil {
		return nil, err
	}
	start := uint64(0)
	if off != nil {
		start = *off
	}
	if start > uint64(len(b)) {
		return nil, errors.New("slice offset out of range")
	}
	end := uint64(len(b))
	switch {
	case length != nil && *length > sliceMax:
		return nil, errors.New("slice length limit")
	case length != nil:
		end = min(start+*length, uint64(len(b)))
	case len(b) > frameMax/2-4096:
		return nil, fmt.Errorf("%s exceeds one response; fetch slices with offset and length", what)
	}
	return map[string]any{"hex": hexOf(b[start:end]), "offset": start, "total": len(b)}, nil
}

func deferredJSON(d *Deferred) any {
	if d == nil {
		return nil
	}
	var last, result any
	if d.LastAttempt > 0 {
		last = d.LastAttempt
	}
	if d.LastResult != "" {
		result = d.LastResult
	}
	return map[string]any{"not_before": d.NotBefore, "expires": d.Expires, "admitted": d.Admitted,
		"last_attempt": last, "last_result": result}
}

// apiTransaction answers a transaction's canonical occurrences, a page at a
// time, with whether it is pending or deferred.
func (n *Node) apiTransaction(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "from", "limit")
	if err != nil {
		respond(w, nil, err)
		return
	}
	id, err := hashArg(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	from, limit, err := pageParams(q, 0)
	if err != nil {
		respond(w, nil, err)
		return
	}
	locs := n.Occurrences(id)
	raw, d, pooled := n.GetTx(id)
	if len(locs) == 0 && !pooled && d == nil {
		respond(w, nil, errors.New("transaction not found"))
		return
	}
	occ, size, more := []any{}, 0, false
	for i := from; i < uint64(len(locs)); i++ {
		if uint64(len(occ)) >= limit {
			more = true
			break
		}
		l := locs[i]
		braw, err := n.Chain.Block(l.Block)
		if err != nil {
			respond(w, nil, err)
			return
		}
		body, err := types.DecodeBody(braw[types.HeaderSize:])
		if err != nil || l.Index >= len(body.Transactions) {
			respond(w, nil, errors.New("invalid transaction index"))
			return
		}
		txRaw := body.Transactions[l.Index]
		tx, err := types.DecodeTransaction(txRaw)
		rc, rerr := types.DecodeReceipt(body.Receipts[l.Index])
		if err != nil || rerr != nil {
			respond(w, nil, errors.New("transaction index identity mismatch"))
			return
		}
		item := map[string]any{"block": l.Block, "height": l.Height, "index": l.Index, "receipt": receiptJSON(rc),
			"hex": inlineHex(txRaw), "transaction": transactionSummary(tx, len(txRaw))}
		ok, err := pushBounded(&occ, item, &size)
		if err != nil {
			respond(w, nil, err)
			return
		}
		if !ok {
			more = true
			break
		}
	}
	var pendingTx any
	if raw != nil {
		if tx, err := types.DecodeTransaction(raw); err == nil {
			pendingTx = map[string]any{"hex": inlineHex(raw), "transaction": transactionSummary(tx, len(raw))}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "occurrences": occ, "from": from,
		"next": from + uint64(len(occ)), "has_more": more, "tip": n.Chain.Tip().ID, "pending": pooled,
		"deferred": deferredJSON(d), "pending_transaction": pendingTx})
}

// submissionLimit is the largest body a submission may have: a transaction
// of the next block's data capacity, in hex, and 4 KiB for the rest.
func (n *Node) submissionLimit() uint64 {
	d := n.nextLimits()[types.D]
	if d > (^uint64(0)-4096)/2 {
		return ^uint64(0)
	}
	return 2*d + 4096
}

// clientAddress is the address a public submission counts against: the
// client's own or, on a connection from this machine, the last address a
// reverse proxy on it appended to X-Forwarded-For. None for a local client
// without one, which is the operator's own.
func clientAddress(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !peer.IsLoopback() {
		return peer
	}
	var last string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		parts := strings.Split(v, ",")
		last = parts[len(parts)-1]
	}
	a, err := netip.ParseAddr(strings.TrimSpace(last))
	if err != nil || a.Unmap().IsLoopback() {
		return netip.Addr{}
	}
	return a.Unmap()
}

// decodeStrict decodes a JSON body refusing unknown fields, as the
// reference's extractor does: 415 without a JSON content type, 400 for bad
// syntax, 422 for a body of the wrong shape.
func decodeStrict(r *http.Request, body []byte, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return &apiError{http.StatusUnsupportedMediaType, "Expected request with `Content-Type: application/json`"}
	}
	if !json.Valid(body) {
		return &apiError{http.StatusBadRequest, "Failed to parse the request body as JSON"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &apiError{http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: " + err.Error()}
	}
	return nil
}

func (n *Node) apiSubmit(w http.ResponseWriter, r *http.Request) {
	limit := n.submissionLimit()
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(min(limit, 1<<62))+1))
	if err != nil || uint64(len(body)) > limit {
		respond(w, nil, &apiError{http.StatusRequestEntityTooLarge, "request body larger than a transaction can be"})
		return
	}
	var p struct {
		Hex       *string `json:"hex"`
		NotBefore *uint64 `json:"not_before"`
	}
	if err := decodeStrict(r, body, &p); err != nil {
		respond(w, nil, err)
		return
	}
	if p.Hex == nil {
		respond(w, nil, &apiError{http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: missing field `hex`"})
		return
	}
	raw, err := unhex(*p.Hex, n.nextLimits()[types.D])
	if err != nil {
		respond(w, nil, err)
		return
	}
	res, err := n.Submit(raw, p.NotBefore, clientAddress(r), false)
	respond(w, res, err)
}

func (n *Node) apiMempool(w http.ResponseWriter, r *http.Request) {
	items, size := []any{}, 0
	for _, p := range n.PooledItems(1024) {
		_, d, _ := n.GetTx(p.ID)
		item := map[string]any{"id": p.ID, "bytes": len(p.Raw), "budget": p.Tx.Budget, "deferred": deferredJSON(d)}
		if ok, err := pushBounded(&items, item, &size); err != nil || !ok {
			break
		}
	}
	deferred := []any{}
	for _, d := range n.DeferredItems() {
		item := map[string]any{"id": d.ID, "bytes": len(d.Raw), "budget": d.Tx.Budget, "deferred": deferredJSON(d)}
		if ok, err := pushBounded(&deferred, item, &size); err != nil || !ok {
			break
		}
	}
	bytes := n.PoolBytes()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "bytes": bytes, "deferred": deferred})
}

// Voting describes where capacity voting stands at the tip (rules §10):
// the epoch the next block belongs to, the share of its blocks' work so far
// voting each way and what that would decide, and the capacities of this
// epoch and the next two, which earlier epochs' votes already decided.
func (n *Node) Voting() map[string]any {
	g := n.Chain.Genesis()
	e := uint64(g.VoteEpochLength)
	tip := n.Chain.Tip()
	height := tip.Height
	epoch := height / e
	lowest := uint64(1)
	if epoch >= 2 {
		lowest = (epoch-2)*e + 1
	}
	tallies := map[uint64]*consensus.Tally{}
	for b := tip; b.Height >= lowest && b.Height > 0; b = b.Parent {
		k := (b.Height - 1) / e
		if tallies[k] == nil {
			tallies[k] = consensus.NewTally()
		}
		tallies[k].Add(b.Votes, b.BlockWork)
	}
	limits := n.nextLimits()
	capacities := []any{map[string]any{"epoch": epoch, "from": epoch*e + 1, "limits": limits}}
	for next := epoch + 1; next <= epoch+2; next++ {
		if next >= 3 {
			t := tallies[next-3]
			if t == nil {
				t = consensus.NewTally()
			}
			limits = consensus.ApplyDirections(limits, t.Decide(), g.MinimumLimits)
		}
		capacities = append(capacities, map[string]any{"epoch": next, "from": next*e + 1, "limits": limits})
	}
	cur := tallies[epoch]
	if cur == nil {
		cur = consensus.NewTally()
	}
	percent := func(part *big.Int) float64 {
		if cur.W.Sign() == 0 {
			return 0
		}
		basis := new(big.Int).Div(new(big.Int).Mul(part, big.NewInt(10000)), cur.W)
		if !basis.IsUint64() {
			return 100
		}
		return float64(basis.Uint64()) / 100
	}
	var up, down [4]float64
	var outcome [4]string
	dirs := cur.Decide()
	for i := 0; i < 4; i++ {
		up[i], down[i] = percent(cur.Up[i]), percent(cur.Down[i])
		outcome[i] = [...]string{"hold", "increase", "decrease"}[dirs[i]]
	}
	return map[string]any{"height": height, "epoch": epoch, "epoch_blocks": e, "next_epoch_from": (epoch+1)*e + 1,
		"this_epoch": map[string]any{"blocks": height - epoch*e, "increase_percent": up, "decrease_percent": down,
			"outcome_so_far": outcome, "takes_effect_from": (epoch+3)*e + 1},
		"capacities": capacities}
}

func (n *Node) apiVotes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, n.Voting())
}

func eventJSON(tx int, r *types.Receipt, i int) map[string]any {
	e := r.Events[i]
	return map[string]any{"transaction_index": tx, "transaction": r.TransactionID, "event_index": i,
		"contract": e.Contract, "payload_hex": hexOf(e.Payload)}
}

func (n *Node) receiptsOf(id hashing.Hash) (*types.Header, []*types.Receipt, error) {
	raw, err := n.Chain.Block(id)
	if err != nil {
		return nil, nil, err
	}
	h, err := types.DecodeHeader(raw[:types.HeaderSize])
	if err != nil {
		return nil, nil, err
	}
	body, err := types.DecodeBody(raw[types.HeaderSize:])
	if err != nil {
		return nil, nil, err
	}
	rs := make([]*types.Receipt, len(body.Receipts))
	for i, b := range body.Receipts {
		if rs[i], err = types.DecodeReceipt(b); err != nil {
			return nil, nil, err
		}
	}
	return h, rs, nil
}

// apiEvents pages events by canonical block: of every program, or of one
// program or one of its topics, skipping blocks without any.
func (n *Node) apiEvents(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "from", "limit", "contract", "topic")
	if err != nil {
		respond(w, nil, err)
		return
	}
	from, limit, err := pageParams(q, 1)
	if err != nil {
		respond(w, nil, err)
		return
	}
	from = max(from, 1)
	tip := n.Chain.Tip()
	items, size := []any{}, 0
	contract, hasContract := q["contract"]
	topicText, hasTopic := q["topic"]
	if !hasContract {
		if hasTopic {
			respond(w, nil, errors.New("a topic is read with its program: give contract too"))
			return
		}
		for h := from; h < from+limit; h++ {
			e, ok := n.Chain.AtHeight(h)
			if !ok {
				break
			}
			hdr, rs, err := n.receiptsOf(e.ID)
			if err != nil {
				respond(w, nil, err)
				return
			}
			events := []any{}
			for t, rc := range rs {
				for i := range rc.Events {
					events = append(events, eventJSON(t, rc, i))
				}
			}
			ok, err = pushBounded(&items, map[string]any{"block": e.ID, "height": hdr.Height,
				"timestamp": hdr.Timestamp, "events": events}, &size)
			if err != nil {
				respond(w, nil, err)
				return
			}
			if !ok {
				break
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "from": from, "next": from + uint64(len(items)),
			"tip": tip.ID, "height": tip.Height})
		return
	}
	cid, err := hashArg(contract)
	if err != nil {
		respond(w, nil, err)
		return
	}
	var topic *hashing.Hash
	if hasTopic {
		t, err := hashArg(topicText)
		if err != nil {
			respond(w, nil, err)
			return
		}
		topic = &t
	}
	found, more := n.EventsOf(cid, topic, from, int(limit))
	var next *uint64
	for _, places := range found {
		_, rs, err := n.receiptsOf(places[0].Block)
		if err != nil {
			respond(w, nil, err)
			return
		}
		hdr, _ := n.Chain.Get(places[0].Block)
		events := []any{}
		for _, p := range places {
			if p.Tx >= len(rs) || p.Event >= len(rs[p.Tx].Events) {
				respond(w, nil, errors.New("stored event out of place"))
				return
			}
			events = append(events, eventJSON(p.Tx, rs[p.Tx], p.Event))
		}
		ok, err := pushBounded(&items, map[string]any{"block": places[0].Block, "height": places[0].Height,
			"timestamp": hdr.Timestamp, "events": events}, &size)
		if err != nil {
			respond(w, nil, err)
			return
		}
		if !ok {
			h := places[0].Height
			next = &h
			break
		}
	}
	if next == nil {
		var v uint64
		switch {
		case len(found) > 0 && more:
			v = found[len(found)-1][0].Height + 1
		case len(found) == 0 && more:
			v = from
		default:
			v = max(from, tip.Height+1)
		}
		next = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "from": from, "next": *next,
		"tip": tip.ID, "height": tip.Height})
}

// tipView is the tip's state, which reads in one answer come from.
func (n *Node) tipView() (*chain.Entry, state.Tree, error) {
	tip := n.Chain.Tip()
	st, err := n.Chain.StateAt(tip)
	if err != nil {
		return nil, state.Tree{}, fmt.Errorf("storage: %v", err)
	}
	return tip, st, nil
}

func (n *Node) apiContract(w http.ResponseWriter, r *http.Request) {
	id, err := hashArg(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	_, st, err := n.tipView()
	if err != nil {
		respond(w, nil, err)
		return
	}
	mb, ok := st.Get(types.ManifestKey(id))
	if !ok {
		respond(w, nil, errors.New("contract not found"))
		return
	}
	m, err := types.DecodeManifest(mb)
	if err != nil {
		respond(w, nil, errors.New("corrupt contract manifest"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "code_hash": m.CodeHash, "code_bytes": m.CodeLength,
		"code_price": m.CodePrice, "state_root": st.Root()})
}

// recordJSON is a storage record, and with others its Merkle proof.
func recordJSON(st state.Tree, contract, key hashing.Hash, others map[string]string) map[string]any {
	k := [32]byte(key)
	rk := types.StorageKey(contract, &k)
	var value any
	if v, ok := st.Get(rk); ok {
		value = hexOf(v)
	}
	out := map[string]any{"contract": contract, "key": key, "value_hex": value}
	if others != nil {
		p := st.Prove(rk)
		siblings := make([]hashing.Hash, len(p.Siblings))
		copy(siblings, p.Siblings)
		proof := map[string]any{"record_key": rk, "end": p.End.String(), "siblings": siblings}
		if p.End == state.EndOther {
			ok := p.OtherKey.String()
			if _, seen := others[ok]; !seen {
				others[ok] = hexOf(p.OtherValue)
			}
			proof["other_key"] = ok
		}
		out["proof"] = proof
	}
	return out
}

func (n *Node) apiStorage(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, "proof")
	if err != nil {
		respond(w, nil, err)
		return
	}
	proof := false
	if s, ok := q["proof"]; ok {
		if proof, err = strconv.ParseBool(s); err != nil || (s != "true" && s != "false") {
			respond(w, nil, &apiError{http.StatusBadRequest, "Failed to deserialize query string: proof: provided string was not `true` or `false`"})
			return
		}
	}
	id, err := hashArg(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	key, err := hashArg(r.PathValue("key"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	tip, st, err := n.tipView()
	if err != nil {
		respond(w, nil, err)
		return
	}
	var others map[string]string
	if proof {
		others = map[string]string{}
	}
	out := recordJSON(st, id, key, others)
	out["state_root"], out["tip"] = st.Root(), tip.ID
	if proof {
		out["others"] = others
	}
	writeJSON(w, http.StatusOK, out)
}

const tooLarge = "JSON response size limit; ask for fewer entries or records, or read the block raw"

// apiStorageBatch reads 1 to 1,024 records from one state, so records read
// together never straddle a block.
func (n *Node) apiStorageBatch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		respond(w, nil, &apiError{http.StatusRequestEntityTooLarge, "request body too large"})
		return
	}
	var p struct {
		Reads []struct {
			Contract *string `json:"contract"`
			Key      *string `json:"key"`
		} `json:"reads"`
		Proof *bool `json:"proof"`
	}
	if err := decodeStrict(r, body, &p); err != nil {
		respond(w, nil, err)
		return
	}
	if len(p.Reads) == 0 || len(p.Reads) > batchMax {
		respond(w, nil, fmt.Errorf("reads must name 1 to %d records", batchMax))
		return
	}
	tip, st, err := n.tipView()
	if err != nil {
		respond(w, nil, err)
		return
	}
	type pair struct{ c, k hashing.Hash }
	asked := make([]pair, len(p.Reads))
	size := 0
	for i, rd := range p.Reads {
		if rd.Contract == nil || rd.Key == nil {
			respond(w, nil, &apiError{http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: missing field"})
			return
		}
		c, err := hashArg(*rd.Contract)
		if err != nil {
			respond(w, nil, err)
			return
		}
		k, err := hashArg(*rd.Key)
		if err != nil {
			respond(w, nil, err)
			return
		}
		kk := [32]byte(k)
		if v, ok := st.Get(types.StorageKey(c, &kk)); ok {
			size += 2 * len(v)
		}
		asked[i] = pair{c, k}
	}
	if size > answerMax {
		respond(w, nil, errors.New(tooLarge))
		return
	}
	proof := p.Proof != nil && *p.Proof
	var others map[string]string
	if proof {
		others = map[string]string{}
	}
	read := map[pair]map[string]any{}
	values := make([]any, 0, len(asked))
	size = 0
	for _, a := range asked {
		v, ok := read[a]
		if !ok {
			before := len(others)
			v = recordJSON(st, a.c, a.k, others)
			if len(others) != before {
				size += len(others[v["proof"].(map[string]any)["other_key"].(string)])
			}
			read[a] = v
		}
		b, _ := json.Marshal(v)
		size += len(b)
		if size > answerMax {
			respond(w, nil, errors.New(tooLarge))
			return
		}
		values = append(values, v)
	}
	out := map[string]any{"values": values, "state_root": st.Root(), "tip": tip.ID}
	if proof {
		out["others"] = others
	}
	writeJSON(w, http.StatusOK, out)
}

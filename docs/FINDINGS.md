# Findings and issues log

This log records what each stage found: interpretations of the rules where a
reading could differ, problems found in our own implementation, and what to
verify next. Section numbers (§n) refer to `ting-protocol-rules.md`.

## Stage 1: implementation from the rules alone

Date: 2026-10-20. Inputs: `ting-protocol-rules.md` only; no reference source,
wiki or seed was consulted.

### What is implemented

Every consensus rule in the document: canonical encodings (§2), genesis and
identities (§3), blocks, envelopes and receipts (§4), contracts and records
(§5), the state tree (§6), block validation and the state transition (§7),
proof of work, work and fork choice (§8), timestamps and the retarget (§9),
capacity voting (§10), budgets, counters, growth and capacity (§11–§14), the
price list (§15), the execution profile, binary format and validation (§16–
§18, §25), the instruction set and abstract machine (§19–§20), exact metering
(§21), the twelve host functions (§22), deploy and invoke (§23), calls,
rollback and read-only (§24), the boundary rules of §26, and seed replay
(§27). SHA-256 comes from Go's standard library, which §29 allows.

### Check values reproduced

All of §28 that can be checked without a seed:

| Check | Result |
|---|---|
| Development and experimental ChainId and GenesisBlockId (§28.1–28.2) | match |
| Production profile fields (§28.3) | match |
| Empty state root, producer hash, transactions root, receipts root (§28.4) | match |
| Envelope sizes and data bases, empty receipt, empty block (§28.5) | match |
| Code price of 0 bytes and of the preamble; CodeHash charge for 0 bytes | match |
| Record key, ContractId and value-leaf hash prices | match |
| Memory prices at 32, 33, 48, 256, 1,024, 4,096, 16,384 and 65,536 pages | match |
| 1-byte copy and fill at 48 pages, 33-byte host copy | match |
| Stack pages at 8,192 and 8,193 slots | match |
| Work at targets 1 and 2^256−1; two-thirds and one-half voting thresholds | match |
| Raw SHA-256 of "" and "abc" (§29) | match |

### Interpretations taken

Points where the text leaves a choice, and what this implementation does.
None of them should matter if the text is read as intended, but each is a
place to look first if the seed disagrees.

1. **Export order.** §17.3 lists `invoke` and `memory` but not their order;
   both orders are accepted. A `memory` export must name index 0.
2. **Table and memory sections** must hold exactly one entry: a count of 0 is
   an empty section (rejected by §17.2) and more than one is "at most one".
3. **Vector counts against remaining bytes.** Each count is compared with the
   section's remaining bytes times a minimum entry size before allocating.
   This only changes when a malformed module is rejected, never whether.
4. **More than twelve imports** are rejected as soon as the count is read,
   since names must be distinct members of the twelve.
5. **Memory prices at 0 pages** use the first tier (load 18, store 1, word 4,
   span 0), reading "at most 64 KiB" as including zero.
6. **Block-capacity guard during a child's receive debit.** If the receive
   debit of `call`/`deploy` fits the child's budget but not the block, the
   block is invalid (§21.6), rather than the child ending with status 3.
7. **Initializer that fails to start** (for example, its preparation debit
   exhausts compute): the deployment rolls back its installation and reports
   that status with an empty result, as a failure "before the initializer"
   (§23.1).
8. **Corrupt state** (a manifest without matching code, an installed module
   that fails validation, a malformed manifest seen by `code_hash`) is a node
   failure: no receipt is produced and the block is not marked invalid
   (§23.3, §26.5).
9. **Body versus header.** A body whose producer data, transactions or
   carried receipts do not hash to the header's commitments is rejected
   without remembering the BlockId as invalid, because the same header with
   its real body may be valid. Only header-rule failures and execution
   failures of a body that matches every commitment are remembered. The
   carried receipts are checked against the receipts root before execution;
   the derived ones are then compared byte for byte.
10. **Early size bound.** A block whose encoded length exceeds the active D
    capacity is refused before its body is decoded (§7.2, §26.7).
11. **Seed check first.** At a seed height the BlockId is compared with the
    list before the parent is looked up, so a wrong block is refused even
    when its parent is unknown (§27.1 "before any other check").
12. **Fork choice** follows the reference policy stated in §8.2: a branch
    that extends the tip wins as soon as it has more work; any other branch
    needs more than half its head's block work as margin.
13. **Manifest code price.** §23.1 step 4 says the manifest "records the code
    price debited in step 3", but step 3 debits the code price plus the
    CodeHash price. The manifest stores the code price alone, as §5.3 and
    §15.7 describe, and that is what a later call charges.
14. **A getter length of 2^32 − 1.** `call_result` returns the length of the
    last child result; a result of 2^32 − 1 bytes would read like the absent
    sentinel, which the rules reserve only for storage and `code_hash`. D
    budgets make such a result unreachable.
15. **Tables without element segments** are accepted, every entry null.
    §16.4 says a table "is filled once by active element segments", which
    could be read as requiring one; §17.3's "entries not listed are null"
    supports accepting it.
16. **Mempool deduplicates identical bytes.** The protocol permits repeated
    occurrences (§4.4); a block template may still carry the same bytes
    twice when offered twice. This is policy only.

### Independent review

Three read-only review passes compared the code with the rules, one each for
execution and metering (§11–§15, §19–§24, §26), WebAssembly validation (§16–
§18, §25) and chain rules (§1–§10, §14, §27). Results are summarized here as
they arrive.

- Execution and metering: no open discrepancies. It independently found the
  4 GiB `readKey` wrap listed below (already fixed), and contributed
  interpretations 13 and 14.
- Chain rules: no discrepancy affecting validity or any consensus value. It
  compared the retarget with an exact independent computation on 24,000
  cases (both saturation thresholds, negative exponents, half-lives of 1–3),
  the state tree with the §6.2 definition on low-entropy and near-equal keys,
  and the capacity schedule with a direct `limit(e) = apply(direction(e − 3),
  limit(e − 1))` for epoch lengths 1 to 5. It reported four deviations in
  policy and robustness, all fixed (see below), and noted that no seed list
  is built in yet: the development list arrives with stage 2, and until then
  the node warns that seed heights are unchecked.
- WebAssembly validation: no accept/reject, reservation or branch-target
  difference. It compared the integer readers with a literal transcription
  of §26.6 on every 1- to 3-byte string and 3M edge-biased longer ones; the
  validator with a line-by-line transcription of the §25 pseudocode on 1M
  random function bodies (accept/reject, maximum operand height and
  reservation all equal); compiled branches with a reference structured
  interpreter on 51k random control-flow programs run through the real VM;
  and 79 hand-built module-shape edge cases. It found two robustness issues,
  both fixed (see below), and the ambiguity recorded as interpretation 15.

### Issues found and fixed during stage 1

- **Poisoned BlockIds (fixed).** The first version remembered a BlockId as
  invalid whenever its block failed, including when only the body was wrong.
  A peer could have sent a valid header with a corrupted body and made the
  node refuse the real block forever. Fixed as described in interpretation 9;
  `TestRejections` covers it.
- **Listed seed blocks could be blacklisted (fixed).** A block whose BlockId
  the seed list pins, but which this node rejects, was remembered as invalid.
  §27.1 makes that the implementation's fault: it now raises a
  `SeedConformanceError`, is not remembered, and the node stops; `replay`
  prints the first differing receipt field.
- **Size bound before decoding (fixed).** The body was decoded before the
  D-capacity size check, contrary to §2.1 ("checked against ... the active
  capacity before anything is allocated"). The check now comes first.
- **Validation work per declared local (fixed).** Each local group was
  expanded one entry per local, so a 6-byte body declaring 16,384 locals did
  16,384 steps of work; §18.4 says validation work never grows "with
  declared counts that cost no bytes". Local types are now looked up by
  group. 20,000 such functions decode in well under a second.
- **Error contract (fixed, no consensus effect).** A few truncated
  instructions returned an error not wrapping `ErrInvalid`; the outcome was
  already status 9. Every decode error now wraps it.
- **Fork choice order (fixed, policy only).** When a block connects queued
  orphans, candidate heads are now tried most work first, as §8.2 describes
  the reference policy.
- **Miner repeated its own transactions (fixed, policy only).** The miner
  built its next template before the asynchronous tip handler removed the
  previous block's transactions from the mempool, so a deployment was mined
  twice; the second occurrence correctly failed with status 10. The miner
  now removes its block's transactions synchronously.
- **Local policy under a saturated target (fixed).** The development
  genesis is in 2023, so the chain is far behind schedule and its target is
  the PoW limit: every hash qualifies and a miner produces thousands of
  blocks per second, each timestamped at least median + 1, running ahead of
  the clock. The node now never mines a block more than 7,200 s ahead of its
  clock (the §9.1 deferral window), offers `-interval` pacing, skips a
  snapshot while one is being written, and logs tips periodically.
- **4 GiB boundary (fixed).** Reading a host key or budget at the last bytes
  of a full 4 GiB memory computed `ptr + 32` in 32-bit arithmetic (§26.4:
  "the mathematical endpoint can be 2^32").
- Several sample programs in tests trapped on empty initializer inputs,
  correctly per §22.3 (a length of `n − 32` wraps to a huge span). The
  samples now guard short inputs.

### How stage 1 was tested

- Unit tests per package, including exact hand-computed counters for a
  minimal deployment (C 74,008, S 7, D 191, G 152), a call, an echo call
  with storage, events and output, invalid code (status 9), absent targets
  (status 8), exhaustion of C and G at exact amounts, nested and read-only
  calls, nested deployment and identifier collision (status 10), context
  selectors, call_indirect traps, memory growth and bulk operations, stack
  pages and wide arithmetic.
- The state tree is checked against a direct implementation of the §6.2
  hash definition over random insert/update/delete sequences, including keys
  that split at depth 256.
- VectorRoot is checked against a literal implementation for many sizes.
- Typing corner cases of §25: unreachable code, nested frames that start
  reachable, `select` with unknowns, `br_table` label agreement in
  unreachable code, measurement of a function that ends unreachable.
- Chain tests mine blocks, replay them on a fresh node pinned to their ids,
  reorganize to a heavier branch, connect orphans, re-derive pruned states,
  and reject thirteen kinds of tampered blocks.
- `ting devnet` mines a 31-block chain with 279 transactions whose
  top-level receipts include statuses 0, 1, 2, 3, 8 and 9, with nested
  calls returning 0, 8 and 11; `ting replay` accepts it.
- Fuzzing: 21.6M decoder runs and 15.3M deploy-and-call executions of
  arbitrary bytes, with no crash and consumption always within budget.

This shows the implementation agrees with itself and with the check values;
it cannot show agreement with the reference node. That is stage 2.

### To verify first in stage 2

Areas where an error would not show up in self-consistency tests, roughly
in order of risk:

1. Maximum operand height and reservations of real compiler output (§25.4):
   any difference changes C on every call.
2. The code-price scan on real modules (§26.1), including repeated sections.
3. Host-function charge order and amounts, especially getters, `call` and
   `deploy` (§22, §26.4).
4. Instance preparation price: data words rounded once over the sum, empty
   segments counting 12 C (§26.4).
5. Memory price boundaries at 1, 16, 32 and 33 pages and the curve above.
6. Receipt and event encodings and the receipts root.
7. Retarget arithmetic for negative exponents with fractions, and the median
   with fewer than eleven blocks.
8. Capacity voting across epoch boundaries (the seed may be too short to
   reach one with E = 20,160; the experimental network's E = 4 is better).

### Not consensus, not done yet

- Peer-to-peer networking and synchronization (stage 2 will define how to
  talk to the existing node).
- Deferring headers more than 7,200 s ahead of the local clock (§9.1
  admission policy).
- Disk-backed state (state snapshots already make restarts fast).
- Interpreter speed-ups.

## Stage 2: the production network and the peer protocol

Date: 2026-10-21. New inputs: the updated `ting-protocol-rules.md` (production
network fixed: genesis 1792540800, initial C 450,000,000 and S 2,048, ChainId
`15d6549c…`, GenesisBlockId `24b76a52…`, a 325-block seed ending at
`00000000e04237d2…`) and `peer-protocol.md`, with the seed node
`2.29.97.212:18442`. The consensus rules themselves did not change.

### Result: the implementation agrees with the reference node

Syncing from the reference node over the peer protocol, this implementation
validated every block of the production chain on the first attempt, with no
change to any consensus code:

| | |
|---|---|
| Seed | 325 blocks, 23,540 transactions: every receipt byte for byte, every receipts root, state root and resource total; height 325 is the pinned `00000000e04237d2…` |
| Whole chain | 469 blocks and 23,547 transactions at the time of writing, followed live afterwards |
| Final seed state | root `5567b339…`, 31,482 records |
| Offline replay | the seed saved as a bundle replays in 2.1 s |

What the seed exercises (`ting stats`):

- receipt statuses 0, 1, 2, 3, 4, 5, 6, 8, 9 and 10 at top level (7 is
  unreachable in practice and 11 only occurs in nested calls);
- 13,231 deployments of 9,881 distinct valid and 854 invalid modules, and
  10,309 calls;
- all 115 opcodes and all twelve host functions;
- tables (including 6 with no element entries and 3 of size 0), globals,
  data segments (31 empty ones), memories of 0 initial pages, above 2 MiB and
  growable;
- blocks at the capacity limits: one transaction used C 449,374,517 of
  450,000,000 and S 2,048 of 2,048.

Interpretation 15 (tables without element segments are valid) is confirmed:
such deployments succeed on the reference node too. The other stage-1
interpretations are either confirmed by matching receipts in cases the seed
covers or remain untested; none was contradicted.

### The seed list

The rules publish the production seed's length and last BlockId, not the
whole list. Because every header names its parent, the last BlockId
determines the BlockId at every seed height, so the node pins only
`(325, 00000000e04237d2…)` and derives the list from headers that end there
before executing any seed block. On restart the list is re-derived from the
stored blocks. `ting replay -network production` accepts a bundle without a
list for the same reason.

### Peer protocol implementation

- `internal/p2p`: strict frame and field codec, every request, answer and
  announcement; a pipelining client; a server with the hello rules, request
  units (200/s after a burst of 2,000, waiting rather than failing), sliced
  pushes with transfer times, inbound limits (125, 8 per range, newest of the
  largest range evicted), and bans for invalid data; header-first sync with
  pipelined `raw` downloads.
- `internal/node`: the node's backend (headers, raw, import, submit,
  inventory, pending, deferred, transaction_raw, addresses, announcements at
  most every 0.2 s), and outbound sessions that follow a peer, push our own
  blocks with `import` when our tip has more work, and exchange pools.
- Local policy as the document describes: blocks more than 7,200 s ahead are
  refused with `timestamp ahead of the clock`; blocks on a branch more than a
  day's work behind the tip with `branch too far behind the tip`;
  transactions included in the last 1,440 blocks with success, or the last 10
  at all, with `transaction already included`; a deferred set for
  `not_before` transactions.

### Checked against the reference node

- The three example frames of the protocol document encode byte for byte.
- Hello and state, headers (paging, `no common ancestor`), raw (`block not
  found`), transaction_raw (`transaction not found`), inventory, pending,
  addresses, and ten pipelined requests: the reference node's answers decode
  under our strict rules, and our server gives the same answers to the same
  requests (`ting probe`).
- One difference, fixed: for a `raw` offset beyond the item the reference
  answers `slice offset out of range`. Observed boundaries, now matched: an
  offset equal to the length gives an empty slice; a slice running past the
  end is cut short.
- Our node serves the same state as the reference node: height, tip and
  accumulated work equal.

### Not checked against the reference node

- A reference node connecting to us: it cannot reach this machine. Our server
  is tested against our own client, including two goting nodes syncing,
  relaying a transaction and mining it over the protocol.
- `import` and `submit` to the reference node: pushing a block or a
  transaction to the production network publishes it, so it was not done
  without explicit approval. Both paths are tested between goting nodes.
- Address encoding against real data: the reference node's discovery is off
  (it answers no addresses).

### Peer protocol points the document leaves open

1. How `submit` answers an invalid envelope: no error text is listed. We
   answer `invalid transaction: …` without counting it as invalid data,
   since only `invalid header`/`invalid block` are named as such.
2. How `import` answers a block whose parent is unknown: we hold it and
   answer Taken.
3. Whether a push may be interleaved with other requests: we allow it; the
   push's state survives.
4. Whether `raw` with an offset past the end is an error: observed, it is.
5. Whether a deferred transaction that becomes due must first execute
   successfully ("pooled once due and successful"): we pool it when due.

## Stage 3: comparison with the reference implementation

Date: 2026-10-21. New input: the reference repository
(`github.com/ting-organization/ting`, checked out beside this one as
`../ting-reference`). Its `ting-protocol-rules.md` is identical to the copy
stage 2 used.

### Conformance material

`ting conformance -dir ../ting-reference/conformance` (and
`go test ./internal/conformance`, which finds the checkout automatically or
through `TING_REFERENCE`) runs everything the reference publishes:

| Material | Result |
|---|---|
| 148 consensus vectors in nine groups: primitives, envelopes, headers, blocks, difficulty, voting, module structure, work, price and metering tables | all pass |
| The mined voting chain: 33 blocks on a genesis with three-block epochs; each block's capacity limits and target checked against the manifest | passes, through all 4 capacity changes |
| The production seed (325 blocks, 23,540 transactions) | passes; the bundle synced over the peer protocol in stage 2 is byte-identical to the published one |
| The development seed (519 blocks, 23,552 transactions), never seen before | passes on the first run |

The development seed is built from the reference's VM test suite, which
targets interactions and edges deliberately: reentrancy, nested rollbacks,
ordinals kept across failed initializers, every affordable memory-price
transition, atomic output debits at every word edge, 4,096-record state
churn, and blocks filled exactly to each capacity.

Two vectors failed on the first run; neither is a consensus difference:

1. **An epoch with no work** (`v6-voting/direction-no-signals-hold`). The
   rules' test `3 × U ≥ 2 × W` reads as an increase when `W = 0`; the
   reference decides hold. A real epoch always carries work, so no chain can
   reach the case. Aligned: an empty tally decides hold.
2. **A header naming another parent** (`v3-headers/another-parent`). The
   vector judges a block as the next block on a given chain; this node looks
   the parent up by its id and holds a block with an unknown parent as an
   orphan, which is right on a network. Added `Chain.ValidateNext(parent,
   raw)`, which validates against an explicit parent, for vectors and tools.

Also fixed while adding the harness: `StackPages` overflowed for heights near
2^64 (`ceil` computed as `(h + 8191) / 8192`); the table vector includes
2^64 − 1. Unreachable under any budget, but the formula is now exact.

### Side-by-side comparison: consensus

Four reviews read the two implementations side by side, each backed by
throwaway differential tests run through `go test -overlay` (nothing of
them is in the repository):

| Area | Method | Result |
|---|---|---|
| Retarget | 20,000 random cases against a transcription of the reference's `asert`, negative exponents, half-lives 1–3, both saturation thresholds | 0 mismatches |
| Capacity voting | incremental vote state against the reference's `active_limits(history)`, epoch lengths 1–6, limits near 2^64 | equal |
| State tree | 3,000 batches on random older trees (forks), keys splitting at bit 255, deletes, checkpoint and revert | every tree matches §6.2 |
| Module decoder and code price | a Go port of the reference decoder, LEB reader and `price::code`, fuzzed: 3M LEB inputs, 2M byte strings, 12M generated and mutated modules (2.6M valid), 336 cases on the reservation and local bounds | identical verdicts, prices and per-function profiles |
| Memory tiers and stack pages | all 65,537 page counts | identical |
| Interpreter, host imports, deployment, rollback | line-by-line reading | no difference |

**No consensus difference was found.** Every difference is in node
behaviour: how much work is done before a block is rejected, how
rejections are classified, and what the node does around the rules. The
ones in the core, and what became of them:

| Difference | Reference | Before | Now |
|---|---|---|---|
| Block guard during validation | `min(capacity, header totals)`: stops at the first debit past the declared totals | capacity only; ran the whole block, then compared totals | as the reference |
| Carried receipts | decoded strictly at block decode: status > 11, events on a failed receipt, bad framing rejected | never decoded; compared as bytes after execution | decoded strictly first; `DecodeReceipt` has the failed-receipt-events rule |
| Header version and votes | rejected while decoding | rejected later, after a block was held as an orphan | checked before a header is indexed; no orphans |
| Code record | read only on a cache miss, after the code price is paid | read on every invocation before the price debit | as the reference, with the reference's length, price and CodeHash invariants on a miss |
| Code cache weight | decoded size (`Module::weight`) | `8 × code length + 512`, about 3× too low | `Module.Weight()` |
| Block assembly | discarded-compute allowance, attempt records `{fit, alone}` | neither; a never-fitting transaction stayed forever | `Builder.LimitDiscarded`, `Attempts`; `!fit && alone` leaves the pool |
| `ApplyDirections` decrease at 0 | saturates at 0 | wrapped (unreachable: limits ≥ minimum ≥ 1) | saturates |
| Block full mid-transaction | the guard fails every later charge as Structure until the invocation unwinds | aborts the transaction at once | unchanged: in both, the transaction is left out or the block is invalid; no receipt differs |
| Corrupt state | panics | `ErrStateCorrupt`, a block-level error | unchanged (unreachable from valid state) |

The reference core has state proofs and a storage-backed state reader;
this implementation keeps state in memory as persistent trees and has no
proofs (see the open items below).

### Side-by-side comparison: the node

The node comparison found the larger gaps, none affecting block validity
but several affecting safety on a public network and interoperation with
reference peers. Each was closed by following the reference's code and
wiki (`node/wiki/node/*.md`):

**Chain index and synchronization** (`internal/chain`, `internal/p2p/sync.go`)

- *Header first.* The chain is now an index of headers with statuses
  (header, stored, valid, invalid). Every header is checked in full when
  indexed (seed list, version, chain, height, parent, median time, exact
  target, votes, PoW, used ≤ limits, clock + 7,200 s). Bodies are
  downloaded only along the best header branch; side branches are stored
  and executed only when they become the best complete branch. Before,
  every block was executed on arrival and side branches below the
  retained depth were re-executed from the fork for every new block
  (measured: quadratic, 50 ms for 400 side blocks under a 900-block tip).
- *Work threshold and presync.* A header whose `parent work + work(target)`
  is below `tip work − 1440 × work(tip target)` is refused (`branch too far
  behind the tip`) unless already indexed. A peer's headers below the
  threshold are checked without being kept, with a salted bit every 600
  headers (buffer 14,400) and fetched again once they pass, so a genuinely
  heavier deep fork is still adopted. Before, a one-block fork from genesis
  pushed under a 2,010-block tip was accepted and executed.
- *Deep forks.* Synchronization continues from the last header received
  rather than restarting from the canonical locator, and falls back to the
  locator on `no common ancestor`; the locator includes up to 8 side tips.
  Before, a peer whose first 2,000 blocks after a fork did not overtake us
  was never followed (the test spun until its timeout).
- *No unchecked orphans.* A body needs an indexed header; otherwise
  `unknown parent`. Before, 1,024 raw blocks of up to the tip's data
  capacity were held before any header or PoW check: a random 310-byte
  "block" was answered `Taken`.
- *Download scheduling.* A window of 1,024 beyond the lowest missing block,
  64 per peer, never the same block from two peers, per-block transfer time
  of 60 s + 10 s per MiB, a lapsed block not asked of the same peer for
  120 s, `block not found` hands the block to other peers.
- *Built-in seed lists.* The production (325) and development (519) seed
  ids are embedded; every header at a seed height is checked against them
  from the start. Before, the list was derived lazily from a pin, and one
  foreign block at height 1 pushed before the pin locked a fresh node out.
- *Offenders.* A peer whose stored block later fails validation, when its
  branch is activated, is banned like one whose data fails at once.
- *Tip hooks* run synchronously in the commit path, so the transaction
  index and the pool cannot miss a tip (before: a lossy buffered channel).

**Transaction pool and deferred set** (`internal/node/txpool.go`)

- 1,024 transactions and `max(16 MiB, 4 × D)` bytes (before: 100,000 and no
  byte bound), each at most the next block's data capacity, 24-hour expiry.
  Reference peers end a connection whose inventory lists more than their
  own pool holds, so the old size broke interoperation.
- Fair-share eviction by source address, operator submissions never
  evicted; FirstIn selection by `(tried, nth of its source, source's last
  turn, arrival)`; discarded compute per template bounded by one block's.
- Deferred set: 4,096 entries, 256 per source, horizon 366 days (we had
  365), expiry 30 days after due; a due entry is tried on the tip and
  admitted only if it succeeds and fits, stays in the deferred set, and
  leaves only when a canonical block above its `since` includes it with a
  successful receipt; a reorg restores it. Persisted to `deferred.json`.
  Before, due entries were pooled without a trial and any inclusion, even
  a failed one, removed them.
- "Already included" refusals (1,440 blocks if successful, 10 if included
  at all) apply only to relayed transactions, not local submissions.

**Peer protocol** (`internal/p2p`, `internal/node/peers.go`,
`internal/node/addressbook.go`)

- Push rules as the reference: the first slice's header is checked at
  once (invalid: ban; stored: `Taken{known}`), the error texts match
  (`nothing being pushed`, `invalid block: truncated header`, `invalid
  block: larger than its capacity`), request units are charged by the
  ceiling of bytes / 64 KiB, an undecodable first frame closes silently,
  `Dial` checks version, chain id and nonce, the hello's answer is due
  within 5 s.
- Relay compares inventories both ways for pending and deferred sets and
  sends only what the peer lacks; fetched items are bounded by the data
  capacity and a transfer time, must be what was asked, and the deferred
  flag must match. Pushing puts the last block the peer took first in the
  locator and continues at once while more than 32 remain.
- *Discovery and the address book*, new: `peers.json` of at most 1,024
  addresses (evicting unproven before proven, never configured);
  production seed nodes looked up at start (5 s each) and learned as
  discovered addresses; addresses asked every 10 s while short of peers,
  else every 10 minutes, at most 32 taken from one answer and only those
  the sender could reach; shared only if proven in the last 24 h without
  a failure since, reachable by the asker, never its own announced
  address. Up to `max_peers` (8) connections in all, configured ones
  included, discovered ones from distinct /16 (IPv4) or /32 (IPv6) groups,
  most recently proven first; one more while the tip is 10 minutes stale,
  then the discovered peer with the oldest last block is dropped; one
  unproven address probed every 2 minutes when all slots are taken;
  retries after 1 minute doubling to an hour (configured peers: 1 minute).
  An address that reaches this node itself is forgotten until restart.
- *Bans* are by range (an IPv4 address, an IPv6 /64) for a day, capped at
  4,096, never a range holding a configured peer nor loopback; configuring
  a peer lifts a ban; a ban ends open inbound and outbound connections in
  the range and removes its addresses from the book. Before, the server
  banned exact IPs in an unbounded map and the client banned configured
  peers, including for non-linking headers.

Checked live: a node with no configured peer and an empty data directory
found the production network through the built-in seed node, synchronized
to the tip (height 497), and recorded the seed as proven in `peers.json`.

**Storage and operator control** (`internal/chain`)

- *Block files* now use the reference layout:
  - segments `blocks/NNNNNNNN.dat` of up to 128 MiB, each beginning with
    `TINGBLK\0` and the ChainId;
  - frames of `u64 length ‖ block ‖ H("node-block", block)`;
  - every frame forced to disk before it counts as stored.

  Before, a single `blocks.log` had no magic, chain id or checksum and was
  never fsynced. A power loss could leave a log that stopped the node from
  starting.

  Now a torn or zero-filled tail is truncated, a frame whose checksum fails is
  passed over and its block fetched again, and files made for another genesis
  are refused rather than reset. Loading retries blocks stored before their
  parents, which happens with out-of-order downloads, instead of dropping them.
- *Operator marks* (`invalidate`, `reconsider`), persisted in `marks.json`:
  - a marked block and every block above it are left and never connected;
  - peers sending them get `block marked invalid by this node's operator`
    without a ban;
  - a block not yet seen is refused when it comes;
  - `reconsider` clears the marks and invalid statuses of the branch and
    reconnects the best branch.
- *Diagnostics* in the status document:
  - the last reorganization (`replaced`, `fork_height`, `deep` at 6 blocks
    or more);
  - a warning when a branch that failed validation here carries more than 6
    blocks of work beyond the tip, meaning this node or much of the network
    follows other rules.

**Interfaces** (`internal/node/api.go`, `internal/control`,
`internal/daemon`, `cmd/ting`)

- *REST API*: the reference node's `/v1` interface on `127.0.0.1:18441`:
  - **Shapes and names.** The same endpoints and field names (`used`,
    `result_hex`, `payload_hex`, `producer_hex`, decimal-string `nonce`, hex
    `target`, `status_name` as `Success` … `ReadOnly`), and lowercase-only
    hex.
  - **Paging.** `from`/`limit` ≤ 100 with `next`/`has_more`, pages stopping
    before 16 MiB, and unknown query parameters refused.
  - **Errors.** Mapped the same way (404 / 503 / 504 / 400, 413 / 415 / 422
    for submissions).
  - **Protections.** Request slots per core, a 35 s deadline, `nosniff` /
    `no-referrer` / `no-store` headers and optional CORS.
  - **New endpoints.** The program event index (`/v1/events?contract&topic`),
    voting (`/v1/votes`), the mempool and deferred set (`/v1/mempool`), and
    storage reads with **Merkle proofs** in the reference's proof format (a
    new `state.Tree.Prove`; checked against `VerifyProof` on random trees).

  The old local `/status`, `/blocks/...` and `POST /blocks` paths are gone.
  The reference serves explorer HTML at those paths.
- *Control channel* on loopback `127.0.0.1:18440`:
  - **Framing.** u32-framed JSON, with the handshake in which each side proves
    with HMAC-SHA-256 over both nonces that it holds `control.token`. The
    token never crosses the connection.
  - **Methods.** The reference's methods: `status`, `peers`, `add_peer`,
    `remove_peer`, `api`, `miner`, `votes`, `template`, `solution`, `mine`,
    `submit`, `invalidate`, `reconsider` and `stop`.
  - **Limits and texts.** The same limits (1 KiB hello, 16 connections,
    65,536 structural characters) and texts (`unauthorized`, `unknown control
    method`, the "did not prove it holds this data directory's token"
    message).
- *Data directory and CLI*:
  - **Files.** `config.json` with the reference's fields and bounds (unknown
    fields refused), `genesis.bin`, `control.token` (0600), `runtime.json`,
    `node.lock`, `peers.json`, `deferred.json` and `marks.json`.
  - **Commands.** `init` (production by default, `--dev`, `--genesis`),
    `run`, `start`, `stop`, `status`, `peers`, `probe`, `add` / `remove
    node`, `api enable` / `disable`, `mine`, `miner
    start|stop|status|producer|votes`, `votes`, `template`, `solution`,
    `submit`, `invalidate` and `reconsider`, with the global `--data-dir`.
  - **Offline edits.** Settings edits apply to the running node or, while it
    is stopped, to `config.json` under the lock.
- *Built-in seeds*: `init` imports the network's seed, as the reference does.
  A development chain therefore starts at height 519, and its blocks must
  match the reference's development seed. The development and production
  seed bundles are embedded, gzip-compressed (3.8 MB), and checked against the
  built-in id lists.
- *Miner*: it can be started and stopped at run time, and its producer data
  and votes changed. It takes a fresh template every 2^24 nonces per thread
  and waits while the tip is more than a block behind the best header.
  Templates and solutions use the reference's JSON, and the default producer
  data is now empty (it was `goting`).

Checked live:

- Two development data directories (`init --dev`) on one machine. The second
  synchronized from the first through `add node`.
- On the first: `mine`, `template`, `invalidate` / `reconsider`, `miner
  start/stop/producer/votes` and `api disable/enable --cors` through the
  control channel. A wrong token was refused.
- A production data directory imported the 325-block seed at `init` and then
  synchronized to the network tip (508) from the reference node.

### Remaining differences

None of these affects which blocks are valid.

| Area | Reference | This node |
|---|---|---|
| State storage | redb database with records, tree nodes and per-block undo/redo; every block committed; undo pruned to `undo_window` | bbolt database with records, tree nodes and undo data (no redo); the base trails the tip by 128–256 blocks and moves in one commit; undo kept for `undo_window` blocks below the base (43,200; 0 keeps all); `db_cache_mib` accepted but unused, the operating system's page cache serving instead |
| Header index | held headers above a settled height; older ones in the database | every header in memory, restored from the database's block rows at start |
| Deep reorganizations | below the undo window, replay from genesis in the background | below the undo window, refused |
| Explorer, wiki, Swagger UI, `/openapi.json`, `ting docs` | served | not served |
| Status | published every 0.2 s; `/health` answers 504 when that is older than 30 s | computed per request; `settled_height` is the database's base |
| Pluggable selection and nonce search | Rust traits a mining fork replaces | fixed (FirstIn turns, CPU search) |

### Issues found in this stage

- A listed seed block that failed execution was marked invalid like any
  other. A seed block is canonical by definition, so a failure there is this
  node's fault. It now stops the node with a `SeedConformanceError` (the
  conformance runs and seed replays pass, so this has not happened).
- The development seed made the built-in seed list apply to development
  chains. Tests and `devnet` that mine their own chain from genesis now say so
  with `chain.NoSeed`.
- Peer goroutines of a connection could outlive it: one task ending left the
  others running until they noticed the closed connection. Every task of a
  connection now ends before the connection is counted as ended (found by the
  race detector in the discovery test).

## Stage 3, continued: sync speed, VM memory and the database

Date: 2026-10-21. The repository was published as
[ting-organization/goting](https://github.com/ting-organization/goting), and
the module path became `github.com/ting-organization/goting`.

### How fast a node syncs from zero

Measured against the production network (height 513, 8.65 MB of blocks), from
a machine 47 ms from the seed node:

| Measurement | Result |
|---|---|
| A fresh node, 0 to 513, from the seed node | 13.1 s |
| The 325 seed blocks (23,540 transactions) validated locally | 2.1 s |
| Download from the seed node, 1 to 63 requests in flight | 0.82–0.88 MB/s |
| This machine's download from a CDN | 11 MB/s |

The download, not validation, set the pace: the slow stretches are exactly the
large blocks. Deeper pipelining did not help, so the limit was bandwidth per
connection on the seed node's side, not latency.

Between two nodes on one machine, a 30,938-block development chain synced at
about 127 blocks a second. That is the peer protocol's request units: 200 a
second after a burst of 2,000, with `raw` costing 1 plus 1 per 64 KiB. The
reference node charges the same, so one peer serves at most about 100 small
blocks a second to one connection. This is protection against abusive peers,
not a fault; more peers multiply it. Chain processing alone runs at 7,400
small blocks a second in memory and 3,300 with the database.

### VM: linear memory churn

Profiling the seed replay found `vm.newMachine` allocating **28.5 GB** for
23,540 transactions. Every invocation allocated and zeroed the module's whole
declared minimum memory, about 1 MB for typical compiled programs, even when
it touched a few bytes. Only 16% of CPU went to the interpreter; the rest was
the garbage collector, `madvise` and thread wake-ups.

Linear memory now comes from a pool by power-of-two size class and is zeroed
when its invocation ends, including when a fault or a full block abandons it.
Pooled buffers are zero throughout their capacity. Memories above 64 MiB,
which only programs declaring hundreds of MiB have, are mapped from the
operating system outside the Go heap and unmapped when the invocation ends.
The seed's VM tests reach those sizes on purpose.

Tests check that every invocation sees fresh memory after reuse, after a trap,
and in pages added by `memory.grow`, for pooled and mapped memories. A
deliberately broken clear makes them fail.

| Seed replay | Before | After |
|---|---|---|
| Wall time | 2.06 s | 1.33 s |
| CPU time | 5.2 s | 1.37 s |
| Peak resident memory | 1.09 GB | 0.50–0.65 GB |

The remaining peak is the seed's own programs touching hundreds of MB.

### The database

The state, undo data, block list and indexes now live in a database
(`chain.db`); see ARCHITECTURE.md, "With a database". In short:

- **State.** Flat records and branch nodes keyed by position (split bit and
  prefix), so the database holds exactly the current tree. Records read in one
  lookup.
- **Window.** In memory, the trees of the blocks above a base block rest on the
  stored tree through stubs that load on demand, each load checked against its
  parent's hash.
- **Flush.** The base moves in one atomic commit every 128 blocks, with undo
  data, index rows and block rows. The trees left in memory are rebuilt and
  every root checked.
- **Rewind.** A reorganization forking below the base applies undo data back
  to the fork first, checking every intermediate root.
- **Restart.** Only the frames after the block-file cursor are scanned, and
  only the blocks above the base execute: none after a clean stop or `init`.

| Measurement (production, 538 blocks) | Before | After |
|---|---|---|
| `init` (imports the 325-block seed) | 4.2 s with per-frame fsync | 2.0 s |
| Restart | re-execute, or load `state.snap` and re-check every block | 20 ms, 45 MB resident |
| A transaction at height 2, after its block was flushed | in memory | through the database index |

**Choosing the engine.** Pebble was the first choice. Its zstd compression
links C code whenever cgo is enabled, and that broke ordinary and `-race`
builds on this machine: the installed Command Line Tools cannot link against
the macOS 27 SDK. It would also make every installation depend on a working C
toolchain. Pebble v1 has the same dependency.

bbolt is pure Go, small, and a B+tree like the reference's redb, so it was
chosen instead. Its writer cannot grow the file while read transactions are
open, so views do not pin snapshots. They read the database as it is, which is
correct for any tree whose block descends from the base; rewinds bump an epoch
that expires older views.

The file grows by bbolt's mapping steps and reuses freed pages: 46 MB for
31,500 records here.

**Block-file durability changed with it.** Each frame used to be forced to
disk before `Put` returned, several milliseconds each with macOS's full fsync.
Frames are now forced before every commit that moves the base past them, and
otherwise at most 2 s after they are written. A frame lost in a crash lies
above the base and is fetched again, which is the reference's ordering:
frames before the commit that refers to them.

**How it was tested:**

- a differential test of stored against in-memory trees: 300 rounds of random
  inserts, updates and deletes over keys with long shared prefixes, with
  flushes, checking roots, reads, proofs, walks, and that the store holds
  exactly the tree's branches after every flush;
- chain tests in database mode against memory mode, block by block;
- restart, including that only frames after the cursor are scanned;
- a reorganization forking below the base and back again;
- a damaged database stopping the node instead of judging blocks invalid;
- the node's indexes through the database across a restart;
- live: production init, sync to the tip and restart; a development pair
  syncing over the peer protocol.

**Issues found while building it.** A state the API derived on its own,
concurrently with a rewind, could have been kept on an expired view. Its next
use would then look like a storage failure and stop the node. Such a state is
no longer kept: the chain counts rewinds and compares before keeping one.

**Not done yet:**

- every header stays in memory;
- `db_cache_mib` is accepted but unused;
- a reorganization below the undo window is refused, where the reference
  replays from genesis in the background.

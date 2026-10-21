# Architecture

This describes how goting is built and where it is designed to scale. The
consensus rules come from `ting-protocol-rules.md`; section numbers below
(§n) refer to it.

## Data flow of one block

```
raw bytes ──► chain.ProcessBlock
               ├─ BlockId = H("block", header)          (types)
               ├─ seed check, parent lookup / orphan pool
               ├─ header rules: version, chain, height, parent,
               │  median time, exact target, votes, PoW,
               │  declared resources ≤ capacity            (consensus)
               ├─ body: D bound, producer hash, transactions root,
               │  carried-receipts root, envelopes          (commit, types)
               ├─ exec.Builder over the parent's state.Tree
               │    └─ vm.ExecuteTransaction per transaction
               │         ├─ data base D, root dispatch C
               │         ├─ activation stack: calls, deploys, initializers
               │         └─ Machine.run: metered interpreter + host calls
               ├─ receipts byte-for-byte, receipts root, state root, totals
               └─ store block, new Entry, fork choice, prune old states
```

## State

`state.Tree` is an immutable compact sparse Merkle tree (§6). Applying a
batch returns a new tree that shares every untouched subtree with the old
one, so:

- each block entry keeps its own state as a pointer; restoring an ancestor for
  a reorganization (§8.3) is O(1);
- a batch mutates in place only nodes it created itself (tracked by a batch
  generation), so a block's k writes cost O(k·log n) allocations and one
  rehash per dirty node;
- the single-sided nodes the hash definition places between a parent and a
  deeper split are not stored; they are computed while hashing;
- large batches rehash the top levels in parallel;
- `Build` constructs a tree from sorted records in O(n).

During execution an `Overlay` layers writes over the parent's tree with an
undo journal: an invocation's checkpoint is a journal position, rollback pops
entries (§24.2). At the end of a block the overlay's net writes are applied
to the tree in one batch.

A tree can also rest on a stored tree, a `state.Source`. Its unchanged
subtrees are then *stubs*: a node that carries only its hash, split and a key
below it, and loads its children from the source when a change, a proof or a
walk needs them. Each load checks that the stored children hash to the stub's
hash. A record below a stub is read from the source's flat record table, in
one lookup, without walking stored nodes. `state.Delta` computes what
replacing a stored tree with a tree derived from it writes:

- every branch the new tree holds in memory, all of them new;
- the positions of the old tree's branches, on the paths of the changed keys,
  that the new tree no longer has.

A branch is stored by its position, its split bit and the key bits above it,
so the database holds exactly the current tree, with nothing to collect.

### With a database

`internal/db` is an embedded bbolt file (a B+tree, like the reference's
redb) with these tables:

- records;
- branch nodes by position;
- each flushed block's undo data;
- a row per stored block (its header, sizes and frame location);
- the transaction-occurrence and event indexes;
- metadata: format, ChainId, the base, the block-file cursor and the undo
  floor.

It changes only in atomic, durable transactions.

The chain keeps the state of one canonical block, the **base**, in the
database. In memory it keeps the states of the blocks above it, each a tree
resting on the stored one, and each block's change list. When the tip is twice
`RetainDepth` (128) above the base, one commit moves the base to `RetainDepth`
below the tip. That commit holds:

- the blocks' net record changes and branch deltas;
- their undo data (each changed record's previous value);
- their index rows;
- rows for the blocks stored since the last commit, and the new block-file
  cursor.

The block files are forced to disk first. The trees left in memory are then
rebuilt on the new base from the change lists, and every root is checked
against its header.

A reorganization that forks below the base first applies undo data back to the
fork, checking each root on the way, and commits that as the new base. The new
branch then executes as any other. Undo data is kept for 43,200 blocks below
the base.

A branch that forks below that window needs undo data the database no longer
holds (`chain/replay.go`, after the reference's deep reorganizations). The
node then keeps following the best branch it can connect, and in the
background replays the base's ancestors from genesis into `replay/chain.db`,
checking every state root and keeping the undo data of each block above the
fork. That undo data is copied into the database a step at a time, the undo
floor is lowered, and the reorganization goes on. Nothing is pruned while a
replay runs; a failed one is tried again a minute later at the earliest, and a
restart starts it again. The status reports `replaying` meanwhile. An
operator's mark below the window takes the base down the same way.

Views read the database as it is rather than a pinned snapshot. That is
correct for every tree whose block descends from the base: moving the base
forward along a tree's branch never changes what its stubs stand for. A rewind
increments an epoch, and views from before it refuse to read. A state the API
derives across a rewind is not kept.

Restart reads the metadata, rebuilds the block index from the stored-block
rows without reading the block files, scans only the frames after the cursor,
and attaches the base's stored state, checked against its header's root. Only
the blocks above the base execute again, and none at all after a clean stop or
`init`, which settle the base at the tip.

The transaction and event indexes are written for flushed blocks and filtered
by the canonical chain when read, so a reorganization never deletes rows. The
node indexes in memory only the blocks above the base.

### Without a database

The chain keeps states for the last `RetainDepth` blocks and every
`CheckpointInterval`-th canonical block (default 1000). A block whose parent
state was dropped is validated after re-executing stored blocks from the
nearest retained ancestor. Tests, the conformance runner and the replay tools
use this mode.

## The VM

`wasm.Decode` is a single pass: section framing, shape rules, every count
checked against the bytes left before allocating, and each function body
validated with the exact algorithm of §25 while it is compiled into a flat
`[]Instr`. Compilation resolves every branch to a target pc, the operand
height to cut to and the arity, so the interpreter never searches for an
`end`; `else` becomes a free jump, block `end`s disappear, and the function's
final `end` becomes a free return. The validator records each function's
reservation (parameters + locals + maximum operand height, unknowns counted)
as consensus data.

`vm.Machine` interprets the compiled code. The dispatch loop keeps pc, sp,
fp and the remaining compute in locals and writes them back only around
calls, host functions and exits. Each instruction's static price is stored
inline; loads and stores are priced from a precomputed table of the 65,537
memory sizes (§15.2).

Budgets (§11) are nested frames. A child's budget never exceeds what its
parent has left and both are charged alike from then on, so each frame is a
single absolute limit on the transaction's consumption and a debit fits every
enclosing frame exactly when it fits the innermost one: an atomic debit is
four comparisons, not a walk up the frames. The block-capacity guard (§21.6)
is a second limit checked after the frame, so an unaffordable debit never
counts toward the block.

Nested calls and deployments never use the native stack (§20.4, §24.4):
`call` and `deploy` suspend the machine and return a request to the driver
in `vm/tx.go`, which keeps an explicit stack of activations, runs the child,
and resumes the parent with the child's status. Function calls inside an
instance use an explicit frame stack too, so nesting is bounded only by
budgets.

Validated modules are cached by CodeHash (`vm.CodeCache`, LRU, safe for
concurrent use, invalid verdicts included). Charges never depend on it: code
reads and prices are debited exactly as on a cold start (§23.4).

## Chain and node

`chain.Chain` is a header index. Every header is checked in full when it is
indexed: seed list, version, chain, height, parent, median time, exact
target, votes, PoW, used ≤ limits, and the clock. Each entry has a status:
header, stored, valid or invalid. Block processing is serialized by one
mutex, and the index has a read-write lock, so API readers never wait on
execution.

The best header is the indexed header with the most work that is not invalid.
Bodies are downloaded only along its branch. A body is stored once it matches
its header's commitments. A branch is executed only when it becomes the best
complete branch that overtakes the tip by the fork-choice margin (§8.2), most
work first. A failure marks the block and its descendants invalid. Peers that
delivered any of those blocks become offenders, and the node bans them.

Header chains with less work than the tip's minus 1,440 blocks are refused
unless they are already indexed. A peer's headers below that threshold are
checked without being kept, with a salted bit every 600 headers, and fetched
again once they pass (presync). The operator's marks (`marks.json`) refuse a
marked block and everything above it without blaming peers.

Tip hooks (`OnTip`) run synchronously in the commit path, so the node's
transaction and event indexes and its pool follow every tip. Subscribers
(`Subscribe`) get a buffered copy for logging and announcements.

Blocks are stored in the reference node's block-file layout
(`chain.FileStore`):

- Segments `blocks/NNNNNNNN.dat` of up to 128 MiB, each beginning with
  `TINGBLK\0` and the ChainId.
- Frames of `u64 length ‖ block ‖ H("node-block", block)`.
- Every frame is forced to disk before `Put` returns.

On open, the index of frame locations is rebuilt by scanning. A torn or
zero-filled tail of the last segment is truncated, and a frame whose checksum
fails is passed over and its block fetched again. Files made for another
genesis are refused. On load, blocks stored before their parents (bodies
arrive out of order) are retried until no more connect.

With a database, frames are forced to disk before every commit that moves
the base past them, and otherwise at most 2 s after they are written; a frame
lost in a crash lies above the base and is fetched again.

A block whose frame fails when it is read later, for executing it,
re-deriving a state, moving the base or a replay, is forgotten and fetched
again rather than stopping the node, as in the reference. A block not yet
executed goes back to a header, so no branch through it connects until it is
stored again. An executed one stays valid and is fetched before any other
block, and the base waits for it. The block is stored again in a new frame,
which a restart reads in place of the one the database lists. The database
(below) replaces the state snapshots earlier versions wrote; `ting replay
-snapshot` and `ting snapshot` remain as tools.

The pool and the deferred set (`node/txpool.go`) follow the reference node's
policy:

- The pool holds 1,024 transactions and `max(16 MiB, 4 × D)` bytes, with fair
  share by source address and FirstIn turns.
- The deferred set holds 4,096 entries, 256 per source. A due entry is tried on
  the tip before it is admitted, and it stays in the set until a successful
  canonical inclusion above its `since`.
- A template throws away at most one block's compute.

The miner hashes the constant prefix of the domain-separated header once per
worker and resumes SHA-256 from that midstate for each nonce. It takes a fresh
template every 2^24 nonces per thread, so the timestamp moves on and new
transactions get in.

`internal/daemon` runs a node from a data directory:

- It reads `config.json`, `genesis.bin` and `control.token`, and holds
  `node.lock`.
- It serves the control channel (`internal/control`) on loopback. Each request
  needs an HMAC-SHA-256 proof keyed by the token over both sides' nonces; the
  token itself never crosses the connection.
- It manages the REST API (`node.API`), which can be enabled and disabled at
  run time.

## Peer protocol

`internal/p2p` implements the reference node's peer protocol
(`docs/peer-protocol.md`). Frames are read with a buffer that grows as bytes
arrive, so a length field reserves nothing. Fields decode strictly: every count
and length is within its bound, flags are 0 or 1, work is in its shortest form,
and no byte is left over.

A `Conn` is a connection this node opened. Any number of goroutines may send
requests on it. Answers are matched to requests in order through a FIFO of
waiters, up to the 64 requests the reference node pipelines. Announcements go
to their own channel, and a keep-alive `state` request goes out after 30 idle
seconds.

The `Syncer` asks for headers page after page, continuing from the last
header received, and hands each page to `chain.HeadersFrom`. It then asks the
chain which blocks to download (`BlocksWanted`):

- The window is 1,024 blocks beyond the lowest missing one, at most 64 per
  peer, and never the same block from two peers.
- Each block has a transfer time of 60 s + 10 s per MiB.
- Blocks are fetched with `raw` in slices and processed as they arrive.

The `Server` answers inbound peers through a `Backend` (implemented by
`node.Peers`):

- Each connection is handled sequentially, which keeps answers in request
  order. Announcements are written between answers under the same write lock.
- Request units are a token bucket per connection, charged by the ceiling of
  bytes / 64 KiB.
- A pushed block's header is checked on its first slice.

`node.Peers` keeps the outbound connections. Configured peers are always
connected. Discovered peers come from the address book (`peers.json`, at most
1,024), up to `max_peers` in all, from distinct /16 or /32 groups, with one
more while the tip is stale. Each connection runs four loops:

- the syncer;
- a push loop that sends our blocks with `import` when our tip has more work;
- a pool loop that compares pending and deferred inventories both ways;
- an address loop.

## Where it scales and where it does not yet

Scales:

- validation memory and work are proportional to input bytes everywhere a peer
  controls a length or count;
- state writes cost O(log n), and recent states are shared, not copied;
- ordered commitments, state rehashing and nonce search use all cores;
- execution depth is bounded by budgets, not the Go stack;
- a cheap low-work branch costs neither memory nor execution (threshold and
  presync), and side branches are executed only when they become the best;
- linear memory comes from a pool by size class and is zeroed when an
  invocation ends, and memories above 64 MiB are mapped outside the Go heap
  (allocating a module's whole declared memory for every call had kept the
  garbage collector busier than the interpreter);
- with a database, memory holds the window above the base, the header index
  and caches, and restart time depends on the window, not the chain.

Measured on an Apple M3:

- The interpreter runs about 1.1 billion C per second on arithmetic loops
  (about 2.4 ns per instruction). A block at the development capacity of 100M C
  executes in about 90 ms.
- Validating blocks of storage-writing transactions runs at about 120,000
  transactions per second.

Measured on the production chain (538 blocks): `init` imports the seed in
2.0 s; a restart takes about 20 ms and 45 MB resident. The chain processes
about 7,400 small blocks a second in memory and 3,300 with the database.
Syncing from one peer is capped near 100 small blocks a second by the peer
protocol's request units (200 a second, 2 per `raw`), as with the reference
node; more peers multiply it.

Not yet:

- Every header stays in memory (about 500 bytes each). The reference keeps
  only the headers above a settled height in memory.
- The interpreter checks compute before every instruction. Charging per
  straight-line run, or a register form, would speed it up.

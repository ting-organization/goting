# goting

A Go implementation of the Ting protocol and node: canonical encodings, the
state tree, the metered WebAssembly VM, the host interface, deploy/invoke,
block validation, difficulty, capacity voting, fork choice, the peer
protocol, and a node that operates like the reference node.

Status: **stage 3**.

- Stage 1 was written from `ting-protocol-rules.md` alone. It validated the
  production network's 325 seed blocks and every later block on the first
  attempt.
- Stage 2 added the peer protocol, so this node syncs from, serves and relays
  to reference nodes.
- Stage 3 compared it with the reference implementation
  ([ting-organization/ting](https://github.com/ting-organization/ting)). All of
  the reference's 151 conformance items pass, and no consensus difference was
  found. The node now follows the reference node's policies and interfaces:
  header-first sync, pool and deferred-set policy, discovery and bans, block
  files, operator marks, the `/v1` REST API, the control channel, the data
  directory and the CLI.

Findings, open questions and the remaining differences are logged in
[docs/FINDINGS.md](docs/FINDINGS.md). The design is described in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Build and test

Requires Go 1.24 or newer. There are no dependencies outside the standard
library.

```bash
go install github.com/ting-organization/goting/cmd/ting@latest
```

or, in a checkout:

```bash
go build -o bin/ting ./cmd/ting
```

```bash
go test ./...
```

With the reference repository checked out beside this one (or named by
`TING_REFERENCE`), the tests also run its conformance material:

```bash
bin/ting conformance -dir ../ting-reference/conformance
```

## Running a node

The node commands match the reference node's CLI. They use a data directory,
`~/.ting` by default or the one named by the global `--data-dir DIR`. Results
print as JSON; errors print as `ting: <error>` with exit status 1.

```bash
bin/ting init
```

This creates a production node: `config.json`, `genesis.bin`, `control.token`
(mode 0600) and the imported production seed. Use `--dev` for a private
development chain (it imports the 519-block development seed), or
`--genesis FILE` to join another network. Other flags: `--name`, `--control`,
`--p2p` (default `0.0.0.0:18442` on production, loopback otherwise),
`--api-bind` and `--api`.

```bash
bin/ting start
```

Runs the node in the background with output to `node.log`. It waits until the
node answers, then prints its status. `ting run` runs it in the foreground;
`ting stop` stops it.

On production, a node finds the network through the built-in seed node with no
peer configured. It keeps up to `max_peers` (8) connections from distinct
network groups, shares proven addresses, and bans peers that send invalid data
for a day, by range.

```bash
bin/ting status
bin/ting peers
bin/ting votes
```

`status` shows the reference's status document plus the listeners. `peers`
lists configured and discovered peers with their sync status. `votes` shows
capacity voting so far this epoch, the capacities already decided, and this
node's votes.

```bash
bin/ting add node 203.0.113.5:18442
bin/ting api enable --cors
bin/ting miner start --threads 4 --spacing 60
bin/ting miner votes increase hold hold hold
bin/ting miner producer 6869
```

Settings changes apply to the running node and are saved in `config.json`, or
only saved when no node is running. Other forms: `remove node`, `api disable`,
`miner stop` and `miner status`.

```bash
bin/ting mine --count 5
bin/ting template --tx <id>
bin/ting solution <template> <nonce>
```

These are for development mining and external miners. A template carries
`header_hex` and the nonce's place (offset 278, 16 bytes, little-endian). A
solution is checked against the target and then imported.

```bash
bin/ting submit tx.bin --not-before 1800000000
```

Submits a transaction envelope. With `--not-before` it is deferred: held until
due, and admitted once it executes successfully on the tip.

```bash
bin/ting invalidate <id|height>
bin/ting reconsider <id|height>
```

Operator marks, kept in `marks.json`. The node leaves a marked block and every
block above it, and refuses them from peers without banning the peers.

```bash
bin/ting probe 2.29.97.212:18442
```

Says hello over the peer protocol and shows the answer.

### REST API

The REST API is off by default (`--api` at init or `ting api enable`) and
listens on `127.0.0.1:18441`. It serves the reference node's `/v1` interface
with the same field names, paging (`from`/`limit` ≤ 100, `next`, stopping
before 16 MiB), error mapping and protections:

| Method | Path |
|---|---|
| GET | `/health`, `/v1/status`, `/v1/metrics` |
| GET | `/v1/blocks?from&limit`, `/v1/blocks/{height\|id}`, `/v1/blocks/{id}/raw?offset&length` |
| GET | `/v1/transactions/{id}?from&limit` (occurrences, pending, deferred) |
| POST | `/v1/transactions` `{"hex", "not_before"?}` |
| GET | `/v1/mempool`, `/v1/votes` |
| GET | `/v1/events?from&limit&contract&topic` |
| GET | `/v1/contracts/{id}`, `/v1/contracts/{id}/storage/{key}?proof=true` |
| POST | `/v1/storage` `{"reads": [{"contract", "key"}], "proof"?}` |

Storage reads can carry Merkle proofs against the state root, in the
reference's proof format.

### Running from flags

```bash
bin/ting node -network development -data ./data -mine
```

Runs a node configured by flags instead of a data directory's `config.json`.
This is useful for tests and scripts.

## Tools

| Command | What it does |
|---|---|
| `ting info -network N` | Prints a network's genesis configuration, ChainId and GenesisBlockId |
| `ting replay -bundle F [-list L]` | Validates a seed bundle block by block (rules §27.2) and reports the first differing receipt field |
| `ting sync -network production` | Validates a peer's chain block by block |
| `ting peer -ask state\|headers\|block\|addresses` | Asks a peer one question |
| `ting probe -network N` | Sends read-only requests whose answers compare nodes |
| `ting tx deploy\|call ...` | Builds a transaction envelope |
| `ting decode -block\|-tx\|-receipt` | Decodes one item |
| `ting devnet -blocks N -out F` | Mines a sample development chain that exercises the VM |
| `ting stats -bundle F` | Summarizes what a bundle exercises |
| `ting snapshot -in F` | Prints a state snapshot as JSON lines |
| `ting conformance -dir D` | Runs the reference's vectors, voting chain and seeds |

## Data directory

| File | Content |
|---|---|
| `config.json` | Settings, with the reference's fields; unknown fields are refused |
| `genesis.bin` | The network's 150-byte genesis |
| `control.token` | 64 hex characters (mode 0600); its HMAC proofs authorize the control channel |
| `runtime.json` | The running node's pid and listeners; removed on a clean stop |
| `node.lock` | One node per directory |
| `blocks/NNNNNNNN.dat` | Block files: `TINGBLK\0`, ChainId, then `u64 length ‖ block ‖ H("node-block", block)` frames, as the reference lays them out |
| `state.snap` | A state snapshot every 1,000 blocks, so a restart does not re-execute the chain |
| `peers.json`, `deferred.json`, `marks.json` | Address book, deferred transactions, operator marks |

## Layout

| Package | Rules | Role |
|---|---|---|
| `internal/hashing` | §2.4, §15.5 | Domain-separated SHA-256 and hash prices |
| `internal/codec` | §2.1–2.2 | Canonical fixed-width encoding |
| `internal/types` | §3–§5, §28 | Genesis, headers, bodies, envelopes, receipts and identifiers |
| `internal/commit` | §2.5 | Ordered commitments (VectorRoot) |
| `internal/state` | §6 | Persistent compact sparse Merkle tree, journaled overlay and proofs |
| `internal/wasm` | §16–§19, §25, §26.1 | Strict decoder, validator, compiler and code price |
| `internal/vm` | §11–§15, §19–§24, §26 | Metered interpreter, host interface and deploy/invoke driver |
| `internal/exec` | §7.3, §14 | Block execution and assembly with the capacity guard |
| `internal/consensus` | §8–§10 | Work, retarget, median time and capacity voting |
| `internal/chain` | §7, §8, §27 | Header index, validation, fork choice, block files and operator marks |
| `internal/miner` | — | Block templates and parallel nonce search |
| `internal/seed` | §27 | Bundles, lists, replay and the built-in seeds |
| `internal/conformance` | — | Runner for the reference's conformance material |
| `internal/p2p` | Peer protocol | Frames, messages, client, server and header-first sync |
| `internal/node` | — | Pool and deferred set, mining, peers, discovery and the REST API |
| `internal/control` | — | Control channel |
| `internal/daemon` | — | Data directory, configuration, and the node's control methods |
| `internal/samples`, `internal/wasm/wasmtest` | — | Test programs assembled without a toolchain |

The built-in seed bundles in `internal/seed/bundles` and the protocol documents
in `docs` come from the reference repository (MIT or Apache-2.0); see
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).

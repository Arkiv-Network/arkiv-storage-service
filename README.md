# arkiv-storage-service

The Go EntityDB — a private query index for Arkiv databases.

## What it is

`arkiv-storage-service` is a standalone Go process that maintains a queryable index of Arkiv entity state. It is one of three components in the Arkiv EntityDB architecture:

- **EntityRegistry** — a smart contract on the L3 that validates all Arkiv mutations, emits logs, and stores `arkiv_stateRoot` per block
- **Reth ExEx** — an execution extension inside the L3 Reth node that watches sealed blocks, parses `EntityRegistry` logs, and forwards operations here via JSON-RPC
- **arkiv-storage-service** (this repo) — consumes chain events from the ExEx, maintains the entity state index, and serves queries to SDK clients

## What it does

The service maintains two storage layers backed by PebbleDB:

- A **Merkle Patricia Trie** (via go-ethereum's `StateDB`) that holds one account per entity. Each entity account's `codeHash` commits to the full entity RLP, and a system account holds annotation bitmap hashes and ID→address mappings as trie-committed storage slots.
- **PebbleDB bitmaps** (`roaring64`) that index entities by annotation key/value pairs, enabling fast equality, range, and glob queries.

After processing each block, the service returns `arkiv_stateRoot` — the trie root — to the ExEx, which submits it to the `EntityRegistry` contract. This anchors the private trie to the L3 world state, enabling a two-level Merkle proof chain from entity payload all the way to L1.

## JSON-RPC interface

The service exposes two JSON-RPC endpoints:

**Chain ingest** (called by the ExEx only):
- `arkiv_commitChain` — apply a sequence of blocks to the canonical head
- `arkiv_revert` — revert blocks back to a common ancestor
- `arkiv_reorg` — atomically revert and re-apply blocks

**Query server** (called by SDK clients):
- `arkiv_query` — SQL-like queries against latest or historical state
- `arkiv_getEntity` — fetch a single entity by address
- `arkiv_getEntityProof` — Merkle proof for entity payload verification
- `arkiv_getBitmapProof` — Merkle proof for equality query completeness verification

## Further reading

Full architecture design: [`architecture.md`](architecture.md)

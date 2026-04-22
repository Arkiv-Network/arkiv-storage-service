# Arkiv EntityDB: Reth ExEx + Standalone Go Service

## Contents

- [Abstract](#abstract)
- [1. Architecture Overview](#1-architecture-overview)
  - [Components](#components)
  - [Data Flow](#data-flow)
- [2. EntityRegistry Smart Contract](#2-entityregistry-smart-contract)
  - [Role](#role)
  - [arkiv_stateRoot Storage](#arkiv_stateroot-storage)
  - [Verification](#verification)
- [3. ExEx → EntityDB JSON-RPC Interface](#3-exex--entitydb-json-rpc-interface)
  - [ExEx Filtering and Forwarding](#exex-filtering-and-forwarding)
  - [Forwarded Types](#forwarded-types)
  - [arkiv_commitChain](#arkiv_commitchain)
  - [arkiv_revert](#arkiv_revert)
  - [arkiv_reorg](#arkiv_reorg)
- [4. Go EntityDB Internals](#4-go-entitydb-internals)
  - [Dependencies](#dependencies)
  - [Role: Query Index](#role-query-index)
  - [Entity Accounts](#entity-accounts)
    - [Address Derivation](#address-derivation)
    - [Account Structure](#account-structure)
    - [EntityRLP](#entityrlp)
    - [entity_id](#entity_id)
    - [System Account](#system-account)
  - [Annotation Bitmaps](#annotation-bitmaps)
    - [Content-Addressed Storage](#content-addressed-storage)
    - [PebbleDB Layout](#pebbledb-layout)
    - [Write Path](#write-path)
    - [Read Path](#read-path)
  - [Lifecycle](#lifecycle)
    - [Create](#create)
    - [Update](#update)
    - [Delete](#delete)
    - [Entity Expiration (Housekeeping)](#entity-expiration-housekeeping)
    - [Extend / ChangeOwner](#extend--changeowner)
  - [Block Commit and State Root](#block-commit-and-state-root)
- [5. Reorg Handling](#5-reorg-handling)
  - [Trie Reversion](#trie-reversion)
  - [PebbleDB Journal](#pebbledb-journal)
  - [Revert Procedure](#revert-procedure)
  - [Journal Pruning](#journal-pruning)
- [6. Query Execution](#6-query-execution)
  - [Latest-State Queries](#latest-state-queries)
    - [Equality and Inclusion Queries](#equality-and-inclusion-queries)
    - [Range Queries](#range-queries)
    - [Glob / Prefix Queries](#glob--prefix-queries)
  - [Historical Queries](#historical-queries)
    - [Historical Equality Queries](#historical-equality-queries)
    - [Historical Range and Glob Queries](#historical-range-and-glob-queries)
  - [Query Completeness Proofs](#query-completeness-proofs)
  - [No Completeness Guarantee for Range / Glob](#no-completeness-guarantee-for-range--glob)
- [7. Query Server HTTP API](#7-query-server-http-api)
- [8. Summary](#8-summary)
  - [Storage Layout](#storage-layout)
  - [Component Responsibilities](#component-responsibilities)
- [9. Open Question: Gas Model](#9-open-question-gas-model)

---

> **⚠ Work in progress.** The mechanism by which `arkiv_stateRoot` is fed back from the EntityDB to the `EntityRegistry` contract is not fully designed. Specifically: whether the ExEx→EntityDB JSON-RPC call should be synchronous or asynchronous, how the resulting state root is submitted to the contract without impacting L3 block production, and whether the submission transaction requires gossip are all open questions. This affects the verifiability model described throughout this document. Read §2 (arkiv_stateRoot Storage) for the known open points.

---

## Abstract

This document describes the Arkiv EntityDB architecture, composed of three components: an `EntityRegistry` smart contract deployed on the Reth chain, a Reth execution extension (ExEx), and a standalone Go EntityDB service.

Arkiv databases are L3s. The Reth node, the `EntityRegistry` contract, and the ExEx all run on the L3. The L3 settles against an L2 (OP Stack), which in turn settles against Ethereum L1.

All Arkiv mutations flow through the `EntityRegistry` contract. The contract validates each operation and emits logs that the ExEx parses and forwards to the Go EntityDB. After the EntityDB processes each block it produces `arkiv_stateRoot` — the root of its internal entity state trie. The ExEx submits this root back to the `EntityRegistry` contract, where it is stored per block in contract storage and committed in the main chain's `stateRoot` at the next block. This is the verifiability anchor.

The Go EntityDB maintains a private query index — a Merkle Patricia Trie and PebbleDB annotation bitmaps — optimised for fast entity lookup and SQL-like queries. Although the trie is private (not the main Reth world state), its root is anchored on-chain, enabling a two-level proof chain:

1. EntityDB provides a Merkle proof of entity payload against `arkiv_stateRoot` at block N (entity trie proof).
2. Reth provides a Merkle proof of `arkiv_stateRoot` against the L3 `stateRoot` at block N+1 (anchor proof via contract storage).

**What this design provides:**
- Verifiable entity payloads: two-level Merkle proof chain anchored in the main chain's `stateRoot`
- On-chain validation of all Arkiv operations via the `EntityRegistry` contract
- Fast SQL-like queries (latest and historical) via the Go EntityDB query index
- Clean reorg handling without full state replay — no special reorg logic needed for the `arkiv_stateRoot` commitment
- No modification to Reth required — the ExEx is a read-only observer

**What this design does not provide:**
- Query completeness proofs for range or glob queries (the annotation key space is not enumerable from on-chain state)
- Zero-lag verifiability — `arkiv_stateRoot` for block N is committed at block N+1

---

## 1. Architecture Overview

### Components

```
SDK / clients
     |
     | EntityRegistry.execute(Op[] ops)
     | (standard L3 transaction)
     v
┌─────────────────────────────────────────────────────────────────┐
│  L3 Reth Node                                                    │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  EntityRegistry (smart contract, on L3)                   │   │
│  │  - Validates Arkiv operations                             │   │
│  │  - Emits logs consumed by ExEx                            │   │
│  │  - Stores arkiv_stateRoot[N] submitted by ExEx            │   │
│  │  - Committed in L3 stateRoot on every block               │   │
│  └──────────────────────────────────────────────────────────┘   │
│                    ▲ arkiv_stateRoot[N]                          │
│                    │ (tx submitted by ExEx after block N)        │
│  ┌─────────────────┴────────────────────────────────────────┐   │
│  │  Arkiv ExEx                                               │   │
│  │  - Watches sealed blocks for EntityRegistry calls         │   │
│  │  - Parses logs, forwards ops to Go EntityDB               │   │
│  │  - Submits arkiv_stateRoot returned by EntityDB           │   │
│  └─────────────────────────────────┬────────────────────────┘   │
└────────────────────────────────────│────────────────────────────┘
                                     │ HTTP JSON-RPC
                                     ▼
┌─────────────────────────────────────────────────────────────────┐
│  Go EntityDB Process (query index)                               │
│                                                                  │
│  ┌─────────────────────┐   ┌──────────────────────────────┐    │
│  │  Chain Ingest API   │   │  Query Server                 │    │
│  │  commitChain        │   │  arkiv_query                  │    │
│  │  revert             │   │  arkiv_getEntity              │    │
│  │  reorg              │   │                               │    │
│  └──────────┬──────────┘   └──────────────┬───────────────┘    │
│             │                              │                     │
│             ▼                              ▼                     │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  State Engine                                             │   │
│  │  - go-ethereum StateDB / Trie (library, not node)        │   │
│  │  - PebbleDB: entity code, bitmaps, ID maps, journal      │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
                                     ▲
                        HTTP JSON-RPC (query clients)
                        SDK / frontend / other services
```

### Data Flow

1. A client submits a standard L3 transaction calling `EntityRegistry.execute(Op[] ops)`. The contract validates each operation and emits a log per operation. The resulting storage changes are committed in the L3 block's `stateRoot` when the block is sealed.
2. Reth commits the sealed block. The ExEx receives a `ChainCommitted { chain }` notification.
3. For each block, the ExEx filters to successful calls to `ENTITY_REGISTRY_ADDRESS`, reads the emitted logs to extract the typed operations (including `txSeq`, `opSeq`, and `entity_address` for Create ops), and assembles one `ArkivBlock` per block. Blocks with no `EntityRegistry` calls are still forwarded with an empty `operations` list.
4. The ExEx calls `arkiv_commitChain` on the Go EntityDB and receives `arkiv_stateRoot` in the response — the root of the EntityDB's trie after applying block N. *(Open: whether this call is synchronous and whether waiting for it delays ExEx completion, which could affect block production, is not yet resolved — see §2.)*
5. The ExEx submits a transaction to `EntityRegistry.setArkivStateRoot(blockN, arkiv_stateRoot)`, targeting block N+1. *(Open: the submission mechanism — whether this goes through the normal mempool and gossip, or is a privileged direct injection — is not yet designed.)*
6. Query clients call `arkiv_query` or `arkiv_getEntity` on the Go EntityDB's query server.

On `ChainReverted`, the ExEx sends `arkiv_revert` with block identifiers only. The EntityDB replays its journal in reverse. The `arkiv_stateRoot` commitments for the reverted blocks are also reverted automatically — they live in contract storage, which reverts with the chain. On `ChainReorged`, the ExEx sends `arkiv_reorg` atomically, then submits fresh `arkiv_stateRoot` values for the new chain.

---

## 2. EntityRegistry Smart Contract

### Role

The `EntityRegistry` is a smart contract deployed on the L3. It is the single on-chain entry point for all Arkiv mutations. Clients submit standard L3 transactions calling `EntityRegistry.execute(Op[] calldata ops)`. The contract:

- Validates each operation (ownership checks, expiry, correct caller)
- Dispatches to per-operation logic (`_create`, `_update`, `_extend`, `_transfer`, `_delete`, `_expire`)
- Emits a log per operation, consumed by the ExEx to drive the EntityDB
- Stores `arkiv_stateRoot` per block, submitted by the ExEx after it processes each block

The internal changeset hash mechanism (see `experiments/change-set-hash-v3.md`) is also maintained by the contract and provides an independent audit trail of all mutations, but it is not the primary verifiability mechanism in this design.

### arkiv_stateRoot Storage

> **⚠ This section describes the intended design. Several aspects are unresolved.**

After the EntityDB processes block N it returns `arkiv_stateRoot_N`. The ExEx then submits a transaction calling:

```solidity
function setArkivStateRoot(uint64 blockNumber, bytes32 stateRoot) external onlyExEx;
```

The contract stores this in a mapping committed in the L3's world state:

```solidity
mapping(uint64 => bytes32) public arkivStateRoots;
// arkivStateRoots[N] = arkiv_stateRoot_N, stored at block N+1
```

**Open questions:**

- **Synchrony.** The ExEx currently calls `arkiv_commitChain` synchronously and blocks until the EntityDB returns `arkiv_stateRoot`. Depending on how long the EntityDB takes to apply a block, this may delay the ExEx from emitting `FinishedHeight`, potentially creating backpressure on block production. An asynchronous model (ExEx fires-and-forgets, EntityDB pushes the root later) avoids this but complicates sequencing.

- **Transaction submission.** Once the ExEx has `arkiv_stateRoot_N`, it needs to get it into block N+1 as a contract call. The straightforward path is submitting a signed transaction through the normal mempool. Whether this transaction needs to be gossiped across the L3 network, or can be injected directly (as a system transaction by the sequencer), depends on the L3 sequencer design and has not been decided.

- **Missed submission.** If the submission transaction is not included in block N+1 (e.g., due to gas, nonce issues, or sequencer censorship), `arkivStateRoots[N]` will be absent or stale. The retry and recovery strategy is not yet defined.

### Verification

The two-level proof chain works as follows for a client that wants to verify entity payload P at block N:

**Step 1 — entity proof (EntityDB trie → arkiv_stateRoot).**
`arkiv_getProof(entityAddress, blockN)` returns a Merkle proof from `arkiv_stateRoot_N` to the entity's account node, proving `codeHash = keccak256(RLP(entity))`. The client fetches the RLP bytes and verifies `keccak256(RLP(entity)) == codeHash` to confirm the payload.

**Step 2 — anchor proof (L3 stateRoot → contract storage).**
`eth_getProof(ENTITY_REGISTRY_ADDRESS, [slot(arkivStateRoots[N])], blockHash_{N+1})` proves that `arkivStateRoots[N] = arkiv_stateRoot_N` against the L3 `stateRoot` at block N+1. The L3 `stateRoot` is covered by the OP Stack fault proof system and anchored to L2 and ultimately L1.

Combining both steps: the payload is bound to `codeHash`, `codeHash` is in the entity account committed under `arkiv_stateRoot_N`, and `arkiv_stateRoot_N` is committed in the L3 world state at block N+1.

**Reorg safety.** `arkivStateRoots[N]` lives in contract storage and reverts with the chain if block N+1 is reorged out. The EntityDB journal independently reverts block N's effects. No special reorg handling is required for the commitment — both sides converge automatically.

---

## 3. ExEx → EntityDB JSON-RPC Interface

All methods are JSON-RPC 2.0 over HTTP. The ExEx is the only caller of the chain ingest methods. Query clients call the query methods directly.

### ExEx Filtering and Forwarding

Reth delivers chain events to an ExEx via the `ExExNotification` enum:

```rust
pub enum ExExNotification {
    ChainCommitted { chain: Arc<Chain> },
    ChainReverted  { chain: Arc<Chain> },
    ChainReorged   { old_chain: Arc<Chain>, new_chain: Arc<Chain> },
}
```

`Chain` is Reth's type for a contiguous sequence of executed blocks. It exposes an iterator over `(SealedBlockWithSenders, ExecutionOutcome)` pairs — one per block — where `SealedBlockWithSenders` carries the sealed header, the full ordered transaction list with recovered senders, and `ExecutionOutcome` carries the receipts for each transaction in the same order.

The ExEx does not forward full blocks. For each block in the chain it:

1. Reads the sealed header to extract the three fields the EntityDB needs: `number`, `hash`, `parent_hash`.
2. Zips transactions (with senders) against their receipts.
3. Filters to transactions where `tx.to == ENTITY_REGISTRY_ADDRESS` and `receipt.status == 1`. Failed calls are discarded — the contract reverted and no state changes were applied, so the EntityDB must not apply them either.
4. For each passing transaction, reads the logs emitted by the contract to extract the typed operation list. Each log includes the contract's internal `txSeq` and `opSeq` counters (the same counters driving the V3 changeset hash) and the derived `entity_address`. These are used as-emitted — the ExEx does not recompute them from calldata positions.
5. Passes `expires_at` through as a block number directly from the contract log — no timestamp conversion is needed. The EntityDB housekeeping process works in block numbers.

If a block contains no Arkiv transactions it is still forwarded as an `ArkivBlock` with an empty `operations` list. The EntityDB must advance its state root for every block in the canonical chain, even empty ones, so that block-number-to-state-root mappings remain complete.

For `ChainReverted` and the `old_chain` side of `ChainReorged`, the ExEx does **not** re-parse calldata. The EntityDB reverts using its own journal; it needs only the block identifiers (number + hash) in newest-first order.

### Forwarded Types

The Rust types the ExEx constructs and serializes:

```rust
/// A block header subset — only the fields the EntityDB needs.
///
/// - number:      used as the block-number key in the journal, in the arkiv_root
///                mapping (blockHash → stateRoot), and for housekeeping (comparing
///                block.number against entity.expires_at to identify expired entities).
/// - hash:        used as the other half of the journal key and for the arkiv_root
///                mapping. Also the identifier returned to the ExEx so it can submit
///                the arkiv_stateRoot commitment referencing the correct block.
/// - parent_hash: used as a continuity check — the EntityDB verifies that each
///                incoming block's parent_hash matches the hash of the block it
///                processed immediately before. This guards against gaps or
///                out-of-order delivery. Could be dropped if the ExEx guarantees
///                strict ordering, but it is cheap to include.
#[derive(Serialize)]
pub struct ArkivBlockHeader {
    pub number:      u64,
    pub hash:        B256,
    pub parent_hash: B256,
}

/// A block containing only the Arkiv operations extracted from successful transactions.
/// Non-Arkiv transactions and failed transactions are omitted entirely.
#[derive(Serialize)]
pub struct ArkivBlock {
    pub header:     ArkivBlockHeader,
    pub operations: Vec<ArkivOperation>,  // empty if the block contained no Arkiv transactions
}

/// Minimal block identifier used for revert payloads.
#[derive(Serialize)]
pub struct ArkivBlockRef {
    pub number: u64,
    pub hash:   B256,
}

#[derive(Serialize)]
#[serde(tag = "type", rename_all = "camelCase")]
pub enum ArkivOperation {
    Create(CreateOp),
    Update(UpdateOp),
    Delete(DeleteOp),
    Extend(ExtendOp),
    ChangeOwner(ChangeOwnerOp),
}

#[derive(Serialize)]
pub struct CreateOp {
    pub tx_seq:       u32,       // EntityRegistry's per-block transaction sequence counter
    pub op_seq:       u32,       // EntityRegistry's per-transaction operation sequence counter
    pub sender:       Address,   // becomes Creator in EntityRLP
    pub payload:      Bytes,
    pub content_type: String,
    pub expires_at:   u64,       // block number, passed through directly from contract log
    pub owner:        Address,
    pub annotations:  Vec<Annotation>,
    // entity_address is also emitted in the contract log and can be included here
    // for convenience, but is fully derivable as keccak256(blockNumber || tx_seq || op_seq)[:20]
    pub entity_address: Address,
}

#[derive(Serialize)]
pub struct UpdateOp {
    pub entity_address: Address,
    pub payload:        Bytes,
    pub content_type:   String,
    pub expires_at:     u64,     // block number
    pub annotations:    Vec<Annotation>,
}

#[derive(Serialize)]
pub struct DeleteOp {
    pub entity_address: Address,
}

#[derive(Serialize)]
pub struct ExtendOp {
    pub entity_address: Address,
    pub new_expires_at: u64,     // block number
}

#[derive(Serialize)]
pub struct ChangeOwnerOp {
    pub entity_address: Address,
    pub new_owner:      Address,
}

#[derive(Serialize)]
#[serde(untagged)]
pub enum Annotation {
    String  { key: String, string_value:  String },
    Numeric { key: String, numeric_value: u64    },
}
```

The ExEx builds one `ArkivBlock` per block, regardless of whether any Arkiv transactions were found. The `operations` field is empty for blocks with no Arkiv activity. For revert payloads it builds `Vec<ArkivBlockRef>`.

### arkiv_commitChain

Apply a contiguous sequence of `ArkivBlock`s to the canonical head. Blocks must be ordered oldest-first. The EntityDB applies them in sequence; if any block fails, the call returns an error and no state from that call is committed.

**Request:**
```json
{
  "method": "arkiv_commitChain",
  "params": [{
    "blocks": [
      {
        "header": {
          "number":     "0x3039",
          "hash":       "0xabc...",
          "parentHash": "0xdef..."
        },
        "operations": [
          {
            "type":          "create",
            "txSeq":         1,
            "opSeq":         1,
            "entityAddress": "0x...",
            "sender":        "0x...",
            "payload":       "0x...",
            "contentType":   "application/json",
            "expiresAt":     13500,
            "owner":         "0x...",
            "annotations": [
              { "key": "type",     "stringValue":  "note" },
              { "key": "priority", "numericValue": 5      }
            ]
          }
        ]
      }
    ]
  }]
}
```

**Response:** `{}` on success, JSON-RPC error on failure.

### arkiv_revert

Revert a contiguous sequence of blocks from the canonical head back to the common ancestor. Blocks are identified by number and hash only — the EntityDB uses its journal to undo state changes and does not need the original operations. Blocks must be ordered newest-first.

**Request:**
```json
{
  "method": "arkiv_revert",
  "params": [{
    "blocks": [
      { "number": "0x303b", "hash": "0x..." },
      { "number": "0x303a", "hash": "0x..." },
      { "number": "0x3039", "hash": "0x..." }
    ]
  }]
}
```

**Response:** `{}` on success, JSON-RPC error on failure.

### arkiv_reorg

Atomically revert a set of blocks and commit a new set. Semantically equivalent to `arkiv_revert` followed by `arkiv_commitChain`, but issued as a single call so the EntityDB never exposes an intermediate state to concurrent query clients. Reverted blocks are `ArkivBlockRef` (newest-first); new blocks are full `ArkivBlock`s (oldest-first).

**Request:**
```json
{
  "method": "arkiv_reorg",
  "params": [{
    "revertedBlocks": [
      { "number": "0x303a", "hash": "0x..." },
      { "number": "0x3039", "hash": "0x..." }
    ],
    "newBlocks": [
      {
        "header": { "number": "0x3039", "hash": "0x...", "parentHash": "0x..." },
        "operations": []
      },
      {
        "header": { "number": "0x303a", "hash": "0x...", "parentHash": "0x..." },
        "operations": [{ "type": "delete", "entityAddress": "0x..." }]
      }
    ]
  }]
}
```

**Response:** `{}` on success, JSON-RPC error on failure.

---

## 4. Go EntityDB Internals

### Dependencies

The Go EntityDB imports go-ethereum as a library. It uses:

- `go-ethereum/core/state` — `StateDB` for trie-based account and storage management
- `go-ethereum/trie` — Merkle Patricia Trie, `TrieDB` with `HashScheme` for historical root retention. `HashScheme` is go-ethereum's original trie storage scheme: every trie node is written to the database keyed by its hash, and old nodes are never overwritten. This means every historical state root remains accessible as long as the underlying node bytes exist in PebbleDB. It is what makes historical entity queries possible without a separate snapshot mechanism — the EntityDB can open a read-only `StateDB` against any past `arkiv_stateRoot` it has processed. (The alternative, `PathScheme`, writes nodes at fixed paths and overwrites on update, which destroys historical state.)
- `go-ethereum/ethdb/pebble` — PebbleDB backend (the same backend geth uses in production)

It does not import or start any Ethereum node infrastructure: no P2P, no transaction pool, no engine API, no block production, no `eth_*` JSON-RPC namespace.

### Role: Query Index

The Go EntityDB is a query engine. Its private trie and PebbleDB bitmaps exist to serve fast entity lookups and SQL-like annotation queries. It performs no validation — all validation is handled by the `EntityRegistry` contract before operations reach the EntityDB. The EntityDB blindly applies whatever the ExEx forwards.

`arkiv_stateRoot` — the root of the EntityDB's trie after each block — is submitted to the `EntityRegistry` contract by the ExEx and stored in the L3 world state (§2). This anchors the EntityDB's trie to the main chain, enabling the two-level proof chain described in the abstract. The L3 `stateRoot` covers the contract storage slot holding `arkiv_stateRoot`, which is what clients verify against; the EntityDB trie itself is private and not directly covered.

**Immutable vs mutable state.** Two distinct kinds of state live in the EntityDB:

- **Immutable, content-addressed** — entity RLP blobs (`"c" + codeHash`) and bitmap byte arrays (`"arkiv_bm" + hash`) are written once and never overwritten. They piggyback on the trie's versioning mechanism: the trie root changes when content changes, and old content is never deleted. These entries require no journal.
- **Mutable, journaled** — bitmap pointer entries (`"arkiv_annot"`), ID↔address mappings (`"arkiv_id"`, `"arkiv_addr"`), and trie account state are updated in place on each block. These entries are recorded in a per-block journal so they can be reversed on reorg (§5).

### Entity Accounts

#### Address Derivation

Each entity has a dedicated Ethereum account. The account address is the first 20 bytes of the entity key:

```
entity_key     = keccak256(blockNumber || txSeq || opSeq)   // 32 bytes
entity_address = entity_key[:20]                            // 20 bytes — account address in the trie
```

`txSeq` is the `EntityRegistry` contract's internal transaction sequence counter for the current block; `opSeq` is the per-operation counter within that transaction. Both are emitted in the contract log that the ExEx reads — they are the same counters that drive the V3 changeset hash (see `experiments/change-set-hash-v3.md`). `blockNumber` is the L3 block number.

This derivation is chosen because all three inputs are **computable inside the EVM**: `block.number` is a standard opcode, and `txSeq`/`opSeq` are the contract's own counters maintained in storage. An earlier design used `keccak256(txHash || opIndex)`, but `tx.hash` is not accessible in the EVM — there is no opcode for it — so the contract could not verify or emit the derived entity address. With the current scheme the contract can compute and log `entity_address` at creation time, so clients and the ExEx always have the authoritative address from the receipt without needing to re-derive it.

The payload is intentionally excluded from the derivation: the entity address is a pure identity anchor; content commitment is handled by `codeHash`.

#### Account Structure

```
Entity Account  (address = entity_key[:20])
  nonce    = 0
  balance  = 0
  codeHash = keccak256(RLP(entity))   // commits to full entity state in the trie
  code     = RLP(entity)              // stored in PebbleDB under "c" + codeHash

  storage slots: none
```

`codeHash` is set to `keccak256(RLP(entity))`. The Go StateDB stores the RLP bytes in PebbleDB under `"c" + codeHash`. The EntityDB trie is never executed by an EVM, so no special prefix is needed to guard against bytecode interpretation. On every `Update`, the entity is re-encoded, `keccak256` is recomputed, and `SetCode` is called with the new bytes.

A live entity account is never EIP-161-empty: the condition requires `nonce==0 && balance==0 && codeHash==emptyCodeHash`, but a live entity always has `codeHash ≠ emptyCodeHash`. No explicit nonce is needed to keep it alive. After deletion (`SetCode(nil)`), the account becomes EIP-161-empty and `StateDB.Finalise` prunes it from the trie — which is the desired behaviour. The `"arkiv_addr"` PebbleDB entry serves as the tombstone for any operational needs; there is no reason to retain a trie stub for a deleted entity.

#### EntityRLP

```go
type EntityRLP struct {
    Payload            []byte
    Owner              common.Address
    Creator            common.Address
    ExpiresAt          uint64
    CreatedAtBlock     uint64
    ContentType        string
    Key                common.Hash          // full 32-byte key = keccak256(blockNumber || txSeq || opSeq)
    StringAnnotations  []StringAnnotationRLP
    NumericAnnotations []NumericAnnotationRLP
}
```

The full entity state — payload, owner, creator, expiry, creation block, content type, the full 32-byte key, and all annotations — is in one RLP blob stored as the account's code field. Fetching the code bytes and verifying `keccak256(bytes) == codeHash` confirms the payload against the trie.

`Creator` and `CreatedAtBlock` are in the RLP to support built-in annotations (`$creator`, `$createdAtBlock`) without extra storage slots. The full 32-byte `Key` is included so that callers with only the 20-byte `entity_address` can recover the complete key (and therefore the derivation inputs) — the last 12 bytes are not stored anywhere else.

#### entity_id

Each entity is assigned a `uint64` sequential ID at `Create` time, taken from the `entity_count` counter in the system account. The ID→address mapping is **trie-committed** via a system account storage slot (see System Account below), making it provable via `eth_getProof`. Two PebbleDB entries mirror the same information for fast access on the query hot path:

```
"arkiv_id"   + uint64_id (8 bytes big-endian)  →  entity_address (20 bytes)       (fast cache, mutable)
"arkiv_addr" + entity_address (20 bytes)        →  uint64_id (8 bytes big-endian)  (fast cache, mutable)
```

`"arkiv_addr"` is used during Delete and Expire to look up the entity's ID before removing it from bitmaps. `"arkiv_id"` resolves query result IDs to entity addresses without a trie read on the hot path. Both are mutable and journaled for reorg handling.

The system account slot is the authoritative, trie-committed source for ID→address. The PebbleDB entries are a performance cache that avoids trie traversal during query resolution. Both are kept in sync: written together at Create and the slot cleared at Delete/Expire (while the PebbleDB entries remain as tombstones).

#### System Account

A dedicated system account holds global index state:

```
System Account  (address = keccak256("arkiv.system")[:20])
  nonce    = 1                        // prevents EIP-161 pruning — account has no code, so nonce must be non-zero
  storage slots:
    slot[keccak256("entity_count")]                              →  uint64           (monotonically increasing, assigned at Create)
    slot[keccak256("annot" || annotKey || "\x00" || annotVal)]  →  bytes32           (bitmap hash; one per distinct annotation pair)
    slot[keccak256("id"   || uint64_id)]                        →  entity_address    (20 bytes zero-padded to 32; one per live entity)
```

The system account has no code, so its `codeHash` is `emptyCodeHash`. Without nonce=1 it would satisfy the EIP-161 empty condition (`nonce==0 && balance==0 && codeHash==emptyCodeHash`) and be pruned by `StateDB.Finalise` despite holding storage. Entity accounts do not need this treatment — a live entity's `codeHash ≠ emptyCodeHash`, and a deleted entity is intentionally pruned.

All three slot types are trie-committed and therefore included in the `arkiv_stateRoot` that is anchored on-chain:
- `entity_count` — canonical source for ID assignment.
- `annot` slots — commit the current bitmap hash for each `(annotKey, annotVal)` pair.
- `id` slots — commit the ID→address mapping for every live entity, enabling verifiable query completeness proofs (see §6).

### Annotation Bitmaps

#### Content-Addressed Storage

Bitmaps use `roaring64` — compressed bitsets over 64-bit unsigned integers. Each entity is assigned a compact `uint64` ID at `Create` time (Ethereum addresses cannot be stored directly in a roaring bitmap). ID-to-address and address-to-ID mappings are maintained in PebbleDB.

Every version of a bitmap is written to PebbleDB under a content-addressed key:

```
"arkiv_bm" + keccak256(bitmap_bytes)  →  bitmap_bytes   (immutable; never overwritten or deleted)
```

The mutable pointer for a given `(annotKey, annotVal)` pair stores the hash of the current bitmap, not the bytes:

```
"arkiv_annot" + annotKey + "\x00" + annotVal  →  keccak256(bitmap_bytes)   (mutable pointer to current version)
```

The system account slot for the same pair stores the same hash:

```
slot[keccak256("annot" || annotKey || "\x00" || annotVal)]  →  keccak256(bitmap_bytes)
```

Both point to the same value at all times. The trie slot is the authoritative commitment; the `"arkiv_annot"` entry is a fast lookup that avoids a trie read on the hot query path.

This is structurally identical to how geth handles contract code: the trie stores `codeHash`, the raw bytes live in PebbleDB under `"c" + codeHash`. Old bitmap versions are never deleted from `"arkiv_bm"` — a given hash either has its pre-image in the store or it does not.

An append-only existence index tracks every `(annotKey, annotVal)` pair that has ever been written:

```
"arkiv_pairs" + annotKey + "\x00" + annotVal  →  \x01
```

This index is never reverted — once a pair has existed, it is part of the permanent record and enables historical range and glob queries (see §5).

#### PebbleDB Layout

```
"c"            + codeHash (32 bytes)                    →  RLP(entity)
"arkiv_bm"     + keccak256(bitmap_bytes) (32 bytes)     →  bitmap_bytes                  (immutable, content-addressed)
"arkiv_annot"  + annotKey + "\x00" + annotVal           →  keccak256(bitmap_bytes)        (mutable pointer, updated in place)
"arkiv_id"     + uint64_id (8 bytes big-endian)         →  entity_address (20 bytes)      (fast cache; authoritative copy is system account trie slot)
"arkiv_addr"   + entity_address (20 bytes)              →  uint64_id (8 bytes big-endian) (fast cache; tombstone left in place on Delete/Expire)
"arkiv_pairs"  + annotKey + "\x00" + annotVal           →  \x01                           (append-only existence flag)
"arkiv_root"   + blockHash (32 bytes)                   →  stateRoot (32 bytes)           (canonical head mapping)
"arkiv_journal"+ blockNumber (8 bytes) + blockHash (32 bytes) + entry_index (4 bytes)     →  journal entry (see §4)
```

The `"\x00"` separator between `annotKey` and `annotVal` ensures prefix scans cannot accidentally match a key that is a prefix of another. Keys and values must not contain `"\x00"` bytes; this is enforced at the application layer.

#### Write Path

On any operation that modifies a bitmap for a given `(annotKey, annotVal)` pair:

1. Read the current hash `H_old` from `"arkiv_annot" + annotKey + "\x00" + annotVal` (treat as empty bitmap if absent).
2. If `H_old` is non-zero, fetch bytes from `"arkiv_bm" + H_old`; deserialize.
3. Apply the change (add or remove `entity_id`).
4. Serialize the new bitmap; compute `H_new = keccak256(new_bytes)`.
5. Write `"arkiv_bm" + H_new → new_bytes` (new immutable entry; `H_old` entry is left in place).
6. Write `H_new` to `"arkiv_annot" + annotKey + "\x00" + annotVal` (mutable pointer).
7. Write `H_new` to the system account slot via `StateDB.SetState` (trie-committed).
8. If this is the first time this pair is written, write `"arkiv_pairs" + annotKey + "\x00" + annotVal → \x01`.
9. Record `{key: "arkiv_annot" + ..., oldValue: H_old}` in the per-block journal.

#### Read Path

For an equality query on `(annotKey, annotVal)`:
1. Read `H` from `"arkiv_annot" + annotKey + "\x00" + annotVal`.
2. If absent or zero-valued, the bitmap is empty.
3. Fetch bytes from `"arkiv_bm" + H`; deserialize.

For historical reads, step 1 is replaced by a trie lookup of the system account slot at the target block's state root (see §5).

### Lifecycle

#### Create

1. Read and increment `entity_count` in the system account; the new value is `entity_id`.
2. `CreateAccount` on the entity address (nonce defaults to 0; no explicit nonce write needed).
3. Write `slot[keccak256("id" || entity_id)] → address` in the system account via `StateDB.SetState` (trie-committed; reverts automatically on reorg).
4. Write `"arkiv_id" + entity_id → address` and `"arkiv_addr" + address → entity_id` in PebbleDB. Record both in the per-block journal (fast-path cache; reverted explicitly on reorg).
5. For each annotation `(k, v)` including built-ins (`$all`, `$creator`, `$createdAtBlock`, `$owner`, `$key`, `$expiration`, `$contentType`): run the bitmap write path (§3.2.3).
6. Encode entity as `RLP(entity)`; call `SetCode` on the entity account.

#### Update

1. Decode the current annotation set from the entity's existing code: `entity.DecodeRLP(code[1:])`.
2. For each annotation removed: run the bitmap write path (remove `entity_id`).
3. For each annotation added: run the bitmap write path (add `entity_id`).
4. Unchanged annotations require no bitmap writes.
5. Re-encode entity; call `SetCode` with new bytes. Built-in annotation bitmaps for `$owner`, `$expiration`, `$contentType` are updated if those fields changed.

#### Delete

1. Decode the current annotation set from the entity's existing code.
2. Read `entity_id` from `"arkiv_addr" + address` in PebbleDB.
3. For each annotation: run the bitmap write path (remove `entity_id`).
4. Clear `slot[keccak256("id" || entity_id)]` in the system account via `StateDB.SetState(..., 0)` (trie-committed; reverts automatically on reorg).
5. Call `SetCode(nil)`. The account now has nonce=0, balance=0, `codeHash=emptyCodeHash` — EIP-161-empty. `StateDB.Finalise` will prune it from the trie, which is the desired outcome. Entity accounts have no storage, so `handleDestruction` will not error.
6. Leave `"arkiv_id"` and `"arkiv_addr"` entries in place — they serve as tombstones in PebbleDB. Record no journal entry for them; they are never reverted on a chain revert. When the trie is reverted to the pre-delete state root (via HashScheme), the entity account and the system account `id` slot both reappear automatically.

#### Entity Expiration (Housekeeping)

Identical to Delete. The housekeeping process runs periodically, scanning entities whose `expiresAt` block number has passed, and applies expiration as a synthetic Delete against the current canonical head. Expiration operations are not sourced from the ExEx; they are applied directly by the Go EntityDB based on the `ExpiresAt` field in each entity's RLP.

#### Extend / ChangeOwner

Re-encode the RLP with the updated field; call `SetCode` with the new bytes. Update any affected built-in annotation bitmaps (`$expiration` for Extend; `$owner` for ChangeOwner). No `entity_id` slot changes.

### Block Commit and State Root

After processing all operations for a block, the EntityDB:

1. Calls `StateDB.Commit(blockNumber, true)` to flush trie changes and obtain the new `stateRoot`.
2. Writes `"arkiv_root" + blockHash → stateRoot` to PebbleDB.
3. Updates the in-memory canonical head pointer to the new block number, hash, and state root.

The `TrieDB` is opened with `HashScheme` retaining state roots for all committed blocks. This enables historical trie lookups without a separate snapshot mechanism.

---

## 5. Reorg Handling

### Trie Reversion

The `TrieDB` with `HashScheme` retains the state root for every committed block. Reverting the trie to a prior block is a no-op: the EntityDB simply opens a `StateDB` against the target state root. No trie writes are needed for reversion.

### PebbleDB Journal

Mutable PebbleDB entries (`"arkiv_annot"`, `"arkiv_id"`, `"arkiv_addr"`) are not versioned by the trie. To support reversion, the EntityDB maintains a per-block journal of the before-values of every mutable PebbleDB entry modified during that block. The system account trie slots (`annot`, `id`) do not require journaling — they revert automatically with the trie via HashScheme.

Each journal entry records:

```go
type JournalEntry struct {
    Key      []byte  // the PebbleDB key that was modified
    OldValue []byte  // the value before the modification (nil if the key did not exist)
}
```

Journal entries are written to PebbleDB under:

```
"arkiv_journal" + blockNumber (8 bytes big-endian) + blockHash (32 bytes) + entryIndex (4 bytes big-endian)  →  RLP(JournalEntry)
```

Writes to immutable entries (`"arkiv_bm"`, `"arkiv_pairs"`, `"arkiv_root"`) are never journaled. Immutable entries accumulate and are never reverted.

### Revert Procedure

To revert a sequence of blocks (newest-first):

For each block being reverted:

1. Read all journal entries for `(blockNumber, blockHash)` from PebbleDB.
2. Replay them in reverse entry-index order:
   - If `OldValue` is nil, delete the key.
   - Otherwise, write `OldValue` back to the key.
3. Delete the journal entries for this block.
4. Delete `"arkiv_root" + blockHash`.

After all blocks are reverted, update the canonical head pointer to the common ancestor block.

The trie is already at the correct state (it retains all historical roots); only the mutable PebbleDB entries require explicit reversion.

### Journal Pruning

Journal entries for blocks older than the finalization depth (typically 50,400 L2 blocks, corresponding to the 7-day Optimism challenge window) may be pruned. Once a block is finalized, it will never be reverted and its journal entries serve no purpose. The EntityDB runs a background goroutine that deletes journal entries for finalized blocks. The finalization depth is configurable.

---

## 6. Query Execution

All queries are evaluated against PebbleDB bitmap data. The query AST and in-memory bitmap operations (intersection, union, range iteration) are unchanged from the original design.

### Latest-State Queries

Latest-state queries read from the current `"arkiv_annot"` mutable pointers and then fetch bitmap bytes from `"arkiv_bm"`.

#### Equality and Inclusion Queries

```
Query: contentType = "image/png" AND status = "approved"

1. Read H1 from "arkiv_annot" + "contentType" + "\x00" + "image/png"
2. Read H2 from "arkiv_annot" + "status" + "\x00" + "approved"
3. Fetch bitmap bytes from "arkiv_bm" + H1; deserialize → bm1
4. Fetch bitmap bytes from "arkiv_bm" + H2; deserialize → bm2
5. Compute intersection bm1 ∩ bm2 in memory.
6. For each uint64_id in the result: read "arkiv_id" + id → entity_address.
7. Optionally fetch full entity: eth_getCode(entity_address) or PebbleDB "c" + codeHash.
```

An inclusion query (`status IN ("approved", "pending")`) unions the bitmaps for each value before intersecting with other terms.

#### Range Queries

Numeric annotation values are stored with fixed-width big-endian encoding so lexicographic byte order matches numeric order. A range query prefix-scans the `"arkiv_annot"` namespace:

```
Query: score > 10

1. Iterate PebbleDB keys from "arkiv_annot" + "score" + "\x00" + encode(11)
                           to "arkiv_annot" + "score" + "\x01"
2. For each key, read the hash H; fetch "arkiv_bm" + H; deserialize; OR into accumulator.
3. For each uint64_id in accumulator: resolve address via "arkiv_id".
```

#### Glob / Prefix Queries

Prefix-scan `"arkiv_annot"` on `annotKey + "\x00" + matchPrefix`; for each matching key, read the hash, fetch the bitmap from `"arkiv_bm"`, OR into accumulator.

### Historical Queries

Historical queries target a specific block identified by block number or block hash. The EntityDB resolves the block hash to a state root via `"arkiv_root" + blockHash`, then opens a read-only `StateDB` against that state root.

#### Historical Equality Queries

For a historical equality query on `(annotKey, annotVal)` at block `B`:

```
1. Resolve stateRoot_B from "arkiv_root" + blockHash_B.
2. Open StateDB at stateRoot_B (read-only trie access; no writes).
3. Read system account slot[keccak256("annot" || annotKey || "\x00" || annotVal)] → H.
4. If H is zero, the pair did not exist at block B; bitmap is empty.
5. Fetch "arkiv_bm" + H → bitmap_bytes (content-addressed; always present if H ≠ 0).
6. Deserialize; for each uint64_id: read "arkiv_id" + id → entity_address.
7. Fetch entity code at block B: StateDB at stateRoot_B → eth_getCode(entity_address).
```

Step 5 always succeeds because `"arkiv_bm"` entries are immutable — once written, they are never deleted. The hash committed in the trie at block `B` is guaranteed to have its pre-image in the store.

#### Historical Range and Glob Queries

Historical range and glob queries cannot read the current `"arkiv_annot"` mutable pointers — those reflect the latest state. Instead, they enumerate candidate pairs from the append-only `"arkiv_pairs"` index:

```
Query: score > 10 at block B

1. Prefix-scan "arkiv_pairs" + "score" + "\x00" + encode(11)
             to "arkiv_pairs" + "score" + "\x01"
   → yields all (annotKey="score", annotVal) pairs that have ever existed with value ≥ 11.
2. For each such pair, read system account slot at stateRoot_B (as in historical equality query).
   If the slot is zero, the pair did not exist at block B; skip.
3. Fetch bitmap from "arkiv_bm" + H; OR into accumulator.
4. Resolve addresses; fetch entity state at stateRoot_B.
```

The `"arkiv_pairs"` index is a superset of the pairs that existed at any given block. Pairs that were first written after block `B` will have a zero slot value at `stateRoot_B` and are skipped at step 2 with no cost beyond the trie read. Pairs that were written before block `B` and subsequently had their bitmap emptied will yield an empty bitmap at step 3 and contribute nothing to the accumulator.

Historical range and glob queries are therefore correct but may perform more trie reads than the equivalent latest-state query. The overhead is proportional to the number of pairs in `"arkiv_pairs"` that match the prefix scan.

### Query Completeness Proofs

For equality queries, a client can verify that the query result is complete and unmodified without trusting the node operator.

For an equality query on `(annotKey, annotVal)` at block `B`:

```
1. eth_getProof(systemAccount, [slot[keccak256("annot" || annotKey || "\x00" || annotVal)]], blockHash_B)
     → proves bitmap hash H against stateRoot_B → OutputRoot on L1.
2. Server provides bitmap_bytes; client verifies keccak256(bitmap_bytes) == H and decodes the ID set {id1, id2, ...}.
3. eth_getProof(systemAccount, [slot[keccak256("id" || id1)], slot[keccak256("id" || id2)], ...], blockHash_B)
     → proves each id → entity_address mapping against the same stateRoot_B.
4. Client asserts the set of entity_addresses in the response matches the addresses from step 3 exactly.
5. For each entity_address, optionally verify the payload: fetch RLP bytes and confirm keccak256(bytes) == codeHash
   from the entity account (itself verifiable via eth_getProof on the entity account at blockHash_B).
```

Steps 1–3 are a single multi-slot `eth_getProof` call against the system account. The bitmap and the ID→address mappings are both committed in `arkiv_stateRoot`, so the entire query result — which entities match, and what their addresses are — is verifiable without trusting the EntityDB.

For a multi-condition equality query, the client fetches one bitmap proof per term (step 1–2 for each), intersects the bitmaps locally, and then performs a single step-3 proof for the IDs in the intersection.

### No Completeness Guarantee for Range / Glob

Because range and glob queries enumerate bitmap hashes from `"arkiv_pairs"` (a PebbleDB structure outside the trie), a node can omit entries from the prefix scan without producing an incorrect `stateRoot`. The individual bitmap hashes are trie-committed and verifiable per pair, but the set of pairs matching a prefix is not. Clients requiring completeness guarantees should restrict their queries to equality predicates, or trust the node operator for range and glob results.

---

## 7. Query Server HTTP API

The Go EntityDB exposes a JSON-RPC 2.0 HTTP server for query clients. This is the same endpoint used by the Arkiv SDK.

**arkiv_query** — Execute a query against latest or historical state.

```json
{
  "method": "arkiv_query",
  "params": [{
    "query":        "contentType = \"image/png\" AND status = \"approved\"",
    "atBlock":      "0xabc...",
    "withPayload":  true,
    "withMetadata": true,
    "limit":        50,
    "cursor":       null
  }]
}
```

`atBlock` is optional; if omitted, the query runs against the latest canonical head. If provided, it must be a block hash for which the EntityDB has a stored state root.

**arkiv_getEntity** — Retrieve a single entity by address.

```json
{
  "method": "arkiv_getEntity",
  "params": [{
    "entityAddress": "0x...",
    "atBlock":       "0xabc..."
  }]
}
```

**arkiv_getEntityProof** — Return the full `eth_getProof` response for an entity account at a given block. Clients use this to independently verify entity state against the L1-anchored `OutputRoot`.

```json
{
  "method": "arkiv_getEntityProof",
  "params": [{
    "entityAddress": "0x...",
    "atBlock":       "0xabc..."
  }]
}
```

**arkiv_getBitmapProof** — Return the `eth_getProof` for a system account annotation slot at a given block. Clients use this to verify bitmap hashes for equality query completeness proofs.

```json
{
  "method": "arkiv_getBitmapProof",
  "params": [{
    "annotKey": "status",
    "annotVal": "approved",
    "atBlock":  "0xabc..."
  }]
}
```

---

## 8. Summary

### Storage Layout

```
Trie (committed in stateRoot, retained for all blocks by TrieDB/HashScheme):

  system account (address = keccak256("arkiv.system")[:20]):
    nonce                                                           → 1  (required: no code, so nonce prevents EIP-161 pruning)
    slot[keccak256("entity_count")]                                → uint64
    slot[keccak256("annot" || annotKey || "\x00" || annotVal)]     → keccak256(bitmap_bytes)  (one per distinct annotation pair)
    slot[keccak256("id"   || uint64_id)]                          → entity_address (zero-padded)  (one per live entity; cleared on Delete/Expire)

  entity account (one per live entity; deleted entities are pruned from the trie):
    nonce                                                           → 0
    codeHash                                                        → keccak256(RLP(entity))  (non-empty; prevents EIP-161 pruning)

PebbleDB (outside trie, same underlying database):

  "c"             + codeHash (32 bytes)                    →  RLP(entity)
  "arkiv_bm"      + keccak256(bitmap_bytes) (32 bytes)     →  bitmap_bytes                     (immutable, content-addressed)
  "arkiv_annot"   + annotKey + "\x00" + annotVal           →  keccak256(bitmap_bytes)           (mutable pointer to current bitmap)
  "arkiv_id"      + uint64_id (8 bytes big-endian)         →  entity_address (20 bytes)
  "arkiv_addr"    + entity_address (20 bytes)              →  uint64_id (8 bytes big-endian)
  "arkiv_pairs"   + annotKey + "\x00" + annotVal           →  \x01                              (append-only existence index)
  "arkiv_root"    + blockHash (32 bytes)                   →  stateRoot (32 bytes)
  "arkiv_journal" + blockNumber + blockHash + entryIndex   →  RLP(JournalEntry)                 (prunable after finalization)
```

### Component Responsibilities

| Responsibility | EntityRegistry Contract | Reth ExEx | Go EntityDB |
|---|---|---|---|
| Validating Arkiv operations | Yes | No | No |
| Accumulating changeset hash | Yes | No | No |
| On-chain mutation record (verifiable) | Yes | No | No |
| Ethereum block execution and validation | Via Reth EVM | No | No |
| P2P, mempool, engine API | Via Reth | No | No |
| Decoding Op[] calldata from sealed blocks | No | Yes | No |
| Notifying EntityDB of chain events | No | Yes | No |
| Maintaining private query index (trie + bitmaps) | No | No | Yes |
| Handling reorgs in query index | No | Detects and signals | Applies via journal |
| Serving entity queries | No | No | Yes |
| Housekeeping / expiration | No | No | Yes |
| Generating trie proofs (query index) | No | No | Yes |

---

## 9. Open Question: Gas Model

> **Status: unresolved.**

How does the L3 charge gas for Arkiv operations?

The `EntityRegistry` contract is executed by Reth's EVM in the normal way — standard EVM gas applies to the contract execution itself (storage writes for the changeset hash, log emission, `arkivStateRoots` updates). This is well-defined.

What is less clear is whether Arkiv operations should carry an additional gas cost beyond EVM execution gas — for example, a fee proportional to payload size to cover DA costs, or a flat fee per operation to cover EntityDB infrastructure costs. Options include:

- **EVM gas only** — let the contract's SSTORE and calldata costs serve as the natural gas model. No special handling needed.
- **Custom gas surcharge in the contract** — the contract charges an additional fee per byte of payload or per operation, collected into a treasury address.
- **L3 sequencer-level pricing** — the L3 sequencer applies custom transaction pricing rules outside the EVM (analogous to OP Stack's L1 data fee). This would require node-level configuration.

This question does not affect the EntityDB or ExEx design and can be resolved independently.

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
    - [Entity](#entity)
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
  - [PebbleDB Reversion](#pebbledb-reversion)
  - [Revert Procedure](#revert-procedure)
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

> **Design decision.** The storage service does not currently submit `arkiv_stateRoot` to the `EntityRegistry` contract. The storage service and arkiv-op-reth run as separate, independent processes. The storage service computes a state root after each block and returns it to the ExEx, but that root is not yet anchored on-chain. This will remain the case until the storage service has demonstrated sufficient determinism — expected to be around one year of production operation. On-chain commitment and the full verifiability proof chain described in §2 are planned for a future phase.
>
> In the current phase, **partial verification** is available: the `EntityRegistry` contract stores a `coreHash` commitment for every live entity (§2), which clients can use to verify that the payload and attributes returned by the EntityDB are authentic. Query result completeness (i.e. that the result set contains all matching entities) cannot be verified in the current phase.

---

## Abstract

This document describes the Arkiv EntityDB architecture, composed of three components: an `EntityRegistry` smart contract deployed on the op-reth chain, a Reth execution extension (ExEx), and a standalone Go EntityDB service.

Arkiv databases are L3s. The op-reth node, the `EntityRegistry` contract, and the ExEx all run on the L3. The L3 settles against an L2 (OP Stack), which in turn settles against Ethereum L1.

All Arkiv mutations flow through the `EntityRegistry` contract. The contract validates each operation and emits logs that the ExEx parses and forwards to the Go EntityDB. After the EntityDB processes each block it computes `arkiv_stateRoot` — the root of its internal entity state trie — but does not currently submit it on-chain. The EntityDB and the op-reth node run as separate, independent processes.

The Go EntityDB maintains a private query index — a Merkle Patricia Trie and PebbleDB annotation bitmaps — optimised for fast entity lookup and SQL-like queries. The trie root is computed per block and retained internally to support historical queries, but is not currently anchored to the main chain's `stateRoot`.

A future phase will anchor `arkiv_stateRoot` on-chain via the `EntityRegistry` contract, enabling a two-level verifiability proof chain (see §2). That phase is deferred until the EntityDB has demonstrated sufficient determinism in production.

**What this design provides:**
- On-chain validation of all Arkiv operations via the `EntityRegistry` contract
- Per-entity payload verification: clients can recompute `coreHash` from entity data returned by the EntityDB and compare it against the `coreHash` stored in `EntityRegistry._commitments` at any block (see §2)
- Fast SQL-like queries (latest and historical) via the Go EntityDB query index
- Clean reorg handling without full state replay
- No modification to op-reth required — the ExEx is a read-only observer

**What this design does not provide (current phase):**
- Query result completeness proofs — there is no on-chain commitment to annotation bitmaps, so a node could return an incomplete result set without detection
- Verifiability of range or glob query results for the same reason

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
│  L3 op-reth Node                                                │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  EntityRegistry (smart contract, on L3)                  │   │
│  │  - Validates Arkiv operations                            │   │
│  │  - Emits logs consumed by ExEx                           │   │
│  │  - Committed in L3 stateRoot on every block              │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  Arkiv ExEx                                              │   │
│  │  - Watches sealed blocks for EntityRegistry calls        │   │
│  │  - Parses logs, forwards ops to Go EntityDB              │   │
│  └─────────────────────────────────┬────────────────────────┘   │
└────────────────────────────────────│────────────────────────────┘
                                     │ HTTP JSON-RPC
                                     ▼
┌─────────────────────────────────────────────────────────────────┐
│  Go EntityDB Process (query index)                              │
│                                                                 │
│  ┌─────────────────────┐   ┌──────────────────────────────┐     │
│  │  Chain Ingest API   │   │  Query Server                │     │
│  │  commitChain        │   │  arkiv_query                 │     │
│  │  revert             │   │  arkiv_getEntityByAddress    │     │
│  │  reorg              │   │  arkiv_getEntityCount        │     │
│  └──────────┬──────────┘   └──────────────┬───────────────┘     │
│             │                             │                     │
│             ▼                             ▼                     │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  State Engine                                            │   │
│  │  - go-ethereum StateDB / Trie (library, not node)        │   │
│  │  - PebbleDB: entity code, bitmaps, ID maps               │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
                                     ▲
                        HTTP JSON-RPC (query clients)
                        SDK / frontend / other services
```

### Data Flow

1. A client submits a standard L3 transaction calling `EntityRegistry.execute(Op[] ops)`. The contract validates each operation and emits a log per operation. The resulting storage changes are committed in the L3 block's `stateRoot` when the block is sealed.
2. op-reth commits the sealed block. The ExEx receives a `ChainCommitted { chain }` notification.
3. For each block, the ExEx filters to successful calls to `ENTITY_REGISTRY_ADDRESS`, reads the emitted logs to extract the typed operations (including `entityKey` for Create ops), and assembles one `ArkivBlock` per block. Blocks with no `EntityRegistry` calls are still forwarded with an empty `transactions` list.
4. The ExEx calls `arkiv_commitChain` on the Go EntityDB. The EntityDB applies the block, computes `arkiv_stateRoot`, and returns it in the response. The ExEx does not currently submit it anywhere.
5. Query clients call `arkiv_query`, `arkiv_getEntityByAddress`, or `arkiv_getEntityCount` on the Go EntityDB's query server.

On `ChainReverted`, the ExEx sends `arkiv_revert` with block identifiers only. The EntityDB reverts the trie via HashScheme and repopulates mutable PebbleDB entries from the trie. On `ChainReorged`, the ExEx sends `arkiv_reorg` atomically.

---

## 2. EntityRegistry Smart Contract

### Role

The `EntityRegistry` is a smart contract deployed on the L3. It is the single on-chain entry point for all Arkiv mutations. Clients submit standard L3 transactions calling `EntityRegistry.execute(Op[] calldata ops)`. The contract:

- Validates each operation (ownership checks, expiry, correct caller)
- Dispatches to per-operation logic (`_create`, `_update`, `_extend`, `_transfer`, `_delete`, `_expire`)
- Emits a log per operation, consumed by the ExEx to drive the EntityDB

The internal changeset hash mechanism (see `experiments/change-set-hash-v3.md`) is also maintained by the contract and provides an independent audit trail of all mutations.

### arkiv_stateRoot Storage (future)

> **Deferred.** On-chain commitment of `arkiv_stateRoot` is not implemented in the current phase. The EntityDB computes the state root internally after each block but does not submit it to the contract. This section describes the intended future design for reference.

When on-chain commitment is enabled, after the EntityDB processes block N the ExEx will submit a transaction calling:

```solidity
function setArkivStateRoot(uint64 blockNumber, bytes32 stateRoot) external onlyExEx;
```

The contract will store this in a mapping committed in the L3's world state:

```solidity
mapping(uint64 => bytes32) public arkivStateRoots;
// arkivStateRoots[N] = arkiv_stateRoot_N, stored at block N+1
```

Open design questions for that phase: synchrony of the ExEx→EntityDB call relative to block production; transaction submission mechanism (mempool vs. direct sequencer injection); and missed-submission recovery strategy.

### Verification (current)

The `EntityRegistry` stores a `Commitment` for every live entity:

```solidity
struct Commitment {
    address creator;       // immutable
    BlockNumber createdAt; // immutable
    BlockNumber updatedAt; // updated on every mutation
    BlockNumber expiresAt; // updated on extend
    address owner;         // updated on transfer
    bytes32 coreHash;      // EIP-712 hash of: entityKey, creator, createdAt, contentType, payload, attributes
}
```

`coreHash` is the key field. It commits to the full immutable content of the entity — payload, contentType, and attributes — via an EIP-712 structured hash. `entityHash` (emitted in every `EntityOperation` event) wraps `coreHash` with the mutable fields:

```
coreHash   = EIP-712.hashStruct(CoreHash(entityKey, creator, createdAt, contentType, payload, attributes))
entityHash = EIP-712.hashStruct(EntityHash(coreHash, owner, updatedAt, expiresAt))
```

**Per-entity verification procedure.** To verify that entity data returned by the EntityDB at block B is authentic:

1. Retrieve the entity from the EntityDB at block B (payload, contentType, attributes, creator, createdAt, owner, updatedAt, expiresAt, entityKey).
2. Recompute `coreHash` using the EIP-712 formula above.
3. Call `EntityRegistry.commitment(entityKey)` at block B via `eth_call` (or `eth_getProof` for a cryptographic proof). Read the stored `coreHash`.
4. Assert the two `coreHash` values are equal.

Both the EntityDB (via its internal HashScheme trie) and the EVM state trie (via the chain's archive) are queryable at any historical block, so this procedure works identically for current and historical entity state. If an entity was deleted at block B+N, querying both at block B will find it in both, and the coreHash comparison holds.

**What this does not cover.** This is a per-entity verification only. The `EntityRegistry` has no on-chain commitment to the annotation bitmap index, so a client cannot verify that a query result contains *all* matching entities — only that each individual entity in the result is authentic.

### Verification (future)

> **Deferred.** Full query-level verifiability depends on on-chain `arkiv_stateRoot` commitment, which is not active in the current phase.

The intended two-level proof chain for a client verifying entity payload P at block N:

**Step 1 — entity proof (EntityDB trie → arkiv_stateRoot).**
`arkiv_getProof(entityAddress, blockN)` returns a Merkle proof from `arkiv_stateRoot_N` to the entity's account node, proving `codeHash = keccak256(RLP(entity))`. The client fetches the RLP bytes and verifies `keccak256(RLP(entity)) == codeHash` to confirm the payload.

**Step 2 — anchor proof (L3 stateRoot → contract storage).**
`eth_getProof(ENTITY_REGISTRY_ADDRESS, [slot(arkivStateRoots[N])], blockHash_{N+1})` proves that `arkivStateRoots[N] = arkiv_stateRoot_N` against the L3 `stateRoot` at block N+1. The L3 `stateRoot` is covered by the OP Stack fault proof system and anchored to L2 and ultimately L1.

Combining both steps: the payload is bound to `codeHash`, `codeHash` is in the entity account committed under `arkiv_stateRoot_N`, and `arkiv_stateRoot_N` is committed in the L3 world state at block N+1.

---

## 3. ExEx → EntityDB JSON-RPC Interface

All methods are JSON-RPC 2.0 over HTTP. The ExEx is the only caller of the chain ingest methods. Query clients call the query methods directly.

### ExEx Filtering and Forwarding

op-reth delivers chain events to an ExEx via the `ExExNotification` enum:

```rust
pub enum ExExNotification {
    ChainCommitted { chain: Arc<Chain> },
    ChainReverted  { chain: Arc<Chain> },
    ChainReorged   { old_chain: Arc<Chain>, new_chain: Arc<Chain> },
}
```

`Chain` is op-reth's type for a contiguous sequence of executed blocks. It exposes an iterator over `(SealedBlockWithSenders, ExecutionOutcome)` pairs — one per block — where `SealedBlockWithSenders` carries the sealed header, the full ordered transaction list with recovered senders, and `ExecutionOutcome` carries the receipts for each transaction in the same order.

The ExEx does not forward full blocks. For each block in the chain it:

1. Reads the sealed header to extract the three fields the EntityDB needs: `number`, `hash`, `parent_hash`.
2. Zips transactions (with senders) against their receipts.
3. Filters to transactions where `tx.to == ENTITY_REGISTRY_ADDRESS` and `receipt.status == 1`. Failed calls are discarded — the contract reverted and no state changes were applied, so the EntityDB must not apply them either.
4. For each passing transaction, reads the logs emitted by the contract to extract the typed operation list. Each `EntityOperation` log includes the `entityKey` (the 32-byte `keccak256(chainId || registry || owner || nonce)` value minted by the contract). The EntityDB derives the trie account address as `entityKey[:20]`. The ExEx does not recompute keys locally.
5. Passes `expires_at` through as a block number directly from the contract log — no timestamp conversion is needed. The EntityDB housekeeping process works in block numbers.

If a block contains no Arkiv transactions it is still forwarded as an `ArkivBlock` with an empty `transactions` list. The EntityDB must advance its state root for every block in the canonical chain, even empty ones, so that block-number-to-state-root mappings remain complete.

For `ChainReverted` and the `old_chain` side of `ChainReorged`, the ExEx does **not** re-parse calldata. The EntityDB reverts by reverting the trie (free via HashScheme) and repopulating mutable PebbleDB entries from the trie in a single pass against the common ancestor state root; it needs only the block identifiers (number + hash, newest-first).

### Forwarded Types

The Rust types the ExEx constructs and serializes:

```rust
/// A block header subset — only the fields the EntityDB needs.
///
/// - number:      hex-encoded u64; used as the key in the arkiv_root/arkiv_blknum
///                mappings (blockHash → stateRoot), and for housekeeping
///                (comparing block.number against entity.expires_at).
/// - hash:        used as the key in the arkiv_root and arkiv_parent mappings.
/// - parent_hash: continuity check — the EntityDB can verify each block's
///                parent_hash matches the hash it last processed.
/// - changeset_hash: rolling changeset hash after the last operation in this block;
///                B256::ZERO for blocks with no operations. Informational only —
///                the EntityDB does not currently use this field.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ArkivBlockHeader {
    #[serde(with = "hex_u64")]
    pub number:         u64,
    pub hash:           B256,
    pub parent_hash:    B256,
    pub changeset_hash: B256,
}

/// A block containing only the Arkiv transactions extracted from successful
/// EntityRegistry calls. Non-Arkiv transactions and failed calls are omitted.
/// Blocks with no Arkiv activity are still forwarded with an empty transactions list.
#[derive(Serialize)]
pub struct ArkivBlock {
    pub header:       ArkivBlockHeader,
    pub transactions: Vec<ArkivTransaction>,
}

/// A single EntityRegistry call with its decoded operations.
/// sender is placed here (at the transaction level) rather than duplicated
/// into each individual operation.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ArkivTransaction {
    pub hash:       B256,
    pub index:      u32,
    pub sender:     Address,   // becomes Creator in CreateOp; injected by the EntityDB
    pub operations: Vec<ArkivOperation>,
}

/// Minimal block identifier used for revert payloads.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ArkivBlockRef {
    #[serde(with = "hex_u64")]
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
    #[serde(rename = "transfer")]
    Transfer(TransferOp),
    Expire(ExpireOp),
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateOp {
    pub entity_key:   B256,      // keccak256(chainId || registry || owner || nonce) — from EntityOperation log
    pub owner:        Address,
    pub expires_at:   u64,       // hex-encoded block number
    pub entity_hash:  B256,
    pub changeset_hash: B256,
    pub payload:      Bytes,
    pub content_type: String,
    pub attributes:   Vec<Attribute>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct UpdateOp {
    pub entity_key:   B256,
    pub owner:        Address,
    pub entity_hash:  B256,
    pub changeset_hash: B256,
    pub payload:      Bytes,
    pub content_type: String,
    pub attributes:   Vec<Attribute>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct DeleteOp {
    pub entity_key: B256,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ExtendOp {
    pub entity_key: B256,
    pub expires_at: u64,     // hex-encoded block number (new absolute expiry)
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TransferOp {
    pub entity_key: B256,
    pub new_owner:  Address,
}

/// ExpireOp removes an entity that has passed its expiration block.
/// Semantically identical to DeleteOp from the EntityDB's perspective.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ExpireOp {
    pub entity_key: B256,
}

#[derive(Serialize)]
#[serde(untagged)]
pub enum Attribute {
    String  { key: String, string_value:  String },
    Numeric { key: String, numeric_value: u64    },
}
```

**Key wire format notes:**
- `number` and `expires_at` are serialized as hex strings (`"0x..."`), matching go-ethereum's `hexutil.Uint64`.
- `sender` lives at the `ArkivTransaction` level, not inside individual operations. The EntityDB injects it into `CreateOp.Sender` (the entity's `Creator`) when processing each block.
- User-supplied key-value pairs are called `attributes` on the wire (not `annotations`).
- The op type tag for ownership transfer is `"transfer"`; the EntityDB also accepts `"changeOwner"` as an alias for backward compatibility.
- `"expire"` is a distinct op type (entity past its expiration block is removed by a caller); the EntityDB treats it identically to `"delete"`.

The ExEx builds one `ArkivBlock` per block, regardless of whether any Arkiv transactions were found. The `transactions` field is empty for blocks with no Arkiv activity. For revert payloads it builds `Vec<ArkivBlockRef>`.

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
          "number":        "0x3039",
          "hash":          "0xabc...",
          "parentHash":    "0xdef...",
          "changesetHash": "0x..."
        },
        "transactions": [
          {
            "hash":   "0x...",
            "index":  0,
            "sender": "0x...",
            "operations": [
              {
                "type":        "create",
                "entityKey":   "0x1234...abcd",
                "payload":     "0x...",
                "contentType": "application/json",
                "expiresAt":   "0x34bc",
                "owner":       "0x...",
                "attributes": [
                  { "key": "type",     "stringValue":  "note" },
                  { "key": "priority", "numericValue": 5      }
                ]
              }
            ]
          }
        ]
      }
    ]
  }]
}
```

**Response (success):**
```json
{ "result": { "stateRoot": "0x7f8e..." } }
```

`stateRoot` is the `arkiv_stateRoot` after applying the last block in the batch. JSON-RPC error on failure.

### arkiv_revert

Revert a contiguous sequence of blocks from the canonical head back to the common ancestor. Blocks are identified by number and hash only (newest-first); the EntityDB does not need the original operations. The EntityDB reverts the trie via HashScheme and repopulates mutable PebbleDB entries from the trie at the common ancestor state root in a single pass, regardless of how many blocks are listed.

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

**Response (success):**
```json
{ "result": { "stateRoot": "0xaaa..." } }
```

`stateRoot` is the `arkiv_stateRoot` of the new canonical head after reverting. JSON-RPC error on failure.

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
        "header": { "number": "0x3039", "hash": "0x...", "parentHash": "0x...", "changesetHash": "0x00...00" },
        "transactions": []
      },
      {
        "header": { "number": "0x303a", "hash": "0x...", "parentHash": "0x...", "changesetHash": "0x..." },
        "transactions": [
          {
            "hash": "0x...", "index": 0, "sender": "0x...",
            "operations": [{ "type": "delete", "entityKey": "0x..." }]
          }
        ]
      }
    ]
  }]
}
```

**Response (success):**
```json
{ "result": { "stateRoot": "0xbbb..." } }
```

`stateRoot` is the `arkiv_stateRoot` after applying the new chain. JSON-RPC error on failure.

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

`arkiv_stateRoot` — the root of the EntityDB's trie after each block — is computed and retained internally but is not currently submitted on-chain (see the design decision note and §2).

**Immutable vs mutable state.** Three distinct kinds of state live in the EntityDB:

- **Immutable, content-addressed** — entity RLP blobs (`"c" + codeHash`) and bitmap byte arrays (`"arkiv_bm" + hash`) are written once and never overwritten. They piggyback on the trie's versioning mechanism: the trie root changes when content changes, and old content is never deleted. These entries require no journal.
- **Mutable, auto-reverting (trie)** — trie account state (entity `codeHash`, system account slots). The logical account values change on each block via `StateDB.SetCode`/`SetState`, but the underlying `HashScheme` trie nodes are immutable and retained at the storage layer — each node is stored by hash and never overwritten. Reversion is free: the EntityDB simply re-opens a `StateDB` against the prior `arkiv_stateRoot`; no journal is needed.
- **Mutable, repopulated from trie on reorg (PebbleDB)** — bitmap pointer entries (`"arkiv_annot"`), ID↔address mappings (`"arkiv_id"`, `"arkiv_addr"`). These are updated in place and are not versioned by the trie. On reorg, they are repopulated by scanning `arkiv_pairs` and reading the corresponding system account trie slots at the reverted state root (§5). No per-block journal is maintained; reorg is rare and a full scan is acceptable.

### Entity Accounts

#### Address Derivation

Each entity has a dedicated Ethereum account. The `entityKey` is minted by the `EntityRegistry` contract and emitted in the `EntityOperation` log. The trie account address is the first 20 bytes of that key:

```
entity_key     = keccak256(chainId || registry || owner || nonce)   // 32 bytes — from EntityOperation log
entity_address = entity_key[:20]                                     // 20 bytes — account address in the trie
```

The key is derived from the owner's address and a per-owner monotonic nonce, bound to the chain and registry contract address to prevent cross-chain and cross-deployment collisions. The ExEx reads `entityKey` directly from the `EntityOperation` log and forwards it to the EntityDB; no local recomputation is needed.

The payload is intentionally excluded from the derivation: the entity address is a pure identity anchor; content commitment is handled by `codeHash`.

#### Account Structure

```
Entity Account  (address = entity_key[:20])
  nonce    = 0
  balance  = 0
  codeHash = keccak256(RLP(entity))   // commits to full entity state in the trie

  storage slots: none
```

The RLP bytes are **not** stored inside the account. `codeHash` is the only entity-related field in the trie account; the actual `RLP(entity)` bytes are stored separately in PebbleDB under `"c" + codeHash` (content-addressed, outside the trie).

`codeHash` is set to `keccak256(RLP(entity))`. The Go StateDB stores the RLP bytes in PebbleDB under `"c" + codeHash`. The EntityDB trie is never executed by an EVM, so no special prefix is needed to guard against bytecode interpretation. On every `Update`, the entity is re-encoded, `keccak256` is recomputed, and `SetCode` is called with the new bytes.

A live entity account is never EIP-161-empty: the condition requires `nonce==0 && balance==0 && codeHash==emptyCodeHash`, but a live entity always has `codeHash ≠ emptyCodeHash`. No explicit nonce is needed to keep it alive. After deletion (`SetCode(nil)`), the account becomes EIP-161-empty and `StateDB.Finalise` prunes it from the trie — which is the desired behaviour. The `"arkiv_addr"` PebbleDB entry serves as the tombstone for any operational needs; there is no reason to retain a trie stub for a deleted entity.

#### Entity

```go
type Entity struct {
    Payload            []byte
    Owner              common.Address
    Creator            common.Address
    ExpiresAt          uint64
    CreatedAtBlock     uint64
    ContentType        string
    Key                common.Hash          // full 32-byte entityKey = keccak256(chainId || registry || owner || nonce)
    StringAnnotations  []stringAnnot
    NumericAnnotations []numericAnnot
}
```

The full entity state — payload, owner, creator, expiry, creation block, content type, the full 32-byte key, and all annotations — is in one RLP blob stored as the account's code field. Fetching the code bytes and verifying `keccak256(bytes) == codeHash` confirms the payload against the trie.

`Creator` and `CreatedAtBlock` are in the RLP to support built-in annotations (`$creator`, `$createdAtBlock`) without extra storage slots. The full 32-byte `Key` is the contract's `entityKey` — it is stored here so that the query API can return it to clients, who need it to call `EntityRegistry.commitment(entityKey)` for payload verification (§2). The last 12 bytes are not recoverable from the 20-byte `entity_address` alone.

#### entity_id

Each entity is assigned a `uint64` sequential ID at `Create` time, taken from the `entity_count` counter in the system account. The ID→address mapping is **trie-committed** via a system account storage slot (see System Account below), making it provable via `eth_getProof`. Two PebbleDB entries mirror the same information for fast access on the query hot path:

```
"arkiv_id"   + uint64_id (8 bytes big-endian)  →  entity_address (20 bytes)       (fast cache, mutable)
"arkiv_addr" + entity_address (20 bytes)        →  uint64_id (8 bytes big-endian)  (fast cache, mutable)
```

`"arkiv_addr"` is used during Delete and Expire to look up the entity's ID before removing it from bitmaps. `"arkiv_id"` resolves query result IDs to entity addresses without a trie read on the hot path. Both are populated at Create and serve as a performance cache; the system account trie slot is the authoritative source and is repopulated from on reorg.

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
"arkiv_root"   + blockHash (32 bytes)                   →  stateRoot (32 bytes)           (per-block state root; written on ProcessBlock, deleted on RevertBlock)
"arkiv_parent" + blockHash (32 bytes)                   →  parentHash (32 bytes)          (per-block parent pointer; written on ProcessBlock, deleted on RevertBlock)
"arkiv_blknum" + blockNumber (8 bytes big-endian)       →  blockHash (32 bytes)           (block number→hash lookup; written on ProcessBlock, deleted on RevertBlock)
"arkiv_head"                                            →  blockNumber (8 bytes) + blockHash (32 bytes) + stateRoot (32 bytes)  (canonical head; overwritten on every ProcessBlock and RevertBlock)
```

The `"\x00"` separator between `annotKey` and `annotVal` ensures prefix scans cannot accidentally match a key that is a prefix of another. Keys and values must not contain `"\x00"` bytes; this is enforced at the application layer.

#### Write Path

Bitmap mutations are accumulated in an in-memory per-block cache (`CacheStore.dirtyBitmaps`) and flushed to PebbleDB once at the end of the block. This ensures each annotation produces exactly one content-addressed blob per block, regardless of how many operations touched it.

On first access to a `(annotKey, annotVal)` pair within a block:

1. Read the current hash `H_old` from `"arkiv_annot" + annotKey + "\x00" + annotVal` (treat as empty bitmap if absent).
2. If `H_old` is non-zero, fetch bytes from `"arkiv_bm" + H_old`; deserialize into the in-memory cache.

On each operation that touches the pair within the block:

3. Apply the change (add or remove `entity_id`) to the cached in-memory bitmap. No PebbleDB write yet.

At block commit (`flushBitmaps`, called once per block before `StateDB.Commit`):

4. Serialize the final bitmap; compute `H_new = keccak256(new_bytes)`.
5. Write `"arkiv_bm" + H_new → new_bytes` (one new immutable entry per dirty annotation; `H_old` entry is left in place).
6. Write `H_new` to `"arkiv_annot" + annotKey + "\x00" + annotVal` (mutable pointer).
7. Write `H_new` to the system account slot via `StateDB.SetState` (trie-committed).
8. If this is the first time this pair has ever been written, write `"arkiv_pairs" + annotKey + "\x00" + annotVal → \x01`.

#### Read Path

For an equality query on `(annotKey, annotVal)`:
1. Read `H` from `"arkiv_annot" + annotKey + "\x00" + annotVal`.
2. If absent or zero-valued, the bitmap is empty.
3. Fetch bytes from `"arkiv_bm" + H`; deserialize.

For historical reads, step 1 is replaced by a trie lookup of the system account slot at the target block's state root (see §5).

### Lifecycle

Entity state is also accumulated in an in-memory per-block cache (`CacheStore.dirtyEntities`). Operations read from and write to this cache; encoding and `SetCode` are deferred to a single `flushEntities` step at commit time. This means each entity is encoded exactly once per block regardless of how many operations touched it, and the redundant decode→encode round-trips that would otherwise occur when the same entity is modified multiple times in one block are eliminated.

#### Create

1. Read and increment `entity_count` in the system account; the new value is `entity_id`.
2. `CreateAccount` on the entity address (nonce defaults to 0; no explicit nonce write needed).
3. Write `slot[keccak256("id" || entity_id)] → address` in the system account via `StateDB.SetState` (trie-committed; reverts automatically on reorg).
4. Write `"arkiv_id" + entity_id → address` and `"arkiv_addr" + address → entity_id` in PebbleDB (fast-path cache; repopulated from trie on reorg).
5. Store the new `Entity` in the dirty entity cache. `SetCode` is deferred to `flushEntities` at commit.
6. For each annotation `(k, v)` including built-ins (`$all`, `$creator`, `$createdAtBlock`, `$owner`, `$key`, `$expiration`, `$contentType`): update the in-memory bitmap cache. The blob write is deferred to `flushBitmaps` at commit.

#### Update

1. Load the entity from the dirty cache, or decode from trie on first access (subsequent accesses within the block hit the cache).
2. For each annotation removed: update the in-memory bitmap (remove `entity_id`).
3. For each annotation added: update the in-memory bitmap (add `entity_id`).
4. Unchanged annotations require no bitmap updates.
5. Store the updated `Entity` in the dirty entity cache. `SetCode` and bitmap blob writes are deferred to commit.

#### Delete

1. Load the entity from the dirty cache or trie.
2. Read `entity_id` from `"arkiv_addr" + address` in PebbleDB.
3. For each annotation: update the in-memory bitmap (remove `entity_id`). Bitmap blob writes are deferred to `flushBitmaps`.
4. Clear `slot[keccak256("id" || entity_id)]` in the system account via `StateDB.SetState(..., 0)` (trie-committed; reverts automatically on reorg).
5. Remove the entity from the dirty cache and call `SetCode(nil)` immediately. The account is now EIP-161-empty and will be pruned by `StateDB.Finalise`. `SetCode(nil)` must be applied immediately (not deferred) so that subsequent operations in the same block see the entity as absent.
6. Leave `"arkiv_id"` and `"arkiv_addr"` entries in place — they serve as tombstones in PebbleDB. When the trie is reverted to the pre-delete state root (via HashScheme), the entity account and the system account `id` slot both reappear automatically; the PebbleDB entries are repopulated from the trie as part of the reorg procedure.

#### Entity Expiration (Housekeeping)

Identical to Delete. The housekeeping process runs periodically, scanning entities whose `expiresAt` block number has passed, and applies expiration as a synthetic Delete against the current canonical head. Expiration operations are not sourced from the ExEx; they are applied directly by the Go EntityDB based on the `ExpiresAt` field in each entity's RLP.

#### Extend / ChangeOwner

Load the entity from the dirty cache or trie; update the relevant field in place. Update the affected built-in annotation bitmap in the in-memory cache (`$expiration` for Extend; `$owner` for ChangeOwner). Both the entity encoding and the bitmap blob write are deferred to commit. No `entity_id` slot changes.

### Block Commit and State Root

Block processing uses a write-ahead staging layer (`CacheStore`) to guarantee that all writes for a block land atomically. This matters because the EntityDB has two independent write paths — the trie (`TrieDB.Commit`) and direct PebbleDB mutations (bitmaps, ID maps) — with no shared transaction boundary. Without coordination, a crash between them would leave the stores inconsistent: the trie ahead of the bitmap index, or vice versa. A partially-written block could corrupt query results or make reorg recovery unreliable.

`CacheStore` solves this by staging all writes — trie nodes, entity RLP blobs, bitmaps, ID maps, and the canonical head pointer — in an in-memory `memorydb` during block processing. Nothing touches PebbleDB until the very end, when a single `batch.Write()` flushes everything atomically. The canonical head (`arkiv_head`) is the last key written into the batch, so it acts as a commit gate: if the process crashes before `batch.Write()` completes, PebbleDB's WAL guarantees the write either fully applied or fully rolled back, and `arkiv_head` either reflects the new block or the previous one — never a partial state.

After processing all operations for a block, the EntityDB:

1. Calls `flushEntities()`: encodes each dirty entity once and calls `SetCode`. One RLP encode + one `SetCode` per entity per block, regardless of how many operations modified it.
2. Calls `flushBitmaps()`: serialises each dirty bitmap once, writes one content-addressed blob and one pointer update per annotation to the staging `memorydb`, and updates the corresponding system account trie slot via `StateDB.SetState`. One blob per annotation per block.
3. Calls `StateDB.Commit(blockNumber, true, false)` to finalise trie changes (entity code and annotation slots from steps 1–2 included); dirty nodes move into the per-block `TrieDB`'s memory cache. The third argument (`false`) suppresses deletion of empty state objects — not required here since entity deletions are handled explicitly via `SetCode(nil)`.
4. Calls `TrieDB.Commit(stateRoot, false)` to flush trie nodes into the staging `memorydb` (not yet to PebbleDB). `false` means old nodes are not garbage-collected; `HashScheme` retains all historical roots.
5. Flushes block index entries (`arkiv_root`, `arkiv_parent`, `arkiv_blknum`), and the canonical head (`arkiv_head`) into the same staging `memorydb`.
6. Calls a single `batch.Write()` to atomically copy all staged entries to PebbleDB.
7. Updates the in-memory canonical head pointer.

On restart, `New()` reads `"arkiv_head"` to restore the canonical head. If the key is absent (fresh database) the store starts from an empty trie (`EmptyRootHash`).

---

## 5. Reorg Handling

### Trie Reversion

The `TrieDB` with `HashScheme` retains the state root for every committed block. Reverting the trie to a prior block is a no-op: the EntityDB simply opens a `StateDB` against the target state root. No trie writes are needed for reversion.

### PebbleDB Reversion

Mutable PebbleDB entries (`"arkiv_annot"`, `"arkiv_id"`, `"arkiv_addr"`) are not versioned by the trie. Rather than maintaining a per-block journal of before-values, the EntityDB repopulates them from the trie after reversion. This is correct because the authoritative values for all mutable PebbleDB entries are already committed in the system account trie slots, which revert automatically via HashScheme. Reorg is rare, so the cost of a full repopulation scan is acceptable.

**`arkiv_annot` repopulation.** The `arkiv_pairs` set records every `(annotKey, annotVal)` pair ever seen (it is immutable and never reverted). For each pair, the current bitmap hash is stored in the system account at `slot[keccak256("annot" || annotKey + "\x00" + annotVal)]`. After reverting the trie, the EntityDB scans all entries under `"arkiv_pairs"`, reads the corresponding system account slot at the reverted state root, and writes the result back to `"arkiv_annot"`. Pairs whose slot is now zero (e.g. because they were first introduced in a reverted block) have their `"arkiv_annot"` entry deleted.

**`arkiv_id` / `arkiv_addr` repopulation.** The ID→address mapping is stored in the system account at `slot[keccak256("id" || entity_id)]` for each live entity. After reversion, the EntityDB iterates `"arkiv_id"` entries and re-resolves each from the trie; entries for entities whose slot is now zero (deleted or not yet created at the reverted height) are removed.

### Revert Procedure

Revert is atomic. All writes — PebbleDB repopulation, block index key deletions, and the updated `arkiv_head` — are collected into a single `rawDB.NewBatch()` and flushed in one `batch.Write()`. The trie requires no writes (HashScheme retains all historical roots); `arkiv_head` is the last key written into the batch, serving as the commit gate.

Given a list of reverted blocks (newest-first), the common ancestor is the parent of the oldest block in the list:

1. Look up the parent hash of the oldest reverted block via `"arkiv_parent" + oldestBlockHash`. Look up the ancestor's state root via `"arkiv_root" + ancestorHash` (or `EmptyRootHash` if absent).
2. Open a read-only `StateDB` at the ancestor state root.
3. Repopulate `"arkiv_annot"`: for each pair in `"arkiv_pairs"`, read the system account slot at the ancestor state root and write or delete the `"arkiv_annot"` entry accordingly.
4. Repopulate `"arkiv_id"` / `"arkiv_addr"`: for each entry in `"arkiv_id"`, re-resolve from the system account slot; remove entries whose slot is zero.
5. Add deletions of `"arkiv_root"`, `"arkiv_parent"`, and `"arkiv_blknum"` entries for all reverted blocks to the batch.
6. Add the updated `"arkiv_head"` pointing to the ancestor block to the batch.
7. Call `batch.Write()` — all of the above lands atomically.

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

### Query Completeness Proofs (future)

> **Deferred.** Query completeness proofs depend on `arkiv_stateRoot` being anchored on-chain, which is not active in the current phase. In the current phase, clients can verify individual entity payloads via the `EntityRegistry` `coreHash` commitment (§2), but cannot verify that a query result contains *all* matching entities.

When `arkiv_stateRoot` is anchored on-chain, equality query completeness becomes verifiable. For an equality query on `(annotKey, annotVal)` at block `B`:

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

For a multi-condition equality query, the client fetches one bitmap proof per term (step 1–2 for each), intersects the bitmaps locally, and then performs a single step-3 proof for the IDs in the intersection.

### No Completeness Guarantee for Range / Glob

Range and glob queries have no completeness guarantee, even in the future phase when `arkiv_stateRoot` is anchored on-chain. These queries enumerate bitmap hashes from `"arkiv_pairs"` — a PebbleDB structure outside the trie — so a node can omit entries from the prefix scan without producing an incorrect `stateRoot`. The individual bitmap hashes are trie-committed and verifiable per pair, but the set of pairs matching a prefix is not. Clients requiring completeness guarantees must restrict their queries to equality predicates.

---

## 7. Query Server HTTP API

The Go EntityDB exposes a JSON-RPC 2.0 HTTP server for query clients. This is the same endpoint used by the Arkiv SDK.

Three methods are currently implemented: `arkiv_query`, `arkiv_getEntityByAddress`, and `arkiv_getEntityCount`. Verifiability endpoints (`arkiv_getEntityProof`, `arkiv_getBitmapProof`) are deferred to the future phase in which `arkiv_stateRoot` is anchored on-chain (§6).

### Common types

`arkiv_query` and `arkiv_getEntityByAddress` accept a shared optional `Options` object:

```json
{
  "atBlock":        "0x...",       // block number (hex Uint64); omit or 0 for the head
  "includeData":    { ... },       // see IncludeData below; omit for sensible defaults
  "resultsPerPage": "0x32",        // hex Uint64; capped at 200
  "cursor":         "0x..."        // hex-encoded ID returned in a previous response
}
```

`atBlock` is currently restricted to the canonical head: any non-zero value is accepted but the lookup uses the head state root. Historical queries by block number are reserved for a future block-number→hash index.

`IncludeData` is a per-field selector controlling which entity fields are populated in each result. The zero value `{}` returns no fields; omitting `includeData` (or passing `null`) selects a sensible default — every field below except `syntheticAttributes`, `transactionIndexInBlock`, and `operationIndexInTransaction`:

```json
{
  "key":                         true,
  "attributes":                  true,    // user-defined annotations
  "syntheticAttributes":         false,   // built-in $-prefixed annotations
  "payload":                     true,
  "contentType":                 true,
  "expiration":                  true,
  "owner":                       true,
  "creator":                     true,
  "createdAtBlock":              true,
  "lastModifiedAtBlock":         true,
  "transactionIndexInBlock":     false,
  "operationIndexInTransaction": false
}
```

Each result is an `EntityData` object. Fields are present only when requested via `IncludeData`; `attributes`/`syntheticAttributes` populate the same `stringAttributes` and `numericAttributes` arrays.

```json
{
  "key":                         "0x...",       // 32-byte entity key
  "value":                       "0x...",       // payload bytes
  "contentType":                 "image/png",
  "expiresAt":                   1234,
  "owner":                       "0x...",
  "creator":                     "0x...",
  "createdAtBlock":              100,
  "lastModifiedAtBlock":         105,
  "transactionIndexInBlock":     0,
  "operationIndexInTransaction": 2,
  "stringAttributes":            [{ "key": "category", "value": "doc" }],
  "numericAttributes":           [{ "key": "priority", "value": 5 }]
}
```

### arkiv_query

Evaluate a query against the bitmap index and return matching entities, newest-first by internal ID.

**Request** (positional params: `[query, options?]`):

```json
{
  "method": "arkiv_query",
  "params": [
    "contentType = \"image/png\" && category = \"approved\"",
    {
      "includeData":    { "key": true, "payload": true, "owner": true },
      "resultsPerPage": "0x32"
    }
  ]
}
```

**Response:**

```json
{
  "result": {
    "data":        [ { "key": "0x...", "value": "0x...", "owner": "0x..." }, ... ],
    "blockNumber": "0x3039",
    "cursor":      "0x4f"
  }
}
```

`blockNumber` is the block at which the result is reported (the head when `atBlock` is omitted). `cursor` is present only when more results remain: pass it back in the next request's `Options.cursor` to fetch the following page (the next page contains IDs strictly less than the cursor).

### arkiv_getEntityByAddress

Retrieve a single entity by its 20-byte trie account address (`entityKey[:20]`).

**Request** (positional params: `[address, options?]`):

```json
{
  "method": "arkiv_getEntityByAddress",
  "params": [
    "0x1111111111111111111111111111111111111111",
    { "includeData": { "key": true, "payload": true, "creator": true } }
  ]
}
```

**Response:** a single `EntityData` object, or a JSON-RPC error if no entity exists at that address.

### arkiv_getEntityCount

Return the total number of live entities at the head.

```json
{ "method": "arkiv_getEntityCount", "params": [] }
```

**Response:** `{ "result": 1234 }`.

### Future: proof endpoints

> **Deferred.** These methods are not implemented in the current phase. They become useful once `arkiv_stateRoot` is anchored on-chain (§6); until then, clients can verify individual entity payloads via the `EntityRegistry` `coreHash` commitment (§2).

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
  "arkiv_root"    + blockHash (32 bytes)                   →  stateRoot (32 bytes)              (per-block; deleted on RevertBlock)
  "arkiv_parent"  + blockHash (32 bytes)                   →  parentHash (32 bytes)             (per-block parent pointer; deleted on RevertBlock)
  "arkiv_blknum"  + blockNumber (8 bytes big-endian)       →  blockHash (32 bytes)              (block number→hash lookup; deleted on RevertBlock)
  "arkiv_head"                                             →  blockNumber + blockHash + stateRoot (72 bytes; canonical head, survives restart)
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
| Computing and returning arkiv_stateRoot | No | No | Yes |
| Anchoring arkiv_stateRoot on-chain (future) | Stores per block | Submits tx | Returns root |
| Maintaining private query index (trie + bitmaps) | No | No | Yes |
| Handling reorgs in query index | No | Detects and signals | Repopulates from trie |
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

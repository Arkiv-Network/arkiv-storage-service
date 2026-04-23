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

> **Design decision.** The EntityDB does not currently submit `arkiv_stateRoot` to the `EntityRegistry` contract. The EntityDB and op-reth run as separate, independent processes. The EntityDB computes a state root internally after each block, but that root is not anchored on-chain. This will remain the case until the EntityDB has demonstrated sufficient determinism — expected to be around one year of production operation. On-chain commitment and the full verifiability proof chain described in §2 are planned for a future phase.
>
> In the current phase, **partial verification** is available: the `EntityRegistry` contract stores a `coreHash` commitment for every live entity (§2), which clients can use to verify that the payload and attributes returned by the EntityDB are authentic. Query result completeness (i.e. that the result set contains all matching entities) cannot be verified in the current phase.

---

## Abstract

This document describes the Arkiv EntityDB architecture, composed of three components: an `EntityRegistry` smart contract deployed on the Reth chain, a Reth execution extension (ExEx), and a standalone Go EntityDB service.

Arkiv databases are L3s. The Reth node, the `EntityRegistry` contract, and the ExEx all run on the L3. The L3 settles against an L2 (OP Stack), which in turn settles against Ethereum L1.

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
│  L3 Reth Node                                                    │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  EntityRegistry (smart contract, on L3)                   │   │
│  │  - Validates Arkiv operations                             │   │
│  │  - Emits logs consumed by ExEx                            │   │
│  │  - Committed in L3 stateRoot on every block               │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │  Arkiv ExEx                                               │   │
│  │  - Watches sealed blocks for EntityRegistry calls         │   │
│  │  - Parses logs, forwards ops to Go EntityDB               │   │
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
3. For each block, the ExEx filters to successful calls to `ENTITY_REGISTRY_ADDRESS`, reads the emitted logs to extract the typed operations (including `entityKey` for Create ops), and assembles one `ArkivBlock` per block. Blocks with no `EntityRegistry` calls are still forwarded with an empty `operations` list.
4. The ExEx calls `arkiv_commitChain` on the Go EntityDB. The EntityDB applies the block and computes `arkiv_stateRoot` internally, but does not return it to the ExEx and the ExEx does not submit it anywhere.
5. Query clients call `arkiv_query` or `arkiv_getEntity` on the Go EntityDB's query server.

On `ChainReverted`, the ExEx sends `arkiv_revert` with block identifiers only. The EntityDB replays its journal in reverse. On `ChainReorged`, the ExEx sends `arkiv_reorg` atomically.

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
4. For each passing transaction, reads the logs emitted by the contract to extract the typed operation list. Each `EntityOperation` log includes the `entityKey` (the 32-byte `keccak256(chainId || registry || owner || nonce)` value minted by the contract). The EntityDB derives the trie account address as `entityKey[:20]`. The ExEx does not recompute keys locally.
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
    pub entity_key:   B256,      // keccak256(chainId || registry || owner || nonce) — from EntityOperation log
    pub sender:       Address,   // becomes Creator in EntityRLP
    pub payload:      Bytes,
    pub content_type: String,
    pub expires_at:   u64,       // block number, passed through directly from contract log
    pub owner:        Address,
    pub annotations:  Vec<Annotation>,
}

#[derive(Serialize)]
pub struct UpdateOp {
    pub entity_key:  B256,
    pub payload:     Bytes,
    pub content_type: String,
    pub expires_at:  u64,        // block number
    pub annotations: Vec<Annotation>,
}

#[derive(Serialize)]
pub struct DeleteOp {
    pub entity_key: B256,
}

#[derive(Serialize)]
pub struct ExtendOp {
    pub entity_key:    B256,
    pub new_expires_at: u64,     // block number
}

#[derive(Serialize)]
pub struct ChangeOwnerOp {
    pub entity_key: B256,
    pub new_owner:  Address,
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
            "type":        "create",
            "entityKey":   "0x1234...abcd",
            "sender":      "0x...",
            "payload":     "0x...",
            "contentType": "application/json",
            "expiresAt":   13500,
            "owner":       "0x...",
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

`arkiv_stateRoot` — the root of the EntityDB's trie after each block — is computed and retained internally but is not currently submitted on-chain (see the design decision note and §2).

**Immutable vs mutable state.** Two distinct kinds of state live in the EntityDB:

- **Immutable, content-addressed** — entity RLP blobs (`"c" + codeHash`) and bitmap byte arrays (`"arkiv_bm" + hash`) are written once and never overwritten. They piggyback on the trie's versioning mechanism: the trie root changes when content changes, and old content is never deleted. These entries require no journal.
- **Mutable, journaled** — bitmap pointer entries (`"arkiv_annot"`), ID↔address mappings (`"arkiv_id"`, `"arkiv_addr"`), and trie account state are updated in place on each block. These entries are recorded in a per-block journal so they can be reversed on reorg (§5).

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
    Key                common.Hash          // full 32-byte entityKey = keccak256(chainId || registry || owner || nonce)
    StringAnnotations  []StringAnnotationRLP
    NumericAnnotations []NumericAnnotationRLP
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
"arkiv_root"   + blockHash (32 bytes)                   →  stateRoot (32 bytes)           (per-block state root; written on ProcessBlock, deleted on RevertBlock)
"arkiv_parent" + blockHash (32 bytes)                   →  parentHash (32 bytes)          (per-block parent pointer; written on ProcessBlock, deleted on RevertBlock)
"arkiv_head"                                            →  blockNumber (8 bytes) + blockHash (32 bytes) + stateRoot (32 bytes)  (canonical head; overwritten on every ProcessBlock and RevertBlock)
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

> **⚠ Non-atomic writes.** The trie (TrieDB) and the mutable PebbleDB entries have no shared transaction boundary. Steps 1–6 below are performed sequentially, and a crash between any two steps leaves the two stores in an inconsistent state. The current behaviour on restart is to re-open at the last persisted `arkiv_head`, which was written last: if the crash occurred before step 5, `arkiv_head` still points to the previous block, the new trie root is orphaned, and the mutable PebbleDB entries are in the pre-block state — consistent with the old head. The block is silently dropped and the ExEx must re-deliver it. If the crash occurred after step 5 but mid-way through a later write, partial state may be visible. This should be hardened before production with a write-ahead log or equivalent recovery mechanism.

After processing all operations for a block, the EntityDB:

1. Calls `StateDB.Commit(blockNumber, true)` to flush trie changes and obtain the new `stateRoot`.
2. Calls `TrieDB.Commit(stateRoot, false)` to flush trie nodes to the underlying database. `false` means old nodes are not garbage-collected; `HashScheme` retains all historical roots.
3. Persists the per-block journal for mutable PebbleDB entries.
4. Writes `"arkiv_root" + blockHash → stateRoot` and `"arkiv_parent" + blockHash → parentHash` to PebbleDB.
5. Writes `"arkiv_head" → blockNumber + blockHash + stateRoot` to PebbleDB, overwriting the previous canonical head record.
6. Updates the in-memory canonical head pointer.

On restart, `New()` reads `"arkiv_head"` to restore the canonical head. If the key is absent (fresh database) the store starts from an empty trie (`EmptyRootHash`).

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

> **⚠ Non-atomic writes.** The journal batch and the `arkiv_head` update are separate writes with no shared transaction. A crash after the journal batch commits but before `arkiv_head` is updated leaves PebbleDB in the reverted state while the canonical head still points to the reverted block. On restart the store opens at the stale head, with the trie root still valid but the PebbleDB bitmap/ID state inconsistent with it. Recovery logic is needed to detect and resolve this. See §4 (Block Commit and State Root) for the same issue on the commit path.

To revert a sequence of blocks (newest-first):

For each block being reverted:

1. Read all journal entries for `(blockNumber, blockHash)` from PebbleDB.
2. Replay them in reverse entry-index order:
   - If `OldValue` is nil, delete the key.
   - Otherwise, write `OldValue` back to the key.
3. Delete the journal entries for this block.
4. Delete `"arkiv_root" + blockHash` and `"arkiv_parent" + blockHash`.
5. Write the updated `"arkiv_head"` pointing to the parent block.

The trie is already at the correct state (it retains all historical roots); only the mutable PebbleDB entries require explicit reversion.

### Journal Pruning

Once a block is finalized it can never be reverted, so its journal entries serve no purpose and can be deleted.

**Pruning depth.** The EntityDB is an L3 built on OP Stack. A block is considered final once the corresponding L2 output root has been posted and the fault-proof challenge window has elapsed without a successful challenge. The challenge window is 7 days (configurable per OP Stack deployment). At a 2-second L3 block time this corresponds to approximately **302,400 L3 blocks** (7 × 24 × 3600 ÷ 2). Journal entries for blocks older than this depth are safe to delete. The depth is configurable; the default should be set conservatively at or above the deployed challenge window.

**What is pruned.** Only `"arkiv_journal"` entries are deleted. Immutable entries (`"arkiv_bm"`, `"arkiv_pairs"`, `"c" + codeHash`) are never pruned — they are content-addressed and required for historical queries. `"arkiv_root"` and `"arkiv_parent"` entries are also retained indefinitely to support historical state root lookups.

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
  "arkiv_root"    + blockHash (32 bytes)                   →  stateRoot (32 bytes)              (per-block; deleted on RevertBlock)
  "arkiv_parent"  + blockHash (32 bytes)                   →  parentHash (32 bytes)             (per-block parent pointer; deleted on RevertBlock)
  "arkiv_head"                                             →  blockNumber + blockHash + stateRoot (72 bytes; canonical head, survives restart)
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
| Computing arkiv_stateRoot (internal only) | No | No | Yes |
| Anchoring arkiv_stateRoot on-chain (future) | Stores per block | Submits tx | Returns root |
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

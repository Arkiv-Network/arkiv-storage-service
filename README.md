# arkiv-storage-service

A query index for Arkiv entity state.

## What it is

`arkiv-storage-service` is a standalone Go process that maintains a queryable index of Arkiv entity state. It is one of three components in the Arkiv architecture:

- **EntityRegistry** — a smart contract on the L3 that validates all Arkiv mutations and emits logs
- **arkiv-op-reth** — an OP Reth node with an execution extension (ExEx) that watches sealed blocks, parses `EntityRegistry` logs, and forwards operations here via JSON-RPC
- **arkiv-storage-service** (this repo) — consumes chain events from the arkiv-op-reth ExEx, maintains the entity state index, and serves queries to SDK clients

## What it does

The service maintains two storage layers backed by PebbleDB:

- A **Merkle Patricia Trie** (via go-ethereum's `StateDB`) that holds one account per entity. Each entity account's `codeHash` commits to the full entity RLP, and a system account holds annotation bitmap hashes and ID→address mappings as trie-committed storage slots.
- **PebbleDB bitmaps** (`roaring64`) that index entities by annotation key/value pairs, enabling fast equality, range, and glob queries.

## Running

Build and install the `arkiv-storaged` daemon:

```sh
make build    # writes to ./bin/arkiv-storaged
make install  # installs to $GOPATH/bin
```

Run it:

```sh
arkiv-storaged [flags]

Flags:
  -chain-addr  listen address for the chain ingest server  (default 127.0.0.1:2704)
  -query-addr  listen address for the query server         (default 127.0.0.1:2705)
  -data-dir    path to the data directory                  (default ~/.arkiv-storaged)
```

### Configuration file

On startup `arkiv-storaged` reads `<data-dir>/config.yaml`. Command-line flags take precedence over file values. Example:

```yaml
chain-addr: "127.0.0.1:2704"
query-addr: "127.0.0.1:2705"
```

## JSON-RPC interface

The service exposes two HTTP JSON-RPC 2.0 servers:

**Chain ingest** `:2704` — private, called by the arkiv-op-reth ExEx only:
- `arkiv_commitChain` — apply a sequence of blocks to the canonical head
- `arkiv_revert` — revert blocks back to a common ancestor
- `arkiv_reorg` — atomically revert and re-apply blocks

**Query server** `:2705` — called by SDK clients:
- `arkiv_query` — SQL-like queries against latest or historical state
- `arkiv_getEntityByAddress` — fetch a single entity by address
- `arkiv_getEntityCount` — total number of live entities at the head

## Development

```sh
make test   # run all tests (integration tests build and spawn the real binary)
make lint   # run golangci-lint
```

## Further reading

Full architecture design: [`architecture.md`](architecture.md)

package integration_test

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/Arkiv-Network/arkiv-storage-service/chain"
	"github.com/Arkiv-Network/arkiv-storage-service/store"
	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// newClient starts an in-process chain server and returns an RPC client connected to it.
func newClient(t *testing.T) *rpc.Client {
	t.Helper()
	srv, err := chain.New(slog.Default(), store.NewMemory())
	if err != nil {
		t.Fatalf("chain.New: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	client, err := rpc.Dial(ts.URL)
	if err != nil {
		t.Fatalf("rpc.Dial: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestCommitChain(t *testing.T) {
	client := newClient(t)

	req := chain.CommitChainRequest{
		Blocks: []types.ArkivBlock{
			{
				Header: types.ArkivBlockHeader{
					Number:     hexutil.Uint64(1),
					Hash:       common.HexToHash("0x01"),
					ParentHash: common.HexToHash("0x00"),
				},
				Operations: nil,
			},
		},
	}

	if err := client.Call(nil, "arkiv_commitChain", req); err != nil {
		t.Fatalf("arkiv_commitChain: %v", err)
	}
}

func TestRevert(t *testing.T) {
	client := newClient(t)

	// First commit a block so there is something to revert.
	if err := client.Call(nil, "arkiv_commitChain", chain.CommitChainRequest{
		Blocks: []types.ArkivBlock{
			{
				Header: types.ArkivBlockHeader{
					Number:     hexutil.Uint64(1),
					Hash:       common.HexToHash("0x01"),
					ParentHash: common.HexToHash("0x00"),
				},
			},
		},
	}); err != nil {
		t.Fatalf("arkiv_commitChain: %v", err)
	}

	if err := client.Call(nil, "arkiv_revert", chain.RevertRequest{
		Blocks: []types.ArkivBlockRef{
			{Number: hexutil.Uint64(1), Hash: common.HexToHash("0x01")},
		},
	}); err != nil {
		t.Fatalf("arkiv_revert: %v", err)
	}
}

func TestReorg(t *testing.T) {
	client := newClient(t)

	// Commit blocks 1 and 2 on the original chain.
	if err := client.Call(nil, "arkiv_commitChain", chain.CommitChainRequest{
		Blocks: []types.ArkivBlock{
			{Header: types.ArkivBlockHeader{Number: 1, Hash: common.HexToHash("0x01"), ParentHash: common.HexToHash("0x00")}},
			{Header: types.ArkivBlockHeader{Number: 2, Hash: common.HexToHash("0x02"), ParentHash: common.HexToHash("0x01")}},
		},
	}); err != nil {
		t.Fatalf("arkiv_commitChain: %v", err)
	}

	// Reorg: revert blocks 2 and 1, commit a new block 1 and 2.
	if err := client.Call(nil, "arkiv_reorg", chain.ReorgRequest{
		RevertedBlocks: []types.ArkivBlockRef{
			{Number: 2, Hash: common.HexToHash("0x02")},
			{Number: 1, Hash: common.HexToHash("0x01")},
		},
		NewBlocks: []types.ArkivBlock{
			{Header: types.ArkivBlockHeader{Number: 1, Hash: common.HexToHash("0xaa"), ParentHash: common.HexToHash("0x00")}},
			{Header: types.ArkivBlockHeader{Number: 2, Hash: common.HexToHash("0xbb"), ParentHash: common.HexToHash("0xaa")}},
		},
	}); err != nil {
		t.Fatalf("arkiv_reorg: %v", err)
	}
}

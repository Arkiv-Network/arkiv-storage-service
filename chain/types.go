package chain

import "github.com/Arkiv-Network/arkiv-storage-service/types"

// CommitChainRequest is the parameter object for arkiv_commitChain.
type CommitChainRequest struct {
	Blocks []types.ArkivBlock `json:"blocks"`
}

// RevertRequest is the parameter object for arkiv_revert.
type RevertRequest struct {
	Blocks []types.ArkivBlockRef `json:"blocks"`
}

// ReorgRequest is the parameter object for arkiv_reorg.
type ReorgRequest struct {
	RevertedBlocks []types.ArkivBlockRef `json:"revertedBlocks"`
	NewBlocks      []types.ArkivBlock    `json:"newBlocks"`
}

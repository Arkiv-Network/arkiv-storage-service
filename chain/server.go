package chain

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Arkiv-Network/arkiv-storage-service/store"
	"github.com/ethereum/go-ethereum/rpc"
)

// handler implements the arkiv_* JSON-RPC methods consumed by the Reth ExEx.
// Exported methods are registered as arkiv_<camelCaseMethodName>.
type handler struct {
	log   *slog.Logger
	store *store.Store
}

func (h *handler) CommitChain(_ context.Context, req CommitChainRequest) error {
	for _, block := range req.Blocks {
		stateRoot, err := h.store.ProcessBlock(block)
		if err != nil {
			return fmt.Errorf("process block %d: %w", block.Header.Number, err)
		}
		h.log.Info("commitChain", "block", block.Header.Number, "stateRoot", stateRoot)
	}
	return nil
}

func (h *handler) Revert(_ context.Context, req RevertRequest) error {
	for _, ref := range req.Blocks {
		if err := h.store.RevertBlock(ref); err != nil {
			return fmt.Errorf("revert block %d: %w", ref.Number, err)
		}
		h.log.Info("revert", "block", ref.Number)
	}
	return nil
}

func (h *handler) Reorg(_ context.Context, req ReorgRequest) error {
	for _, ref := range req.RevertedBlocks {
		if err := h.store.RevertBlock(ref); err != nil {
			return fmt.Errorf("revert block %d: %w", ref.Number, err)
		}
	}
	for _, block := range req.NewBlocks {
		stateRoot, err := h.store.ProcessBlock(block)
		if err != nil {
			return fmt.Errorf("process block %d: %w", block.Header.Number, err)
		}
		h.log.Info("reorg: committed", "block", block.Header.Number, "stateRoot", stateRoot)
	}
	return nil
}

// Server is an HTTP server exposing the arkiv chain ingest JSON-RPC 2.0 API.
// It is intended to be private — only the Reth ExEx should be able to reach it.
type Server struct {
	rpc *rpc.Server
}

func New(log *slog.Logger, store *store.Store) (*Server, error) {
	srv := rpc.NewServer()
	if err := srv.RegisterName("arkiv", &handler{log: log, store: store}); err != nil {
		return nil, err
	}
	return &Server{rpc: srv}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.rpc.ServeHTTP(w, r)
}

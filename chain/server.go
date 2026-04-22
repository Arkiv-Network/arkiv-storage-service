package chain

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ethereum/go-ethereum/rpc"
)

// handler implements the arkiv_* JSON-RPC methods consumed by the Reth ExEx.
// Exported methods are registered as arkiv_<camelCaseMethodName>.
type handler struct {
	log *slog.Logger
}

func (h *handler) CommitChain(_ context.Context, req CommitChainRequest) error {
	h.log.Info("commitChain", "blocks", len(req.Blocks))
	return nil
}

func (h *handler) Revert(_ context.Context, req RevertRequest) error {
	h.log.Info("revert", "blocks", len(req.Blocks))
	return nil
}

func (h *handler) Reorg(_ context.Context, req ReorgRequest) error {
	h.log.Info("reorg", "reverted", len(req.RevertedBlocks), "new", len(req.NewBlocks))
	return nil
}

// Server is an HTTP server exposing the arkiv chain ingest JSON-RPC 2.0 API.
// It is intended to be private — only the Reth ExEx should be able to reach it.
type Server struct {
	rpc *rpc.Server
}

func New(log *slog.Logger) (*Server, error) {
	srv := rpc.NewServer()
	if err := srv.RegisterName("arkiv", &handler{log: log}); err != nil {
		return nil, err
	}
	return &Server{rpc: srv}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.rpc.ServeHTTP(w, r)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Arkiv-Network/arkiv-storage-service/chain"
	"github.com/Arkiv-Network/arkiv-storage-service/query"
	"github.com/Arkiv-Network/arkiv-storage-service/store"
	"github.com/Arkiv-Network/arkiv-storage-service/version"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"gopkg.in/yaml.v2"
)

const configFileName = "config.yaml"

// config holds all tuneable parameters. YAML keys match the flag names so a
// single struct works for both the file and the CLI override logic.
type config struct {
	ChainAddr string `yaml:"chain-addr"`
	QueryAddr string `yaml:"query-addr"`
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".arkiv-storaged"
	}
	return filepath.Join(home, ".arkiv-storaged")
}

// loadConfig reads <dataDir>/config.yaml. Missing file is not an error.
func loadConfig(dataDir string) (cfg config, err error) {
	path := filepath.Join(dataDir, configFileName)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	err = yaml.NewDecoder(f).Decode(&cfg)
	return
}

func main() {
	// Register flags with their hard-coded defaults.
	chainAddr := flag.String("chain-addr", "127.0.0.1:2704", "address for the chain ingest JSON-RPC server (arkiv-op-reth → storaged)")
	queryAddr := flag.String("query-addr", "127.0.0.1:2705", "address for the query JSON-RPC server (SDK → storaged)")
	dataDir := flag.String("data-dir", defaultDataDir(), "path to the data directory (config.yaml read here; PebbleDB opened at <data-dir>/db)")
	showVersion := flag.Bool("version", false, "print build version information and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `arkiv-storaged — Arkiv entity storage daemon

Maintains a queryable index of Arkiv entity state. It receives committed and
reverted blocks from the arkiv-op-reth ExEx via a private chain ingest server,
and serves entity queries to SDK clients via a separate query server.

Usage:
  arkiv-storaged [flags]

Flags:
`)
		flag.PrintDefaults()
	}

	flag.Parse()

	if *showVersion {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(version.Current()); err != nil {
			fmt.Fprintf(os.Stderr, "print version: %v\n", err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Load config file from the data dir resolved so far.
	// CLI flags always win, so we only apply file values for flags the user
	// did not explicitly pass.
	cfg, err := loadConfig(*dataDir)
	if err != nil {
		log.Error("failed to read config file", "path", filepath.Join(*dataDir, configFileName), "err", err)
		os.Exit(1)
	}

	// Build the set of flags that were explicitly provided on the command line.
	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// Apply config-file values for flags that were not explicitly set.
	if !explicit["chain-addr"] && cfg.ChainAddr != "" {
		*chainAddr = cfg.ChainAddr
	}
	if !explicit["query-addr"] && cfg.QueryAddr != "" {
		*queryAddr = cfg.QueryAddr
	}

	// Open the database at <data-dir>/db so config.yaml and the PebbleDB
	// directory are siblings rather than mixed together.
	dbPath := filepath.Join(*dataDir, "db")
	log.Info("opening pebble database", "path", dbPath)
	kv, err := pebble.New(dbPath, 128, 512, "arkiv", false)
	if err != nil {
		log.Error("failed to open pebble database", "err", err)
		os.Exit(1)
	}
	s := store.New(rawdb.NewDatabase(kv))

	// Build the two HTTP servers.
	chainSrv, err := chain.New(log, s)
	if err != nil {
		log.Error("failed to create chain server", "err", err)
		os.Exit(1)
	}
	querySrv, err := query.New(s)
	if err != nil {
		log.Error("failed to create query server", "err", err)
		os.Exit(1)
	}

	chainHTTP := &http.Server{Addr: *chainAddr, Handler: version.Handler(chainSrv)}
	queryHTTP := &http.Server{Addr: *queryAddr, Handler: version.Handler(querySrv)}

	// Start both servers.
	go func() {
		log.Info("chain ingest server listening", "addr", *chainAddr)
		if err := chainHTTP.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error("chain server error", "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		log.Info("query server listening", "addr", *queryAddr)
		if err := queryHTTP.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error("query server error", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for SIGINT or SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("shutting down")
	chainHTTP.Shutdown(context.Background()) //nolint:errcheck
	queryHTTP.Shutdown(context.Background()) //nolint:errcheck
}

package main

import (
	"fmt"
	"path/filepath"

	"os"

	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/log"
	tmos "github.com/tendermint/tendermint/libs/os"
	tmstrings "github.com/tendermint/tendermint/libs/strings"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/p2p/pex"
	"github.com/tendermint/tendermint/version"

	"github.com/mitchellh/go-homedir"
)

// Config defines the configuration format for TinySeed
type Config struct {
	ListenAddress       string `toml:"laddr" comment:"Address to listen for incoming connections"`
	ChainID             string `toml:"chain_id" comment:"network identifier (todo move to cli flag argument? keeps the config network agnostic)"`
	NodeKeyFile         string `toml:"node_key_file" comment:"path to node_key (relative to tendermint-seed home directory or an absolute path)"`
	AddrBookFile        string `toml:"addr_book_file" comment:"path to address book (relative to tendermint-seed home directory or an absolute path)"`
	AddrBookStrict      bool   `toml:"addr_book_strict" comment:"Set true for strict routability rules\n Set false for private or local networks"`
	MaxNumInboundPeers  int    `toml:"max_num_inbound_peers" comment:"maximum number of inbound connections"`
	MaxNumOutboundPeers int    `toml:"max_num_outbound_peers" comment:"maximum number of outbound connections"`
	Seeds               string `toml:"seeds" comment:"seed nodes we can use to discover peers"`
}

// DefaultConfig returns a seed config initialized with default values
func DefaultConfig(homeDir string) *Config {
	return &Config{
		ListenAddress:       "tcp://0.0.0.0:36656",
		ChainID:             "columbus-5",
		NodeKeyFile:         filepath.Join(homeDir, "config/node_key.json"),
		AddrBookFile:        filepath.Join(homeDir, "data/addrbook.json"),
		AddrBookStrict:      true,
		MaxNumInboundPeers:  1000,
		MaxNumOutboundPeers: 1000,
		Seeds:               "e999fc20aa5b87c1acef8677cf495ad85061cfb9@seed.terra.delightlabs.io:26656,6d8e943c049a80c161a889cb5fcf3d184215023e@public-seed2.terra.dev:26656,87048bf71526fb92d73733ba3ddb79b7a83ca11e@public-seed.terra.dev:26656",
	}
}

// TinySeed lives here.  Smol ting.
func main() {
	idOverride := os.Getenv("ID")
	seedOverride := os.Getenv("SEEDS")
	listenAddressOverride := os.Getenv("LISTENADDRESS")
	userHomeDir, err := homedir.Dir()
	if err != nil {
		panic(err)
	}
	homeDir := filepath.Join(userHomeDir, ".tinyseed")
	configFile := "config/config.toml"
	configFilePath := filepath.Join(homeDir, configFile)
	MkdirAllPanic(filepath.Dir(configFilePath), os.ModePerm)
	SeedConfig := DefaultConfig(homeDir)
	if idOverride != "" {
		SeedConfig.ChainID = idOverride
	}
	if seedOverride != "" {
		SeedConfig.Seeds = seedOverride
	}
	if listenAddressOverride != "" {
		SeedConfig.ListenAddress = listenAddressOverride
	}
	Start(*SeedConfig)
}

// MkdirAllPanic invokes os.MkdirAll but panics if there is an error
func MkdirAllPanic(path string, perm os.FileMode) {
	err := os.MkdirAll(path, perm)
	if err != nil {
		panic(err)
	}
}

// Start starts a Tenderseed
func Start(SeedConfig Config) {
	logger := log.NewTMLogger(
		log.NewSyncWriter(os.Stdout),
	)

	chainID := SeedConfig.ChainID
	nodeKeyFilePath := SeedConfig.NodeKeyFile
	addrBookFilePath := SeedConfig.AddrBookFile

	MkdirAllPanic(filepath.Dir(nodeKeyFilePath), os.ModePerm)
	MkdirAllPanic(filepath.Dir(addrBookFilePath), os.ModePerm)

	cfg := config.DefaultP2PConfig()
	cfg.AllowDuplicateIP = true

	// allow a lot of inbound peers since we disconnect from them quickly in seed mode
	cfg.MaxNumInboundPeers = SeedConfig.MaxNumInboundPeers

	// keep trying to make outbound connections to exchange peering info
	cfg.MaxNumOutboundPeers = SeedConfig.MaxNumOutboundPeers

	nodeKey, err := p2p.LoadOrGenNodeKey(nodeKeyFilePath)
	if err != nil {
		panic(err)
	}

	logger.Info("tenderseed",
		"key", nodeKey.ID(),
		"key path", nodeKeyFilePath,
		"address book path", addrBookFilePath,
		"listen", SeedConfig.ListenAddress,
		"chain", chainID,
		"strict-routing", SeedConfig.AddrBookStrict,
		"max-inbound", SeedConfig.MaxNumInboundPeers,
		"max-outbound", SeedConfig.MaxNumOutboundPeers,
	)

	// TODO(roman) expose per-module log levels in the config
	filteredLogger := log.NewFilter(logger, log.AllowInfo())

	protocolVersion :=
		p2p.NewProtocolVersion(
			version.P2PProtocol,
			version.BlockProtocol,
			0,
		)

	// NodeInfo gets info on your node
	nodeInfo := p2p.DefaultNodeInfo{
		ProtocolVersion: protocolVersion,
		DefaultNodeID:   nodeKey.ID(),
		ListenAddr:      SeedConfig.ListenAddress,
		Network:         chainID,
		Version:         "0.5.9",
		Channels:        []byte{pex.PexChannel},
		Moniker:         fmt.Sprintf("%s-seed", chainID),
	}

	addr, err := p2p.NewNetAddressString(p2p.IDAddressString(nodeInfo.DefaultNodeID, nodeInfo.ListenAddr))
	if err != nil {
		panic(err)
	}

	transport := p2p.NewMultiplexTransport(nodeInfo, *nodeKey, p2p.MConnConfig(cfg))
	if err := transport.Listen(*addr); err != nil {
		panic(err)
	}

	book := pex.NewAddrBook(addrBookFilePath, SeedConfig.AddrBookStrict)
	book.SetLogger(filteredLogger.With("module", "book"))

	pexReactor := pex.NewReactor(book, &pex.ReactorConfig{
		SeedMode: true,
		Seeds:    tmstrings.SplitAndTrim(SeedConfig.Seeds, ",", " "),
	})
	pexReactor.SetLogger(filteredLogger.With("module", "pex"))

	sw := p2p.NewSwitch(cfg, transport)
	sw.SetLogger(filteredLogger.With("module", "switch"))
	sw.SetNodeKey(nodeKey)
	sw.SetAddrBook(book)
	sw.AddReactor("pex", pexReactor)

	// last
	sw.SetNodeInfo(nodeInfo)

	tmos.TrapSignal(logger, func() {
		logger.Info("shutting down...")
		book.Save()
		err := sw.Stop()
		if err != nil {
			panic(err)
		}
	})

	err = sw.Start()
	if err != nil {
		panic(err)
	}

	sw.Wait()
}

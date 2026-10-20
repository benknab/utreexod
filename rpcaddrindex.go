package main

import (
	"errors"

	"github.com/utreexo/utreexod/blockchain/indexers"
	"github.com/utreexo/utreexod/btcjson"
)

func handleGetAddrIndexInfo(s *rpcServer, _ interface{}, _ <-chan struct{}) (interface{}, error) {
	if s.cfg.BackgroundAddrIndex == nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "Address index must be enabled (--addrindex)"}
	}
	status, err := s.cfg.BackgroundAddrIndex.Snapshot()
	if err != nil {
		return nil, internalRPCError(err.Error(), "Failed to read address index status")
	}
	return btcjson.AddrIndexInfoResult(status), nil
}

func addressIndexRPCError(err error) error {
	if errors.Is(err, indexers.ErrAddrIndexNotReady) {
		return &btcjson.RPCError{Code: btcjson.ErrRPCInWarmup, Message: err.Error()}
	}
	return internalRPCError(err.Error(), "Failed to load address index entries")
}

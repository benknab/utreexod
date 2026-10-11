package main

import "github.com/utreexo/utreexod/btcjson"

func handleGetProofIndexInfo(s *rpcServer, _ interface{}, _ <-chan struct{}) (interface{}, error) {
	if s.cfg.BackgroundProofIndex == nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "background proof indexing is not enabled"}
	}
	return btcjson.ProofIndexInfoResult(s.cfg.BackgroundProofIndex.Snapshot()), nil
}

func handleSetProofIndexPaused(s *rpcServer, cmd interface{}, _ <-chan struct{}) (interface{}, error) {
	if s.cfg.BackgroundProofIndex == nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "background proof indexing is not enabled"}
	}
	s.cfg.BackgroundProofIndex.SetPaused(cmd.(*btcjson.SetProofIndexPausedCmd).Paused)
	return btcjson.ProofIndexInfoResult(s.cfg.BackgroundProofIndex.Snapshot()), nil
}

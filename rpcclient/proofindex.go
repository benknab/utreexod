package rpcclient

import (
	"encoding/json"

	"github.com/utreexo/utreexod/btcjson"
)

// FutureProofIndexInfoResult is a pending proof-index status response.
type FutureProofIndexInfoResult chan *Response

func (r FutureProofIndexInfoResult) Receive() (*btcjson.ProofIndexInfoResult, error) {
	data, err := ReceiveFuture(r)
	if err != nil {
		return nil, err
	}
	var result btcjson.ProofIndexInfoResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetProofIndexInfoAsync requests progress without waiting for proof generation.
func (c *Client) GetProofIndexInfoAsync() FutureProofIndexInfoResult {
	return c.SendCmd(btcjson.NewGetProofIndexInfoCmd())
}

func (c *Client) GetProofIndexInfo() (*btcjson.ProofIndexInfoResult, error) {
	return c.GetProofIndexInfoAsync().Receive()
}

// SetProofIndexPausedAsync requests a pause or resume at the next block boundary.
func (c *Client) SetProofIndexPausedAsync(paused bool) FutureProofIndexInfoResult {
	return c.SendCmd(btcjson.NewSetProofIndexPausedCmd(paused))
}

func (c *Client) SetProofIndexPaused(paused bool) (*btcjson.ProofIndexInfoResult, error) {
	return c.SetProofIndexPausedAsync(paused).Receive()
}

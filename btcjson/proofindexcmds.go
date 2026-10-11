package btcjson

// ProofIndexInfoResult reports progress separately from conventional validation.
type ProofIndexInfoResult struct {
	Height          int32  `json:"height"`
	BestBlockHash   string `json:"bestblockhash"`
	ValidatedHeight int32  `json:"validatedheight"`
	Synced          bool   `json:"synced"`
	Paused          bool   `json:"paused"`
	Initializing    bool   `json:"initializing"`
	Processing      bool   `json:"processing"`
	Stopped         bool   `json:"stopped"`
	LastError       string `json:"error,omitempty"`
}

// GetProofIndexInfoCmd requests independently progressing proof-index status.
type GetProofIndexInfoCmd struct{}

func NewGetProofIndexInfoCmd() *GetProofIndexInfoCmd { return &GetProofIndexInfoCmd{} }

// SetProofIndexPausedCmd pauses or resumes proof indexing at a block boundary.
type SetProofIndexPausedCmd struct {
	Paused bool
}

func NewSetProofIndexPausedCmd(paused bool) *SetProofIndexPausedCmd {
	return &SetProofIndexPausedCmd{Paused: paused}
}

func init() {
	MustRegisterCmd("getproofindexinfo", (*GetProofIndexInfoCmd)(nil), 0)
	MustRegisterCmd("setproofindexpaused", (*SetProofIndexPausedCmd)(nil), 0)
}

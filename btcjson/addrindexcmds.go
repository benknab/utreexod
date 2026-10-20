package btcjson

// AddrIndexInfoResult reports committed address-index progress separately from
// block validation. Synced requires the same block hash, not just the height.
type AddrIndexInfoResult struct {
	Height          int32  `json:"height"`
	BestBlockHash   string `json:"bestblockhash"`
	ValidatedHeight int32  `json:"validatedheight"`
	Synced          bool   `json:"synced"`
	Stopped         bool   `json:"stopped"`
	LastError       string `json:"error,omitempty"`
}

// GetAddrIndexInfoCmd requests background address-index progress.
type GetAddrIndexInfoCmd struct{}

func NewGetAddrIndexInfoCmd() *GetAddrIndexInfoCmd { return &GetAddrIndexInfoCmd{} }

func init() {
	MustRegisterCmd("getaddrindexinfo", (*GetAddrIndexInfoCmd)(nil), 0)
}

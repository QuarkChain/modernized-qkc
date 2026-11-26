package p2pnet

import qkctypes "github.com/ethereum/go-ethereum/qkc/types"

// RootHeadersResult contains decoded business data returned by a root-header query.
type RootHeadersResult struct {
	RootTip *qkctypes.RootBlockHeader
	Headers []*qkctypes.RootBlockHeader
}

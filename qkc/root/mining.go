package root

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/qkc/mining"
)

var _ mining.MinerBackend = (*RootChain)(nil)

// BuildMiningWork packages a root candidate at its base seal difficulty.
func (c *RootChain) BuildMiningWork(ctx context.Context, coinbase common.Address) (*mining.Candidate, error) {
	block, err := c.CreateBlockToMine(ctx, coinbase)
	if err != nil {
		return nil, err
	}
	return &mining.Candidate{
		MiningWork: mining.MiningWork{
			SealHash: c.engine.SealHash(block.Header()), Number: block.NumberU64(),
			Difficulty: block.Difficulty().ToBig(), Time: block.Time(),
		},
		Parent: block.ParentHash(),
		Submit: func(nonce types.BlockNonce, mixHash common.Hash) error {
			return c.InsertMinedBlock(block.WithMiningResult(nonce.Uint64(), mixHash, nil))
		},
	}, nil
}

// MiningHead returns the canonical root tip once the chain is ready.
func (c *RootChain) MiningHead() (common.Hash, common.Hash, error) {
	tip := c.RootTip()
	if !c.Ready() {
		return common.Hash{}, common.Hash{}, errChainNotReady
	}
	return tip.Hash(), common.Hash{}, nil
}

// SubscribeMiningHead registers for canonical root tip changes.
func (c *RootChain) SubscribeMiningHead(ch chan<- struct{}) event.Subscription {
	sub := c.headFeed.Subscribe(ch)
	tracked := c.headScope.Track(sub)
	if tracked == nil {
		sub.Unsubscribe()
	}
	return tracked
}

package root

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/qkc/mining"
)

var _ mining.MinerBackend = (*RootChain)(nil)

// BuildMiningWork packages a root candidate at its base seal difficulty.
// TODO(mining): Support signed candidates that qualify for the PoSW discount.
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
			err := c.InsertMinedBlock(block.WithMiningResult(nonce.Uint64(), mixHash, nil))
			if errors.Is(err, errNotOnTip) {
				return mining.ErrStaleWork
			}
			return err
		},
	}, nil
}

// MiningHead returns the canonical root tip and a zero anchor.
func (c *RootChain) MiningHead() (common.Hash, common.Hash, error) {
	head, err := c.MiningCurrentHead()
	return head, common.Hash{}, err
}

// MiningCurrentHead reads the canonical root tip once the chain is ready.
func (c *RootChain) MiningCurrentHead() (common.Hash, error) {
	tip := c.RootTip()
	if !c.Ready() {
		return common.Hash{}, errChainNotReady
	}
	return tip.Hash(), nil
}

// SubscribeMiningHead registers for canonical root tip changes.
func (c *RootChain) SubscribeMiningHead(ch chan<- struct{}) event.Subscription {
	return c.SubscribeChainHeadEvent(ch)
}

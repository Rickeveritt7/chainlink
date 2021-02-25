package fluxmonitorv2

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/utils"
)

var fluxAggregatorABI = eth.MustGetABI(flux_aggregator_wrapper.FluxAggregatorABI)

type ContractSubmitter interface {
	Submit(ctx context.Context, roundID uint32, submission []byte) error
}

type FluxAggregatorContractSubmitter struct {
	contractAddress common.Address
}

func NewFluxAggregatorContractSubmitter(
	address common.Address,
) *FluxAggregatorContractSubmitter {
	return &FluxAggregatorContractSubmitter{
		contractAddress: address,
	}
}

// Submit submits the answer by writing a EthTx for the bulletprooftxmanager to
// pick up
func (oc *FluxAggregatorContractSubmitter) Submit(ctx context.Context, roundID uint32, submission []byte) error {
	roundIDArg := utils.EVMWordUint64(uint64(roundID))

	_, err := fluxAggregatorABI.Pack("submit", roundIDArg, submission)
	if err != nil {
		return errors.Wrap(err, "abi.Pack failed")
	}

	// Write an Eth TX to the DB for the bulletproof transaction manager to execute

	return nil

	// return errors.Wrap(oc.transmitter.CreateEthTransaction(ctx, oc.contractAddress, payload), "failed to send Eth transaction")
}

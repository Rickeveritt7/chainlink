package fluxmonitorv2

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink/core/internal/gethwrappers/generated/flux_aggregator_wrapper"
)

//go:generate mockery --name ContractSubmitter --output ./mocks/ --case=underscore

// ContractSubmitter defines an interface to submit an eth transaction
type ContractSubmitter interface {
	Submit(roundID *big.Int, submission *big.Int) error
}

type FluxAggregatorContractSubmitter struct {
	flux_aggregator_wrapper.FluxAggregatorInterface
	fromAddress common.Address
}

// NewFluxAggregatorContractSubmitter constructs a new NewFluxAggregatorContractSubmitter
func NewFluxAggregatorContractSubmitter(
	contract flux_aggregator_wrapper.FluxAggregatorInterface,
	fromAddress common.Address,
) *FluxAggregatorContractSubmitter {
	return &FluxAggregatorContractSubmitter{
		FluxAggregatorInterface: contract,
		fromAddress:             fromAddress,
	}
}

// Submit submits the answer by writing a EthTx for the bulletprooftxmanager to
// pick up
func (c *FluxAggregatorContractSubmitter) Submit(roundID *big.Int, submission *big.Int) error {
	_, err := c.FluxAggregatorInterface.Submit(&bind.TransactOpts{
		From: c.fromAddress,
	}, roundID, submission)

	return err

	// TransactOpts is the collection of authorization data required to create a
	// valid Ethereum transaction.
	// type TransactOpts struct {
	// 	From   common.Address // Ethereum account to send the transaction from
	// 	Nonce  *big.Int       // Nonce to use for the transaction execution (nil = use pending state)
	// 	Signer SignerFn       // Method to use for signing the transaction (mandatory)

	// 	Value    *big.Int // Funds to transfer along the transaction (nil = 0 = no funds)
	// 	GasPrice *big.Int // Gas price to use for the transaction execution (nil = gas price oracle)
	// 	GasLimit uint64   // Gas limit to set for the transaction execution (0 = estimate)

	// 	Context context.Context // Network context to support cancellation and timeouts (nil = no timeout)
	// }

	// roundIDArg := utils.EVMWordUint64(uint64(roundID))

	// _, err := fluxAggregatorABI.Pack("submit", roundIDArg, submission)
	// if err != nil {
	// 	return errors.Wrap(err, "abi.Pack failed")
	// }

	// Write an Eth TX to the DB for the bulletproof transaction manager to execute
}

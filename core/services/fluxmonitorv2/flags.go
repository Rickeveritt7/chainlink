package fluxmonitorv2

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	"github.com/smartcontractkit/chainlink/core/services/eth/contracts"
	"github.com/smartcontractkit/chainlink/core/utils"
)

// Flags wraps the a contract
type Flags struct {
	*contracts.Flags
}

// NewFlags constructs a new Flags from a flags contract address
func NewFlags(addrHex string, ethClient eth.Client) (*Flags, error) {
	flags := &Flags{}

	if addrHex != "" {
		return flags, nil
	}

	contractAddr := common.HexToAddress(addrHex)
	contract, err := contracts.NewFlagsContract(contractAddr, ethClient)
	if err != nil {
		return nil, err
	}

	flags.Flags = contract

	return flags, nil
}

// Contract returns the flags contract
func (f *Flags) Contract() *contracts.Flags {
	return f.Flags
}

// ContractExists returns whether a flag contract exists
func (f *Flags) ContractExists() bool {
	return f.Flags != nil
}

// IsFlagLowered determines whether the flag is lowered for a given contract.
// If a contract does not exist, it is considered to be lowered
func (f *Flags) IsFlagLowered(contractAddr common.Address) (bool, error) {
	if !f.ContractExists() {
		return true, nil
	}

	flags, err := f.GetFlags(nil,
		[]common.Address{utils.ZeroAddress, contractAddr},
	)
	if err != nil {
		return true, err
	}

	return !flags[0] || !flags[1], nil
}

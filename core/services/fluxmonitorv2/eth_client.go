package fluxmonitorv2

import (
	"context"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/smartcontractkit/chainlink/core/services/eth"
)

// FluxMonitorEthClient wraps an eth.Client, overriding the SendTransaction
// function to send to the BPTXM
type FluxMonitorEthClient struct {
	eth.Client
	orm ORM
}

// NewFluxMonitorEthClient constructs a new FluxMonitorEthClient
func NewFluxMonitorEthClient(ethClient eth.Client, orm ORM) *FluxMonitorEthClient {
	return &FluxMonitorEthClient{
		Client: ethClient,
		orm:    orm,
	}
}

// SendTransaction overrides the SendTransaction of the EthClient to insert
// the transaction into the database for the BPTXM.
func (c *FluxMonitorEthClient) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	// t.fromAddress, toAddress, payload, value, t.gasLimit

	tx.To()    // ToAddress
	tx.Data()  // Payload
	tx.Value() // Value
	tx.Gas()   // Gas Limit
	// tx. // From

	// store.CreateEthTransaction(...)

	return nil
}

// sender, err := types.Sender(types.NewEIP155Signer(big.NewInt(int64(c.chainId))), tx)
// if err != nil {
// 	logger.Panic(fmt.Errorf("invalid transaction: %v", err))
// }
// pendingNonce, err := c.b.PendingNonceAt(ctx, sender)
// if err != nil {
// 	panic(fmt.Errorf("unable to determine nonce for account %s: %v", sender.Hex(), err))
// }
// // the simulated backend does not gracefully handle tx rebroadcasts (gas bumping) so just
// // ignore the situation where nonces are reused
// // github.com/ethereum/go-ethereum/blob/fb2c79df1995b4e8dfe79f9c75464d29d23aaaf4/accounts/abi/bind/backends/simulated.go#L556
// if tx.Nonce() < pendingNonce {
// 	return nil
// }

// err = c.b.SendTransaction(ctx, tx)
// c.b.Commit()
// return err

// func (t *transmitter) CreateEthTransaction(ctx context.Context, toAddress gethCommon.Address, payload []byte) error {
// 	value := 0
// 	res, err := t.db.ExecContext(ctx, `
// INSERT INTO eth_txes (from_address, to_address, encoded_payload, value, gas_limit, state, created_at)
// SELECT $1,$2,$3,$4,$5,'unstarted',NOW()
// WHERE NOT EXISTS (
//     SELECT 1 FROM eth_tx_attempts
// 	JOIN eth_txes ON eth_txes.id = eth_tx_attempts.eth_tx_id
// 	WHERE eth_txes.from_address = $1
// 		AND eth_txes.state = 'unconfirmed'
// 		AND eth_tx_attempts.state = 'insufficient_eth'
// );
// `, t.fromAddress, toAddress, payload, value, t.gasLimit)
// 	if err != nil {
// 		return errors.Wrap(err, "transmitter failed to insert eth_tx")
// 	}

// 	rowsAffected, err := res.RowsAffected()
// 	if err != nil {
// 		return errors.Wrap(err, "transmitter failed to get RowsAffected on eth_tx insert")
// 	}
// 	if rowsAffected == 0 {
// 		err := errors.Errorf("Skipped OCR transmission because wallet is out of eth: %s", t.fromAddress.Hex())
// 		logger.Warnw(err.Error(),
// 			"fromAddress", t.fromAddress,
// 			"toAddress", toAddress,
// 			"payload", "0x"+hex.EncodeToString(payload),
// 			"value", value,
// 			"gasLimit", t.gasLimit,
// 		)
// 		return err
// 	}
// 	return nil
// }

// func (t *transmitter) FromAddress() gethCommon.Address {
// 	return t.fromAddress
// }

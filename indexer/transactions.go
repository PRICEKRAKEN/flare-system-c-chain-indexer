package indexer

import (
	"context"
	"encoding/hex"
	"flare-ftso-indexer/config"
	"flare-ftso-indexer/database"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/cenkalti/backoff/v4"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
)

type TransactionsBatch struct {
	Transactions []*types.Transaction
	toBlock      []*types.Block
	toIndex      []uint64
	toReceipt    []*types.Receipt
	toPolicy     []transactionsPolicy
	mu           sync.RWMutex
}

func (tb *TransactionsBatch) Add(
	tx *types.Transaction,
	block *types.Block,
	index uint64,
	receipt *types.Receipt,
	policy transactionsPolicy,
) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.Transactions = append(tb.Transactions, tx)
	tb.toBlock = append(tb.toBlock, block)
	tb.toIndex = append(tb.toIndex, index)
	tb.toReceipt = append(tb.toReceipt, receipt)
	tb.toPolicy = append(tb.toPolicy, policy)
}

func countReceipts(txs *TransactionsBatch) int {
	i := 0
	for _, e := range txs.toReceipt {
		if e != nil {
			i++
		}
	}

	return i
}

func (ci *BlockIndexer) getTransactionsReceipt(
	ctx context.Context, transactionBatch *TransactionsBatch, start, stop int,
) error {
	bOff := backoff.NewExponentialBackOff()
	bOff.MaxElapsedTime = config.BackoffMaxElapsedTime

	for i := start; i < stop; i++ {
		transactionBatch.mu.RLock()
		tx := *transactionBatch.Transactions[i]
		policy := transactionBatch.toPolicy[i]
		transactionBatch.mu.RUnlock()

		var receipt *types.Receipt

		if policy.status || policy.collectEvents {
			err := backoff.Retry(
				func() (err error) {
					ctx, cancelFunc := context.WithTimeout(ctx, config.DefaultTimeout)
					defer cancelFunc()

					receipt, err = ci.client.TransactionReceipt(ctx, tx.Hash())
					return err
				},
				bOff,
			)

			if err != nil {
				return errors.Wrap(err, "getTransactionsReceipt")
			}
		}

		transactionBatch.mu.Lock()
		transactionBatch.toReceipt[i] = receipt
		transactionBatch.mu.Unlock()
	}

	return nil
}

func (ci *BlockIndexer) processTransactions(transactionBatch *TransactionsBatch) (*DatabaseStructData, error) {
	data := NewDatabaseStructData()

	transactionBatch.mu.RLock()
	defer transactionBatch.mu.RUnlock()

	for i := range transactionBatch.Transactions {
		tx := transactionBatch.Transactions[i]
		block := transactionBatch.toBlock[i]
		receipt := transactionBatch.toReceipt[i]
		txIndex := transactionBatch.toIndex[i]
		policy := transactionBatch.toPolicy[i]

		dbTx, err := buildDBTx(tx, receipt, block, txIndex)
		if err != nil {
			return nil, err
		}

		data.Transactions = append(data.Transactions, dbTx)
		database.TransactionId.Add(1)

		// if it was chosen to get the logs of the transaction we process it
		if receipt != nil && policy.collectEvents {
			for _, log := range receipt.Logs {
				dbLog, err := buildDBLog(dbTx, log, block)
				if err != nil {
					return nil, err
				}

				data.Logs = append(data.Logs, dbLog)

				key := fmt.Sprintf("%s%d", dbLog.TransactionHash, dbLog.LogIndex)
				data.LogHashIndexCheck[key] = true
			}
		}
	}

	return data, nil
}

func buildDBTx(
	tx *types.Transaction, receipt *types.Receipt, block *types.Block, txIndex uint64,
) (*database.Transaction, error) {
	txData := hex.EncodeToString(tx.Data())
	funcSig := txData[:8]

	fromAddress, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx) // todo: this is a bit slow
	if err != nil {
		return nil, errors.Wrap(err, "types.Sender")
	}

	status := uint64(2)
	if receipt != nil {
		status = receipt.Status
	}

	base := database.BaseEntity{ID: database.TransactionId.Load()}
	return &database.Transaction{
		BaseEntity:       base,
		Hash:             tx.Hash().Hex()[2:],
		FunctionSig:      funcSig,
		Input:            txData,
		BlockNumber:      block.NumberU64(),
		BlockHash:        block.Hash().Hex()[2:],
		TransactionIndex: txIndex,
		FromAddress:      strings.ToLower(fromAddress.Hex()[2:]),
		ToAddress:        strings.ToLower(tx.To().Hex()[2:]),
		Status:           status,
		Value:            tx.Value().Text(16),
		GasPrice:         tx.GasPrice().String(),
		Gas:              tx.Gas(),
		Timestamp:        block.Time(),
	}, nil
}

func buildDBLog(dbTx *database.Transaction, log *types.Log, block *types.Block) (*database.Log, error) {
	if blockNum := block.Number(); blockNum.Cmp(new(big.Int).SetUint64(log.BlockNumber)) != 0 {
		return nil, errors.Errorf("block number mismatch %s != %d", blockNum, log.BlockNumber)
	}

	var topics [numTopics]string

	for j := 0; j < numTopics; j++ {
		if len(log.Topics) > j {
			topics[j] = log.Topics[j].Hex()[2:]
		} else {
			topics[j] = nullTopic
		}
	}

	return &database.Log{
		TransactionID:   dbTx.ID,
		Address:         log.Address.Hex()[2:],
		Data:            hex.EncodeToString(log.Data),
		Topic0:          topics[0],
		Topic1:          topics[1],
		Topic2:          topics[2],
		Topic3:          topics[3],
		TransactionHash: log.TxHash.Hex()[2:],
		LogIndex:        uint64(log.Index),
		Timestamp:       block.Time(),
		BlockNumber:     log.BlockNumber,
	}, nil
}

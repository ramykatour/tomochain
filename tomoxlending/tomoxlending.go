package tomoxlending

import (
	"errors"
	"fmt"
	"github.com/tomochain/tomochain/consensus"
	"github.com/tomochain/tomochain/core/types"
	"github.com/tomochain/tomochain/p2p"
	"github.com/tomochain/tomochain/tomox"
	"github.com/tomochain/tomochain/tomox/tradingstate"
	"github.com/tomochain/tomochain/tomoxDAO"
	"github.com/tomochain/tomochain/tomoxlending/lendingstate"
	"gopkg.in/karalabe/cookiejar.v2/collections/prque"
	"math/big"
	"strconv"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/tomochain/tomochain/common"
	"github.com/tomochain/tomochain/core/state"
	"github.com/tomochain/tomochain/log"
	"github.com/tomochain/tomochain/rpc"
)

const (
	ProtocolName       = "tomoxlending"
	ProtocolVersion    = uint64(1)
	ProtocolVersionStr = "1.0"
	defaultCacheLimit  = 1024
)

var (
	ErrNonceTooHigh = errors.New("nonce too high")
	ErrNonceTooLow  = errors.New("nonce too low")
)

type Lending struct {
	leveldb    tomoxDAO.TomoXDAO
	mongodb    tomoxDAO.TomoXDAO
	Triegc     *prque.Prque          // Priority queue mapping block numbers to tries to gc
	StateCache lendingstate.Database // State database to reuse between imports (contains state cache)    *lendingstate.TradingStateDB

	orderNonce map[common.Address]*big.Int

	tomox              *tomox.TomoX
	lendingItemHistory *lru.Cache
}

func (l *Lending) Protocols() []p2p.Protocol {
	return []p2p.Protocol{}
}

func (l *Lending) Start(server *p2p.Server) error {
	return nil
}

func (l *Lending) Stop() error {
	return nil
}

func New(tomox *tomox.TomoX) *Lending {
	itemCacheLimit, _ := lru.New(defaultCacheLimit)
	lending := &Lending{
		orderNonce:         make(map[common.Address]*big.Int),
		Triegc:             prque.New(),
		lendingItemHistory: itemCacheLimit,
	}

	lending.leveldb = tomox.GetLevelDB()

	if tomox.IsSDKNode() { // this is an add-on DBEngine for SDK nodes
		lending.mongodb = tomox.GetMongoDB()
	}

	lending.StateCache = lendingstate.NewDatabase(lending.leveldb)
	return lending
}

func (l *Lending) GetLevelDB() tomoxDAO.TomoXDAO {
	return l.leveldb
}

func (l *Lending) GetMongoDB() tomoxDAO.TomoXDAO {
	return l.mongodb
}

// APIs returns the RPC descriptors the Lending implementation offers
func (l *Lending) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: ProtocolName,
			Version:   ProtocolVersionStr,
			Service:   NewPublicTomoXLendingAPI(l),
			Public:    true,
		},
	}
}

// Version returns the Lending sub-protocols version number.
func (l *Lending) Version() uint64 {
	return ProtocolVersion
}

func (l *Lending) ProcessOrderPending(createdBlockTime uint64,coinbase common.Address, chain consensus.ChainContext, pending map[common.Address]types.OrderTransactions, statedb *state.StateDB, lendingStatedb *lendingstate.LendingStateDB, tradingStateDb tradingstate.TradingStateDB) ([]*lendingstate.LendingItem, map[common.Hash]lendingstate.MatchingResult) {
	lendingItems := []*lendingstate.LendingItem{}
	matchingResults := map[common.Hash]lendingstate.MatchingResult{}

	txs := types.NewOrderTransactionByNonce(types.OrderTxSigner{}, pending)
	for {
		tx := txs.Peek()
		if tx == nil {
			break
		}
		log.Debug("ProcessOrderPending start", "len", len(pending))
		log.Debug("Get pending orders to process", "address", tx.UserAddress(), "nonce", tx.Nonce())
		V, R, S := tx.Signature()

		bigstr := V.String()
		n, e := strconv.ParseInt(bigstr, 10, 8)
		if e != nil {
			continue
		}

		order := &lendingstate.LendingItem{
			Nonce:           big.NewInt(int64(tx.Nonce())),
			Quantity:        tx.Quantity(),
			Interest:        tx.Price(),
			Relayer:         tx.ExchangeAddress(),
			UserAddress:     tx.UserAddress(),
			LendingToken:    tx.BaseToken(),
			CollateralToken: tx.QuoteToken(),
			Status:          tx.Status(),
			Side:            tx.Side(),
			Type:            tx.Type(),
			Hash:            tx.OrderHash(),
			LendingId:       tx.OrderID(),
			Signature: &lendingstate.Signature{
				V: byte(n),
				R: common.BigToHash(R),
				S: common.BigToHash(S),
			},
		}
		cancel := false
		if order.Status == lendingstate.LendingStatusCancelled {
			cancel = true
		}

		log.Info("Process order pending", "orderPending", order, "LendingToken", order.LendingToken.Hex(), "CollateralToken", order.CollateralToken)
		originalOrder := &lendingstate.LendingItem{}
		*originalOrder = *order
		originalOrder.Quantity = lendingstate.CloneBigInt(order.Quantity)

		if cancel {
			order.Status = lendingstate.LendingStatusCancelled
		}

		_, newRejectedOrders, err := l.CommitOrder(createdBlockTime,coinbase, chain, statedb, lendingStatedb, tradingStateDb, lendingstate.GetLendingOrderBookHash(order.LendingToken, order.Term), order)

		for _, reject := range newRejectedOrders {
			log.Debug("Reject order", "reject", *reject)
		}

		switch err {
		case ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Debug("Skipping order with low nonce", "sender", tx.UserAddress(), "nonce", tx.Nonce())
			txs.Shift()
			continue

		case ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Debug("Skipping order account with high nonce", "sender", tx.UserAddress(), "nonce", tx.Nonce())
			txs.Pop()
			continue

		case nil:
			// everything ok
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
			continue
		}

		// orderID has been updated
		originalOrder.LendingId = order.LendingId
		lendingItems = append(lendingItems, originalOrder)
		matchingResults[order.Hash] = lendingstate.MatchingResult{
			//Trades:  newTrades,
			Rejects: newRejectedOrders,
		}
	}
	return lendingItems, matchingResults
}

// there are 3 tasks need to complete (for SDK nodes) after matching
// 1. Put takerLendingItem to database
// 2.a Update status, filledAmount of makerLendingItem
// 2.b. Put lendingTrade to database
// 3. Update status of rejected items
func (l *Lending) SyncDataToSDKNode(takerLendingItem *lendingstate.LendingItem, txHash common.Hash, txMatchTime time.Time, trades []*lendingstate.LendingTrade, rejectedItems []*lendingstate.LendingItem, dirtyOrderCount *uint64) error {
	var (
		// originTakerLendingItem: item getting from database
		originTakerLendingItem, updatedTakerLendingItem *lendingstate.LendingItem
		makerDirtyHashes                                []string
		makerDirtyFilledAmount                          map[string]*big.Int
		err                                             error
	)
	db := l.GetMongoDB()
	sc := db.InitLendingBulk()
	defer sc.Close()
	// 1. put processed takerLendingItem to database
	lastState := lendingstate.LendingItemHistoryItem{}
	// Typically, takerItem has never existed in database
	// except cancel case: in this case, item existed in database with status = OPEN, then use send another lendingItem to cancel it
	val, err := db.GetObject(takerLendingItem.Hash, &lendingstate.LendingItem{})
	if err == nil && val != nil {
		originTakerLendingItem = val.(*lendingstate.LendingItem)
		lastState = lendingstate.LendingItemHistoryItem{
			TxHash:       originTakerLendingItem.TxHash,
			FilledAmount: lendingstate.CloneBigInt(originTakerLendingItem.FilledAmount),
			Status:       originTakerLendingItem.Status,
			UpdatedAt:    originTakerLendingItem.UpdatedAt,
		}
	}
	if originTakerLendingItem != nil {
		updatedTakerLendingItem = originTakerLendingItem
	} else {
		updatedTakerLendingItem = takerLendingItem
	}

	if takerLendingItem.Status != lendingstate.LendingStatusCancelled {
		updatedTakerLendingItem.Status = lendingstate.LendingStatusOpen
	} else {
		updatedTakerLendingItem.Status = lendingstate.LendingStatusCancelled
	}
	updatedTakerLendingItem.TxHash = txHash
	if updatedTakerLendingItem.CreatedAt.IsZero() {
		updatedTakerLendingItem.CreatedAt = txMatchTime
	}
	if txMatchTime.Before(updatedTakerLendingItem.UpdatedAt) || (txMatchTime.Equal(updatedTakerLendingItem.UpdatedAt) && *dirtyOrderCount == 0) {
		log.Debug("Ignore old lendingItem/lendingTrades taker", "txHash", txHash.Hex(), "txTime", txMatchTime.UnixNano(), "updatedAt", updatedTakerLendingItem.UpdatedAt.UnixNano())
		return nil
	}
	*dirtyOrderCount++

	l.UpdateLendingItemCache(updatedTakerLendingItem.LendingToken, updatedTakerLendingItem.CollateralToken, updatedTakerLendingItem.Hash, txHash, lastState)
	updatedTakerLendingItem.UpdatedAt = txMatchTime

	// 2. put trades to database and update status
	log.Debug("Got lendingTrades", "number", len(trades), "txhash", txHash.Hex())
	makerDirtyFilledAmount = make(map[string]*big.Int)
	for _, tradeRecord := range trades {
		// 2.a. put to trades

		if tradeRecord.CreatedAt.IsZero() {
			tradeRecord.CreatedAt = txMatchTime
		}
		tradeRecord.UpdatedAt = txMatchTime

		log.Debug("LendingTrade history ", "Term", tradeRecord.Term, "amount", tradeRecord.Amount, "Interest", tradeRecord.Interest,
			"borrower", tradeRecord.Borrower.Hex(), "investor", tradeRecord.Investor.Hex(), "TakerOrderHash", tradeRecord.TakerOrderHash.Hex(), "MakerOrderHash", tradeRecord.MakerOrderHash.Hex(),
			"borrowing", tradeRecord.BorrowingFee.String(), "investingFee", tradeRecord.InvestingFee.String())
		if err := db.PutObject(tradeRecord.Hash, tradeRecord); err != nil {
			return fmt.Errorf("SDKNode: failed to store lendingTrade %s", err.Error())
		}

		// 2.b. update status and filledAmount
		filledAmount := tradeRecord.Amount
		// maker dirty order
		makerFilledAmount := big.NewInt(0)
		if amount, ok := makerDirtyFilledAmount[tradeRecord.MakerOrderHash.Hex()]; ok {
			makerFilledAmount = lendingstate.CloneBigInt(amount)
		}
		makerFilledAmount.Add(makerFilledAmount, filledAmount)
		makerDirtyFilledAmount[tradeRecord.MakerOrderHash.Hex()] = makerFilledAmount
		makerDirtyHashes = append(makerDirtyHashes, tradeRecord.MakerOrderHash.Hex())

		//updatedTakerOrder = l.updateMatchedOrder(updatedTakerOrder, filledAmount, txMatchTime, txHash)
		//  update filledAmount, status of takerOrder
		updatedTakerLendingItem.FilledAmount.Add(updatedTakerLendingItem.FilledAmount, filledAmount)
		if updatedTakerLendingItem.FilledAmount.Cmp(updatedTakerLendingItem.Quantity) < 0 && updatedTakerLendingItem.Type == lendingstate.Limit {
			updatedTakerLendingItem.Status = lendingstate.LendingStatusPartialFilled
		} else {
			updatedTakerLendingItem.Status = lendingstate.LendingStatusFilled
		}
	}

	// update status for Market orders
	if updatedTakerLendingItem.Type == lendingstate.Market {
		if updatedTakerLendingItem.FilledAmount.Cmp(big.NewInt(0)) > 0 {
			updatedTakerLendingItem.Status = lendingstate.LendingStatusFilled
		} else {
			updatedTakerLendingItem.Status = lendingstate.LendingStatusReject
		}
	}

	log.Debug("PutObject processed takerLendingItem",
		"term", updatedTakerLendingItem.Term, "userAddr", updatedTakerLendingItem.UserAddress.Hex(), "side", updatedTakerLendingItem.Side,
		"Interest", updatedTakerLendingItem.Interest, "quantity", updatedTakerLendingItem.Quantity, "filledAmount", updatedTakerLendingItem.FilledAmount, "status", updatedTakerLendingItem.Status,
		"hash", updatedTakerLendingItem.Hash.Hex(), "txHash", updatedTakerLendingItem.TxHash.Hex())
	if err := db.PutObject(updatedTakerLendingItem.Hash, updatedTakerLendingItem); err != nil {
		return fmt.Errorf("SDKNode: failed to put processed takerOrder. Hash: %s Error: %s", updatedTakerLendingItem.Hash.Hex(), err.Error())
	}
	makerItems := db.GetListLendingItemByHashes(makerDirtyHashes)
	log.Debug("Maker dirty lendingItem", "len", len(makerItems), "txhash", txHash.Hex())
	for _, m := range makerItems {
		if txMatchTime.Before(m.UpdatedAt) {
			log.Debug("Ignore old lendingItem/lendingTrades maker", "txHash", txHash.Hex(), "txTime", txMatchTime.UnixNano(), "updatedAt", m.UpdatedAt.UnixNano())
			continue
		}
		lastState = lendingstate.LendingItemHistoryItem{
			TxHash:       m.TxHash,
			FilledAmount: lendingstate.CloneBigInt(m.FilledAmount),
			Status:       m.Status,
			UpdatedAt:    m.UpdatedAt,
		}
		l.UpdateLendingItemCache(m.LendingToken, m.CollateralToken, m.Hash, txHash, lastState)
		m.TxHash = txHash
		m.UpdatedAt = txMatchTime
		m.FilledAmount.Add(m.FilledAmount, makerDirtyFilledAmount[m.Hash.Hex()])
		if m.FilledAmount.Cmp(m.Quantity) < 0 {
			m.Status = lendingstate.LendingStatusPartialFilled
		} else {
			m.Status = lendingstate.LendingStatusFilled
		}
		log.Debug("PutObject processed makerLendingItem",
			"term", m.Term, "userAddr", m.UserAddress.Hex(), "side", m.Side,
			"Interest", m.Interest, "quantity", m.Quantity, "filledAmount", m.FilledAmount, "status", m.Status,
			"hash", m.Hash.Hex(), "txHash", m.TxHash.Hex())
		if err := db.PutObject(m.Hash, m); err != nil {
			return fmt.Errorf("SDKNode: failed to put processed makerOrder. Hash: %s Error: %s", m.Hash.Hex(), err.Error())
		}
	}

	// 3. put rejected orders to leveldb and update status REJECTED
	log.Debug("Got rejected lendingItems", "number", len(rejectedItems), "rejectedLendingItems", rejectedItems)

	if len(rejectedItems) > 0 {
		var rejectedHashes []string
		// updateRejectedOrders
		for _, r := range rejectedItems {
			rejectedHashes = append(rejectedHashes, r.Hash.Hex())
			if updatedTakerLendingItem.Hash == r.Hash && !txMatchTime.Before(r.UpdatedAt) {
				// cache r history for handling reorg
				historyRecord := lendingstate.LendingItemHistoryItem{
					TxHash:       updatedTakerLendingItem.TxHash,
					FilledAmount: lendingstate.CloneBigInt(updatedTakerLendingItem.FilledAmount),
					Status:       updatedTakerLendingItem.Status,
					UpdatedAt:    updatedTakerLendingItem.UpdatedAt,
				}
				l.UpdateLendingItemCache(updatedTakerLendingItem.LendingToken, updatedTakerLendingItem.CollateralToken, updatedTakerLendingItem.Hash, txHash, historyRecord)

				updatedTakerLendingItem.Status = lendingstate.LendingStatusReject
				updatedTakerLendingItem.TxHash = txHash
				updatedTakerLendingItem.UpdatedAt = txMatchTime
				if err := db.PutObject(updatedTakerLendingItem.Hash, updatedTakerLendingItem); err != nil {
					return fmt.Errorf("SDKNode: failed to reject takerOrder. Hash: %s Error: %s", updatedTakerLendingItem.Hash.Hex(), err.Error())
				}
			}
		}
		dirtyRejectedItems := db.GetListLendingItemByHashes(rejectedHashes)
		for _, r := range dirtyRejectedItems {
			if txMatchTime.Before(r.UpdatedAt) {
				log.Debug("Ignore old orders/trades reject", "txHash", txHash.Hex(), "txTime", txMatchTime.UnixNano(), "updatedAt", updatedTakerLendingItem.UpdatedAt.UnixNano())
				continue
			}
			// cache lendingItem for handling reorg
			historyRecord := lendingstate.LendingItemHistoryItem{
				TxHash:       r.TxHash,
				FilledAmount: lendingstate.CloneBigInt(r.FilledAmount),
				Status:       r.Status,
				UpdatedAt:    r.UpdatedAt,
			}
			l.UpdateLendingItemCache(r.LendingToken, r.CollateralToken, r.Hash, txHash, historyRecord)
			dirtyFilledAmount, ok := makerDirtyFilledAmount[r.Hash.Hex()]
			if ok && dirtyFilledAmount != nil {
				r.FilledAmount.Add(r.FilledAmount, dirtyFilledAmount)
			}
			r.Status = lendingstate.LendingStatusReject
			r.TxHash = txHash
			r.UpdatedAt = txMatchTime
			if err = db.PutObject(r.Hash, r); err != nil {
				return fmt.Errorf("SDKNode: failed to update rejectedOder to sdkNode %s", err.Error())
			}
		}
	}

	if err := db.CommitLendingBulk(); err != nil {
		return fmt.Errorf("SDKNode fail to commit bulk update lendingItem/lendingTrades at txhash %s . Error: %s", txHash.Hex(), err.Error())
	}
	return nil
}
func (l *Lending) GetLendingState(block *types.Block) (*lendingstate.LendingStateDB, error) {
	root, err := l.GetLendingStateRoot(block)
	if err != nil {
		return nil, err
	}
	if l.StateCache == nil {
		return nil, errors.New("Not initialized tomox")
	}
	return lendingstate.New(root, l.StateCache)
}

func (l *Lending) GetStateCache() lendingstate.Database {
	return l.StateCache
}

func (l *Lending) GetTriegc() *prque.Prque {
	return l.Triegc
}

func (l *Lending) GetLendingStateRoot(block *types.Block) (common.Hash, error) {
	for _, tx := range block.Transactions() {
		if tx.To() != nil && tx.To().Hex() == common.TomoXStateAddr {
			if len(tx.Data()) > 32 {
				return common.BytesToHash(tx.Data()[32:]), nil
			}
		}
	}
	return lendingstate.EmptyRoot, nil
}

func (l *Lending) UpdateLendingItemCache(LendingToken, CollateralToken common.Address, hash common.Hash, txhash common.Hash, lastState lendingstate.LendingItemHistoryItem) {
	var lendingCacheAtTxHash map[common.Hash]lendingstate.LendingItemHistoryItem
	c, ok := l.lendingItemHistory.Get(txhash)
	if !ok || c == nil {
		lendingCacheAtTxHash = make(map[common.Hash]lendingstate.LendingItemHistoryItem)
	} else {
		lendingCacheAtTxHash = c.(map[common.Hash]lendingstate.LendingItemHistoryItem)
	}
	orderKey := lendingstate.GetLendingItemHistoryKey(LendingToken, CollateralToken, hash)
	_, ok = lendingCacheAtTxHash[orderKey]
	if !ok {
		lendingCacheAtTxHash[orderKey] = lastState
	}
	l.lendingItemHistory.Add(txhash, lendingCacheAtTxHash)
}

func (l *Lending) RollbackLendingItems(txhash common.Hash) {
	db := l.GetMongoDB()
	defer l.lendingItemHistory.Remove(txhash)

	for _, item := range db.GetLendingItemByTxHash(txhash) {
		c, ok := l.lendingItemHistory.Get(txhash)
		log.Debug("tomoxlending reorg: rollback lendingItem", "txhash", txhash.Hex(), "item", lendingstate.ToJSON(item), "lendingItemHistory", c)
		if !ok {
			log.Debug("tomoxlending reorg: remove item due to no lendingItemHistory", "item", lendingstate.ToJSON(item))
			if err := db.DeleteObject(item.Hash, &lendingstate.LendingItem{}); err != nil {
				log.Error("SDKNode: failed to remove reorg lendingItem", "err", err.Error(), "item", lendingstate.ToJSON(item))
			}
			continue
		}
		cacheAtTxHash := c.(map[common.Hash]lendingstate.LendingItemHistoryItem)
		lendingItemHistory, _ := cacheAtTxHash[lendingstate.GetLendingItemHistoryKey(item.LendingToken, item.CollateralToken, item.Hash)]
		if (lendingItemHistory == lendingstate.LendingItemHistoryItem{}) {
			log.Debug("tomoxlending reorg: remove item due to empty lendingItemHistory", "item", lendingstate.ToJSON(item))
			if err := db.DeleteObject(item.Hash, &lendingstate.LendingItem{}); err != nil {
				log.Error("SDKNode: failed to remove reorg lendingItem", "err", err.Error(), "item", lendingstate.ToJSON(item))
			}
			continue
		}
		item.TxHash = lendingItemHistory.TxHash
		item.Status = lendingItemHistory.Status
		item.FilledAmount = lendingstate.CloneBigInt(lendingItemHistory.FilledAmount)
		item.UpdatedAt = lendingItemHistory.UpdatedAt
		log.Debug("tomoxlending reorg: update item to the last lendingItemHistory", "item", lendingstate.ToJSON(item), "lendingItemHistory", lendingItemHistory)
		if err := db.PutObject(item.Hash, item); err != nil {
			log.Error("SDKNode: failed to update reorg item", "err", err.Error(), "item", lendingstate.ToJSON(item))
		}
	}
	log.Debug("tomoxlending reorg: DeleteLendingTradeByTxHash", "txhash", txhash.Hex())
	db.DeleteLendingTradeByTxHash(txhash)

}

func (l *Lending) ProcessLiquidationData(time *big.Int, statedb *state.StateDB, tradingState *tradingstate.TradingStateDB, lendingState *lendingstate.LendingStateDB) {
	// process liquidation price lending
	allPairs, err := tradingstate.GetAllTradingPairs(statedb)
	if err != nil {
		if err != nil {
			log.Error("Fail when get all trading pairs", "error", err)
			return
		}
	}
	for orderbook, _ := range allPairs {
		liquidationPrice := tradingState.GetMediumPriceLastEpoch(orderbook)
		lowestPrice, liquidationData := tradingState.GetLowestLiquidationPriceData(orderbook, liquidationPrice)
		for lowestPrice.Sign() > 0 && lowestPrice.Cmp(liquidationPrice) < 0 {
			for lendingBook, tradingIds := range liquidationData {
				for _, tradingIdHash := range tradingIds {
					tradingId := new(big.Int).SetBytes(tradingIdHash.Bytes()).Uint64()
					// process liquidation price

					// remove tradingId
					tradingState.RemoveLiquidationPrice(orderbook, lowestPrice, lendingBook, tradingId)
				}
			}
			lowestPrice, liquidationData = tradingState.GetLowestLiquidationPriceData(orderbook, liquidationPrice)
		}
	}

	// get All
	allLendingPairs := lendingstate.GetAllLendingPairs(statedb)
	for lendingBook, _ := range allLendingPairs {
		lowestTime, tradingIds := lendingState.GetLowestLiquidationTime(lendingBook, time)
		for lowestTime.Sign() > 0 && lowestTime.Cmp(time) < 0 {
			for _, tradingId := range tradingIds {
				//process liquidation time

				// remove trading Id
				lendingState.RemoveLiquidationData(lendingBook, lowestTime.Uint64(), tradingId)
			}
		}
	}
}

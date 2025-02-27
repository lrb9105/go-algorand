// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

// Package node is the Algorand node itself, with functions exposed to the frontend
package node

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/agreement/gossip"
	"github.com/algorand/go-algorand/catchup"
	"github.com/algorand/go-algorand/compactcert"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/account"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/committee"
	"github.com/algorand/go-algorand/data/pools"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/verify"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/network"
	"github.com/algorand/go-algorand/network/messagetracer"
	"github.com/algorand/go-algorand/node/indexer"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/rpcs"
	"github.com/algorand/go-algorand/util/db"
	"github.com/algorand/go-algorand/util/execpool"
	"github.com/algorand/go-algorand/util/metrics"
	"github.com/algorand/go-algorand/util/timers"
	"github.com/algorand/go-deadlock"
	uuid "github.com/satori/go.uuid"
)

// StatusReport represents the current basic status of the node
type StatusReport struct {
	LastRound                          basics.Round
	LastVersion                        protocol.ConsensusVersion
	NextVersion                        protocol.ConsensusVersion
	NextVersionRound                   basics.Round
	NextVersionSupported               bool
	LastRoundTimestamp                 time.Time
	SynchronizingTime                  time.Duration
	CatchupTime                        time.Duration
	HasSyncedSinceStartup              bool
	StoppedAtUnsupportedRound          bool
	LastCatchpoint                     string // the last catchpoint hit by the node. This would get updated regardless of whether the node is catching up using catchpoints or not.
	Catchpoint                         string // the catchpoint where we're currently catching up to. If the node isn't in fast catchup mode, it will be empty.
	CatchpointCatchupTotalAccounts     uint64
	CatchpointCatchupProcessedAccounts uint64
	CatchpointCatchupVerifiedAccounts  uint64
	CatchpointCatchupTotalBlocks       uint64
	CatchpointCatchupAcquiredBlocks    uint64
}

// TimeSinceLastRound returns the time since the last block was approved (locally), or 0 if no blocks seen
func (status StatusReport) TimeSinceLastRound() time.Duration {
	if status.LastRoundTimestamp.IsZero() {
		return time.Duration(0)
	}

	return time.Since(status.LastRoundTimestamp)
}

// AlgorandFullNode specifies and implements a full Algorand node.
type AlgorandFullNode struct {
	mu        deadlock.Mutex
	ctx       context.Context
	cancelCtx context.CancelFunc
	config    config.Local

	ledger *data.Ledger
	net    network.GossipNode

	transactionPool *pools.TransactionPool
	txHandler       *data.TxHandler
	accountManager  *data.AccountManager

	agreementService         *agreement.Service
	catchupService           *catchup.Service
	catchpointCatchupService *catchup.CatchpointCatchupService
	blockService             *rpcs.BlockService
	ledgerService            *rpcs.LedgerService
	txPoolSyncerService      *rpcs.TxSyncer

	indexer *indexer.Indexer

	rootDir     string
	genesisID   string
	genesisHash crypto.Digest
	devMode     bool // is this node operates in a developer mode ? ( benign agreement, broadcasting transaction generates a new block )

	log logging.Logger

	// syncStatusMu used for locking lastRoundTimestamp and hasSyncedSinceStartup
	// syncStatusMu added so OnNewBlock wouldn't be blocked by oldKeyDeletionThread during catchup
	syncStatusMu          deadlock.Mutex
	lastRoundTimestamp    time.Time
	hasSyncedSinceStartup bool

	cryptoPool                         execpool.ExecutionPool
	lowPriorityCryptoVerificationPool  execpool.BacklogPool
	highPriorityCryptoVerificationPool execpool.BacklogPool
	catchupBlockAuth                   blockAuthenticatorImpl

	oldKeyDeletionNotify        chan struct{}
	monitoringRoutinesWaitGroup sync.WaitGroup

	tracer messagetracer.MessageTracer

	compactCert *compactcert.Worker
}

// TxnWithStatus represents information about a single transaction,
// in particular, whether it has appeared in some block yet or not,
// and whether it was kicked out of the txpool due to some error.
type TxnWithStatus struct {
	Txn transactions.SignedTxn

	// Zero indicates no confirmation
	ConfirmedRound basics.Round

	// PoolError indicates that the transaction was kicked out of this
	// node's transaction pool (and specifies why that happened).  An
	// empty string indicates the transaction wasn't kicked out of this
	// node's txpool due to an error.
	PoolError string

	// ApplyData is the transaction.ApplyData, if committed.
	ApplyData transactions.ApplyData
}

// MakeFull sets up an Algorand full node
// (i.e., it returns a node that participates in consensus)
func MakeFull(log logging.Logger, rootDir string, cfg config.Local, phonebookAddresses []string, genesis bookkeeping.Genesis) (*AlgorandFullNode, error) {
	node := new(AlgorandFullNode)
	node.rootDir = rootDir
	node.log = log.With("name", cfg.NetAddress)
	node.genesisID = genesis.ID()
	node.genesisHash = crypto.HashObj(genesis)
	node.devMode = genesis.DevMode

	if node.devMode {
		cfg.DisableNetworking = true
	}
	node.config = cfg

	// tie network, block fetcher, and agreement services together
	p2pNode, err := network.NewWebsocketNetwork(node.log, node.config, phonebookAddresses, genesis.ID(), genesis.Network)
	if err != nil {
		log.Errorf("could not create websocket node: %v", err)
		return nil, err
	}
	p2pNode.SetPrioScheme(node)
	node.net = p2pNode

	accountListener := makeTopAccountListener(log)

	// load stored data
	genesisDir := filepath.Join(rootDir, genesis.ID())
	ledgerPathnamePrefix := filepath.Join(genesisDir, config.LedgerFilenamePrefix)

	// create initial ledger, if it doesn't exist
	err = os.Mkdir(genesisDir, 0700)
	if err != nil && !os.IsExist(err) {
		log.Errorf("Unable to create genesis directory: %v", err)
		return nil, err
	}
	var genalloc bookkeeping.GenesisBalances
	genalloc, err = bootstrapData(genesis, log)
	if err != nil {
		log.Errorf("Cannot load genesis allocation: %v", err)
		return nil, err
	}

	node.cryptoPool = execpool.MakePool(node)
	node.lowPriorityCryptoVerificationPool = execpool.MakeBacklog(node.cryptoPool, 2*node.cryptoPool.GetParallelism(), execpool.LowPriority, node)
	node.highPriorityCryptoVerificationPool = execpool.MakeBacklog(node.cryptoPool, 2*node.cryptoPool.GetParallelism(), execpool.HighPriority, node)
	node.ledger, err = data.LoadLedger(node.log, ledgerPathnamePrefix, false, genesis.Proto, genalloc, node.genesisID, node.genesisHash, []ledger.BlockListener{}, cfg)
	if err != nil {
		log.Errorf("Cannot initialize ledger (%s): %v", ledgerPathnamePrefix, err)
		return nil, err
	}

	node.transactionPool = pools.MakeTransactionPool(node.ledger.Ledger, cfg, node.log)

	blockListeners := []ledger.BlockListener{
		node.transactionPool,
		node,
	}

	if node.config.EnableTopAccountsReporting {
		blockListeners = append(blockListeners, &accountListener)
	}
	node.ledger.RegisterBlockListeners(blockListeners)
	node.txHandler = data.MakeTxHandler(node.transactionPool, node.ledger, node.net, node.genesisID, node.genesisHash, node.lowPriorityCryptoVerificationPool)

	// Indexer setup
	if cfg.IsIndexerActive && cfg.Archival {
		node.indexer, err = indexer.MakeIndexer(genesisDir, node.ledger, false)
		if err != nil {
			logging.Base().Errorf("failed to make indexer -  %v", err)
			return nil, err
		}
	}

	node.blockService = rpcs.MakeBlockService(node.log, cfg, node.ledger, p2pNode, node.genesisID)
	node.ledgerService = rpcs.MakeLedgerService(cfg, node.ledger, p2pNode, node.genesisID)
	rpcs.RegisterTxService(node.transactionPool, p2pNode, node.genesisID, cfg.TxPoolSize, cfg.TxSyncServeResponseSize)

	crashPathname := filepath.Join(genesisDir, config.CrashFilename)
	crashAccess, err := db.MakeAccessor(crashPathname, false, false)
	if err != nil {
		log.Errorf("Cannot load crash data: %v", err)
		return nil, err
	}

	blockValidator := blockValidatorImpl{l: node.ledger, verificationPool: node.highPriorityCryptoVerificationPool}
	agreementLedger := makeAgreementLedger(node.ledger, node.net)
	var agreementClock timers.Clock
	if node.devMode {
		agreementClock = timers.MakeFrozenClock()
	} else {
		agreementClock = timers.MakeMonotonicClock(time.Now())
	}
	agreementParameters := agreement.Parameters{
		Logger:         log,
		Accessor:       crashAccess,
		Clock:          agreementClock,
		Local:          node.config,
		Network:        gossip.WrapNetwork(node.net, log),
		Ledger:         agreementLedger,
		BlockFactory:   node,
		BlockValidator: blockValidator,
		KeyManager:     node,
		RandomSource:   node,
		BacklogPool:    node.highPriorityCryptoVerificationPool,
	}
	node.agreementService = agreement.MakeService(agreementParameters)

	node.catchupBlockAuth = blockAuthenticatorImpl{Ledger: node.ledger, AsyncVoteVerifier: agreement.MakeAsyncVoteVerifier(node.lowPriorityCryptoVerificationPool)}
	node.catchupService = catchup.MakeService(node.log, node.config, p2pNode, node.ledger, node.catchupBlockAuth, agreementLedger.UnmatchedPendingCertificates, node.lowPriorityCryptoVerificationPool)
	node.txPoolSyncerService = rpcs.MakeTxSyncer(node.transactionPool, node.net, node.txHandler.SolicitedTxHandler(), time.Duration(cfg.TxSyncIntervalSeconds)*time.Second, time.Duration(cfg.TxSyncTimeoutSeconds)*time.Second, cfg.TxSyncServeResponseSize)

	registry, err := ensureParticipationDB(genesisDir, node.log)
	if err != nil {
		log.Errorf("unable to initialize the participation registry database: %v", err)
		return nil, err
	}
	node.accountManager = data.MakeAccountManager(log, registry)

	err = node.loadParticipationKeys()
	if err != nil {
		log.Errorf("Cannot load participation keys: %v", err)
		return nil, err
	}

	node.oldKeyDeletionNotify = make(chan struct{}, 1)

	catchpointCatchupState, err := node.ledger.GetCatchpointCatchupState(context.Background())
	if err != nil {
		log.Errorf("unable to determine catchpoint catchup state: %v", err)
		return nil, err
	}
	if catchpointCatchupState != ledger.CatchpointCatchupStateInactive {
		node.catchpointCatchupService, err = catchup.MakeResumedCatchpointCatchupService(context.Background(), node, node.log, node.net, node.ledger.Ledger, node.config)
		if err != nil {
			log.Errorf("unable to create catchpoint catchup service: %v", err)
			return nil, err
		}
	}

	node.tracer = messagetracer.NewTracer(log).Init(cfg)
	gossip.SetTrace(agreementParameters.Network, node.tracer)

	compactCertPathname := filepath.Join(genesisDir, config.CompactCertFilename)
	compactCertAccess, err := db.MakeAccessor(compactCertPathname, false, false)
	if err != nil {
		log.Errorf("Cannot load compact cert data: %v", err)
		return nil, err
	}
	node.compactCert = compactcert.NewWorker(compactCertAccess, node.log, node.accountManager, node.ledger.Ledger, node.net, node)

	return node, err
}

func bootstrapData(genesis bookkeeping.Genesis, log logging.Logger) (bookkeeping.GenesisBalances, error) {
	genalloc := make(map[basics.Address]basics.AccountData)
	for _, entry := range genesis.Allocation {
		addr, err := basics.UnmarshalChecksumAddress(entry.Address)
		if err != nil {
			log.Errorf("Cannot parse genesis addr %s: %v", entry.Address, err)
			return bookkeeping.GenesisBalances{}, err
		}

		_, present := genalloc[addr]
		if present {
			err = fmt.Errorf("repeated allocation to %s", entry.Address)
			log.Error(err)
			return bookkeeping.GenesisBalances{}, err
		}

		genalloc[addr] = entry.State
	}

	feeSink, err := basics.UnmarshalChecksumAddress(genesis.FeeSink)
	if err != nil {
		log.Errorf("Cannot parse fee sink addr %s: %v", genesis.FeeSink, err)
		return bookkeeping.GenesisBalances{}, err
	}

	rewardsPool, err := basics.UnmarshalChecksumAddress(genesis.RewardsPool)
	if err != nil {
		log.Errorf("Cannot parse rewards pool addr %s: %v", genesis.RewardsPool, err)
		return bookkeeping.GenesisBalances{}, err
	}

	return bookkeeping.MakeTimestampedGenesisBalances(genalloc, feeSink, rewardsPool, genesis.Timestamp), nil
}

// Config returns a copy of the node's Local configuration
func (node *AlgorandFullNode) Config() config.Local {
	return node.config
}

// Start the node: connect to peers and run the agreement service while obtaining a lock. Doesn't wait for initial sync.
func (node *AlgorandFullNode) Start() {
	node.mu.Lock()
	defer node.mu.Unlock()

	// Set up a context we can use to cancel goroutines on Stop()
	node.ctx, node.cancelCtx = context.WithCancel(context.Background())

	// The start network is being called only after the various services start up.
	// We want to do so in order to let the services register their callbacks with the
	// network package before any connections are being made.
	startNetwork := func() {
		if !node.config.DisableNetworking {
			// start accepting connections
			node.net.Start()
			node.config.NetAddress, _ = node.net.Address()
		}
	}

	if node.catchpointCatchupService != nil {
		startNetwork()
		node.catchpointCatchupService.Start(node.ctx)
	} else {
		node.catchupService.Start()
		node.agreementService.Start()
		node.txPoolSyncerService.Start(node.catchupService.InitialSyncDone)
		node.blockService.Start()
		node.ledgerService.Start()
		node.txHandler.Start()
		node.compactCert.Start()
		startNetwork()
		// start indexer
		if idx, err := node.Indexer(); err == nil {
			err := idx.Start()
			if err != nil {
				node.log.Errorf("indexer failed to start, turning it off - %v", err)
				node.config.IsIndexerActive = false
			} else {
				node.log.Info("Indexer was started successfully")
			}
		} else {
			node.log.Infof("Indexer is not available - %v", err)
		}

		node.startMonitoringRoutines()
	}

}

// startMonitoringRoutines starts the internal monitoring routines used by the node.
func (node *AlgorandFullNode) startMonitoringRoutines() {
	node.monitoringRoutinesWaitGroup.Add(3)

	// PKI TODO: Remove this with #2596
	// Periodically check for new participation keys
	go node.checkForParticipationKeys()

	go node.txPoolGaugeThread()
	// Delete old participation keys
	go node.oldKeyDeletionThread()

	// TODO re-enable with configuration flag post V1
	//go logging.UsageLogThread(node.ctx, node.log, 100*time.Millisecond, nil)
}

// waitMonitoringRoutines waits for all the monitoring routines to exit. Note that
// the node.mu must not be taken, and that the node's context should have been canceled.
func (node *AlgorandFullNode) waitMonitoringRoutines() {
	node.monitoringRoutinesWaitGroup.Wait()
}

// ListeningAddress retrieves the node's current listening address, if any.
// Returns true if currently listening, false otherwise.
func (node *AlgorandFullNode) ListeningAddress() (string, bool) {
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.net.Address()
}

// Stop stops running the node. Once a node is closed, it can never start again.
func (node *AlgorandFullNode) Stop() {
	node.mu.Lock()
	defer func() {
		node.mu.Unlock()
		node.waitMonitoringRoutines()
		// we want to shut down the compactCert last, since the oldKeyDeletionThread might depend on it when making the
		// call to LatestSigsFromThisNode.
		node.compactCert.Shutdown()
		node.compactCert = nil
	}()

	node.net.ClearHandlers()
	if !node.config.DisableNetworking {
		node.net.Stop()
	}
	if node.catchpointCatchupService != nil {
		node.catchpointCatchupService.Stop()
	} else {
		node.txHandler.Stop()
		node.agreementService.Shutdown()
		node.catchupService.Stop()
		node.txPoolSyncerService.Stop()
		node.blockService.Stop()
		node.ledgerService.Stop()
	}
	node.catchupBlockAuth.Quit()
	node.highPriorityCryptoVerificationPool.Shutdown()
	node.lowPriorityCryptoVerificationPool.Shutdown()
	node.cryptoPool.Shutdown()
	node.cancelCtx()
	if node.indexer != nil {
		node.indexer.Shutdown()
	}
}

// note: unlike the other two functions, this accepts a whole filename
func (node *AlgorandFullNode) getExistingPartHandle(filename string) (db.Accessor, error) {
	filename = filepath.Join(node.rootDir, node.genesisID, filename)

	_, err := os.Stat(filename)
	if err == nil {
		return db.MakeErasableAccessor(filename)
	}
	return db.Accessor{}, err
}

// Ledger exposes the node's ledger handle to the algod API code
func (node *AlgorandFullNode) Ledger() *data.Ledger {
	return node.ledger
}

// writeDevmodeBlock generates a new block for a devmode, and write it to the ledger.
func (node *AlgorandFullNode) writeDevmodeBlock() (err error) {
	var vb *ledgercore.ValidatedBlock
	vb, err = node.transactionPool.AssembleDevModeBlock()
	if err != nil || vb == nil {
		return
	}

	// add the newly generated block to the ledger
	err = node.ledger.AddValidatedBlock(*vb, agreement.Certificate{})
	return err
}

// BroadcastSignedTxGroup broadcasts a transaction group that has already been signed.
func (node *AlgorandFullNode) BroadcastSignedTxGroup(txgroup []transactions.SignedTxn) (err error) {
	// in developer mode, we need to take a lock, so that each new transaction group would truly
	// render into a unique block.
	if node.devMode {
		node.mu.Lock()
		defer func() {
			// if we added the transaction successfully to the transaction pool, then
			// attempt to generate a block and write it to the ledger.
			if err == nil {
				err = node.writeDevmodeBlock()
			}
			node.mu.Unlock()
		}()
	}

	lastRound := node.ledger.Latest()
	var b bookkeeping.BlockHeader
	b, err = node.ledger.BlockHdr(lastRound)
	if err != nil {
		node.log.Errorf("could not get block header from last round %v: %v", lastRound, err)
		return err
	}

	_, err = verify.TxnGroup(txgroup, b, node.ledger.VerifiedTransactionCache())
	if err != nil {
		node.log.Warnf("malformed transaction: %v", err)
		return err
	}

	err = node.transactionPool.Remember(txgroup)
	if err != nil {
		node.log.Infof("rejected by local pool: %v - transaction group was %+v", err, txgroup)
		return err
	}

	err = node.ledger.VerifiedTransactionCache().Pin(txgroup)
	if err != nil {
		logging.Base().Infof("unable to pin transaction: %v", err)
	}

	var enc []byte
	var txids []transactions.Txid
	for _, tx := range txgroup {
		enc = append(enc, protocol.Encode(&tx)...)
		txids = append(txids, tx.ID())
	}
	err = node.net.Broadcast(context.TODO(), protocol.TxnTag, enc, false, nil)
	if err != nil {
		node.log.Infof("failure broadcasting transaction to network: %v - transaction group was %+v", err, txgroup)
		return err
	}
	node.log.Infof("Sent signed tx group with IDs %v", txids)
	return nil
}

// ListTxns returns SignedTxns associated with a specific account in a range of Rounds (inclusive).
// TxnWithStatus returns the round in which a particular transaction appeared,
// since that information is not part of the SignedTxn itself.
func (node *AlgorandFullNode) ListTxns(addr basics.Address, minRound basics.Round, maxRound basics.Round) ([]TxnWithStatus, error) {
	result := make([]TxnWithStatus, 0)
	for r := minRound; r <= maxRound; r++ {
		h, err := node.ledger.AddressTxns(addr, r)
		if err != nil {
			return nil, err
		}
		for _, tx := range h {
			result = append(result, TxnWithStatus{
				Txn:            tx.SignedTxn,
				ConfirmedRound: r,
				ApplyData:      tx.ApplyData,
			})
		}
	}
	return result, nil
}

// GetTransaction looks for the required txID within with a specific account within a range of rounds (inclusive) and
// returns the SignedTxn and true iff it finds the transaction.
func (node *AlgorandFullNode) GetTransaction(addr basics.Address, txID transactions.Txid, minRound basics.Round, maxRound basics.Round) (TxnWithStatus, bool) {
	// start with the most recent round, and work backwards:
	// this will abort early if it hits pruned rounds
	if maxRound < minRound {
		return TxnWithStatus{}, false
	}
	r := maxRound
	for {
		h, err := node.ledger.AddressTxns(addr, r)
		if err != nil {
			return TxnWithStatus{}, false
		}
		for _, tx := range h {
			if tx.ID() == txID {
				return TxnWithStatus{
					Txn:            tx.SignedTxn,
					ConfirmedRound: r,
					ApplyData:      tx.ApplyData,
				}, true
			}
		}
		if r == minRound {
			break
		}
		r--
	}
	return TxnWithStatus{}, false
}

// GetPendingTransaction looks for the required txID in the recent ledger
// blocks, in the txpool, and in the txpool's status cache.  It returns
// the SignedTxn (with status information), and a bool to indicate if the
// transaction was found.
func (node *AlgorandFullNode) GetPendingTransaction(txID transactions.Txid) (res TxnWithStatus, found bool) {
	// We need to check both the pool and the ledger's blocks.
	// If the transaction is found in a committed block, that
	// takes precedence.  But we check the pool first, because
	// otherwise there could be a race between the pool and the
	// ledger, where the block wasn't in the ledger at first,
	// but by the time we check the pool, it's not there either
	// because it committed.

	// The default return value is found=false, which is
	// appropriate if the transaction isn't found anywhere.

	// Check if it's in the pool or evicted from the pool.
	tx, txErr, found := node.transactionPool.Lookup(txID)
	if found {
		res = TxnWithStatus{
			Txn:            tx,
			ConfirmedRound: 0,
			PoolError:      txErr,
		}
		found = true

		// Keep looking in the ledger..
	}

	var maxLife basics.Round
	latest := node.ledger.Latest()
	proto, err := node.ledger.ConsensusParams(latest)
	if err == nil {
		maxLife = basics.Round(proto.MaxTxnLife)
	} else {
		node.log.Errorf("node.GetPendingTransaction: cannot get consensus params for latest round %v", latest)
	}
	maxRound := latest
	minRound := maxRound.SubSaturate(maxLife)

	for r := minRound; r <= maxRound; r++ {
		tx, found, err := node.ledger.LookupTxid(txID, r)
		if err != nil || !found {
			continue
		}
		return TxnWithStatus{
			Txn:            tx.SignedTxn,
			ConfirmedRound: r,
			ApplyData:      tx.ApplyData,
		}, true
	}

	// Return whatever we found in the pool (if anything).
	return
}

// Status returns a StatusReport structure reporting our status as Active and with our ledger's LastRound
func (node *AlgorandFullNode) Status() (s StatusReport, err error) {
	node.syncStatusMu.Lock()
	s.LastRoundTimestamp = node.lastRoundTimestamp
	s.HasSyncedSinceStartup = node.hasSyncedSinceStartup
	node.syncStatusMu.Unlock()

	node.mu.Lock()
	defer node.mu.Unlock()
	if node.catchpointCatchupService != nil {
		// we're in catchpoint catchup mode.
		lastBlockHeader := node.catchpointCatchupService.GetLatestBlockHeader()
		s.LastRound = lastBlockHeader.Round
		s.LastVersion = lastBlockHeader.CurrentProtocol
		s.NextVersion, s.NextVersionRound, s.NextVersionSupported = lastBlockHeader.NextVersionInfo()
		s.StoppedAtUnsupportedRound = s.LastRound+1 == s.NextVersionRound && !s.NextVersionSupported

		// for now, I'm leaving this commented out. Once we refactor some of the ledger locking mechanisms, we
		// should be able to make this call work.
		//s.LastCatchpoint = node.ledger.GetLastCatchpointLabel()

		// report back the catchpoint catchup progress statistics
		stats := node.catchpointCatchupService.GetStatistics()
		s.Catchpoint = stats.CatchpointLabel
		s.CatchpointCatchupTotalAccounts = stats.TotalAccounts
		s.CatchpointCatchupProcessedAccounts = stats.ProcessedAccounts
		s.CatchpointCatchupVerifiedAccounts = stats.VerifiedAccounts
		s.CatchpointCatchupTotalBlocks = stats.TotalBlocks
		s.CatchpointCatchupAcquiredBlocks = stats.AcquiredBlocks
		s.CatchupTime = time.Now().Sub(stats.StartTime)
	} else {
		// we're not in catchpoint catchup mode
		var b bookkeeping.BlockHeader
		s.LastRound = node.ledger.Latest()
		b, err = node.ledger.BlockHdr(s.LastRound)
		if err != nil {
			return
		}
		s.LastVersion = b.CurrentProtocol
		s.NextVersion, s.NextVersionRound, s.NextVersionSupported = b.NextVersionInfo()

		s.StoppedAtUnsupportedRound = s.LastRound+1 == s.NextVersionRound && !s.NextVersionSupported
		s.LastCatchpoint = node.ledger.GetLastCatchpointLabel()
		s.SynchronizingTime = node.catchupService.SynchronizingTime()
		s.CatchupTime = node.catchupService.SynchronizingTime()
	}

	return
}

// GenesisID returns the ID of the genesis node.
func (node *AlgorandFullNode) GenesisID() string {
	node.mu.Lock()
	defer node.mu.Unlock()

	return node.genesisID
}

// GenesisHash returns the hash of the genesis configuration.
func (node *AlgorandFullNode) GenesisHash() crypto.Digest {
	node.mu.Lock()
	defer node.mu.Unlock()

	return node.genesisHash
}

// PoolStats returns a PoolStatus structure reporting stats about the transaction pool
func (node *AlgorandFullNode) PoolStats() PoolStats {
	r := node.ledger.Latest()
	last, err := node.ledger.Block(r)
	if err != nil {
		node.log.Warnf("AlgorandFullNode: could not read ledger's last round: %v", err)
		return PoolStats{}
	}

	return PoolStats{
		NumConfirmed:   uint64(len(last.Payset)),
		NumOutstanding: uint64(node.transactionPool.PendingCount()),
		NumExpired:     uint64(node.transactionPool.NumExpired(r)),
	}
}

// SuggestedFee returns the suggested fee per byte recommended to ensure a new transaction is processed in a timely fashion.
// Caller should set fee to max(MinTxnFee, SuggestedFee() * len(encoded SignedTxn))
func (node *AlgorandFullNode) SuggestedFee() basics.MicroAlgos {
	return basics.MicroAlgos{Raw: node.transactionPool.FeePerByte()}
}

// GetPendingTxnsFromPool returns a snapshot of every pending transactions from the node's transaction pool in a slice.
// Transactions are sorted in decreasing order. If no transactions, returns an empty slice.
func (node *AlgorandFullNode) GetPendingTxnsFromPool() ([]transactions.SignedTxn, error) {
	return bookkeeping.SignedTxnGroupsFlatten(node.transactionPool.PendingTxGroups()), nil
}

// ensureParticipationDB opens or creates a participation DB.
func ensureParticipationDB(genesisDir string, log logging.Logger) (account.ParticipationRegistry, error) {
	accessorFile := filepath.Join(genesisDir, config.ParticipationRegistryFilename)
	accessor, err := db.OpenPair(accessorFile, false)
	if err != nil {
		return nil, err
	}
	return account.MakeParticipationRegistry(accessor, log)
}

// Reload participation keys from disk periodically
func (node *AlgorandFullNode) checkForParticipationKeys() {
	defer node.monitoringRoutinesWaitGroup.Done()
	ticker := time.NewTicker(node.config.ParticipationKeysRefreshInterval)
	for {
		select {
		case <-ticker.C:
			err := node.loadParticipationKeys()
			if err != nil {
				node.log.Errorf("Could not refresh participation keys: %v", err)
			}
		case <-node.ctx.Done():
			ticker.Stop()
			return
		}
	}
}

// ListParticipationKeys returns all participation keys currently installed on the node
func (node *AlgorandFullNode) ListParticipationKeys() (partKeys []account.ParticipationRecord, err error) {
	return node.accountManager.Registry().GetAll(), nil
}

// GetParticipationKey retries the information of a participation id from the node
func (node *AlgorandFullNode) GetParticipationKey(partKeyID account.ParticipationID) (account.ParticipationRecord, error) {
	rval := node.accountManager.Registry().Get(partKeyID)

	if rval.IsZero() {
		return account.ParticipationRecord{}, account.ErrParticipationIDNotFound
	}

	return rval, nil
}

// RemoveParticipationKey given a participation id, remove the records from the node
func (node *AlgorandFullNode) RemoveParticipationKey(partKeyID account.ParticipationID) error {

	// Need to remove the file and then remove the entry in the registry
	// Let's first get the recorded information from the registry so we can lookup the file

	partRecord := node.accountManager.Registry().Get(partKeyID)

	if partRecord.IsZero() {
		return account.ErrParticipationIDNotFound
	}

	genID := node.GenesisID()

	outDir := filepath.Join(node.rootDir, genID)

	filename := config.PartKeyFilename(partRecord.ParticipationID.String(), uint64(partRecord.FirstValid), uint64(partRecord.LastValid))
	fullyQualifiedFilename := filepath.Join(outDir, filepath.Base(filename))

	err := node.accountManager.Registry().Delete(partKeyID)
	if err != nil {
		return err
	}

	// PKI TODO: pick a better timeout, this is just something short. This could also be removed if we change
	// POST /v2/participation and DELETE /v2/participation to return "202 OK Accepted" instead of waiting and getting
	// the error message.
	err = node.accountManager.Registry().Flush(500 * time.Millisecond)
	if err != nil {
		return err
	}

	// Only after deleting and flushing do we want to remove the file
	_ = os.Remove(fullyQualifiedFilename)

	return nil
}

// AppendParticipationKeys given a participation id, remove the records from the node
func (node *AlgorandFullNode) AppendParticipationKeys(partKeyID account.ParticipationID, keys account.StateProofKeys) error {
	err := node.accountManager.Registry().AppendKeys(partKeyID, keys)
	if err != nil {
		return err
	}

	// PKI TODO: pick a better timeout, this is just something short. This could also be removed if we change
	// POST /v2/participation and DELETE /v2/participation to return "202 OK Accepted" instead of waiting and getting
	// the error message.
	return node.accountManager.Registry().Flush(500 * time.Millisecond)
}

func createTemporaryParticipationKey(outDir string, partKeyBinary []byte) (string, error) {
	var sb strings.Builder

	// Create a temporary filename with a UUID so that we can call this function twice
	// in a row without worrying about collisions
	sb.WriteString("tempPartKeyBinary.")
	sb.WriteString(uuid.NewV4().String())
	sb.WriteString(".bin")

	tempFile := filepath.Join(outDir, filepath.Base(sb.String()))

	file, err := os.Create(tempFile)

	if err != nil {
		return "", err
	}

	_, err = file.Write(partKeyBinary)

	file.Close()

	if err != nil {
		os.Remove(tempFile)
		return "", err
	}

	return tempFile, nil
}

// InstallParticipationKey Given a participation key binary stream install the participation key.
func (node *AlgorandFullNode) InstallParticipationKey(partKeyBinary []byte) (account.ParticipationID, error) {
	genID := node.GenesisID()

	outDir := filepath.Join(node.rootDir, genID)

	fullyQualifiedTempFile, err := createTemporaryParticipationKey(outDir, partKeyBinary)
	// We need to make sure no tempfile is created/remains if there is an error
	// However, we will eventually rename this file but if we fail in-between
	// this point and the rename we want to ensure that we remove the temporary file
	// After we rename, this will fail anyway since the file will not exist

	// Explicitly ignore the error with a closure
	defer func(name string) {
		_ = os.Remove(name)
	}(fullyQualifiedTempFile)

	if err != nil {
		return account.ParticipationID{}, err
	}

	inputdb, err := db.MakeErasableAccessor(fullyQualifiedTempFile)
	if err != nil {
		return account.ParticipationID{}, err
	}
	defer inputdb.Close()

	partkey, err := account.RestoreParticipation(inputdb)
	if err != nil {
		return account.ParticipationID{}, err
	}
	defer partkey.Close()

	if partkey.Parent == (basics.Address{}) {
		return account.ParticipationID{}, fmt.Errorf("cannot install partkey with missing (zero) parent address")
	}

	// Tell the AccountManager about the Participation (dupes don't matter) so we ignore the return value
	_ = node.accountManager.AddParticipation(partkey)

	// PKI TODO: pick a better timeout, this is just something short. This could also be removed if we change
	// POST /v2/participation and DELETE /v2/participation to return "202 OK Accepted" instead of waiting and getting
	// the error message.
	err = node.accountManager.Registry().Flush(500 * time.Millisecond)
	if err != nil {
		return account.ParticipationID{}, err
	}

	newFilename := config.PartKeyFilename(partkey.ID().String(), uint64(partkey.FirstValid), uint64(partkey.LastValid))
	newFullyQualifiedFilename := filepath.Join(outDir, filepath.Base(newFilename))

	err = os.Rename(fullyQualifiedTempFile, newFullyQualifiedFilename)

	if err != nil {
		return account.ParticipationID{}, nil
	}

	return partkey.ID(), nil
}

func (node *AlgorandFullNode) loadParticipationKeys() error {
	// Generate a list of all potential participation key files
	genesisDir := filepath.Join(node.rootDir, node.genesisID)
	files, err := ioutil.ReadDir(genesisDir)
	if err != nil {
		return fmt.Errorf("AlgorandFullNode.loadPartitipationKeys: could not read directory %v: %v", genesisDir, err)
	}

	// For each of these files
	for _, info := range files {
		// If it can't be a participation key database, skip it
		if !config.IsPartKeyFilename(info.Name()) {
			continue
		}
		filename := info.Name()

		// Fetch a handle to this database
		handle, err := node.getExistingPartHandle(filename)
		if err != nil {
			if db.IsErrBusy(err) {
				// this is a special case:
				// we might get "database is locked" when we attempt to access a database that is concurrently updating its participation keys.
				// that database is clearly already on the account manager, and doesn't need to be processed through this logic, and therefore
				// we can safely ignore that fail case.
				continue
			}
			return fmt.Errorf("AlgorandFullNode.loadParticipationKeys: cannot load db %v: %v", filename, err)
		}

		// Fetch an account.Participation from the database
		part, err := account.RestoreParticipation(handle)
		if err != nil {
			handle.Close()
			if err == account.ErrUnsupportedSchema {
				node.log.Infof("Loaded participation keys from storage: %s %s", part.Address(), info.Name())
				node.log.Warnf("loadParticipationKeys: not loading unsupported participation key: %s; renaming to *.old", info.Name())
				fullname := filepath.Join(genesisDir, info.Name())
				renamedFileName := filepath.Join(fullname, ".old")
				err = os.Rename(fullname, renamedFileName)
				if err != nil {
					node.log.Warn("loadParticipationKeys: failed to rename unsupported participation key file '%s' to '%s': %v", fullname, renamedFileName, err)
				}
			} else {
				return fmt.Errorf("AlgorandFullNode.loadParticipationKeys: cannot load account at %v: %v", info.Name(), err)
			}
		} else {
			// Tell the AccountManager about the Participation (dupes don't matter)
			added := node.accountManager.AddParticipation(part)
			if added {
				node.log.Infof("Loaded participation keys from storage: %s %s", part.Address(), info.Name())
			} else {
				part.Close()
			}
		}
	}

	return nil
}

var txPoolGuage = metrics.MakeGauge(metrics.MetricName{Name: "algod_tx_pool_count", Description: "current number of available transactions in pool"})

func (node *AlgorandFullNode) txPoolGaugeThread() {
	defer node.monitoringRoutinesWaitGroup.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for true {
		select {
		case <-ticker.C:
			txPoolGuage.Set(float64(node.transactionPool.PendingCount()), nil)
		case <-node.ctx.Done():
			return
		}
	}
}

// IsArchival returns true the node is an archival node, false otherwise
func (node *AlgorandFullNode) IsArchival() bool {
	return node.config.Archival
}

// OnNewBlock implements the BlockListener interface so we're notified after each block is written to the ledger
func (node *AlgorandFullNode) OnNewBlock(block bookkeeping.Block, delta ledgercore.StateDelta) {
	if node.ledger.Latest() > block.Round() {
		return
	}
	node.syncStatusMu.Lock()
	node.lastRoundTimestamp = time.Now()
	node.hasSyncedSinceStartup = true
	node.syncStatusMu.Unlock()

	// Wake up oldKeyDeletionThread(), non-blocking.
	select {
	case node.oldKeyDeletionNotify <- struct{}{}:
	default:
	}
}

// oldKeyDeletionThread keeps deleting old participation keys.
// It runs in a separate thread so that, during catchup, we
// don't have to delete key for each block we received.
func (node *AlgorandFullNode) oldKeyDeletionThread() {
	defer node.monitoringRoutinesWaitGroup.Done()
	for {
		select {
		case <-node.ctx.Done():
			return
		case <-node.oldKeyDeletionNotify:
		}

		r := node.ledger.Latest()

		// We need the latest header to determine the next compact cert
		// round, if any.
		latestHdr, err := node.ledger.BlockHdr(r)
		if err != nil {
			switch err.(type) {
			case ledgercore.ErrNoEntry:
				// No need to warn; expected during catchup.
			default:
				node.log.Warnf("Cannot look up latest block %d for deleting ephemeral keys: %v", r, err)
			}
			continue
		}

		// If compact certs are enabled, we need to determine what signatures
		// we already computed, since we can then delete ephemeral keys that
		// were already used to compute a signature (stored in the compact
		// cert db).
		ccSigs, err := node.compactCert.LatestSigsFromThisNode()
		if err != nil {
			node.log.Warnf("Cannot look up latest compact cert sigs: %v", err)
			continue
		}

		// We need to find the consensus protocol used to agree on block r,
		// since that determines the params used for ephemeral keys in block
		// r.  The params come from agreement.ParamsRound(r), which is r-2.
		hdr, err := node.ledger.BlockHdr(agreement.ParamsRound(r))
		if err != nil {
			switch err.(type) {
			case ledgercore.ErrNoEntry:
				// No need to warn; expected during catchup.
			default:
				node.log.Warnf("Cannot look up params block %d for deleting ephemeral keys: %v", agreement.ParamsRound(r), err)
			}
			continue
		}

		agreementProto := config.Consensus[hdr.CurrentProtocol]

		node.mu.Lock()
		node.accountManager.DeleteOldKeys(latestHdr, ccSigs, agreementProto)
		node.mu.Unlock()

		// PKI TODO: Maybe we don't even need to flush the registry.
		// Persist participation registry metrics.
		node.accountManager.FlushRegistry(2 * time.Second)
	}
}

// Uint64 implements the randomness by calling the crypto library.
func (node *AlgorandFullNode) Uint64() uint64 {
	return crypto.RandUint64()
}

// Indexer returns a pointer to nodes indexer
func (node *AlgorandFullNode) Indexer() (*indexer.Indexer, error) {
	if node.indexer != nil && node.config.IsIndexerActive {
		return node.indexer, nil
	}
	return nil, fmt.Errorf("indexer is not active")
}

// GetTransactionByID gets transaction by ID
// this function is intended to be called externally via the REST api interface.
func (node *AlgorandFullNode) GetTransactionByID(txid transactions.Txid, rnd basics.Round) (TxnWithStatus, error) {
	stx, _, err := node.ledger.LookupTxid(txid, rnd)
	if err != nil {
		return TxnWithStatus{}, err
	}
	return TxnWithStatus{
		Txn:            stx.SignedTxn,
		ConfirmedRound: rnd,
		ApplyData:      stx.ApplyData,
	}, nil
}

// StartCatchup starts the catchpoint mode and attempt to get to the provided catchpoint
// this function is intended to be called externally via the REST api interface.
func (node *AlgorandFullNode) StartCatchup(catchpoint string) error {
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.indexer != nil {
		return fmt.Errorf("catching up using a catchpoint is not supported on indexer-enabled nodes")
	}
	if node.catchpointCatchupService != nil {
		stats := node.catchpointCatchupService.GetStatistics()
		// No need to return an error
		if catchpoint == stats.CatchpointLabel {
			return MakeCatchpointAlreadyInProgressError(catchpoint)
		}
		return MakeCatchpointUnableToStartError(stats.CatchpointLabel, catchpoint)
	}
	var err error
	node.catchpointCatchupService, err = catchup.MakeNewCatchpointCatchupService(catchpoint, node, node.log, node.net, node.ledger.Ledger, node.config)
	if err != nil {
		node.log.Warnf("unable to create catchpoint catchup service : %v", err)
		return err
	}
	node.catchpointCatchupService.Start(node.ctx)
	node.log.Infof("starting catching up toward catchpoint %s", catchpoint)
	return nil
}

// AbortCatchup aborts the given catchpoint
// this function is intended to be called externally via the REST api interface.
func (node *AlgorandFullNode) AbortCatchup(catchpoint string) error {
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.catchpointCatchupService == nil {
		return nil
	}
	stats := node.catchpointCatchupService.GetStatistics()
	if stats.CatchpointLabel != catchpoint {
		return fmt.Errorf("unable to abort catchpoint catchup for '%s' - already catching up '%s'", catchpoint, stats.CatchpointLabel)
	}
	node.catchpointCatchupService.Abort()
	return nil
}

// SetCatchpointCatchupMode change the node's operational mode from catchpoint catchup mode and back, it returns a
// channel which contains the updated node context. This function need to work asyncronisly so that the caller could
// detect and handle the usecase where the node is being shut down while we're switching to/from catchup mode without
// deadlocking on the shared node mutex.
func (node *AlgorandFullNode) SetCatchpointCatchupMode(catchpointCatchupMode bool) (outCtxCh <-chan context.Context) {
	// create a non-buffered channel to return the newly created context. The fact that it's non-buffered here
	// is imporant, as it allows us to syncronize the "receiving" of the new context before canceling of the previous
	// one.
	ctxCh := make(chan context.Context)
	outCtxCh = ctxCh
	go func() {
		node.mu.Lock()
		// check that the node wasn't canceled. If it have been canceled, it means that the node.Stop() was called, in which case
		// we should close the channel.
		if node.ctx.Err() == context.Canceled {
			close(ctxCh)
			node.mu.Unlock()
			return
		}
		if catchpointCatchupMode {
			// stop..
			defer func() {
				node.mu.Unlock()
				node.waitMonitoringRoutines()
			}()
			node.net.ClearHandlers()
			node.txHandler.Stop()
			node.agreementService.Shutdown()
			node.catchupService.Stop()
			node.txPoolSyncerService.Stop()
			node.blockService.Stop()
			node.ledgerService.Stop()

			prevNodeCancelFunc := node.cancelCtx

			// Set up a context we can use to cancel goroutines on Stop()
			node.ctx, node.cancelCtx = context.WithCancel(context.Background())
			ctxCh <- node.ctx

			prevNodeCancelFunc()
			return
		}
		defer node.mu.Unlock()
		// start
		node.transactionPool.Reset()
		node.catchupService.Start()
		node.agreementService.Start()
		node.txPoolSyncerService.Start(node.catchupService.InitialSyncDone)
		node.blockService.Start()
		node.ledgerService.Start()
		node.txHandler.Start()

		// start indexer
		if idx, err := node.Indexer(); err == nil {
			err := idx.Start()
			if err != nil {
				node.log.Errorf("indexer failed to start, turning it off - %v", err)
				node.config.IsIndexerActive = false
			} else {
				node.log.Info("Indexer was started successfully")
			}
		} else {
			node.log.Infof("Indexer is not available - %v", err)
		}

		// Set up a context we can use to cancel goroutines on Stop()
		node.ctx, node.cancelCtx = context.WithCancel(context.Background())

		node.startMonitoringRoutines()

		// at this point, the catchpoint catchup is done ( either successfully or not.. )
		node.catchpointCatchupService = nil

		ctxCh <- node.ctx
	}()
	return

}

// validatedBlock satisfies agreement.ValidatedBlock
type validatedBlock struct {
	vb *ledgercore.ValidatedBlock
}

// WithSeed satisfies the agreement.ValidatedBlock interface.
func (vb validatedBlock) WithSeed(s committee.Seed) agreement.ValidatedBlock {
	lvb := vb.vb.WithSeed(s)
	return validatedBlock{vb: &lvb}
}

// Block satisfies the agreement.ValidatedBlock interface.
func (vb validatedBlock) Block() bookkeeping.Block {
	blk := vb.vb.Block()
	return blk
}

// AssembleBlock implements Ledger.AssembleBlock.
func (node *AlgorandFullNode) AssembleBlock(round basics.Round) (agreement.ValidatedBlock, error) {
	deadline := time.Now().Add(node.config.ProposalAssemblyTime)
	lvb, err := node.transactionPool.AssembleBlock(round, deadline)
	if err != nil {
		if errors.Is(err, pools.ErrStaleBlockAssemblyRequest) {
			// convert specific error to one that would have special handling in the agreement code.
			err = agreement.ErrAssembleBlockRoundStale

			ledgerNextRound := node.ledger.NextRound()
			if ledgerNextRound == round {
				// we've asked for the right round.. and the ledger doesn't think it's stale.
				node.log.Errorf("AlgorandFullNode.AssembleBlock: could not generate a proposal for round %d, ledger and proposal generation are synced: %v", round, err)
			} else if ledgerNextRound < round {
				// from some reason, the ledger is behind the round that we're asking. That shouldn't happen, but error if it does.
				node.log.Errorf("AlgorandFullNode.AssembleBlock: could not generate a proposal for round %d, ledger next round is %d: %v", round, ledgerNextRound, err)
			}
			// the case where ledgerNextRound > round was not implemented here on purpose. This is the "normal case" where the
			// ledger was advancing faster then the agreement by the catchup.
		}
		return nil, err
	}
	return validatedBlock{vb: lvb}, nil
}

// VotingKeys implements the key manager's VotingKeys method, and provides additional validation with the ledger.
// that allows us to load multiple overlapping keys for the same account, and filter these per-round basis.
func (node *AlgorandFullNode) VotingKeys(votingRound, keysRound basics.Round) []account.Participation {
	keys := node.accountManager.Keys(votingRound)

	participations := make([]account.Participation, 0, len(keys))
	accountsData := make(map[basics.Address]basics.OnlineAccountData, len(keys))
	matchingAccountsKeys := make(map[basics.Address]bool)
	mismatchingAccountsKeys := make(map[basics.Address]int)
	const bitMismatchingVotingKey = 1
	const bitMismatchingSelectionKey = 2
	for _, part := range keys {
		acctData, hasAccountData := accountsData[part.Parent]
		if !hasAccountData {
			var err error
			acctData, err = node.ledger.LookupAgreement(keysRound, part.Parent)
			if err != nil {
				node.log.Warnf("node.VotingKeys: Account %v not participating: cannot locate account for round %d : %v", part.Address(), keysRound, err)
				continue
			}
			accountsData[part.Parent] = acctData
		}

		if acctData.VoteID != part.Voting.OneTimeSignatureVerifier {
			mismatchingAccountsKeys[part.Address()] = mismatchingAccountsKeys[part.Address()] | bitMismatchingVotingKey
			continue
		}
		if acctData.SelectionID != part.VRF.PK {
			mismatchingAccountsKeys[part.Address()] = mismatchingAccountsKeys[part.Address()] | bitMismatchingSelectionKey
			continue
		}
		participations = append(participations, part)
		matchingAccountsKeys[part.Address()] = true

		// Make sure the key is registered.
		err := node.accountManager.Registry().Register(part.ID(), votingRound)
		if err != nil {
			node.log.Warnf("Failed to register participation key (%s) with participation registry: %v\n", part.ID(), err)
		}
	}
	// write the warnings per account only if we couldn't find a single valid key for that account.
	for mismatchingAddr, warningFlags := range mismatchingAccountsKeys {
		if matchingAccountsKeys[mismatchingAddr] {
			continue
		}
		if warningFlags&bitMismatchingVotingKey == bitMismatchingVotingKey {
			node.log.Warnf("node.VotingKeys: Account %v not participating on round %d: on chain voting key differ from participation voting key for round %d", mismatchingAddr, votingRound, keysRound)
			continue
		}
		if warningFlags&bitMismatchingSelectionKey == bitMismatchingSelectionKey {
			node.log.Warnf("node.VotingKeys: Account %v not participating on round %d: on chain selection key differ from participation selection key for round %d", mismatchingAddr, votingRound, keysRound)
			continue
		}
	}
	return participations
}

// Record forwards participation record calls to the participation registry.
func (node *AlgorandFullNode) Record(account basics.Address, round basics.Round, participationType account.ParticipationAction) {
	node.accountManager.Record(account, round, participationType)
}

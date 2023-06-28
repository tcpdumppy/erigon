package stagedsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/c2h5oh/datasize"
	"github.com/ledgerwatch/erigon/core/state/temporal"
	"github.com/ledgerwatch/log/v3"
	"github.com/torquem-ch/mdbx-go/mdbx"
	"golang.org/x/sync/errgroup"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"github.com/ledgerwatch/erigon-lib/common/dir"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	kv2 "github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/kv/rawdbv3"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	state2 "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/cmd/state/exec22"
	"github.com/ledgerwatch/erigon/cmd/state/exec3"
	"github.com/ledgerwatch/erigon/common/math"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb/rawdbhelpers"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/ethconfig/estimate"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/turbo/services"
)

var ExecStepsInDB = metrics.NewCounter(`exec_steps_in_db`) //nolint
var ExecRepeats = metrics.NewCounter(`exec_repeats`)       //nolint
var ExecTriggers = metrics.NewCounter(`exec_triggers`)     //nolint

func NewProgress(prevOutputBlockNum, commitThreshold uint64, workersCount int, logPrefix string, logger log.Logger) *Progress {
	return &Progress{prevTime: time.Now(), prevOutputBlockNum: prevOutputBlockNum, commitThreshold: commitThreshold, workersCount: workersCount, logPrefix: logPrefix, logger: logger}
}

type Progress struct {
	prevTime           time.Time
	prevCount          uint64
	prevOutputBlockNum uint64
	prevRepeatCount    uint64
	commitThreshold    uint64

	workersCount int
	logPrefix    string
	logger       log.Logger
}

func (p *Progress) Log(rs *state.StateV3, in *exec22.QueueWithRetry, rws *exec22.ResultsQueue, doneCount, inputBlockNum, outputBlockNum, outTxNum, repeatCount uint64, idxStepsAmountInDB float64) {
	ExecStepsInDB.Set(uint64(idxStepsAmountInDB * 100))
	var m runtime.MemStats
	dbg.ReadMemStats(&m)
	sizeEstimate := rs.SizeEstimate()
	currentTime := time.Now()
	interval := currentTime.Sub(p.prevTime)
	speedTx := float64(doneCount-p.prevCount) / (float64(interval) / float64(time.Second))
	//speedBlock := float64(outputBlockNum-p.prevOutputBlockNum) / (float64(interval) / float64(time.Second))
	var repeatRatio float64
	if doneCount > p.prevCount {
		repeatRatio = 100.0 * float64(repeatCount-p.prevRepeatCount) / float64(doneCount-p.prevCount)
	}
	p.logger.Info(fmt.Sprintf("[%s] Transaction replay", p.logPrefix),
		//"workers", workerCount,
		"blk", outputBlockNum,
		//"blk/s", fmt.Sprintf("%.1f", speedBlock),
		"tx/s", fmt.Sprintf("%.1f", speedTx),
		"pipe", fmt.Sprintf("(%d+%d)->%d/%d->%d/%d", in.NewTasksLen(), in.RetriesLen(), rws.ResultChLen(), rws.ResultChCap(), rws.Len(), rws.Limit()),
		"repeatRatio", fmt.Sprintf("%.2f%%", repeatRatio),
		"workers", p.workersCount,
		"buffer", fmt.Sprintf("%s/%s", common.ByteCount(sizeEstimate), common.ByteCount(p.commitThreshold)),
		"idxStepsInDB", fmt.Sprintf("%.2f", idxStepsAmountInDB),
		//"inBlk", inputBlockNum,
		"step", fmt.Sprintf("%.1f", float64(outTxNum)/float64(ethconfig.HistoryV3AggregationStep)),
		"alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys),
	)
	//var txNums []string
	//for _, t := range rws {
	//	txNums = append(txNums, fmt.Sprintf("%d", t.TxNum))
	//}
	//s := strings.Join(txNums, ",")
	//log.Info(fmt.Sprintf("[%s] Transaction replay queue", logPrefix), "txNums", s)

	p.prevTime = currentTime
	p.prevCount = doneCount
	p.prevOutputBlockNum = outputBlockNum
	p.prevRepeatCount = repeatCount
}

/*
ExecV3 - parallel execution. Has many layers of abstractions - each layer does accumulate
state changes (updates) and can "atomically commit all changes to underlying layer of abstraction"

Layers from top to bottom:
- IntraBlockState - used to exec txs. It does store inside all updates of given txn.
Can understan if txn failed or OutOfGas - then revert all changes.
Each parallel-worker hav own IntraBlockState.
IntraBlockState does commit changes to lower-abstraction-level by method `ibs.MakeWriteSet()`

- StateWriterBufferedV3 - txs which executed by parallel workers can conflict with each-other.
This writer does accumulate updates and then send them to conflict-resolution.
Until conflict-resolution succeed - none of execution updates must pass to lower-abstraction-level.
Object TxTask it's just set of small buffers (readset + writeset) for each transaction.
Write to TxTask happends by code like `txTask.ReadLists = rw.stateReader.ReadSet()`.

- TxTask - objects coming from parallel-workers to conflict-resolution goroutine (ApplyLoop and method ReadsValid).
Flush of data to lower-level-of-abstraction is done by method `agg.ApplyState` (method agg.ApplyHistory exists
only for performance - to reduce time of RwLock on state, but by meaning `ApplyState+ApplyHistory` it's 1 method to
flush changes from TxTask to lower-level-of-abstraction).

- StateV3 - it's all updates which are stored in RAM - all parallel workers can see this updates.
Execution of txs always done on Valid version of state (no partial-updates of state).
Flush of updates to lower-level-of-abstractions done by method `StateV3.Flush`.
On this level-of-abstraction also exists StateReaderV3.
IntraBlockState does call StateReaderV3, and StateReaderV3 call StateV3(in-mem-cache) or DB (RoTx).
WAL - also on this level-of-abstraction - agg.ApplyHistory does write updates from TxTask to WAL.
WAL it's like StateV3 just without reading api (can only write there). WAL flush to disk periodically (doesn't need much RAM).

- RoTx - see everything what committed to DB. Commit is done by rwLoop goroutine.
rwloop does:
  - stop all Workers
  - call StateV3.Flush()
  - commit
  - open new RoTx
  - set new RoTx to all Workers
  - start Workersстартует воркеры

When rwLoop has nothing to do - it does Prune, or flush of WAL to RwTx (agg.rotate+agg.Flush)
*/
func ExecV3(ctx context.Context,
	execStage *StageState, u Unwinder, workerCount int, cfg ExecuteBlockCfg, applyTx kv.RwTx,
	parallel bool, logPrefix string,
	maxBlockNum uint64,
	logger log.Logger,
	initialCycle bool,
) error {
	parallel = false // TODO: e35 doesn't support it yet
	batchSize := cfg.batchSize
	chainDb := cfg.db
	blockReader := cfg.blockReader
	agg, engine := cfg.agg, cfg.engine
	chainConfig, genesis := cfg.chainConfig, cfg.genesis

	defer func() {
		if err := recover(); err != nil {
			log.Error("panic", "err", err)
			debug.PrintStack()
			panic(err)
		}
	}()

	useExternalTx := applyTx != nil
	if !useExternalTx && !parallel {
		var err error
		applyTx, err = chainDb.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer func() { // need callback - because tx may be committed
			applyTx.Rollback()
		}()
		//} else {
		//	if blockSnapshots.Cfg().Enabled {
		//defer blockSnapshots.EnableMadvNormal().DisableReadAhead()
		//}
	}

	var block, stageProgress uint64
	var maxTxNum uint64
	outputTxNum := atomic.Uint64{}
	blockComplete := atomic.Bool{}
	blockComplete.Store(true)

	var inputTxNum uint64
	if execStage.BlockNumber > 0 {
		stageProgress = execStage.BlockNumber
		block = execStage.BlockNumber + 1
	}
	if applyTx != nil {
		agg.SetTx(applyTx)
		if dbg.DiscardHistory() {
			defer agg.DiscardHistory().FinishWrites()
		} else {
			defer agg.StartWrites().FinishWrites()
		}

		var err error
		maxTxNum, err = rawdbv3.TxNums.Max(applyTx, maxBlockNum)
		if err != nil {
			return err
		}
		if block > 0 {
			_outputTxNum, err := rawdbv3.TxNums.Max(applyTx, execStage.BlockNumber)
			if err != nil {
				return err
			}
			outputTxNum.Store(_outputTxNum)
			outputTxNum.Add(1)
			inputTxNum = outputTxNum.Load()
		}
	} else {
		if err := chainDb.View(ctx, func(tx kv.Tx) error {
			var err error
			maxTxNum, err = rawdbv3.TxNums.Max(tx, maxBlockNum)
			if err != nil {
				return err
			}
			if block > 0 {
				_outputTxNum, err := rawdbv3.TxNums.Max(tx, execStage.BlockNumber)
				if err != nil {
					return err
				}
				outputTxNum.Store(_outputTxNum)
				outputTxNum.Add(1)
				inputTxNum = outputTxNum.Load()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	agg.SetTxNum(inputTxNum)

	var outputBlockNum = syncMetrics[stages.Execution]
	inputBlockNum := &atomic.Uint64{}
	var count uint64
	var lock sync.RWMutex

	// MA setio
	doms := cfg.agg.SharedDomains()
	defer cfg.agg.CloseSharedDomains()
	rs := state.NewStateV3(doms, logger)

	//TODO: owner of `resultCh` is main goroutine, but owner of `retryQueue` is applyLoop.
	// Now rwLoop closing both (because applyLoop we completely restart)
	// Maybe need split channels? Maybe don't exit from ApplyLoop? Maybe current way is also ok?

	// input queue
	in := exec22.NewQueueWithRetry(100_000)
	defer in.Close()

	rwsConsumed := make(chan struct{}, 1)
	defer close(rwsConsumed)

	execWorkers, applyWorker, rws, stopWorkers, waitWorkers := exec3.NewWorkersPool(lock.RLocker(), ctx, parallel, chainDb, rs, in, blockReader, chainConfig, genesis, engine, workerCount+1)
	defer stopWorkers()
	applyWorker.DiscardReadList()

	commitThreshold := batchSize.Bytes()
	progress := NewProgress(block, commitThreshold, workerCount, execStage.LogPrefix(), logger)
	logEvery := time.NewTicker(1 * time.Second)
	defer logEvery.Stop()
	pruneEvery := time.NewTicker(2 * time.Second)
	defer pruneEvery.Stop()

	applyLoopWg := sync.WaitGroup{} // to wait for finishing of applyLoop after applyCtx cancel
	defer applyLoopWg.Wait()

	applyLoopInner := func(ctx context.Context) error {
		tx, err := chainDb.BeginRo(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		applyWorker.ResetTx(tx)

		var lastBlockNum uint64

		for outputTxNum.Load() <= maxTxNum {
			if err := rws.Drain(ctx); err != nil {
				return err
			}

			processedTxNum, conflicts, triggers, processedBlockNum, stoppedAtBlockEnd, err := processResultQueue(in, rws, outputTxNum.Load(), rs, agg, tx, rwsConsumed, applyWorker, true, false)
			if err != nil {
				return err
			}

			ExecRepeats.Add(conflicts)
			ExecTriggers.Add(triggers)
			if processedBlockNum > lastBlockNum {
				outputBlockNum.Set(processedBlockNum)
				lastBlockNum = processedBlockNum
			}
			if processedTxNum > 0 {
				outputTxNum.Store(processedTxNum)
				blockComplete.Store(stoppedAtBlockEnd)
			}

		}
		return nil
	}
	applyLoop := func(ctx context.Context, errCh chan error) {
		defer applyLoopWg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				log.Warn("[dbg] apply loop panic", "rec", rec)
			}
			log.Warn("[dbg] apply loop exit")
		}()
		if err := applyLoopInner(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}
	}

	var rwLoopErrCh chan error

	var rwLoopG *errgroup.Group
	if parallel {
		// `rwLoop` lives longer than `applyLoop`
		rwLoop := func(ctx context.Context) error {
			tx, err := chainDb.BeginRw(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback()

			agg.SetTx(tx)
			if dbg.DiscardHistory() {
				defer agg.DiscardHistory().FinishWrites()
			} else {
				defer agg.StartWrites().FinishWrites()
			}

			defer applyLoopWg.Wait()
			applyCtx, cancelApplyCtx := context.WithCancel(ctx)
			defer cancelApplyCtx()
			applyLoopWg.Add(1)
			go applyLoop(applyCtx, rwLoopErrCh)
			for outputTxNum.Load() <= maxTxNum {
				select {
				case <-ctx.Done():
					return ctx.Err()

				case <-logEvery.C:
					stepsInDB := rawdbhelpers.IdxStepsCountV3(tx)
					progress.Log(rs, in, rws, rs.DoneCount(), inputBlockNum.Load(), outputBlockNum.Get(), outputTxNum.Load(), ExecRepeats.Get(), stepsInDB)
					if agg.HasBackgroundFilesBuild() {
						logger.Info(fmt.Sprintf("[%s] Background files build", logPrefix), "progress", agg.BackgroundProgress())
					}
				case <-pruneEvery.C:
					if rs.SizeEstimate() < commitThreshold {
						if agg.CanPrune(tx) {
							if err = agg.Prune(ctx, ethconfig.HistoryV3AggregationStep*10); err != nil { // prune part of retired data, before commit
								return err
							}
						} else {
							if err = agg.Flush(ctx, tx); err != nil {
								return err
							}
						}
						break
					}

					cancelApplyCtx()
					applyLoopWg.Wait()

					var t0, t1, t2, t3, t4 time.Duration
					commitStart := time.Now()
					logger.Info("Committing...", "blockComplete.Load()", blockComplete.Load())
					if err := func() error {
						//Drain results (and process) channel because read sets do not carry over
						for !blockComplete.Load() {
							rws.DrainNonBlocking()
							applyWorker.ResetTx(tx)

							processedTxNum, conflicts, triggers, processedBlockNum, stoppedAtBlockEnd, err := processResultQueue(in, rws, outputTxNum.Load(), rs, agg, tx, nil, applyWorker, false, true)
							if err != nil {
								return err
							}

							ExecRepeats.Add(conflicts)
							ExecTriggers.Add(triggers)
							if processedBlockNum > 0 {
								outputBlockNum.Set(processedBlockNum)
							}
							if processedTxNum > 0 {
								outputTxNum.Store(processedTxNum)
								blockComplete.Store(stoppedAtBlockEnd)
							}
						}
						t0 = time.Since(commitStart)
						lock.Lock() // This is to prevent workers from starting work on any new txTask
						defer lock.Unlock()

						select {
						case rwsConsumed <- struct{}{}:
						default:
						}

						// Drain results channel because read sets do not carry over
						rws.DropResults(func(txTask *exec22.TxTask) {
							rs.ReTry(txTask, in)
						})

						//lastTxNumInDb, _ := rawdbv3.TxNums.Max(tx, outputBlockNum.Get())
						//if lastTxNumInDb != outputTxNum.Load()-1 {
						//	panic(fmt.Sprintf("assert: %d != %d", lastTxNumInDb, outputTxNum.Load()))
						//}

						t1 = time.Since(commitStart)
						tt := time.Now()
						//if err := rs.Flush(ctx, tx, logPrefix, logEvery); err != nil {
						//	return err
						//}
						t2 = time.Since(tt)

						tt = time.Now()
						if err := agg.Flush(ctx, tx); err != nil {
							return err
						}
						t3 = time.Since(tt)

						if err = execStage.Update(tx, outputBlockNum.Get()); err != nil {
							return err
						}

						tx.CollectMetrics()
						tt = time.Now()
						if err = tx.Commit(); err != nil {
							return err
						}
						t4 = time.Since(tt)
						for i := 0; i < len(execWorkers); i++ {
							execWorkers[i].ResetTx(nil)
						}

						return nil
					}(); err != nil {
						return err
					}
					if tx, err = chainDb.BeginRw(ctx); err != nil {
						return err
					}
					defer tx.Rollback()
					agg.SetTx(tx)

					applyCtx, cancelApplyCtx = context.WithCancel(ctx)
					defer cancelApplyCtx()
					applyLoopWg.Add(1)
					go applyLoop(applyCtx, rwLoopErrCh)

					logger.Info("Committed", "time", time.Since(commitStart), "drain", t0, "drain_and_lock", t1, "rs.flush", t2, "agg.flush", t3, "tx.commit", t4)
				}
			}
			//if err = rs.Flush(ctx, tx, logPrefix, logEvery); err != nil {
			//	return err
			//}
			if err = agg.Flush(ctx, tx); err != nil {
				return err
			}
			if err = execStage.Update(tx, outputBlockNum.Get()); err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			return nil
		}

		rwLoopCtx, rwLoopCtxCancel := context.WithCancel(ctx)
		defer rwLoopCtxCancel()
		rwLoopG, rwLoopCtx = errgroup.WithContext(rwLoopCtx)
		defer rwLoopG.Wait()
		rwLoopG.Go(func() error {
			defer rws.Close()
			defer in.Close()
			defer applyLoopWg.Wait()
			defer func() {
				log.Warn("[dbg] rwloop exit")
			}()
			return rwLoop(rwLoopCtx)
		})
	}

	if block < cfg.blockReader.FrozenBlocks() {
		agg.KeepInDB(0)
		defer agg.KeepInDB(ethconfig.HistoryV3AggregationStep)
	}

	getHeaderFunc := func(hash common.Hash, number uint64) (h *types.Header) {
		var err error
		if parallel {
			if err = chainDb.View(ctx, func(tx kv.Tx) error {
				h, err = blockReader.Header(ctx, tx, hash, number)
				if err != nil {
					return err
				}
				return nil
			}); err != nil {
				panic(err)
			}
			return h
		} else {
			h, err = blockReader.Header(ctx, applyTx, hash, number)
			if err != nil {
				panic(err)
			}
			return h
		}
	}
	if !parallel {
		applyWorker.ResetTx(applyTx)
		doms.SetTx(applyTx)
	}

	slowDownLimit := time.NewTicker(time.Second)
	defer slowDownLimit.Stop()

	stateStream := !initialCycle && cfg.stateStream && maxBlockNum-block < stateStreamLimit

	var readAhead chan uint64
	if !parallel {
		// snapshots are often stored on chaper drives. don't expect low-read-latency and manually read-ahead.
		// can't use OS-level ReadAhead - because Data >> RAM
		// it also warmsup state a bit - by touching senders/coninbase accounts and code
		var clean func()
		readAhead, clean = blocksReadAhead(ctx, &cfg, 4)
		defer clean()
	}

	blocksFreezeCfg := cfg.blockReader.FreezingCfg()

	var b *types.Block
	var blockNum uint64
	var err error
Loop:
	for blockNum = block; blockNum <= maxBlockNum; blockNum++ {
		if !parallel {
			select {
			case readAhead <- blockNum:
			default:
			}
		}

		inputBlockNum.Store(blockNum)
		doms.SetBlockNum(blockNum)

		b, err = blockWithSenders(chainDb, applyTx, blockReader, blockNum)
		if err != nil {
			return err
		}
		if b == nil {
			// TODO: panic here and see that overall process deadlock
			return fmt.Errorf("nil block %d", blockNum)
		}
		txs := b.Transactions()
		header := b.HeaderNoCopy()
		skipAnalysis := core.SkipAnalysis(chainConfig, blockNum)
		signer := *types.MakeSigner(chainConfig, blockNum, header.Time)

		f := core.GetHashFn(header, getHeaderFunc)
		getHashFnMute := &sync.Mutex{}
		getHashFn := func(n uint64) common.Hash {
			getHashFnMute.Lock()
			defer getHashFnMute.Unlock()
			return f(n)
		}
		blockContext := core.NewEVMBlockContext(header, getHashFn, engine, nil /* author */)

		if parallel {
			select {
			case err := <-rwLoopErrCh:
				if err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			func() {
				for rws.Len() > rws.Limit() || rs.SizeEstimate() >= commitThreshold {
					select {
					case <-ctx.Done():
						return
					case _, ok := <-rwsConsumed:
						if !ok {
							return
						}
					case <-slowDownLimit.C:
						//logger.Warn("skip", "rws.Len()", rws.Len(), "rws.Limit()", rws.Limit(), "rws.ResultChLen()", rws.ResultChLen())
						//if tt := rws.Dbg(); tt != nil {
						//	log.Warn("fst", "n", tt.TxNum, "in.len()", in.Len(), "out", outputTxNum.Load(), "in.NewTasksLen", in.NewTasksLen())
						//}
						return
					}
				}
			}()
		} else {
			if !initialCycle && stateStream {
				txs, err := blockReader.RawTransactions(context.Background(), applyTx, b.NumberU64(), b.NumberU64())
				if err != nil {
					return err
				}
				cfg.accumulator.StartChange(b.NumberU64(), b.Hash(), txs, false)
			}
		}

		rules := chainConfig.Rules(blockNum, b.Time())
		var gasUsed uint64
		for txIndex := -1; txIndex <= len(txs); txIndex++ {

			// Do not oversend, wait for the result heap to go under certain size
			txTask := &exec22.TxTask{
				BlockNum:        blockNum,
				Header:          header,
				Coinbase:        b.Coinbase(),
				Uncles:          b.Uncles(),
				Rules:           rules,
				Txs:             txs,
				TxNum:           inputTxNum,
				TxIndex:         txIndex,
				BlockHash:       b.Hash(),
				BlockRoot:       b.Root(),
				SkipAnalysis:    skipAnalysis,
				Final:           txIndex == len(txs),
				GetHashFn:       getHashFn,
				EvmBlockContext: blockContext,
				Withdrawals:     b.Withdrawals(),
			}
			if txIndex >= 0 && txIndex < len(txs) {
				txTask.Tx = txs[txIndex]
				txTask.TxAsMessage, err = txTask.Tx.AsMessage(signer, header.BaseFee, txTask.Rules)
				if err != nil {
					return err
				}

				if sender, ok := txs[txIndex].GetSender(); ok {
					txTask.Sender = &sender
				} else {
					sender, err := signer.Sender(txTask.Tx)
					if err != nil {
						return err
					}
					txTask.Sender = &sender
					logger.Warn("[Execution] expensive lazy sender recovery", "blockNum", txTask.BlockNum, "txIdx", txTask.TxIndex)
				}
			}

			if parallel {
				if txTask.TxIndex >= 0 && txTask.TxIndex < len(txs) {
					if ok := rs.RegisterSender(txTask); ok {
						rs.AddWork(ctx, txTask, in)
					}
				} else {
					rs.AddWork(ctx, txTask, in)
				}
			} else {
				count++
				if txTask.Error != nil {
					break Loop
				}
				applyWorker.RunTxTask(txTask)
				if err := func() error {
					if txTask.Error != nil {
						return txTask.Error
					}
					if txTask.Final {
						gasUsed += txTask.UsedGas
						if gasUsed != txTask.Header.GasUsed {
							if txTask.BlockNum > 0 { //Disable check for genesis. Maybe need somehow improve it in future - to satisfy TestExecutionSpec
								return fmt.Errorf("gas used by execution: %d, in header: %d, headerNum=%d, %x", gasUsed, txTask.Header.GasUsed, txTask.Header.Number.Uint64(), txTask.Header.Hash())
							}
						}
						gasUsed = 0
					} else {
						gasUsed += txTask.UsedGas
					}
					return nil
				}(); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, common.ErrStopped) {
						logger.Warn(fmt.Sprintf("[%s] Execution failed", logPrefix), "block", blockNum, "hash", header.Hash().String(), "err", err)
						if cfg.hd != nil {
							cfg.hd.ReportBadHeaderPoS(header.Hash(), header.ParentHash)
						}
						if cfg.badBlockHalt {
							return err
						}
					}
					u.UnwindTo(blockNum-1, header.Hash())
					break Loop
				}

				// MA applystate
				if err := rs.ApplyState4(txTask, agg); err != nil {
					return fmt.Errorf("StateV3.ApplyState: %w", err)
				}
				if err := rs.ApplyLogsAndTraces(txTask, agg); err != nil {
					return fmt.Errorf("StateV3.ApplyLogsAndTraces: %w", err)
				}
				ExecTriggers.Add(rs.CommitTxNum(txTask.Sender, txTask.TxNum, in))
				outputTxNum.Add(1)
			}
			stageProgress = blockNum
			inputTxNum++
		}

		if !parallel && !dbg.DiscardCommitment() {

			rh, err := agg.ComputeCommitment(true, false)
			if err != nil {
				return fmt.Errorf("StateV3.Apply: %w", err)
			}
			if !bytes.Equal(rh, header.Root.Bytes()) {
				if cfg.badBlockHalt {
					return fmt.Errorf("wrong trie root")
				}
				logger.Error(fmt.Sprintf("[%s] Wrong trie root of block %d: %x, expected (from header): %x. Block hash: %x", logPrefix, blockNum, rh, header.Root.Bytes(), header.Hash()))

				if err := agg.Flush(ctx, applyTx); err != nil {
					panic(err)
				}
				if cfg.hd != nil {
					cfg.hd.ReportBadHeaderPoS(header.Hash(), header.ParentHash)
				}
				if maxBlockNum > execStage.BlockNumber {
					unwindTo := (maxBlockNum + execStage.BlockNumber) / 2 // Binary search for the correct block, biased to the lower numbers
					//unwindTo := blockNum - 1

					logger.Warn("Unwinding due to incorrect root hash", "to", unwindTo)
					u.UnwindTo(unwindTo, header.Hash())
				}

				/* uncomment it if need debug state-root missmatch
				if err := agg.Flush(ctx, applyTx); err != nil {
					panic(err)
				}
				oldAlogNonIncrementalHahs, err := core.CalcHashRootForTests(applyTx, header, true)
				if err != nil {
					panic(err)
				}
				if common.BytesToHash(rh) != oldAlogNonIncrementalHahs {
					log.Error(fmt.Sprintf("block hash mismatch - but new-algorithm hash is bad! (means latest state is correct): %x != %x != %x bn =%d", common.BytesToHash(rh), oldAlogNonIncrementalHahs, header.Root, blockNum))
				} else {
					log.Error(fmt.Sprintf("block hash mismatch - and new-algorithm hash is good! (means latest state is NOT correct): %x == %x != %x bn =%d", common.BytesToHash(rh), oldAlogNonIncrementalHahs, header.Root, blockNum))
				}
				*/
				break Loop
			}
		}
		if !parallel {
			outputBlockNum.Set(blockNum)
			// MA commitment
			select {
			case <-logEvery.C:
				stepsInDB := rawdbhelpers.IdxStepsCountV3(applyTx)
				progress.Log(rs, in, rws, count, inputBlockNum.Load(), outputBlockNum.Get(), outputTxNum.Load(), ExecRepeats.Get(), stepsInDB)
				if rs.SizeEstimate() < commitThreshold {
					break
				}

				var t1, t2, t3, t32, t4, t5, t6 time.Duration
				commitStart := time.Now()
				if err := func() error {
					_, err := agg.ComputeCommitment(true, false)
					if err != nil {
						return err
					}
					t1 = time.Since(commitStart)

					// prune befor flush, to speedup flush
					tt := time.Now()
					//TODO: bronen, uncomment after fix tests
					//if agg.CanPrune(applyTx) {
					//	if err = agg.Prune(ctx, ethconfig.HistoryV3AggregationStep*10); err != nil { // prune part of retired data, before commit
					//		return err
					//	}
					//}
					t2 = time.Since(tt)

					tt = time.Now()
					doms.ClearRam()
					if err := agg.Flush(ctx, applyTx); err != nil {
						return err
					}
					t3 = time.Since(tt)

					if blocksFreezeCfg.Produce {
						tt = time.Now()
						agg.BuildFilesInBackground(outputTxNum.Load())
						t5 = time.Since(tt)
						tt = time.Now()
						if agg.CanPrune(applyTx) {
							if err = agg.Prune(ctx, ethconfig.HistoryV3AggregationStep*10); err != nil { // prune part of retired data, before commit
								return err
							}
						}
					}

					if err = execStage.Update(applyTx, outputBlockNum.Get()); err != nil {
						return err
					}

					tt = time.Now()
					applyTx.CollectMetrics()
					t32 = time.Since(tt)
					if !useExternalTx {
						tt = time.Now()
						if err = applyTx.Commit(); err != nil {
							return err
						}
						t4 = time.Since(tt)
						applyTx, err = cfg.db.BeginRw(context.Background())
						if err != nil {
							return err
						}
						agg.StartWrites()
						applyWorker.ResetTx(applyTx)
						agg.SetTx(applyTx)

						doms.SetContext(applyTx.(*temporal.Tx).AggCtx())
						doms.SetTx(applyTx)
					}

					return nil
				}(); err != nil {
					return err
				}
				logger.Info("Committed", "time", time.Since(commitStart),
					"commitment", t1, "prune", t2, "flush", t3, "tx.CollectMetrics", t32, "tx.commit", t4, "aggregate", t5, "prune2", t6)
			default:
			}
		}

		if parallel && blocksFreezeCfg.Produce { // sequential exec - does aggregate right after commit
			agg.BuildFilesInBackground(outputTxNum.Load())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	if parallel {
		logger.Warn("[dbg] all txs sent")
		if err := rwLoopG.Wait(); err != nil {
			return err
		}
		waitWorkers()
	} else {
		//if err = rs.Flush(ctx, applyTx, logPrefix, logEvery); err != nil {
		//	return err
		//}

		if err = agg.Flush(ctx, applyTx); err != nil {
			return err
		}
		//rh, err := rs.Commitment(inputTxNum, agg)
		//if err != nil {
		//	return err
		//}
		//if !bytes.Equal(rh, header.Root.Bytes()) {
		//	return fmt.Errorf("root hash mismatch: %x != %x, bn=%d", rh, header.Root.Bytes(), blockNum)
		//}
		if err = execStage.Update(applyTx, stageProgress); err != nil {
			return err
		}
	}

	if parallel && blocksFreezeCfg.Produce {
		agg.BuildFilesInBackground(outputTxNum.Load())
	}

	if !useExternalTx && applyTx != nil {
		if err = applyTx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func blockWithSenders(db kv.RoDB, tx kv.Tx, blockReader services.BlockReader, blockNum uint64) (b *types.Block, err error) {
	if tx == nil {
		tx, err = db.BeginRo(context.Background())
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
	}
	return blockReader.BlockByNumber(context.Background(), tx, blockNum)
}

func blocksReadAheadV3(ctx context.Context, cfg *ExecuteBlockCfg, workers int) (chan uint64, context.CancelFunc) {
	const readAheadBlocks = 100
	readAhead := make(chan uint64, readAheadBlocks)
	g, gCtx := errgroup.WithContext(ctx)
	for workerNum := 0; workerNum < workers; workerNum++ {
		g.Go(func() (err error) {
			var bn uint64
			var ok bool
			var tx kv.Tx
			defer func() {
				if tx != nil {
					tx.Rollback()
				}
			}()

			for i := 0; ; i++ {
				select {
				case bn, ok = <-readAhead:
					if !ok {
						return
					}
				case <-gCtx.Done():
					return gCtx.Err()
				}

				if i%100 == 0 {
					if tx != nil {
						tx.Rollback()
					}
					tx, err = cfg.db.BeginRo(ctx)
					if err != nil {
						return err
					}
				}

				if err := blocksReadAheadFunc(gCtx, tx, cfg, bn+readAheadBlocks); err != nil {
					return err
				}
			}
		})
	}
	return readAhead, func() {
		close(readAhead)
		_ = g.Wait()
	}
}
func blocksReadAheadFuncV3(ctx context.Context, tx kv.Tx, cfg *ExecuteBlockCfg, blockNum uint64) error {
	block, err := cfg.blockReader.BlockByNumber(ctx, tx, blockNum)
	if err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	senders := block.Body().SendersFromTxs()             //TODO: BlockByNumber can return senders
	stateReader := state.NewReaderV4(tx.(kv.TemporalTx)) //TODO: can do on batch! if make batch thread-safe
	for _, sender := range senders {
		a, _ := stateReader.ReadAccountData(sender)
		if a == nil || a.Incarnation == 0 {
			continue
		}
		if code, _ := stateReader.ReadAccountCode(sender, a.Incarnation, a.CodeHash); len(code) > 0 {
			_, _ = code[0], code[len(code)-1]
		}
	}

	for _, txn := range block.Transactions() {
		to := txn.GetTo()
		if to == nil {
			continue
		}
		a, _ := stateReader.ReadAccountData(*to)
		if a == nil || a.Incarnation == 0 {
			continue
		}
		if code, _ := stateReader.ReadAccountCode(*to, a.Incarnation, a.CodeHash); len(code) > 0 {
			_, _ = code[0], code[len(code)-1]
		}
	}
	_, _ = stateReader.ReadAccountData(block.Coinbase())
	_, _ = block, senders
	return nil
}

func processResultQueue(in *exec22.QueueWithRetry, rws *exec22.ResultsQueue, outputTxNumIn uint64, rs *state.StateV3, agg *state2.AggregatorV3, applyTx kv.Tx, backPressure chan struct{}, applyWorker *exec3.Worker, canRetry, forceStopAtBlockEnd bool) (outputTxNum uint64, conflicts, triggers int, processedBlockNum uint64, stopedAtBlockEnd bool, err error) {
	rwsIt := rws.Iter()
	defer rwsIt.Close()

	var i int
	outputTxNum = outputTxNumIn
	for rwsIt.HasNext(outputTxNum) {
		txTask := rwsIt.PopNext()
		if txTask.Error != nil || !rs.ReadsValid(txTask.ReadLists) {
			conflicts++

			if i > 0 && canRetry {
				//send to re-exex
				rs.ReTry(txTask, in)
				continue
			}

			// resolve first conflict right here: it's faster and conflict-free
			applyWorker.RunTxTask(txTask)
			if txTask.Error != nil {
				return outputTxNum, conflicts, triggers, processedBlockNum, false, txTask.Error
			}
			i++
		}

		if txTask.Final {
			err := rs.ApplyState4(txTask, agg)
			if err != nil {
				return outputTxNum, conflicts, triggers, processedBlockNum, false, fmt.Errorf("StateV3.Apply: %w", err)
			}
			//if !bytes.Equal(rh, txTask.BlockRoot[:]) {
			//	log.Error("block hash mismatch", "rh", hex.EncodeToString(rh), "blockRoot", hex.EncodeToString(txTask.BlockRoot[:]), "bn", txTask.BlockNum, "txn", txTask.TxNum)
			//	return outputTxNum, conflicts, triggers, processedBlockNum, false, fmt.Errorf("block hash mismatch: %x != %x bn =%d, txn= %d", rh, txTask.BlockRoot[:], txTask.BlockNum, txTask.TxNum)
			//}
		}
		triggers += rs.CommitTxNum(txTask.Sender, txTask.TxNum, in)
		outputTxNum++
		if backPressure != nil {
			select {
			case backPressure <- struct{}{}:
			default:
			}
		}
		if err := rs.ApplyLogsAndTraces(txTask, agg); err != nil {
			return outputTxNum, conflicts, triggers, processedBlockNum, false, fmt.Errorf("StateV3.Apply: %w", err)
		}
		fmt.Printf("Applied %d block %d txIndex %d\n", txTask.TxNum, txTask.BlockNum, txTask.TxIndex)
		processedBlockNum = txTask.BlockNum
		stopedAtBlockEnd = txTask.Final
		if forceStopAtBlockEnd && txTask.Final {
			break
		}
	}
	return
}

func reconstituteStep(last bool,
	workerCount int, ctx context.Context, db kv.RwDB, txNum uint64, dirs datadir.Dirs,
	as *libstate.AggregatorStep, chainDb kv.RwDB, blockReader services.FullBlockReader,
	chainConfig *chain.Config, logger log.Logger, genesis *types.Genesis, engine consensus.Engine,
	batchSize datasize.ByteSize, s *StageState, blockNum uint64, total uint64,
) error {
	var startOk, endOk bool
	startTxNum, endTxNum := as.TxNumRange()
	var startBlockNum, endBlockNum uint64 // First block which is not covered by the history snapshot files
	if err := chainDb.View(ctx, func(tx kv.Tx) (err error) {
		startOk, startBlockNum, err = rawdbv3.TxNums.FindBlockNum(tx, startTxNum)
		if err != nil {
			return err
		}
		if startBlockNum > 0 {
			startBlockNum--
			startTxNum, err = rawdbv3.TxNums.Min(tx, startBlockNum)
			if err != nil {
				return err
			}
		}
		endOk, endBlockNum, err = rawdbv3.TxNums.FindBlockNum(tx, endTxNum)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if !startOk {
		return fmt.Errorf("step startTxNum not found in snapshot blocks: %d", startTxNum)
	}
	if !endOk {
		return fmt.Errorf("step endTxNum not found in snapshot blocks: %d", endTxNum)
	}
	if last {
		endBlockNum = blockNum
	}

	logger.Info(fmt.Sprintf("[%s] Reconstitution", s.LogPrefix()), "startTxNum", startTxNum, "endTxNum", endTxNum, "startBlockNum", startBlockNum, "endBlockNum", endBlockNum)

	var maxTxNum = startTxNum

	scanWorker := exec3.NewScanWorker(txNum, as)

	t := time.Now()
	if err := scanWorker.BitmapAccounts(); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Scan accounts history", s.LogPrefix()), "took", time.Since(t))
	}

	t = time.Now()
	if err := scanWorker.BitmapStorage(); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Scan storage history", s.LogPrefix()), "took", time.Since(t))
	}

	t = time.Now()
	if err := scanWorker.BitmapCode(); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Scan code history", s.LogPrefix()), "took", time.Since(t))
	}
	bitmap := scanWorker.Bitmap()

	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()

	logger.Info(fmt.Sprintf("[%s] Ready to replay", s.LogPrefix()), "transactions", bitmap.GetCardinality(), "out of", txNum)
	var lock sync.RWMutex
	reconWorkers := make([]*exec3.ReconWorker, workerCount)
	roTxs := make([]kv.Tx, workerCount)
	chainTxs := make([]kv.Tx, workerCount)
	defer func() {
		for i := 0; i < workerCount; i++ {
			if roTxs[i] != nil {
				roTxs[i].Rollback()
			}
			if chainTxs[i] != nil {
				chainTxs[i].Rollback()
			}
		}
	}()
	for i := 0; i < workerCount; i++ {
		var err error
		if roTxs[i], err = db.BeginRo(ctx); err != nil {
			return err
		}
		if chainTxs[i], err = chainDb.BeginRo(ctx); err != nil {
			return err
		}
	}
	g, reconstWorkersCtx := errgroup.WithContext(ctx)
	defer g.Wait()
	workCh := make(chan *exec22.TxTask, workerCount*4)
	defer func() {
		fmt.Printf("close1\n")
		safeCloseTxTaskCh(workCh)
	}()

	rs := state.NewReconState(workCh)
	prevCount := rs.DoneCount()
	for i := 0; i < workerCount; i++ {
		var localAs *libstate.AggregatorStep
		if i == 0 {
			localAs = as
		} else {
			localAs = as.Clone()
		}
		reconWorkers[i] = exec3.NewReconWorker(lock.RLocker(), reconstWorkersCtx, rs, localAs, blockReader, chainConfig, logger, genesis, engine, chainTxs[i])
		reconWorkers[i].SetTx(roTxs[i])
		reconWorkers[i].SetChainTx(chainTxs[i])
	}

	rollbackCount := uint64(0)

	for i := 0; i < workerCount; i++ {
		i := i
		g.Go(func() error { return reconWorkers[i].Run() })
	}
	commitThreshold := batchSize.Bytes()
	prevRollbackCount := uint64(0)
	prevTime := time.Now()
	reconDone := make(chan struct{}, 1)

	defer close(reconDone)

	commit := func(ctx context.Context) error {
		t := time.Now()
		lock.Lock()
		defer lock.Unlock()
		for i := 0; i < workerCount; i++ {
			roTxs[i].Rollback()
		}
		if err := db.Update(ctx, func(tx kv.RwTx) error {
			if err := rs.Flush(tx); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		for i := 0; i < workerCount; i++ {
			var err error
			if roTxs[i], err = db.BeginRo(ctx); err != nil {
				return err
			}
			reconWorkers[i].SetTx(roTxs[i])
		}
		logger.Info(fmt.Sprintf("[%s] State reconstitution, commit", s.LogPrefix()), "took", time.Since(t))
		return nil
	}
	g.Go(func() error {
		for {
			select {
			case <-reconDone: // success finish path
				return nil
			case <-reconstWorkersCtx.Done(): // force-stop path
				return reconstWorkersCtx.Err()
			case <-logEvery.C:
				var m runtime.MemStats
				dbg.ReadMemStats(&m)
				sizeEstimate := rs.SizeEstimate()
				maxTxNum = rs.MaxTxNum()
				count := rs.DoneCount()
				rollbackCount = rs.RollbackCount()
				currentTime := time.Now()
				interval := currentTime.Sub(prevTime)
				speedTx := float64(count-prevCount) / (float64(interval) / float64(time.Second))
				progress := 100.0 * float64(maxTxNum) / float64(total)
				stepProgress := 100.0 * float64(maxTxNum-startTxNum) / float64(endTxNum-startTxNum)
				var repeatRatio float64
				if count > prevCount {
					repeatRatio = 100.0 * float64(rollbackCount-prevRollbackCount) / float64(count-prevCount)
				}
				prevTime = currentTime
				prevCount = count
				prevRollbackCount = rollbackCount
				logger.Info(fmt.Sprintf("[%s] State reconstitution", s.LogPrefix()), "overall progress", fmt.Sprintf("%.2f%%", progress),
					"step progress", fmt.Sprintf("%.2f%%", stepProgress),
					"tx/s", fmt.Sprintf("%.1f", speedTx), "workCh", fmt.Sprintf("%d/%d", len(workCh), cap(workCh)),
					"repeat ratio", fmt.Sprintf("%.2f%%", repeatRatio), "queue.len", rs.QueueLen(), "blk", syncMetrics[stages.Execution].Get(),
					"buffer", fmt.Sprintf("%s/%s", common.ByteCount(sizeEstimate), common.ByteCount(commitThreshold)),
					"alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys))
				if sizeEstimate >= commitThreshold {
					if err := commit(reconstWorkersCtx); err != nil {
						return err
					}
				}
			}
		}
	})

	var inputTxNum = startTxNum
	var b *types.Block
	var txKey [8]byte
	getHeaderFunc := func(hash common.Hash, number uint64) (h *types.Header) {
		var err error
		if err = chainDb.View(ctx, func(tx kv.Tx) error {
			h, err = blockReader.Header(ctx, tx, hash, number)
			if err != nil {
				return err
			}
			return nil

		}); err != nil {
			panic(err)
		}
		return h
	}

	if err := func() (err error) {
		defer func() {
			close(workCh)
			reconDone <- struct{}{} // Complete logging and committing go-routine
			if waitErr := g.Wait(); waitErr != nil {
				if err == nil {
					err = waitErr
				}
				return
			}
		}()

		for bn := startBlockNum; bn <= endBlockNum; bn++ {
			t = time.Now()
			b, err = blockWithSenders(chainDb, nil, blockReader, bn)
			if err != nil {
				return err
			}
			if b == nil {
				return fmt.Errorf("could not find block %d\n", bn)
			}
			txs := b.Transactions()
			header := b.HeaderNoCopy()
			skipAnalysis := core.SkipAnalysis(chainConfig, bn)
			signer := *types.MakeSigner(chainConfig, bn, header.Time)

			f := core.GetHashFn(header, getHeaderFunc)
			getHashFnMute := &sync.Mutex{}
			getHashFn := func(n uint64) common.Hash {
				getHashFnMute.Lock()
				defer getHashFnMute.Unlock()
				return f(n)
			}
			blockContext := core.NewEVMBlockContext(header, getHashFn, engine, nil /* author */)
			rules := chainConfig.Rules(bn, b.Time())

			for txIndex := -1; txIndex <= len(txs); txIndex++ {
				if bitmap.Contains(inputTxNum) {
					binary.BigEndian.PutUint64(txKey[:], inputTxNum)
					txTask := &exec22.TxTask{
						BlockNum:        bn,
						Header:          header,
						Coinbase:        b.Coinbase(),
						Uncles:          b.Uncles(),
						Rules:           rules,
						TxNum:           inputTxNum,
						Txs:             txs,
						TxIndex:         txIndex,
						BlockHash:       b.Hash(),
						SkipAnalysis:    skipAnalysis,
						Final:           txIndex == len(txs),
						GetHashFn:       getHashFn,
						EvmBlockContext: blockContext,
						Withdrawals:     b.Withdrawals(),
					}
					if txIndex >= 0 && txIndex < len(txs) {
						txTask.Tx = txs[txIndex]
						txTask.TxAsMessage, err = txTask.Tx.AsMessage(signer, header.BaseFee, txTask.Rules)
						if err != nil {
							return err
						}
						if sender, ok := txs[txIndex].GetSender(); ok {
							txTask.Sender = &sender
						}
					} else {
						txTask.Txs = txs
					}

					select {
					case workCh <- txTask:
					case <-reconstWorkersCtx.Done():
						// if ctx canceled, then maybe it's because of error in errgroup
						//
						// errgroup doesn't play with pattern where some 1 goroutine-producer is outside of errgroup
						// but RwTx doesn't allow move between goroutines
						return g.Wait()
					}
				}
				inputTxNum++
			}

			syncMetrics[stages.Execution].Set(bn)
		}
		return err
	}(); err != nil {
		return err
	}

	for i := 0; i < workerCount; i++ {
		roTxs[i].Rollback()
	}
	if err := db.Update(ctx, func(tx kv.RwTx) error {
		if err := rs.Flush(tx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	plainStateCollector := etl.NewCollector(fmt.Sprintf("%s recon plainState", s.LogPrefix()), dirs.Tmp, etl.NewSortableBuffer(etl.BufferOptimalSize), logger)
	defer plainStateCollector.Close()
	codeCollector := etl.NewCollector(fmt.Sprintf("%s recon code", s.LogPrefix()), dirs.Tmp, etl.NewOldestEntryBuffer(etl.BufferOptimalSize), logger)
	defer codeCollector.Close()
	plainContractCollector := etl.NewCollector(fmt.Sprintf("%s recon plainContract", s.LogPrefix()), dirs.Tmp, etl.NewSortableBuffer(etl.BufferOptimalSize), logger)
	defer plainContractCollector.Close()
	var transposedKey []byte

	if err := db.View(ctx, func(roTx kv.Tx) error {
		clear := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.PlainStateR, nil, math.MaxUint32)
		defer clear()
		if err := roTx.ForEach(kv.PlainStateR, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], k[8:]...)
			transposedKey = append(transposedKey, k[:8]...)
			return plainStateCollector.Collect(transposedKey, v)
		}); err != nil {
			return err
		}
		clear2 := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.PlainStateD, nil, math.MaxUint32)
		defer clear2()
		if err := roTx.ForEach(kv.PlainStateD, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], v...)
			transposedKey = append(transposedKey, k...)
			return plainStateCollector.Collect(transposedKey, nil)
		}); err != nil {
			return err
		}
		clear3 := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.CodeR, nil, math.MaxUint32)
		defer clear3()
		if err := roTx.ForEach(kv.CodeR, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], k[8:]...)
			transposedKey = append(transposedKey, k[:8]...)
			return codeCollector.Collect(transposedKey, v)
		}); err != nil {
			return err
		}
		clear4 := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.CodeD, nil, math.MaxUint32)
		defer clear4()
		if err := roTx.ForEach(kv.CodeD, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], v...)
			transposedKey = append(transposedKey, k...)
			return codeCollector.Collect(transposedKey, nil)
		}); err != nil {
			return err
		}
		clear5 := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.PlainContractR, nil, math.MaxUint32)
		defer clear5()
		if err := roTx.ForEach(kv.PlainContractR, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], k[8:]...)
			transposedKey = append(transposedKey, k[:8]...)
			return plainContractCollector.Collect(transposedKey, v)
		}); err != nil {
			return err
		}
		clear6 := kv.ReadAhead(ctx, db, &atomic.Bool{}, kv.PlainContractD, nil, math.MaxUint32)
		defer clear6()
		if err := roTx.ForEach(kv.PlainContractD, nil, func(k, v []byte) error {
			transposedKey = append(transposedKey[:0], v...)
			transposedKey = append(transposedKey, k...)
			return plainContractCollector.Collect(transposedKey, nil)
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if err := db.Update(ctx, func(tx kv.RwTx) error {
		if err := tx.ClearBucket(kv.PlainStateR); err != nil {
			return err
		}
		if err := tx.ClearBucket(kv.PlainStateD); err != nil {
			return err
		}
		if err := tx.ClearBucket(kv.CodeR); err != nil {
			return err
		}
		if err := tx.ClearBucket(kv.CodeD); err != nil {
			return err
		}
		if err := tx.ClearBucket(kv.PlainContractR); err != nil {
			return err
		}
		if err := tx.ClearBucket(kv.PlainContractD); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	if err := chainDb.Update(ctx, func(tx kv.RwTx) error {
		var lastKey []byte
		var lastVal []byte
		if err := plainStateCollector.Load(tx, kv.PlainState, func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
			if !bytes.Equal(k[:len(k)-8], lastKey) {
				if lastKey != nil {
					if e := next(lastKey, lastKey, lastVal); e != nil {
						return e
					}
				}
				lastKey = append(lastKey[:0], k[:len(k)-8]...)
			}
			if v == nil { // `nil` value means delete, `empty value []byte{}` means empty value
				lastVal = nil
			} else {
				lastVal = append(lastVal[:0], v...)
			}
			return nil
		}, etl.TransformArgs{}); err != nil {
			return err
		}
		plainStateCollector.Close()
		if lastKey != nil {
			if len(lastVal) > 0 {
				if e := tx.Put(kv.PlainState, lastKey, lastVal); e != nil {
					return e
				}
			} else {
				if e := tx.Delete(kv.PlainState, lastKey); e != nil {
					return e
				}
			}
		}
		lastKey = nil
		lastVal = nil
		if err := codeCollector.Load(tx, kv.Code, func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
			if !bytes.Equal(k[:len(k)-8], lastKey) {
				if lastKey != nil {
					if e := next(lastKey, lastKey, lastVal); e != nil {
						return e
					}
				}
				lastKey = append(lastKey[:0], k[:len(k)-8]...)
			}
			if v == nil {
				lastVal = nil
			} else {
				lastVal = append(lastVal[:0], v...)
			}
			return nil
		}, etl.TransformArgs{}); err != nil {
			return err
		}
		codeCollector.Close()
		if lastKey != nil {
			if len(lastVal) > 0 {
				if e := tx.Put(kv.Code, lastKey, lastVal); e != nil {
					return e
				}
			} else {
				if e := tx.Delete(kv.Code, lastKey); e != nil {
					return e
				}
			}
		}
		lastKey = nil
		lastVal = nil
		if err := plainContractCollector.Load(tx, kv.PlainContractCode, func(k, v []byte, table etl.CurrentTableReader, next etl.LoadNextFunc) error {
			if !bytes.Equal(k[:len(k)-8], lastKey) {
				if lastKey != nil {
					if e := next(lastKey, lastKey, lastVal); e != nil {
						return e
					}
				}
				lastKey = append(lastKey[:0], k[:len(k)-8]...)
			}
			if v == nil {
				lastVal = nil
			} else {
				lastVal = append(lastVal[:0], v...)
			}
			return nil
		}, etl.TransformArgs{}); err != nil {
			return err
		}
		plainContractCollector.Close()
		if lastKey != nil {
			if len(lastVal) > 0 {
				if e := tx.Put(kv.PlainContractCode, lastKey, lastVal); e != nil {
					return e
				}
			} else {
				if e := tx.Delete(kv.PlainContractCode, lastKey); e != nil {
					return e
				}
			}
		}

		return nil
	}); err != nil {
		return err
	}
	return nil
}

func safeCloseTxTaskCh(ch chan *exec22.TxTask) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
		// Channel was already closed
	default:
		close(ch)
	}
}

func ReconstituteState(ctx context.Context, s *StageState, dirs datadir.Dirs, workerCount int, batchSize datasize.ByteSize, chainDb kv.RwDB,
	blockReader services.FullBlockReader,
	logger log.Logger, agg *state2.AggregatorV3, engine consensus.Engine,
	chainConfig *chain.Config, genesis *types.Genesis) (err error) {
	startTime := time.Now()
	defer agg.EnableMadvNormal().DisableReadAhead()

	// force merge snapshots before reconstitution, to allign domains progress
	// un-finished merge can happen at "kill -9" during merge
	if err := agg.MergeLoop(ctx, estimate.CompressSnapshot.Workers()); err != nil {
		return err
	}

	// Incremental reconstitution, step by step (snapshot range by snapshot range)
	aggSteps, err := agg.MakeSteps()
	if err != nil {
		return err
	}
	if len(aggSteps) == 0 {
		return nil
	}
	lastStep := aggSteps[len(aggSteps)-1]

	var ok bool
	var blockNum uint64 // First block which is not covered by the history snapshot files
	var txNum uint64
	if err := chainDb.View(ctx, func(tx kv.Tx) error {
		_, toTxNum := lastStep.TxNumRange()
		ok, blockNum, err = rawdbv3.TxNums.FindBlockNum(tx, toTxNum)
		if err != nil {
			return err
		}
		if !ok {
			lastBn, lastTn, _ := rawdbv3.TxNums.Last(tx)
			return fmt.Errorf("blockNum for mininmaxTxNum=%d not found. See lastBlockNum=%d,lastTxNum=%d", toTxNum, lastBn, lastTn)
		}
		if blockNum == 0 {
			return fmt.Errorf("not enough transactions in the history data")
		}
		blockNum--
		txNum, err = rawdbv3.TxNums.Max(tx, blockNum)
		if err != nil {
			return err
		}
		txNum++
		return nil
	}); err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("[%s] Blocks execution, reconstitution", s.LogPrefix()), "fromBlock", s.BlockNumber, "toBlock", blockNum, "toTxNum", txNum)

	reconDbPath := filepath.Join(dirs.DataDir, "recondb")
	dir.Recreate(reconDbPath)
	db, err := kv2.NewMDBX(log.New()).Path(reconDbPath).
		Flags(func(u uint) uint {
			return mdbx.UtterlyNoSync | mdbx.NoMetaSync | mdbx.NoMemInit | mdbx.LifoReclaim | mdbx.WriteMap
		}).
		PageSize(uint64(8 * datasize.KB)).
		WithTableCfg(func(defaultBuckets kv.TableCfg) kv.TableCfg { return kv.ReconTablesCfg }).
		Open()
	if err != nil {
		return err
	}
	defer db.Close()
	defer os.RemoveAll(reconDbPath)

	for step, as := range aggSteps {
		logger.Info("Step of incremental reconstitution", "step", step+1, "out of", len(aggSteps), "workers", workerCount)
		if err := reconstituteStep(step+1 == len(aggSteps), workerCount, ctx, db,
			txNum, dirs, as, chainDb, blockReader, chainConfig, logger, genesis,
			engine, batchSize, s, blockNum, txNum,
		); err != nil {
			return err
		}
	}
	db.Close()
	plainStateCollector := etl.NewCollector(fmt.Sprintf("%s recon plainState", s.LogPrefix()), dirs.Tmp, etl.NewSortableBuffer(etl.BufferOptimalSize), logger)
	defer plainStateCollector.Close()
	codeCollector := etl.NewCollector(fmt.Sprintf("%s recon code", s.LogPrefix()), dirs.Tmp, etl.NewOldestEntryBuffer(etl.BufferOptimalSize), logger)
	defer codeCollector.Close()
	plainContractCollector := etl.NewCollector(fmt.Sprintf("%s recon plainContract", s.LogPrefix()), dirs.Tmp, etl.NewSortableBuffer(etl.BufferOptimalSize), logger)
	defer plainContractCollector.Close()

	fillWorker := exec3.NewFillWorker(txNum, aggSteps[len(aggSteps)-1])
	t := time.Now()
	if err := fillWorker.FillAccounts(plainStateCollector); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Filled accounts", s.LogPrefix()), "took", time.Since(t))
	}
	t = time.Now()
	if err := fillWorker.FillStorage(plainStateCollector); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Filled storage", s.LogPrefix()), "took", time.Since(t))
	}
	t = time.Now()
	if err := fillWorker.FillCode(codeCollector, plainContractCollector); err != nil {
		return err
	}
	if time.Since(t) > 5*time.Second {
		logger.Info(fmt.Sprintf("[%s] Filled code", s.LogPrefix()), "took", time.Since(t))
	}

	// Load all collections into the main collector
	if err = chainDb.Update(ctx, func(tx kv.RwTx) error {
		if err = plainStateCollector.Load(tx, kv.PlainState, etl.IdentityLoadFunc, etl.TransformArgs{}); err != nil {
			return err
		}
		plainStateCollector.Close()
		if err = codeCollector.Load(tx, kv.Code, etl.IdentityLoadFunc, etl.TransformArgs{}); err != nil {
			return err
		}
		codeCollector.Close()
		if err = plainContractCollector.Load(tx, kv.PlainContractCode, etl.IdentityLoadFunc, etl.TransformArgs{}); err != nil {
			return err
		}
		plainContractCollector.Close()
		if err := s.Update(tx, blockNum); err != nil {
			return err
		}
		s.BlockNumber = blockNum
		return nil
	}); err != nil {
		return err
	}
	logger.Info(fmt.Sprintf("[%s] Reconstitution done", s.LogPrefix()), "in", time.Since(startTime))
	return nil
}

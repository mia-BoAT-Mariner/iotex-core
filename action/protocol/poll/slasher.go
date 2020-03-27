// Copyright (c) 2020 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package poll

import (
	"context"
	"math/big"

	"github.com/iotexproject/iotex-election/util"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/action/protocol"
	"github.com/iotexproject/iotex-core/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/action/protocol/vote"
	"github.com/iotexproject/iotex-core/blockchain/genesis"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/crypto"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/pkg/util/byteutil"
	"github.com/iotexproject/iotex-core/state"
)

// Slasher is the module to slash candidates
type Slasher struct {
	hu                    config.HeightUpgrade
	productivity          Productivity
	candByHeight          CandidatesByHeight
	getCandidates         GetCandidates
	getKickoutList        GetKickoutList
	getUnprodDelegate     GetUnproductiveDelegate
	indexer               *CandidateIndexer
	numCandidateDelegates uint64
	numDelegates          uint64
	prodThreshold         uint64
	kickoutEpochPeriod    uint64
	maxKickoutPeriod      uint64
	kickoutIntensity      uint32
}

// NewSlasher returns a new Slasher
func NewSlasher(
	gen *genesis.Genesis,
	productivity Productivity,
	candByHeight CandidatesByHeight,
	getCandidates GetCandidates,
	getKickoutList GetKickoutList,
	getUnprodDelegate GetUnproductiveDelegate,
	indexer *CandidateIndexer,
	numCandidateDelegates, numDelegates, thres, koPeriod, maxKoPeriod uint64,
	koIntensity uint32,
) (*Slasher, error) {
	return &Slasher{
		hu:                    config.NewHeightUpgrade(gen),
		productivity:          productivity,
		candByHeight:          candByHeight,
		getCandidates:         getCandidates,
		getKickoutList:        getKickoutList,
		getUnprodDelegate:     getUnprodDelegate,
		indexer:               indexer,
		numCandidateDelegates: numCandidateDelegates,
		numDelegates:          numDelegates,
		prodThreshold:         thres,
		kickoutEpochPeriod:    koPeriod,
		maxKickoutPeriod:      maxKoPeriod,
		kickoutIntensity:      koIntensity,
	}, nil
}

// CreatePreStates is to setup kickout list
func (sh *Slasher) CreatePreStates(ctx context.Context, sm protocol.StateManager, indexer *CandidateIndexer) error {
	bcCtx := protocol.MustGetBlockchainCtx(ctx)
	blkCtx := protocol.MustGetBlockCtx(ctx)
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	epochNum := rp.GetEpochNum(blkCtx.BlockHeight)
	epochStartHeight := rp.GetEpochHeight(epochNum)
	epochLastHeight := rp.GetEpochLastBlockHeight(epochNum)
	nextEpochStartHeight := rp.GetEpochHeight(epochNum + 1)
	hu := config.NewHeightUpgrade(&bcCtx.Genesis)
	if blkCtx.BlockHeight == epochLastHeight && hu.IsPost(config.Easter, nextEpochStartHeight) {
		// if the block height is the end of epoch and next epoch is after the Easter height, calculate blacklist for kick-out and write into state DB
		unqualifiedList, err := sh.CalculateKickoutList(ctx, sm, epochNum+1)
		if err != nil {
			return err
		}
		return setNextEpochBlacklist(sm, indexer, nextEpochStartHeight, unqualifiedList)
	}
	if blkCtx.BlockHeight == epochStartHeight && hu.IsPost(config.Easter, epochStartHeight) {
		prevHeight, err := shiftCandidates(sm)
		if err != nil {
			return err
		}
		afterHeight, err := shiftKickoutList(sm)
		if err != nil {
			return err
		}
		if prevHeight != afterHeight {
			return errors.Wrap(ErrInconsistentHeight, "shifting candidate height is not same as shifting kickout height")
		}
	}
	return nil
}

// ReadState defines slasher's read methods.
func (sh *Slasher) ReadState(
	ctx context.Context,
	sr protocol.StateReader,
	indexer *CandidateIndexer,
	method []byte,
	args ...[]byte,
) ([]byte, error) {
	blkCtx := protocol.MustGetBlockCtx(ctx)
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	epochNum := rp.GetEpochNum(blkCtx.BlockHeight) // tip
	epochStartHeight := rp.GetEpochHeight(epochNum)
	if len(args) != 0 {
		epochNum = byteutil.BytesToUint64(args[0])
		epochStartHeight = rp.GetEpochHeight(epochNum)
	}
	switch string(method) {
	case "CandidatesByEpoch":
		if indexer != nil {
			candidates, err := sh.GetCandidatesFromIndexer(ctx, epochStartHeight)
			if err == nil {
				return candidates.Serialize()
			}
			if err != nil {
				if errors.Cause(err) != ErrIndexerNotExist {
					return nil, err
				}
			}
		}
		candidates, err := sh.GetCandidates(ctx, sr, false)
		if err != nil {
			return nil, err
		}
		return candidates.Serialize()
	case "BlockProducersByEpoch":
		if indexer != nil {
			blockProducers, err := sh.GetBPFromIndexer(ctx, epochStartHeight)
			if err == nil {
				return blockProducers.Serialize()
			}
			if err != nil {
				if errors.Cause(err) != ErrIndexerNotExist {
					return nil, err
				}
			}
		}
		blockProducers, err := sh.GetBlockProducers(ctx, sr, false)
		if err != nil {
			return nil, err
		}
		return blockProducers.Serialize()
	case "ActiveBlockProducersByEpoch":
		if indexer != nil {
			activeBlockProducers, err := sh.GetABPFromIndexer(ctx, epochStartHeight)
			if err == nil {
				return activeBlockProducers.Serialize()
			}
			if err != nil {
				if errors.Cause(err) != ErrIndexerNotExist {
					return nil, err
				}
			}
		}
		activeBlockProducers, err := sh.GetActiveBlockProducers(ctx, sr, false)
		if err != nil {
			return nil, err
		}
		return activeBlockProducers.Serialize()
	case "KickoutListByEpoch":
		if indexer != nil {
			kickoutList, err := indexer.KickoutList(epochStartHeight)
			if err == nil {
				return kickoutList.Serialize()
			}
			if err != nil {
				if errors.Cause(err) != ErrIndexerNotExist {
					return nil, err
				}
			}
		}
		kickoutList, err := sh.GetKickoutList(ctx, sr, false)
		if err != nil {
			return nil, err
		}
		return kickoutList.Serialize()
	default:
		return nil, errors.New("corresponding method isn't found")
	}
}

// EmptyBlacklist returns an empty Blacklist
func (sh *Slasher) EmptyBlacklist() *vote.Blacklist {
	return vote.NewBlacklist(sh.kickoutIntensity)
}

// GetCandidates returns candidate list
func (sh *Slasher) GetCandidates(ctx context.Context, sr protocol.StateReader, readFromNext bool) (state.CandidateList, error) {
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	targetHeight, err := sr.Height()
	if err != nil {
		return nil, err
	}
	// make sure it's epochStartHeight
	targetEpochStartHeight := rp.GetEpochHeight(rp.GetEpochNum(targetHeight))
	if readFromNext {
		targetEpochNum := rp.GetEpochNum(targetEpochStartHeight) + 1
		targetEpochStartHeight = rp.GetEpochHeight(targetEpochNum) // next epoch start height
	}
	if sh.hu.IsPre(config.Easter, targetEpochStartHeight) {
		return sh.candByHeight(sr, targetEpochStartHeight)
	}
	// After Easter height, kick-out unqualified delegates based on productivity
	candidates, stateHeight, err := sh.getCandidates(sr, readFromNext)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get candidates at height %d after easter height", targetEpochStartHeight)
	}
	// to catch the corner case that since the new block is committed, shift occurs in the middle of processing the request
	if rp.GetEpochNum(targetEpochStartHeight) < rp.GetEpochNum(stateHeight) {
		return nil, errors.Wrap(ErrInconsistentHeight, "state factory epoch number became larger than target epoch number")
	}
	unqualifiedList, err := sh.GetKickoutList(ctx, sr, readFromNext)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get kickout list at height %d", targetEpochStartHeight)
	}
	// recalculate the voting power for blacklist delegates
	return filterCandidates(candidates, unqualifiedList, targetEpochStartHeight)
}

// GetBlockProducers returns BP list
func (sh *Slasher) GetBlockProducers(ctx context.Context, sr protocol.StateReader, readFromNext bool) (state.CandidateList, error) {
	candidates, err := sh.GetCandidates(ctx, sr, readFromNext)
	if err != nil {
		return nil, err
	}
	return sh.calculateBlockProducer(candidates)
}

// GetActiveBlockProducers returns active BP list
func (sh *Slasher) GetActiveBlockProducers(ctx context.Context, sr protocol.StateReader, readFromNext bool) (state.CandidateList, error) {
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	targetHeight, err := sr.Height()
	if err != nil {
		return nil, err
	}
	// make sure it's epochStartHeight
	targetEpochStartHeight := rp.GetEpochHeight(rp.GetEpochNum(targetHeight))
	if readFromNext {
		targetEpochNum := rp.GetEpochNum(targetEpochStartHeight) + 1
		targetEpochStartHeight = rp.GetEpochHeight(targetEpochNum) // next epoch start height
	}
	blockProducers, err := sh.GetBlockProducers(ctx, sr, readFromNext)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read block producers at height %d", targetEpochStartHeight)
	}
	return sh.calculateActiveBlockProducer(ctx, blockProducers, targetEpochStartHeight)
}

// GetCandidatesFromIndexer returns candidate list from indexer
func (sh *Slasher) GetCandidatesFromIndexer(ctx context.Context, epochStartHeight uint64) (state.CandidateList, error) {
	candidates, err := sh.indexer.CandidateList(epochStartHeight)
	if err != nil {
		return nil, err
	}
	if sh.hu.IsPre(config.Easter, epochStartHeight) {
		return candidates, nil
	}
	// After Easter height, kick-out unqualified delegates based on productivity
	kickoutList, err := sh.indexer.KickoutList(epochStartHeight)
	if err != nil {
		return nil, err
	}
	// recalculate the voting power for blacklist delegates
	return filterCandidates(candidates, kickoutList, epochStartHeight)
}

// GetBPFromIndexer returns BP list from indexer
func (sh *Slasher) GetBPFromIndexer(ctx context.Context, epochStartHeight uint64) (state.CandidateList, error) {
	candidates, err := sh.GetCandidatesFromIndexer(ctx, epochStartHeight)
	if err != nil {
		return nil, err
	}
	return sh.calculateBlockProducer(candidates)
}

// GetABPFromIndexer returns active BP list from indexer
func (sh *Slasher) GetABPFromIndexer(ctx context.Context, epochStartHeight uint64) (state.CandidateList, error) {
	blockProducers, err := sh.GetBPFromIndexer(ctx, epochStartHeight)
	if err != nil {
		return nil, err
	}
	return sh.calculateActiveBlockProducer(ctx, blockProducers, epochStartHeight)
}

// GetKickoutList returns the kick-out list at given epoch
func (sh *Slasher) GetKickoutList(ctx context.Context, sr protocol.StateReader, readFromNext bool) (*vote.Blacklist, error) {
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	targetHeight, err := sr.Height()
	if err != nil {
		return nil, err
	}
	// make sure it's epochStartHeight
	targetEpochStartHeight := rp.GetEpochHeight(rp.GetEpochHeight(targetHeight))
	if readFromNext {
		targetEpochNum := rp.GetEpochNum(targetEpochStartHeight) + 1
		targetEpochStartHeight = rp.GetEpochHeight(targetEpochNum) // next epoch start height
	}
	if sh.hu.IsPre(config.Easter, targetEpochStartHeight) {
		return nil, errors.New("Before Easter, there is no blacklist in stateDB")
	}
	unqualifiedList, stateHeight, err := sh.getKickoutList(sr, readFromNext)
	if err != nil {
		return nil, err
	}
	// to catch the corner case that since the new block is committed, shift occurs in the middle of processing the request
	if rp.GetEpochNum(targetEpochStartHeight) < rp.GetEpochNum(stateHeight) {
		return nil, errors.Wrap(ErrInconsistentHeight, "state factory tip epoch number became larger than target epoch number")
	}
	return unqualifiedList, nil
}

// CalculateKickoutList calculates kick-out list according to productivity
func (sh *Slasher) CalculateKickoutList(
	ctx context.Context,
	sm protocol.StateManager,
	epochNum uint64,
) (*vote.Blacklist, error) {
	rp := rolldpos.MustGetProtocol(protocol.MustGetRegistry(ctx))
	easterEpochNum := rp.GetEpochNum(sh.hu.EasterBlockHeight())

	nextBlacklist := &vote.Blacklist{
		IntensityRate: sh.kickoutIntensity,
	}
	upd, err := sh.getUnprodDelegate(sm)
	if err != nil {
		if errors.Cause(err) == state.ErrStateNotExist {
			if upd, err = vote.NewUnproductiveDelegate(sh.kickoutEpochPeriod, sh.maxKickoutPeriod); err != nil {
				return nil, errors.Wrap(err, "failed to make new upd")
			}
		} else {
			return nil, errors.Wrapf(err, "failed to read upd struct from state DB at epoch number %d", epochNum)
		}
	}
	unqualifiedDelegates := make(map[string]uint32)
	if epochNum <= easterEpochNum+sh.kickoutEpochPeriod {
		// if epoch number is smaller than easterEpochNum+K(kickout period), calculate it one-by-one (initialize).
		log.L().Debug("Before using kick-out blacklist",
			zap.Uint64("epochNum", epochNum),
			zap.Uint64("easterEpochNum", easterEpochNum),
			zap.Uint64("kickoutEpochPeriod", sh.kickoutEpochPeriod),
		)
		existinglist := upd.DelegateList()
		for _, listByEpoch := range existinglist {
			for _, addr := range listByEpoch {
				if _, ok := unqualifiedDelegates[addr]; !ok {
					unqualifiedDelegates[addr] = 1
				} else {
					unqualifiedDelegates[addr]++
				}
			}
		}
		// calculate upd of epochNum-1 (latest)
		uq, err := sh.calculateUnproductiveDelegatesByEpoch(ctx, sm, rp, epochNum-1)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to calculate current epoch upd %d", epochNum-1)
		}
		for _, addr := range uq {
			if _, ok := unqualifiedDelegates[addr]; !ok {
				unqualifiedDelegates[addr] = 1
			} else {
				unqualifiedDelegates[addr]++
			}
		}
		if err := upd.AddRecentUPD(uq); err != nil {
			return nil, errors.Wrap(err, "failed to add recent upd")
		}
		nextBlacklist.BlacklistInfos = unqualifiedDelegates
		return nextBlacklist, setUnproductiveDelegates(sm, upd)
	}
	// Blacklist[N] = Blacklist[N-1] - Low-productivity-list[N-K-1] + Low-productivity-list[N-1]
	log.L().Debug("Using kick-out blacklist",
		zap.Uint64("epochNum", epochNum),
		zap.Uint64("easterEpochNum", easterEpochNum),
		zap.Uint64("kickoutEpochPeriod", sh.kickoutEpochPeriod),
	)
	prevBlacklist, _, err := sh.getKickoutList(sm, false)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read latest kick-out list")
	}
	blacklistMap := prevBlacklist.BlacklistInfos
	if blacklistMap == nil {
		blacklistMap = make(map[string]uint32)
	}
	skipList := upd.ReadOldestUPD()
	for _, addr := range skipList {
		if _, ok := blacklistMap[addr]; !ok {
			log.L().Fatal("skipping list element doesn't exist among one of existing map")
			continue
		}
		blacklistMap[addr]--
	}
	addList, err := sh.calculateUnproductiveDelegatesByEpoch(ctx, sm, rp, epochNum-1)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to calculate current epoch upd %d", epochNum-1)
	}
	if err := upd.AddRecentUPD(addList); err != nil {
		return nil, errors.Wrap(err, "failed to add recent upd")
	}
	for _, addr := range addList {
		if _, ok := blacklistMap[addr]; ok {
			blacklistMap[addr]++
			continue
		}
		blacklistMap[addr] = 1
	}

	for addr, count := range blacklistMap {
		if count == 0 {
			delete(blacklistMap, addr)
		}
	}
	nextBlacklist.BlacklistInfos = blacklistMap
	return nextBlacklist, setUnproductiveDelegates(sm, upd)
}

func (sh *Slasher) calculateUnproductiveDelegatesByEpoch(ctx context.Context, sr protocol.StateReader, rp *rolldpos.Protocol, epochNum uint64) ([]string, error) {
	blkCtx := protocol.MustGetBlockCtx(ctx)
	bcCtx := protocol.MustGetBlockchainCtx(ctx)
	delegates, err := sh.GetActiveBlockProducers(ctx, sr, false)
	if err != nil {
		return nil, err
	}
	numBlks, produce, err := rp.ProductivityByEpoch(epochNum, bcCtx.Tip.Height, sh.productivity)
	if err != nil {
		return nil, err
	}
	// The current block is not included, so add it
	numBlks++
	if _, ok := produce[blkCtx.Producer.String()]; ok {
		produce[blkCtx.Producer.String()]++
	} else {
		produce[blkCtx.Producer.String()] = 1
	}

	for _, abp := range delegates {
		if _, ok := produce[abp.Address]; !ok {
			produce[abp.Address] = 0
		}
	}
	unqualified := make([]string, 0)
	expectedNumBlks := numBlks / uint64(len(produce))
	for addr, actualNumBlks := range produce {
		if actualNumBlks*100/expectedNumBlks < sh.prodThreshold {
			unqualified = append(unqualified, addr)
		}
	}
	return unqualified, nil
}

// calculateBlockProducer calculates block producer by given candidate list
func (sh *Slasher) calculateBlockProducer(candidates state.CandidateList) (state.CandidateList, error) {
	var blockProducers state.CandidateList
	for i, candidate := range candidates {
		if uint64(i) >= sh.numCandidateDelegates {
			break
		}
		if candidate.Votes.Cmp(big.NewInt(0)) == 0 {
			// if the voting power is 0, exclude from being a block producer(hard kickout)
			continue
		}
		blockProducers = append(blockProducers, candidate)
	}
	return blockProducers, nil
}

// calculateActiveBlockProducer calculates active block producer by given block producer list
func (sh *Slasher) calculateActiveBlockProducer(
	ctx context.Context,
	blockProducers state.CandidateList,
	epochStartHeight uint64,
) (state.CandidateList, error) {
	var blockProducerList []string
	blockProducerMap := make(map[string]*state.Candidate)
	for _, bp := range blockProducers {
		blockProducerList = append(blockProducerList, bp.Address)
		blockProducerMap[bp.Address] = bp
	}
	crypto.SortCandidates(blockProducerList, epochStartHeight, crypto.CryptoSeed)

	length := int(sh.numDelegates)
	if len(blockProducerList) < length {
		// TODO: if the number of delegates is smaller than expected, should it return error or not?
		length = len(blockProducerList)
		log.L().Warn(
			"the number of block producer is less than expected",
			zap.Int("actual block producer", len(blockProducerList)),
			zap.Uint64("expected", sh.numDelegates),
		)
	}
	var activeBlockProducers state.CandidateList
	for i := 0; i < length; i++ {
		activeBlockProducers = append(activeBlockProducers, blockProducerMap[blockProducerList[i]])
	}
	return activeBlockProducers, nil
}

// filterCandidates returns filtered candidate list by given raw candidate/ kick-out list
func filterCandidates(
	candidates state.CandidateList,
	unqualifiedList *vote.Blacklist,
	epochStartHeight uint64,
) (state.CandidateList, error) {
	candidatesMap := make(map[string]*state.Candidate)
	updatedVotingPower := make(map[string]*big.Int)
	intensityRate := float64(uint32(100)-unqualifiedList.IntensityRate) / float64(100)
	for _, cand := range candidates {
		filterCand := cand.Clone()
		if _, ok := unqualifiedList.BlacklistInfos[cand.Address]; ok {
			// if it is an unqualified delegate, multiply the voting power with kick-out intensity rate
			votingPower := new(big.Float).SetInt(filterCand.Votes)
			filterCand.Votes, _ = votingPower.Mul(votingPower, big.NewFloat(intensityRate)).Int(nil)
		}
		updatedVotingPower[filterCand.Address] = filterCand.Votes
		candidatesMap[filterCand.Address] = filterCand
	}
	// sort again with updated voting power
	sorted := util.Sort(updatedVotingPower, epochStartHeight)
	var verifiedCandidates state.CandidateList
	for _, name := range sorted {
		verifiedCandidates = append(verifiedCandidates, candidatesMap[name])
	}
	return verifiedCandidates, nil
}
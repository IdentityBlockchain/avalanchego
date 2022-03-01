// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposervm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ava-labs/avalanchego/codec"
	"github.com/ava-labs/avalanchego/codec/linearcodec"
	"github.com/ava-labs/avalanchego/codec/reflectcodec"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/ava-labs/avalanchego/utils/wrappers"
)

var (
	stateSyncCodec               codec.Manager
	errWrongStateSyncVersion     = errors.New("wrong state sync key version")
	errUnknownLastSummaryBlockID = errors.New("could not retrieve blockID associated with last summary")
	errBadLastSummaryBlock       = errors.New("could not parse last summary block")
)

func init() {
	lc := linearcodec.New(reflectcodec.DefaultTagName, math.MaxUint32)
	stateSyncCodec = codec.NewManager(math.MaxInt32)

	errs := wrappers.Errs{}
	errs.Add(
		lc.RegisterType(&common.Summary{}),
		lc.RegisterType(&common.SummaryHash{}),
		lc.RegisterType(&block.CoreSummaryContent{}),
		lc.RegisterType(&block.ProposerSummaryContent{}),
		stateSyncCodec.RegisterCodec(block.StateSyncDefaultKeysVersion, lc),
	)
	if err := errs.Err; err != nil {
		panic(err)
	}
}

func (vm *VM) RegisterStateSyncer(stateSyncers []ids.ShortID) error {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return common.ErrStateSyncableVMNotImplemented
	}

	return ssVM.RegisterStateSyncer(stateSyncers)
}

func (vm *VM) StateSyncEnabled() (bool, error) {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return false, common.ErrStateSyncableVMNotImplemented
	}

	return ssVM.StateSyncEnabled()
}

func (vm *VM) StateSyncGetLastSummary() (common.Summary, error) {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return common.Summary{}, common.ErrStateSyncableVMNotImplemented
	}

	// Extract core last state summary
	vmSummary, err := ssVM.StateSyncGetLastSummary()
	if err != nil {
		return common.Summary{}, err
	}

	proContent, err := vm.buildProContentFrom(vmSummary)
	if err != nil {
		return common.Summary{}, fmt.Errorf("could not build proposerVm Summary from core one due to: %w", err)
	}

	proSummary, err := stateSyncCodec.Marshal(block.StateSyncDefaultKeysVersion, &proContent)
	if err != nil {
		return common.Summary{}, fmt.Errorf("cannot marshal proposerVMKey due to: %w", err)
	}

	return proSummary, err
}

func (vm *VM) StateSyncGetKeyHash(summary common.Summary) (common.SummaryKey, common.SummaryHash, error) {
	if _, ok := vm.ChainVM.(block.StateSyncableVM); !ok {
		return common.SummaryKey{}, common.SummaryHash{}, common.ErrStateSyncableVMNotImplemented
	}

	proContent := block.ProposerSummaryContent{}
	ver, err := stateSyncCodec.Unmarshal(summary, &proContent)
	if err != nil {
		return common.SummaryKey{}, common.SummaryHash{}, fmt.Errorf("could not unmarshal ProposerSummaryContent due to: %w", err)
	}
	if ver != block.StateSyncDefaultKeysVersion {
		return common.SummaryKey{}, common.SummaryHash{}, errWrongStateSyncVersion
	}

	heightBytes := make([]byte, wrappers.LongLen)
	binary.BigEndian.PutUint64(heightBytes, proContent.CoreContent.Height)

	return heightBytes, hashing.ComputeHash256(summary), nil
}

func (vm *VM) StateSyncGetSummary(key common.SummaryKey) (common.Summary, error) {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return common.Summary{}, common.ErrStateSyncableVMNotImplemented
	}

	coreSummary, err := ssVM.StateSyncGetSummary(key)
	if err != nil {
		return common.Summary{}, fmt.Errorf("could not retrieve core summary due to: %w", err)
	}
	proContent, err := vm.buildProContentFrom(coreSummary)
	if err != nil {
		return common.Summary{}, fmt.Errorf("could not build proposerVm Summary from core one due to: %w", err)
	}

	proSummary, err := stateSyncCodec.Marshal(block.StateSyncDefaultKeysVersion, &proContent)
	if err != nil {
		return common.Summary{}, fmt.Errorf("cannot marshal proposerVMKey due to: %w", err)
	}

	return proSummary, err
}

func (vm *VM) StateSync(accepted []common.Summary) error {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return common.ErrStateSyncableVMNotImplemented
	}

	coreSummaries := make([]common.Summary, 0, len(accepted))
	vm.pendingSummariesBlockIDMapping = make(map[ids.ID]ids.ID)
	for _, summary := range accepted {
		proContent := block.ProposerSummaryContent{}
		ver, err := stateSyncCodec.Unmarshal(summary, &proContent)
		if err != nil {
			return err
		}
		if ver != block.StateSyncDefaultKeysVersion {
			return errWrongStateSyncVersion
		}

		coreSummary, err := stateSyncCodec.Marshal(block.StateSyncDefaultKeysVersion, proContent.CoreContent)
		if err != nil {
			return err
		}

		coreSummaries = append(coreSummaries, coreSummary)

		// record coreVm to proposerVM blockID mapping to be able to
		// complete state-sync by requesting lastSummaryBlockID.
		vm.pendingSummariesBlockIDMapping[proContent.CoreContent.BlkID] = proContent.ProBlkID
	}

	return ssVM.StateSync(coreSummaries)
}

func (vm *VM) GetLastSummaryBlockID() (ids.ID, error) {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return ids.Empty, common.ErrStateSyncableVMNotImplemented
	}

	coreBlkID, err := ssVM.GetLastSummaryBlockID()
	if err != nil {
		return ids.Empty, err
	}
	proBlkID, found := vm.pendingSummariesBlockIDMapping[coreBlkID]
	vm.ctx.Log.Info("coreToProBlkID mapping found %v", proBlkID.String())
	if !found {
		return ids.Empty, errUnknownLastSummaryBlockID
	}
	return proBlkID, nil
}

func (vm *VM) SetLastSummaryBlock(blkByte []byte) error {
	ssVM, ok := vm.ChainVM.(block.StateSyncableVM)
	if !ok {
		return common.ErrStateSyncableVMNotImplemented
	}

	// retrieve core block
	var (
		coreBlkBytes []byte
		blk          Block
		err          error
	)
	if blk, err = vm.parsePostForkBlock(blkByte); err == nil {
		coreBlkBytes = blk.getInnerBlk().Bytes()
	} else if blk, err = vm.parsePreForkBlock(blkByte); err == nil {
		coreBlkBytes = blk.Bytes()
	} else {
		return errBadLastSummaryBlock
	}

	if err := ssVM.SetLastSummaryBlock(coreBlkBytes); err != nil {
		return err
	}

	return blk.conditionalAccept(false /*acceptcoreBlk*/)
}

func (vm *VM) buildProContentFrom(coreSummary common.Summary) (block.ProposerSummaryContent, error) {
	coreContent := block.CoreSummaryContent{}
	ver, err := stateSyncCodec.Unmarshal(coreSummary, &coreContent)
	if err != nil {
		return block.ProposerSummaryContent{}, err
	}
	if ver != block.StateSyncDefaultKeysVersion {
		return block.ProposerSummaryContent{}, errWrongStateSyncVersion
	}

	// retrieve ProBlkID is available
	proBlkID, err := vm.GetBlockIDAtHeight(coreContent.Height)
	if err == database.ErrNotFound {
		// we must have hit the snowman++ fork. Check it.
		currentFork, err := vm.State.GetForkHeight()
		if err != nil {
			return block.ProposerSummaryContent{}, err
		}
		if coreContent.Height > currentFork {
			return block.ProposerSummaryContent{}, err
		}

		proBlkID = coreContent.BlkID
	}
	if err != nil {
		return block.ProposerSummaryContent{}, err
	}

	// Build ProposerSummaryContent
	return block.ProposerSummaryContent{
		ProBlkID:    proBlkID,
		CoreContent: coreContent,
	}, nil
}
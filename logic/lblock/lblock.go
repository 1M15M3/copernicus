package lblock

import (
	"errors"
	"github.com/copernet/copernicus/conf"
	"github.com/copernet/copernicus/errcode"
	"github.com/copernet/copernicus/log"
	"github.com/copernet/copernicus/logic/lblockindex"
	"github.com/copernet/copernicus/logic/lmerkleroot"
	"github.com/copernet/copernicus/logic/ltx"
	"github.com/copernet/copernicus/model"
	"github.com/copernet/copernicus/model/block"
	"github.com/copernet/copernicus/model/blockindex"
	"github.com/copernet/copernicus/model/chain"
	"github.com/copernet/copernicus/model/consensus"
	"github.com/copernet/copernicus/model/tx"
	"github.com/copernet/copernicus/model/versionbits"
	"github.com/copernet/copernicus/persist"
	"github.com/copernet/copernicus/persist/disk"
	"github.com/copernet/copernicus/util"
)

func GetBlockByIndex(bi *blockindex.BlockIndex, param *model.BitcoinParams) (blk *block.Block, err error) {
	blk, ret := disk.ReadBlockFromDisk(bi, param)
	if !ret {
		err = errors.New("disk.ReadBlockFromDisk error")
	}
	return
}

func WriteBlockToDisk(bi *blockindex.BlockIndex, bl *block.Block, inDbp *block.DiskBlockPos) (*block.DiskBlockPos, error) {

	height := bi.Height

	var pos *block.DiskBlockPos
	if inDbp == nil {
		pos = block.NewDiskBlockPos(0, 0)
	} else {
		pos = inDbp
	}

	flag := disk.FindBlockPos(pos, uint32(bl.SerializeSize())+4, height, uint64(bl.GetBlockHeader().Time), inDbp != nil)
	if !flag {
		log.Error("WriteBlockToDisk():FindBlockPos failed")
		return nil, errcode.ProjectError{Code: 2000}
	}

	if inDbp == nil {
		flag = disk.WriteBlockToDisk(bl, pos)
		if !flag {
			log.Error("WriteBlockToDisk():WriteBlockToDisk failed")
			return nil, errcode.ProjectError{Code: 2001}
		}
		log.Debug("block(hash: %s) data is written to disk", bl.GetHash())
	}
	return pos, nil
}

func getLockTime(block *block.Block, indexPrev *blockindex.BlockIndex) int64 {
	params := chain.GetInstance().GetParams()
	lockTimeFlags := 0
	if versionbits.VersionBitsState(indexPrev, params, consensus.DeploymentCSV, versionbits.VBCache) == versionbits.ThresholdActive {
		lockTimeFlags |= consensus.LocktimeMedianTimePast
	}

	var medianTimePast int64
	if indexPrev != nil {
		medianTimePast = indexPrev.GetMedianTimePast()
	}

	lockTimeCutoff := int64(block.Header.Time)
	if lockTimeFlags&consensus.LocktimeMedianTimePast != 0 {
		lockTimeCutoff = medianTimePast
	}

	return lockTimeCutoff
}

func CheckBlock(pblock *block.Block, checkHeader, checkMerlke bool) error {
	// These are checks that are independent of context.
	if pblock.Checked {
		return nil
	}
	bh := pblock.Header
	// Check that the header is valid (particularly PoW).  This is mostly
	// redundant with the call in AcceptBlockHeader.
	if checkHeader {
		if err := CheckBlockHeader(&bh); err != nil {
			return err
		}
	}

	// Check the merkle root.
	mutated := false
	if checkMerlke {
		hashMerkleRoot2 := lmerkleroot.BlockMerkleRoot(pblock.Txs, &mutated)
		if !bh.MerkleRoot.IsEqual(&hashMerkleRoot2) {
			log.Debug("ErrorBadTxMrklRoot")
			return errcode.NewError(errcode.RejectInvalid, "bad-txnmrklroot")
		}
	}

	// Check for merkle tree malleability (CVE-2012-2459): repeating
	// sequences of transactions in a lblock without affecting the merkle
	// root of a lblock, while still invalidating it.
	if mutated {
		log.Debug("ErrorbadTxnsDuplicate")
		return errcode.NewError(errcode.RejectInvalid, "bad-txns-duplicate")
	}

	// First transaction must be coinbase
	if len(pblock.Txs) == 0 {
		return errcode.NewError(errcode.RejectInvalid, "bad-cb-missing")
	}

	// size limits
	nMaxBlockSize := conf.Cfg.Excessiveblocksize
	// Bail early if there is no way this lblock is of reasonable size.
	minTransactionSize := tx.NewEmptyTx().EncodeSize()
	if uint64(len(pblock.Txs)*int(minTransactionSize)) > nMaxBlockSize {
		log.Debug("ErrorBadBlkLength")
		return errcode.NewError(errcode.RejectInvalid, "bad-blk-length")
	}

	currentBlockSize := pblock.EncodeSize()
	if uint64(currentBlockSize) > nMaxBlockSize {
		log.Debug("ErrorBadBlkTxSize")
		return errcode.NewError(errcode.RejectInvalid, "bad-blk-length")
	}

	nMaxBlockSigOps, errSig := consensus.GetMaxBlockSigOpsCount(uint64(currentBlockSize))
	if errSig != nil {
		return errSig
	}

	err := ltx.CheckBlockTransactions(pblock.Txs, nMaxBlockSigOps)
	if err != nil {
		log.Debug("ErrorBadBlkTx: %v", err)
		return err
	}
	pblock.Checked = true

	return nil
}

func ContextualCheckBlock(b *block.Block, indexPrev *blockindex.BlockIndex) error {
	bMonolithEnable := false
	if indexPrev != nil && model.IsMonolithEnabled(indexPrev.GetMedianTimePast()) {
		bMonolithEnable = true
	}

	if !bMonolithEnable {
		if b.EncodeSize() > 8*consensus.OneMegaByte {
			return errcode.NewError(errcode.RejectInvalid, "bad-blk-length")
		}
	}

	var height int32
	if indexPrev != nil {
		height = indexPrev.Height + 1
	}

	lockTimeCutoff := getLockTime(b, indexPrev)

	// Check that all transactions are finalized
	// Enforce rule that the coinBase starts with serialized lblock height
	err := ltx.ContextureCheckBlockTransactions(b.Txs, height, lockTimeCutoff)
	return err
}

// ReceivedBlockTransactions Mark a lblock as having its data received and checked (up to
// * BLOCK_VALID_TRANSACTIONS state).
func ReceivedBlockTransactions(pblock *block.Block,
	pindexNew *blockindex.BlockIndex, pos *block.DiskBlockPos) {
	pindexNew.TxCount = int32(len(pblock.Txs))
	pindexNew.ChainTxCount = 0
	pindexNew.File = pos.File
	pindexNew.DataPos = pos.Pos
	pindexNew.UndoPos = 0
	pindexNew.AddStatus(blockindex.BlockHaveData)
	pindexNew.RaiseValidity(blockindex.BlockValidTransactions)

	gPersist := persist.GetInstance()
	gPersist.AddDirtyBlockIndex(pindexNew)

	gChain := chain.GetInstance()
	if pindexNew.IsGenesis(gChain.GetParams()) || gChain.ParentInBranch(pindexNew) {
		// If indexNew is the genesis lblock or all parents are in branch
		err := gChain.AddToBranch(pindexNew)
		if err != nil {
			log.Error("add block index to branch error:%d", err)
			return
		}
	} else {
		if pindexNew.Prev.IsValid(blockindex.BlockValidTree) {
			err := gChain.AddToOrphan(pindexNew)
			if err != nil {
				log.Error("add block index to orphan error:%d", err)
				return
			}
		}
	}
}

// GetBlockScriptFlags Returns the lscript flags which should be checked for a given lblock
func GetBlockScriptFlags(pindex *blockindex.BlockIndex) uint32 {
	gChain := chain.GetInstance()
	return gChain.GetBlockScriptFlags(pindex)
}

// AcceptBlock Store a block on disk.
func AcceptBlock(pblock *block.Block, fRequested bool, inDbp *block.DiskBlockPos, fNewBlock *bool) (bIndex *blockindex.BlockIndex,
	outDbp *block.DiskBlockPos, err error) {
	if pblock != nil {
		*fNewBlock = false
	}
	bIndex, err = AcceptBlockHeader(&pblock.Header)
	if err != nil {
		return
	}

	// Already Accept Block
	if bIndex.HasData() {
		hash := pblock.GetHash()
		log.Warn("AcceptBlock blk(%s) already received", &hash)
		return
	}

	gChain := chain.GetInstance()
	if !fRequested {
		tip := gChain.Tip()
		fHasMoreWork := false
		if tip == nil {
			fHasMoreWork = true
		} else {
			tipWork := tip.ChainWork
			if bIndex.ChainWork.Cmp(&tipWork) == 1 {
				fHasMoreWork = true
			}
		}
		if !fHasMoreWork {
			log.Debug("AcceptBlockHeader err:%d", 3008)
			err = errcode.ProjectError{Code: 3008}
			return
		}
		fTooFarAhead := bIndex.Height > gChain.Height()+block.MinBlocksToKeep
		if fTooFarAhead {
			log.Debug("AcceptBlockHeader err:%d", 3007)
			err = errcode.ProjectError{Code: 3007}
			return
		}
	}

	*fNewBlock = true
	gPersist := persist.GetInstance()
	if err = CheckBlock(pblock, true, true); err != nil {
		bIndex.AddStatus(blockindex.BlockFailed)
		gPersist.AddDirtyBlockIndex(bIndex)
		return
	}
	if err = ContextualCheckBlock(pblock, bIndex.Prev); err != nil {
		bIndex.AddStatus(blockindex.BlockFailed)
		gPersist.AddDirtyBlockIndex(bIndex)
		return
	}

	// inDbp is nil indicate that this block haven't been write to disk
	// when reindex, inDbp is not nil, and outDbp will be same as inDbp, and block will not be write to disk
	outDbp, err = WriteBlockToDisk(bIndex, pblock, inDbp)
	if err != nil {
		log.Error("AcceptBlockHeader WriteBlockTo Disk err" + err.Error())
		return
	}

	ReceivedBlockTransactions(pblock, bIndex, outDbp)
	return
}

func AcceptBlockHeader(bh *block.BlockHeader) (*blockindex.BlockIndex, error) {
	gChain := chain.GetInstance()
	bIndex := gChain.FindBlockIndex(bh.GetHash())
	if bIndex != nil {
		if bIndex.IsInvalid() {
			log.Debug("AcceptBlockHeader Invalid index")
			return bIndex, errcode.New(errcode.ErrorBlockHeaderNoValid)
		}
		return bIndex, nil
	}

	// This maybe a new blockheader
	err := CheckBlockHeader(bh)
	if err != nil {
		return nil, err
	}

	bIndex = blockindex.NewBlockIndex(bh)
	if !bIndex.IsGenesis(gChain.GetParams()) {
		bIndex.Prev = gChain.FindBlockIndex(bh.HashPrevBlock)
		if bIndex.Prev == nil {
			log.Debug("Find Block in BlockIndexMap err, hash:%s", bh.HashPrevBlock)
			return nil, errcode.New(errcode.ErrorBlockHeaderNoParent)
		}
		if bIndex.Prev.IsInvalid() {
			log.Debug("AcceptBlockHeader Invalid Pre index")
			return nil, errcode.ProjectError{Code: 3100}
		}
		if err := lblockindex.CheckIndexAgainstCheckpoint(bIndex.Prev); err != nil {
			log.Debug("AcceptBlockHeader err:%d", 3100)
			return nil, errcode.ProjectError{Code: 3100}
		}
		if err = ContextualCheckBlockHeader(bh, bIndex.Prev, util.GetAdjustedTime()); err != nil {
			log.Debug("AcceptBlockHeader err:%d", 3101)
			return nil, err
		}
	}

	//bIndex.Height = bIndex.Prev.Height + 1
	//bIndex.TimeMax = util.MaxU32(bIndex.Prev.TimeMax, bIndex.Header.Time)
	//bIndex.AddStatus(lblockindex.StatusWaitingData)

	err = gChain.AddToIndexMap(bIndex)
	if err != nil {
		log.Debug("AcceptBlockHeader AddToIndexMap err")
		return nil, err
	}

	return bIndex, nil
}

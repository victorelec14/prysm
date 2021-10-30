package light

import (
	"github.com/pkg/errors"
	types "github.com/prysmaticlabs/eth2-types"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/encoding/bytesutil"
	"github.com/prysmaticlabs/prysm/network/forks"
	ethpb "github.com/prysmaticlabs/prysm/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/proto/prysm/v1alpha1/block"
	"github.com/prysmaticlabs/prysm/time/slots"
)

// Precomputed values for generalized indices.
const (
	FinalizedRootIndex     = 105
	NextSyncCommitteeIndex = 55
	PREV_DATA_MAX_SIZE     = 64
)

type clientStore struct {
	Snapshot     *ethpb.ClientSnapshot
	ValidUpdates []*ethpb.LightClientUpdate
}

type signatureData struct {
	slot          types.Slot
	forkVersion   []byte
	syncAggregate *ethpb.SyncAggregate
}

func (s *Service) onHead(postState state.BeaconStateAltair, head block.BeaconBlock) error {
	innerState, ok := postState.InnerStateUnsafe().(*ethpb.BeaconStateAltair)
	if !ok {
		return errors.New("not altair")
	}
	tr, err := innerState.GetTree()
	if err != nil {
		return err
	}
	header, err := block.BeaconBlockHeaderFromBlockInterface(head)
	if err != nil {
		return err
	}
	finalityBranch, err := tr.Prove(FinalizedRootIndex)
	if err != nil {
		return err
	}
	nextSyncCommitteeBranch, err := tr.Prove(NextSyncCommitteeIndex)
	if err != nil {
		return err
	}
	blkRoot, err := head.HashTreeRoot()
	if err != nil {
		return err
	}
	s.prevHeadData[blkRoot] = &ethpb.SyncAttestedData{
		Header:                  header,
		FinalityCheckpoint:      innerState.FinalizedCheckpoint,
		FinalityBranch:          finalityBranch.Hashes,
		NextSyncCommittee:       innerState.NextSyncCommittee,
		NextSyncCommitteeBranch: nextSyncCommitteeBranch.Hashes,
	}
	syncAttestedBlockRoot, err := helpers.BlockRootAtSlot(postState, innerState.Slot-1)
	if err != nil {
		return err
	}

	fork, err := forks.Fork(slots.ToEpoch(head.Slot()))
	if err != nil {
		return err
	}
	syncAggregate, err := head.Body().SyncAggregate()
	if err != nil {
		return err
	}
	sigData := &signatureData{
		slot:          head.Slot(),
		forkVersion:   fork.CurrentVersion,
		syncAggregate: syncAggregate,
	}
	// Recover attested data from prevData cache. If not found, this SyncAggregate is useless
	syncAttestedData, ok := s.prevHeadData[bytesutil.ToBytes32(syncAttestedBlockRoot)]
	if !ok {
		return errors.New("useless")
	}
	commmitteePeriodWithFinalized, err := s.persistBestFinalizedUpdate(syncAttestedData, sigData)
	if err != nil {
		return err
	}

	// Then, store the best non finalized update per period
	if err := s.persistBestNonFinalizedUpdate(syncAttestedData, sigData, commmitteePeriodWithFinalized); err != nil {
		return err
	}
	// Prune old prevHeadData
	if len(s.prevHeadData) > PREV_DATA_MAX_SIZE {
		for k := range s.prevHeadData {
			delete(s.prevHeadData, k)
			if len(s.prevHeadData) <= PREV_DATA_MAX_SIZE {
				break
			}
		}
	}
	return nil
}

func (s *Service) onFinalized(postState state.BeaconStateAltair, cpt *ethpb.Checkpoint) error {
	return nil
}

/**
// * Must subcribe to BeaconChain event `finalizedCheckpoint`.
// * Expects the block from `checkpoint.root` and the post state of the block, `block.stateRoot`
// *
// * NOTE: Must be called also on start with the current finalized checkpoint (may be genesis)
// */
//async onFinalized(
//checkpoint: phase0.Checkpoint,
//blockHeader: phase0.BeaconBlockHeader,
//postState: TreeBacked<altair.BeaconState>
//): Promise<void> {
//// Pre-compute the nextSyncCommitteeBranch for this checkpoint, it will never change
//await this.db.lightclientFinalizedCheckpoint.put(checkpoint.epoch, {
//header: blockHeader,
//nextSyncCommittee: postState.nextSyncCommittee,
//// Prove that the `nextSyncCommittee` is included in a finalized state "attested" by the current sync committee
//nextSyncCommitteeBranch: postState.tree.getSingleProof(BigInt(NEXT_SYNC_COMMITTEE_INDEX)),
//});
//
//// TODO: Prune `db.lightclientFinalizedCheckpoint` for epoch < checkpoint.epoch
//// No block will reference the previous finalized checkpoint anymore
//}

func (s *Service) persistBestFinalizedUpdate(syncAttestedData *ethpb.SyncAttestedData, sigData *signatureData) (uint64, error) {
	finalizedEpoch := syncAttestedData.FinalityCheckpoint.Epoch
	_ = finalizedEpoch
	// const finalizedData = await this.db.lightclientFinalizedCheckpoint.get(finalizedEpoch);
	var finalizedData *ethpb.LightClientUpdate
	if finalizedData == nil {
		return 0, nil
	}
	committeePeriod := slots.SyncCommitteePeriod(slots.ToEpoch(syncAttestedData.Header.Slot))
	signaturePeriod := slots.SyncCommitteePeriod(slots.ToEpoch(sigData.slot))
	if committeePeriod != signaturePeriod {
		return 0, nil
	}
	newUpdate := &ethpb.LightClientUpdate{
		Header:                  finalizedData.Header,
		NextSyncCommittee:       finalizedData.NextSyncCommittee,
		NextSyncCommitteeBranch: finalizedData.NextSyncCommitteeBranch,
		FinalityHeader:          syncAttestedData.Header,
		FinalityBranch:          syncAttestedData.FinalityBranch,
		SyncCommitteeBits:       sigData.syncAggregate.SyncCommitteeBits,
		SyncCommitteeSignature:  sigData.syncAggregate.SyncCommitteeSignature,
		ForkVersion:             sigData.forkVersion,
	}
	//const prevBestUpdate = await this.db.bestUpdatePerCommitteePeriod.get(committeePeriod);
	var prevBestUpdate *ethpb.LightClientUpdate
	if prevBestUpdate == nil || isBetterUpdate(prevBestUpdate, newUpdate) {
		//	this.db.bestUpdatePerCommitteePeriod.put(committeePeriod, newUpdate);
	}
	//const prevLatestUpdate = await this.db.latestFinalizedUpdate.get();
	var prevLatestUpdate *ethpb.LightClientUpdate
	if prevLatestUpdate == nil || isLatestBestFinalizedUpdate(prevLatestUpdate, newUpdate) {
		//	this.db.latestFinalizedUpdate.put(newUpdate);
	}
	return committeePeriod, nil
}

func (s Service) persistBestNonFinalizedUpdate(syncAttestedData *ethpb.SyncAttestedData, sigData *signatureData, period uint64) error {
	// TODO: Period can be nil, perhaps.
	committeePeriod := slots.SyncCommitteePeriod(slots.ToEpoch(syncAttestedData.Header.Slot))
	signaturePeriod := slots.SyncCommitteePeriod(slots.ToEpoch(sigData.slot))
	if committeePeriod != signaturePeriod {
		return nil
	}

	newUpdate := &ethpb.LightClientUpdate{
		Header:                  syncAttestedData.Header,
		NextSyncCommittee:       syncAttestedData.NextSyncCommittee,
		NextSyncCommitteeBranch: syncAttestedData.NextSyncCommitteeBranch,
		FinalityHeader:          nil,
		FinalityBranch:          nil,
		SyncCommitteeBits:       sigData.syncAggregate.SyncCommitteeBits,
		SyncCommitteeSignature:  sigData.syncAggregate.SyncCommitteeSignature,
		ForkVersion:             sigData.forkVersion,
	}

	// Optimization: If there's already a finalized update for this committee period, no need to
	// create a non-finalized update>
	if committeePeriod != period {
		//const prevBestUpdate = await this.db.bestUpdatePerCommitteePeriod.get(committeePeriod);
		var prevBestUpdate *ethpb.LightClientUpdate
		if prevBestUpdate == nil || isBetterUpdate(prevBestUpdate, newUpdate) {
			// this.db.bestUpdatePerCommitteePeriod.put(committeePeriod, newUpdate);
		}
	}

	// Store the latest update here overall. Not checking it's the best
	var prevLatestUpdate *ethpb.LightClientUpdate
	//const prevLatestUpdate = await this.db.latestNonFinalizedUpdate.get();
	if prevLatestUpdate == nil || isLatestBestNonFinalizedUpdate(prevLatestUpdate, newUpdate) {
		// TODO: Don't store nextCommittee, that can be fetched through getBestUpdates()
		// await this.db.latestNonFinalizedUpdate.put(newUpdate);
	}
	return nil
}
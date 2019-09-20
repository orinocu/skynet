package host

import (
	"encoding/json"
	"errors"
	"math/bits"
	"sort"
	"sync/atomic"
	"time"

	bolt "github.com/coreos/bbolt"
	"gitlab.com/NebulousLabs/fastrand"
	"gitlab.com/NebulousLabs/merkletree"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
)

// modifyLeafRanges sorts the input slice and resolves overlapping ranges.
// Overlapping ranges won't be merged but instead they will be shortened and the
// overlap will be turned into a new range.
//
// e.g. [1;3) and [2;4) will be turned into [1;2), [2;3), [3;4)
func modifyLeafRanges(ranges []merkletree.LeafRange) []merkletree.LeafRange {
	// Sort the ranges first.
	sortRanges := func(ranges []merkletree.LeafRange) {
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].Start != ranges[j].Start {
				return ranges[i].Start < ranges[j].Start
			}
			return ranges[i].End < ranges[j].End
		})
	}
	sortRanges(ranges)
	// Nothing to do if there are not at least 2 ranges.
	if len(ranges) <= 1 {
		return ranges
	}
	// Merge overlapping ranges.
	tmpRanges := make([]merkletree.LeafRange, len(ranges))
	copy(tmpRanges, ranges)
	ranges = ranges[:0]

	curr := tmpRanges[0]
	tmpRanges = tmpRanges[1:]
	for {
		// Check if we are done.
		if len(tmpRanges) == 0 {
			ranges = append(ranges, curr)
			break
		}
		// Get next element to compare curr to.
		next := tmpRanges[0]
		tmpRanges = tmpRanges[1:]
		// Check for duplicate.
		if curr.Start == next.Start && curr.End == next.End {
			continue // ignore
		}
		if len(ranges) > 0 && ranges[len(ranges)-1].Start == curr.Start && ranges[len(ranges)-1].End == curr.End {
			curr = next
			continue // ignore
		}
		// If they don't overlap there is nothing to do.
		if curr.End <= next.Start {
			ranges = append(ranges, curr)
			curr = next
			continue
		}
		// Resolve overlapping ranges and append them to tmpRanges.
		if curr.Start != next.Start {
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: curr.Start, End: next.Start})
		}
		if curr.End < next.End {
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: next.Start, End: curr.End})
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: curr.End, End: next.End})
		} else if curr.End > next.End {
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: next.Start, End: next.End})
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: next.End, End: curr.End})
		} else if curr.End == next.End {
			tmpRanges = append(tmpRanges, merkletree.LeafRange{Start: next.Start, End: curr.End})
		}
		sortRanges(tmpRanges)
		curr = tmpRanges[0]
		tmpRanges = tmpRanges[1:]
	}
	return ranges
}

// managedRPCLoopSettings writes an RPC response containing the host's
// settings.
func (h *Host) managedRPCLoopSettings(s *rpcSession) error {
	atomic.AddUint64(&h.atomicSettingsCalls, 1)
	s.extendDeadline(modules.NegotiateSettingsTime)

	h.mu.Lock()
	hes := h.externalSettings()
	h.mu.Unlock()
	js, _ := json.Marshal(hes)
	resp := modules.LoopSettingsResponse{
		Settings: js,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}
	return nil
}

// managedRPCLoopLock handles the LoopLock RPC.
func (h *Host) managedRPCLoopLock(s *rpcSession) error {
	s.extendDeadline(modules.NegotiateRecentRevisionTime)

	// Read the request.
	var req modules.LoopLockRequest
	if err := s.readRequest(&req, modules.RPCMinLen); err != nil {
		s.writeError(err)
		return err
	}

	// Another contract may already be locked; locking multiple contracts is
	// not allowed.
	if len(s.so.OriginTransactionSet) != 0 {
		err := errors.New("another contract is already locked")
		s.writeError(err)
		return err
	}

	// Sanity-check the lock timeout
	lockTimeout := time.Duration(req.Timeout) * time.Millisecond
	if lockTimeout > maxObligationLockTimeout {
		err := errors.New("lock timeout is too long")
		s.writeError(err)
		return err
	}

	var newSO storageObligation
	h.mu.RLock()
	err := h.db.View(func(tx *bolt.Tx) error {
		var err error
		newSO, err = getStorageObligation(tx, req.ContractID)
		return err
	})
	h.mu.RUnlock()
	if err != nil {
		s.writeError(errors.New("no record of that contract"))
		return extendErr("could get storage obligation "+req.ContractID.String()+": ", err)
	}

	// get the revision and signatures
	txn := newSO.RevisionTransactionSet[len(newSO.RevisionTransactionSet)-1]
	rev := txn.FileContractRevisions[0]
	var sigs []types.TransactionSignature
	for _, sig := range txn.TransactionSignatures {
		// The transaction may have additional signatures that are only
		// relevant to the host.
		if sig.ParentID == crypto.Hash(rev.ParentID) {
			sigs = append(sigs, sig)
		}
	}

	// verify the challenge response
	hash := crypto.HashAll(modules.RPCChallengePrefix, s.challenge)
	var renterPK crypto.PublicKey
	var renterSig crypto.Signature
	copy(renterPK[:], rev.UnlockConditions.PublicKeys[0].Key)
	copy(renterSig[:], req.Signature)
	if crypto.VerifyHash(hash, renterPK, renterSig) != nil {
		err := errors.New("challenge signature is invalid")
		s.writeError(err)
		return err
	}

	// attempt to lock the storage obligation
	lockErr := h.managedTryLockStorageObligation(req.ContractID, lockTimeout)
	if lockErr == nil {
		s.so = newSO
	}

	// Generate a new challenge.
	fastrand.Read(s.challenge[:])

	// Write the response.
	resp := modules.LoopLockResponse{
		Acquired:     lockErr == nil,
		NewChallenge: s.challenge,
		Revision:     rev,
		Signatures:   sigs,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}
	return nil
}

// managedRPCLoopUnlock handles the LoopUnlock RPC. No response is sent.
func (h *Host) managedRPCLoopUnlock(s *rpcSession) error {
	s.extendDeadline(modules.NegotiateSettingsTime)
	if len(s.so.OriginTransactionSet) != 0 {
		h.managedUnlockStorageObligation(s.so.id())
		s.so = storageObligation{}
	}
	return nil
}

// managedRPCLoopWrite reads an upload request and responds with a signature
// for the new revision.
func (h *Host) managedRPCLoopWrite(s *rpcSession) error {
	s.extendDeadline(modules.NegotiateFileContractRevisionTime)
	// Read the request.
	var req modules.LoopWriteRequest
	if err := s.readRequest(&req, modules.SectorSize*5); err != nil {
		// Reading may have failed due to a closed connection; regardless, it
		// doesn't hurt to try and tell the renter about it.
		s.writeError(err)
		return err
	}
	// If no Merkle proof was requested, the renter's signature should be
	// sent immediately.
	var sigResponse modules.LoopWriteResponse
	if !req.MerkleProof {
		if err := s.readResponse(&sigResponse, modules.RPCMinLen); err != nil {
			return err
		}
	}

	// Check that a contract is locked.
	if len(s.so.OriginTransactionSet) == 0 {
		err := errors.New("no contract locked")
		s.writeError(err)
		return err
	}

	// Read some internal fields for later.
	h.mu.Lock()
	blockHeight := h.blockHeight
	secretKey := h.secretKey
	settings := h.externalSettings()
	h.mu.Unlock()
	currentRevision := s.so.RevisionTransactionSet[len(s.so.RevisionTransactionSet)-1].FileContractRevisions[0]

	// Process each action.
	newRoots := append([]crypto.Hash(nil), s.so.SectorRoots...)
	sectorsChanged := make(map[uint64]struct{})             // indicates if a sector was modified in any way
	wholeSectorsChanged := make(map[uint64]struct{})        // indicates if a whole sector was modified
	segmentsChanged := make(map[uint64]map[uint64]struct{}) // indicates if a partial sector was modified
	var bandwidthRevenue types.Currency
	var sectorsRemoved []crypto.Hash
	var sectorsGained []crypto.Hash
	var gainedSectorData [][]byte
	for _, action := range req.Actions {
		switch action.Type {
		case modules.WriteActionAppend:
			if uint64(len(action.Data)) != modules.SectorSize {
				s.writeError(errBadSectorSize)
				return errBadSectorSize
			}
			// Update sector roots.
			newRoot := crypto.MerkleRoot(action.Data)
			newRoots = append(newRoots, newRoot)
			sectorsGained = append(sectorsGained, newRoot)
			gainedSectorData = append(gainedSectorData, action.Data)

			sectorsChanged[uint64(len(newRoots))-1] = struct{}{}
			wholeSectorsChanged[uint64(len(newRoots))-1] = struct{}{}

			// Update finances
			bandwidthRevenue = bandwidthRevenue.Add(settings.UploadBandwidthPrice.Mul64(modules.SectorSize))

		case modules.WriteActionTrim:
			numSectors := action.A
			if uint64(len(newRoots)) < numSectors {
				err := errors.New("trim size exceeds number of sectors")
				s.writeError(err)
				return err
			}
			// Update sector roots.
			sectorsRemoved = append(sectorsRemoved, newRoots[uint64(len(newRoots))-numSectors:]...)
			newRoots = newRoots[:uint64(len(newRoots))-numSectors]

			sectorsChanged[uint64(len(newRoots))] = struct{}{}
			wholeSectorsChanged[uint64(len(newRoots))] = struct{}{}

		case modules.WriteActionSwap:
			i, j := action.A, action.B
			if i >= uint64(len(newRoots)) || j >= uint64(len(newRoots)) {
				err := errors.New("illegal sector index")
				s.writeError(err)
				return err
			}
			// Update sector roots.
			newRoots[i], newRoots[j] = newRoots[j], newRoots[i]

			sectorsChanged[i] = struct{}{}
			sectorsChanged[j] = struct{}{}
			wholeSectorsChanged[i] = struct{}{}
			wholeSectorsChanged[j] = struct{}{}

		case modules.WriteActionUpdate:
			sectorIndex, offset, length := action.A, action.B, uint64(len(action.Data))
			if uint64(len(action.Data)) > modules.SectorSize || len(action.Data)%crypto.SegmentSize != 0 {
				s.writeError(errBadSectorSize)
				return errBadSectorSize
			}
			if sectorIndex >= uint64(len(newRoots)) {
				err := errors.New("illegal sector index or offset")
				s.writeError(err)
				return err
			} else if offset+uint64(len(action.Data)) > modules.SectorSize {
				s.writeError(errIllegalOffsetAndLength)
				return errIllegalOffsetAndLength
			}
			// Update sector roots.
			sector, err := h.ReadSector(newRoots[sectorIndex])
			if err != nil {
				s.writeError(err)
				return err
			}
			// Remember which sector and segments changed.
			start := offset / crypto.SegmentSize
			end := (offset + length) / crypto.SegmentSize
			if (offset+length)%crypto.SegmentSize != 0 {
				end++
			}
			for segmentIndex := start; segmentIndex < end; segmentIndex++ {
				if _, exists := segmentsChanged[sectorIndex]; !exists {
					segmentsChanged[sectorIndex] = make(map[uint64]struct{})
				}
				if _, exists := segmentsChanged[sectorIndex][segmentIndex]; !exists {
					segmentsChanged[sectorIndex][segmentIndex] = struct{}{}
				}
			}
			sectorsChanged[sectorIndex] = struct{}{}
			// Update the sector
			copy(sector[offset:], action.Data)
			newRoot := crypto.MerkleRoot(sector)
			sectorsRemoved = append(sectorsRemoved, newRoots[sectorIndex])
			sectorsGained = append(sectorsGained, newRoot)
			gainedSectorData = append(gainedSectorData, sector)
			newRoots[sectorIndex] = newRoot

			// Update finances.
			bandwidthRevenue = bandwidthRevenue.Add(settings.UploadBandwidthPrice.Mul64(uint64(len(action.Data))))

		default:
			err := errors.New("unknown action type " + action.Type.String())
			s.writeError(err)
			return err
		}
	}

	// Update finances.
	var storageRevenue, newCollateral types.Currency
	if len(newRoots) > len(s.so.SectorRoots) {
		bytesAdded := modules.SectorSize * uint64(len(newRoots)-len(s.so.SectorRoots))
		blocksRemaining := s.so.proofDeadline() - blockHeight
		blockBytesCurrency := types.NewCurrency64(uint64(blocksRemaining)).Mul64(bytesAdded)
		storageRevenue = settings.StoragePrice.Mul(blockBytesCurrency)
		newCollateral = newCollateral.Add(settings.Collateral.Mul(blockBytesCurrency))
	}

	// If a Merkle proof was requested, construct it.
	newMerkleRoot := cachedMerkleRoot(newRoots)
	var merkleResp modules.LoopWriteMerkleProof
	segmentsPerSector := modules.SectorSize / crypto.SegmentSize
	if req.MerkleProof {
		// Calculate which sectors changed.
		oldNumSectors := uint64(len(s.so.SectorRoots))
		segmentProofRanges := make([]crypto.ProofRange, 0, len(wholeSectorsChanged)+len(segmentsChanged))
		for index := range sectorsChanged {
			// Ignore changes out of range of our pre-modification sectors.
			if index >= oldNumSectors {
				continue
			}
			// If the whole sector changed we add a range for the whole thing.
			if _, exists := wholeSectorsChanged[index]; exists {
				segmentProofRanges = append(segmentProofRanges, crypto.ProofRange{
					Start: index * segmentsPerSector,
					End:   index*segmentsPerSector + segmentsPerSector,
				})
			}
			// Add sub-sector ranges.
			if segments, exists := segmentsChanged[index]; exists {
				for segmentIndex := range segments {
					segmentProofRanges = append(segmentProofRanges, crypto.ProofRange{
						Start: index*segmentsPerSector + segmentIndex,
						End:   index*segmentsPerSector + segmentIndex + 1,
					})
				}
			}
		}
		// Merge overlapping ranges.
		segmentProofRanges = modifyLeafRanges(segmentProofRanges)
		// Record old hashes for all changed sectors.
		var oldLeafHashes []crypto.Hash
		var oldSectorRoots []crypto.Hash
		var modifiedSegments [][]byte
		currentSector := uint64(0)
		for _, r := range segmentProofRanges {
			// add sector hashes for the gaps that are not covered by proof ranges.
			sectorIndex := r.Start / segmentsPerSector
			for index := currentSector; index < sectorIndex; index++ {
				oldSectorRoots = append(oldSectorRoots, s.so.SectorRoots[index])
			}
			currentSector = r.End / segmentsPerSector
			// add sector hash
			if r.End-r.Start == segmentsPerSector {
				oldLeafHashes = append(oldLeafHashes, s.so.SectorRoots[sectorIndex])
				oldSectorRoots = append(oldSectorRoots, s.so.SectorRoots[sectorIndex])
				continue
			}
			// add segments
			sector, err := h.ReadSector(s.so.SectorRoots[sectorIndex])
			if err != nil {
				s.writeError(err)
				return err
			}
			for segmentIndex := uint64(0); segmentIndex < segmentsPerSector; segmentIndex++ {
				segment := sector[segmentIndex*crypto.SegmentSize:][:crypto.SegmentSize]
				if _, exists := segmentsChanged[sectorIndex][segmentIndex]; exists {
					oldLeafHashes = append(oldLeafHashes, crypto.MerkleRoot(segment))
				}
				modifiedSegments = append(modifiedSegments, segment)
			}
		}
		for index := currentSector; index < oldNumSectors; index++ {
			oldSectorRoots = append(oldSectorRoots, s.so.SectorRoots[index])
		}
		// Construct the Merkle proof.
		merkleResp = modules.LoopWriteMerkleProof{
			OldSubtreeHashes: crypto.MerkleDiffProof(segmentProofRanges, oldSectorRoots, modifiedSegments, int(segmentsPerSector)),
			OldLeafHashes:    oldLeafHashes,
			NewMerkleRoot:    newMerkleRoot,
		}
		// Calculate bandwidth cost of proof.
		proofSize := crypto.HashSize * (len(merkleResp.OldSubtreeHashes) + len(oldLeafHashes) + 1)
		if proofSize < modules.RPCMinLen {
			proofSize = modules.RPCMinLen
		}
		bandwidthRevenue = bandwidthRevenue.Add(settings.DownloadBandwidthPrice.Mul64(uint64(proofSize)))
	}

	// construct the new revision
	newRevision := currentRevision
	newRevision.NewRevisionNumber = req.NewRevisionNumber
	for _, action := range req.Actions {
		if action.Type == modules.WriteActionAppend {
			newRevision.NewFileSize += modules.SectorSize
		} else if action.Type == modules.WriteActionTrim {
			newRevision.NewFileSize -= modules.SectorSize * action.A
		}
	}
	newRevision.NewFileMerkleRoot = newMerkleRoot
	newRevision.NewValidProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewValidProofOutputs))
	for i := range newRevision.NewValidProofOutputs {
		newRevision.NewValidProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewValidProofValues[i],
			UnlockHash: currentRevision.NewValidProofOutputs[i].UnlockHash,
		}
	}
	newRevision.NewMissedProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewMissedProofOutputs))
	for i := range newRevision.NewMissedProofOutputs {
		newRevision.NewMissedProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewMissedProofValues[i],
			UnlockHash: currentRevision.NewMissedProofOutputs[i].UnlockHash,
		}
	}

	// verify the new revision
	newRevenue := settings.BaseRPCPrice.Add(storageRevenue).Add(bandwidthRevenue)
	s.so.SectorRoots, newRoots = newRoots, s.so.SectorRoots // verifyRevision assumes new roots
	err := verifyRevision(s.so, newRevision, blockHeight, newRevenue, newCollateral)
	s.so.SectorRoots, newRoots = newRoots, s.so.SectorRoots
	if err != nil {
		s.writeError(err)
		return err
	}

	// If a Merkle proof was requested, send it and wait for the renter's signature.
	if req.MerkleProof {
		if err := s.writeResponse(merkleResp); err != nil {
			return err
		} else if err := s.readResponse(&sigResponse, modules.RPCMinLen); err != nil {
			return err
		}
	}

	// Sign the new revision.
	renterSig := types.TransactionSignature{
		ParentID:       crypto.Hash(newRevision.ParentID),
		CoveredFields:  types.CoveredFields{FileContractRevisions: []uint64{0}},
		PublicKeyIndex: 0,
		Signature:      sigResponse.Signature,
	}
	txn, err := createRevisionSignature(newRevision, renterSig, secretKey, blockHeight)
	if err != nil {
		s.writeError(err)
		return err
	}

	// Update the storage obligation.
	s.so.SectorRoots = newRoots
	s.so.PotentialStorageRevenue = s.so.PotentialStorageRevenue.Add(storageRevenue)
	s.so.RiskedCollateral = s.so.RiskedCollateral.Add(newCollateral)
	s.so.PotentialUploadRevenue = s.so.PotentialUploadRevenue.Add(bandwidthRevenue)
	s.so.RevisionTransactionSet = []types.Transaction{txn}
	h.mu.Lock()
	err = h.modifyStorageObligation(s.so, sectorsRemoved, sectorsGained, gainedSectorData)
	h.mu.Unlock()
	if err != nil {
		s.writeError(err)
		return err
	}

	// Send the response.
	resp := modules.LoopWriteResponse{
		Signature: txn.TransactionSignatures[1].Signature,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}
	return nil
}

// managedRPCLoopRead writes an RPC response containing the requested data
// (along with signatures and an optional Merkle proof).
func (h *Host) managedRPCLoopRead(s *rpcSession) error {
	s.extendDeadline(modules.NegotiateDownloadTime)

	// Read the request.
	var req modules.LoopReadRequest
	if err := s.readRequest(&req, modules.RPCMinLen); err != nil {
		// Reading may have failed due to a closed connection; regardless, it
		// doesn't hurt to try and tell the renter about it.
		s.writeError(err)
		return err
	}

	// As soon as we finish reading the request, we must begin listening for
	// RPCLoopReadStop, which may arrive at any time, and must arrive before the
	// RPC is considered complete.
	stopSignal := make(chan error, 1)
	go func() {
		var id types.Specifier
		err := s.readResponse(&id, modules.RPCMinLen)
		if err != nil {
			stopSignal <- err
		} else if id != modules.RPCLoopReadStop {
			stopSignal <- errors.New("expected 'stop' from renter, got " + id.String())
		} else {
			stopSignal <- nil
		}
	}()

	// Check that a contract is locked.
	if len(s.so.OriginTransactionSet) == 0 {
		err := errors.New("no contract locked")
		s.writeError(err)
		<-stopSignal
		return err
	}

	// Read some internal fields for later.
	h.mu.Lock()
	blockHeight := h.blockHeight
	secretKey := h.secretKey
	settings := h.externalSettings()
	h.mu.Unlock()
	currentRevision := s.so.RevisionTransactionSet[len(s.so.RevisionTransactionSet)-1].FileContractRevisions[0]

	// Validate the request.
	for _, sec := range req.Sections {
		var err error
		switch {
		case uint64(sec.Offset)+uint64(sec.Length) > modules.SectorSize:
			err = errRequestOutOfBounds
		case sec.Length == 0:
			err = errors.New("length cannot be zero")
		case req.MerkleProof && (sec.Offset%crypto.SegmentSize != 0 || sec.Length%crypto.SegmentSize != 0):
			err = errors.New("offset and length must be multiples of SegmentSize when requesting a Merkle proof")
		case len(req.NewValidProofValues) != len(currentRevision.NewValidProofOutputs):
			err = errors.New("wrong number of valid proof values")
		case len(req.NewMissedProofValues) != len(currentRevision.NewMissedProofOutputs):
			err = errors.New("wrong number of missed proof values")
		}
		if err != nil {
			s.writeError(err)
			return err
		}
	}

	// construct the new revision
	newRevision := currentRevision
	newRevision.NewRevisionNumber = req.NewRevisionNumber
	newRevision.NewValidProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewValidProofOutputs))
	for i := range newRevision.NewValidProofOutputs {
		newRevision.NewValidProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewValidProofValues[i],
			UnlockHash: currentRevision.NewValidProofOutputs[i].UnlockHash,
		}
	}
	newRevision.NewMissedProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewMissedProofOutputs))
	for i := range newRevision.NewMissedProofOutputs {
		newRevision.NewMissedProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewMissedProofValues[i],
			UnlockHash: currentRevision.NewMissedProofOutputs[i].UnlockHash,
		}
	}

	// calculate expected cost and verify against renter's revision
	var estBandwidth uint64
	sectorAccesses := make(map[crypto.Hash]struct{})
	for _, sec := range req.Sections {
		// use the worst-case proof size of 2*tree depth (this occurs when
		// proving across the two leaves in the center of the tree)
		estHashesPerProof := 2 * bits.Len64(modules.SectorSize/crypto.SegmentSize)
		estBandwidth += uint64(sec.Length) + uint64(estHashesPerProof*crypto.HashSize)
		sectorAccesses[sec.MerkleRoot] = struct{}{}
	}
	if estBandwidth < modules.RPCMinLen {
		estBandwidth = modules.RPCMinLen
	}
	bandwidthCost := settings.DownloadBandwidthPrice.Mul64(estBandwidth)
	sectorAccessCost := settings.SectorAccessPrice.Mul64(uint64(len(sectorAccesses)))
	totalCost := settings.BaseRPCPrice.Add(bandwidthCost).Add(sectorAccessCost)
	err := verifyPaymentRevision(currentRevision, newRevision, blockHeight, totalCost)
	if err != nil {
		s.writeError(err)
		return err
	}

	// Sign the new revision.
	renterSig := types.TransactionSignature{
		ParentID:       crypto.Hash(newRevision.ParentID),
		CoveredFields:  types.CoveredFields{FileContractRevisions: []uint64{0}},
		PublicKeyIndex: 0,
		Signature:      req.Signature,
	}
	txn, err := createRevisionSignature(newRevision, renterSig, secretKey, blockHeight)
	if err != nil {
		s.writeError(err)
		return err
	}
	hostSig := txn.TransactionSignatures[1].Signature

	// Update the storage obligation.
	paymentTransfer := currentRevision.NewValidProofOutputs[0].Value.Sub(newRevision.NewValidProofOutputs[0].Value)
	s.so.PotentialDownloadRevenue = s.so.PotentialDownloadRevenue.Add(paymentTransfer)
	s.so.RevisionTransactionSet = []types.Transaction{txn}
	h.mu.Lock()
	err = h.modifyStorageObligation(s.so, nil, nil, nil)
	h.mu.Unlock()
	if err != nil {
		s.writeError(err)
		return err
	}

	// enter response loop
	for i, sec := range req.Sections {
		// Fetch the requested data.
		sectorData, err := h.ReadSector(sec.MerkleRoot)
		if err != nil {
			s.writeError(err)
			return err
		}
		data := sectorData[sec.Offset : sec.Offset+sec.Length]

		// Construct the Merkle proof, if requested.
		var proof []crypto.Hash
		if req.MerkleProof {
			proofStart := int(sec.Offset) / crypto.SegmentSize
			proofEnd := int(sec.Offset+sec.Length) / crypto.SegmentSize
			proof = crypto.MerkleRangeProof(sectorData, proofStart, proofEnd)
		}

		// Send the response. If the renter sent a stop signal, or this is the
		// final response, include our signature in the response.
		resp := modules.LoopReadResponse{
			Signature:   nil,
			Data:        data,
			MerkleProof: proof,
		}
		select {
		case err := <-stopSignal:
			if err != nil {
				return err
			}
			resp.Signature = hostSig
			return s.writeResponse(resp)
		default:
		}
		if i == len(req.Sections)-1 {
			resp.Signature = hostSig
		}
		if err := s.writeResponse(resp); err != nil {
			return err
		}
	}
	// The stop signal must arrive before RPC is complete.
	return <-stopSignal
}

// managedRPCLoopFormContract handles the contract formation RPC.
func (h *Host) managedRPCLoopFormContract(s *rpcSession) error {
	// NOTE: this RPC contains two request/response exchanges.
	s.extendDeadline(modules.NegotiateFileContractTime)

	// Read the contract request.
	var req modules.LoopFormContractRequest
	if err := s.readRequest(&req, modules.TransactionSetSizeLimit); err != nil {
		s.writeError(err)
		return err
	}

	h.mu.Lock()
	settings := h.externalSettings()
	h.mu.Unlock()
	if !settings.AcceptingContracts {
		s.writeError(errors.New("host is not accepting new contracts"))
		return nil
	}

	// The host verifies that the file contract coming over the wire is
	// acceptable.
	txnSet := req.Transactions
	var renterPK crypto.PublicKey
	copy(renterPK[:], req.RenterKey.Key)
	if err := h.managedVerifyNewContract(txnSet, renterPK, settings); err != nil {
		s.writeError(err)
		return err
	}
	// The host adds collateral to the transaction.
	txnBuilder, newParents, newInputs, newOutputs, err := h.managedAddCollateral(settings, txnSet)
	if err != nil {
		s.writeError(err)
		return err
	}
	// Send any new inputs and outputs that were added to the transaction.
	resp := modules.LoopContractAdditions{
		Parents: newParents,
		Inputs:  newInputs,
		Outputs: newOutputs,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}

	// The renter will now send transaction signatures for the file contract
	// transaction and a signature for the implicit no-op file contract
	// revision.
	var renterSigs modules.LoopContractSignatures
	if err := s.readResponse(&renterSigs, modules.RPCMinLen); err != nil {
		s.writeError(err)
		return err
	}

	// The host adds the renter transaction signatures, then signs the
	// transaction and submits it to the blockchain, creating a storage
	// obligation in the process.
	h.mu.RLock()
	hostCollateral := contractCollateral(settings, txnSet[len(txnSet)-1].FileContracts[0])
	h.mu.RUnlock()
	hostTxnSignatures, hostRevisionSignature, newSOID, err := h.managedFinalizeContract(txnBuilder, renterPK, renterSigs.ContractSignatures, renterSigs.RevisionSignature, nil, hostCollateral, types.ZeroCurrency, types.ZeroCurrency, settings)
	if err != nil {
		s.writeError(err)
		return err
	}
	defer h.managedUnlockStorageObligation(newSOID)

	// Send our signatures for the contract transaction and initial revision.
	hostSigs := modules.LoopContractSignatures{
		ContractSignatures: hostTxnSignatures,
		RevisionSignature:  hostRevisionSignature,
	}
	if err := s.writeResponse(hostSigs); err != nil {
		return err
	}

	return nil
}

// managedRPCLoopRenewContract handles the LoopRenewContract RPC.
func (h *Host) managedRPCLoopRenewContract(s *rpcSession) error {
	// NOTE: this RPC contains two request/response exchanges.
	s.extendDeadline(modules.NegotiateRenewContractTime)

	// Read the renewal request.
	var req modules.LoopRenewContractRequest
	if err := s.readRequest(&req, modules.TransactionSetSizeLimit); err != nil {
		s.writeError(err)
		return err
	}

	h.mu.Lock()
	settings := h.externalSettings()
	h.mu.Unlock()
	if !settings.AcceptingContracts {
		s.writeError(errors.New("host is not accepting new contracts"))
		return nil
	} else if len(s.so.RevisionTransactionSet) == 0 {
		err := errors.New("no such contract")
		s.writeError(err)
		return err
	}

	// Verify that the transaction coming over the wire is a proper renewal.
	var renterPK crypto.PublicKey
	copy(renterPK[:], req.RenterKey.Key)
	err := h.managedVerifyRenewedContract(s.so, req.Transactions, renterPK)
	if err != nil {
		s.writeError(err)
		return extendErr("verification of renewal failed: ", err)
	}
	txnBuilder, newParents, newInputs, newOutputs, err := h.managedAddRenewCollateral(s.so, settings, req.Transactions)
	if err != nil {
		s.writeError(err)
		return extendErr("failed to add collateral: ", err)
	}
	// Send any new inputs and outputs that were added to the transaction.
	resp := modules.LoopContractAdditions{
		Parents: newParents,
		Inputs:  newInputs,
		Outputs: newOutputs,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}

	// The renter will now send transaction signatures for the file contract
	// transaction and a signature for the implicit no-op file contract
	// revision.
	var renterSigs modules.LoopContractSignatures
	if err := s.readResponse(&renterSigs, modules.RPCMinLen); err != nil {
		s.writeError(err)
		return err
	}

	// The host adds the renter transaction signatures, then signs the
	// transaction and submits it to the blockchain, creating a storage
	// obligation in the process.
	h.mu.RLock()
	fc := req.Transactions[len(req.Transactions)-1].FileContracts[0]
	renewCollateral := renewContractCollateral(s.so, settings, fc)
	renewRevenue := renewBasePrice(s.so, settings, fc)
	renewRisk := renewBaseCollateral(s.so, settings, fc)
	h.mu.RUnlock()
	hostTxnSignatures, hostRevisionSignature, newSOID, err := h.managedFinalizeContract(txnBuilder, renterPK, renterSigs.ContractSignatures, renterSigs.RevisionSignature, s.so.SectorRoots, renewCollateral, renewRevenue, renewRisk, settings)
	if err != nil {
		s.writeError(err)
		return extendErr("failed to finalize contract: ", err)
	}
	defer h.managedUnlockStorageObligation(newSOID)

	// Send our signatures for the contract transaction and initial revision.
	hostSigs := modules.LoopContractSignatures{
		ContractSignatures: hostTxnSignatures,
		RevisionSignature:  hostRevisionSignature,
	}
	if err := s.writeResponse(hostSigs); err != nil {
		return err
	}

	return nil
}

// managedRPCLoopSectorRoots writes an RPC response containing the requested
// contract roots (along with signatures and a Merkle proof).
func (h *Host) managedRPCLoopSectorRoots(s *rpcSession) error {
	s.extendDeadline(modules.NegotiateDownloadTime)

	// Read the request.
	var req modules.LoopSectorRootsRequest
	if err := s.readRequest(&req, modules.RPCMinLen); err != nil {
		// Reading may have failed due to a closed connection; regardless, it
		// doesn't hurt to try and tell the renter about it.
		s.writeError(err)
		return err
	}

	// Check that a contract is locked.
	if len(s.so.OriginTransactionSet) == 0 {
		err := errors.New("no contract locked")
		s.writeError(err)
		return err
	}

	// Read some internal fields for later.
	h.mu.Lock()
	blockHeight := h.blockHeight
	secretKey := h.secretKey
	settings := h.externalSettings()
	h.mu.Unlock()
	currentRevision := s.so.RevisionTransactionSet[len(s.so.RevisionTransactionSet)-1].FileContractRevisions[0]

	// Validate the request.
	var err error
	if req.NumRoots > settings.MaxDownloadBatchSize/crypto.HashSize {
		err = errLargeDownloadBatch
	}
	if req.RootOffset > uint64(len(s.so.SectorRoots)) || req.RootOffset+req.NumRoots > uint64(len(s.so.SectorRoots)) {
		err = errRequestOutOfBounds
	} else if len(req.NewValidProofValues) != len(currentRevision.NewValidProofOutputs) {
		err = errors.New("wrong number of valid proof values")
	} else if len(req.NewMissedProofValues) != len(currentRevision.NewMissedProofOutputs) {
		err = errors.New("wrong number of missed proof values")
	}
	if err != nil {
		s.writeError(err)
		return extendErr("download iteration request failed: ", err)
	}

	// Fetch the roots and construct the Merkle proof
	contractRoots := s.so.SectorRoots[req.RootOffset:][:req.NumRoots]
	proofStart := int(req.RootOffset)
	proofEnd := int(req.RootOffset + req.NumRoots)
	proof := crypto.MerkleSectorRangeProof(s.so.SectorRoots, proofStart, proofEnd)

	// construct the new revision
	newRevision := currentRevision
	newRevision.NewRevisionNumber = req.NewRevisionNumber
	newRevision.NewValidProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewValidProofOutputs))
	for i := range newRevision.NewValidProofOutputs {
		newRevision.NewValidProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewValidProofValues[i],
			UnlockHash: currentRevision.NewValidProofOutputs[i].UnlockHash,
		}
	}
	newRevision.NewMissedProofOutputs = make([]types.SiacoinOutput, len(currentRevision.NewMissedProofOutputs))
	for i := range newRevision.NewMissedProofOutputs {
		newRevision.NewMissedProofOutputs[i] = types.SiacoinOutput{
			Value:      req.NewMissedProofValues[i],
			UnlockHash: currentRevision.NewMissedProofOutputs[i].UnlockHash,
		}
	}

	// calculate expected cost and verify against renter's revision
	responseSize := (req.NumRoots + uint64(len(proof))) * crypto.HashSize
	if responseSize < modules.RPCMinLen {
		responseSize = modules.RPCMinLen
	}
	bandwidthCost := settings.DownloadBandwidthPrice.Mul64(responseSize)
	totalCost := settings.BaseRPCPrice.Add(bandwidthCost)
	err = verifyPaymentRevision(currentRevision, newRevision, blockHeight, totalCost)
	if err != nil {
		s.writeError(err)
		return extendErr("payment validation failed: ", err)
	}

	// Sign the new revision.
	renterSig := types.TransactionSignature{
		ParentID:       crypto.Hash(newRevision.ParentID),
		CoveredFields:  types.CoveredFields{FileContractRevisions: []uint64{0}},
		PublicKeyIndex: 0,
		Signature:      req.Signature,
	}
	txn, err := createRevisionSignature(newRevision, renterSig, secretKey, blockHeight)
	if err != nil {
		s.writeError(err)
		return extendErr("failed to create revision signature: ", err)
	}

	// Update the storage obligation.
	paymentTransfer := currentRevision.NewValidProofOutputs[0].Value.Sub(newRevision.NewValidProofOutputs[0].Value)
	s.so.PotentialDownloadRevenue = s.so.PotentialDownloadRevenue.Add(paymentTransfer)
	s.so.RevisionTransactionSet = []types.Transaction{txn}
	h.mu.Lock()
	err = h.modifyStorageObligation(s.so, nil, nil, nil)
	h.mu.Unlock()
	if err != nil {
		s.writeError(err)
		return extendErr("failed to modify storage obligation: ", err)
	}

	// send the response
	resp := modules.LoopSectorRootsResponse{
		Signature:   txn.TransactionSignatures[1].Signature,
		SectorRoots: contractRoots,
		MerkleProof: proof,
	}
	if err := s.writeResponse(resp); err != nil {
		return err
	}
	return nil
}

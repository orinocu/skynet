package contractmanager

import (
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/writeaheadlog"
)

var (
	addStorageFolderUpdateName    = "AddStorageFolderUpdate"
	sectorMDUpdateName            = "SectorMetadataUpdate"
	sectorDataUpdateName          = "SectorDataUpdate"
	removeStorageFolderUpdateName = "RemoveStorageFolderUpdate"
	growStorageFolderUpdateName   = "GrowStorageFolderUpdate"
	shrinkStorageFolderUpdateName = "ShrinkStorageFolderUpdate"
)

type (
	// sectorUpdate is an idempotent update to the sector metadata.
	sectorUpdate struct {
		Count  uint16
		Folder uint16
		ID     sectorID
		Index  uint32
	}
	// walUpdate wraps a writeaheadlog.Update and adds a file to be able to
	// reuse open file handles when applying the update.
	walUpdate struct {
		writeaheadlog.Update
		f modules.File
	}
)

// addStorageFolderUpdate creates a WAL update for adding a new storage folder.
func addStorageFolderUpdate(sf *storageFolder) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         addStorageFolderUpdateName,
			Instructions: encoding.MarshalAll(sf.path, uint64(len(sf.usage))),
		},
		nil,
	}
}

// sectorMetadataUpdate creates a WAL update for updating a storage folder's
// metadata.
func sectorMetadataUpdate(sf *storageFolder, su sectorUpdate) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         sectorMDUpdateName,
			Instructions: encoding.MarshalAll(sf.metadataFilePath, su),
		},
		sf.metadataFile,
	}
}

// sectorDataUpdate creates a WAL update for updating a sector's data.
func sectorDataUpdate(file modules.File, path string, sectorIndex uint32, data []byte) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         sectorDataUpdateName,
			Instructions: encoding.MarshalAll(path, sectorIndex, data),
		},
		file,
	}
}

// truncateUpdate creates a WAL update for the writeaheadlog which truncates a
// file to the specified size.
func truncateUpdate(file modules.File, path string, newSize int64) walUpdate {
	return walUpdate{
		writeaheadlog.TruncateUpdate(path, newSize),
		file,
	}
}

// removeStorageFolderUpdate creates a WAL update for emptying out a storage
// folder on disk.
func removeStorageFolderUpdate(index uint16, path string) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         removeStorageFolderUpdateName,
			Instructions: encoding.MarshalAll(index, path),
		},
		nil, // no file needed
	}
}

// growStorageFolderUpdate creates a WAL update for growing out a storage
// folder on disk.
func growStorageFolderUpdate(index uint16, newSectorCount uint32) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         growStorageFolderUpdateName,
			Instructions: encoding.MarshalAll(index, newSectorCount),
		},
		nil, // no file needed
	}
}

// shrinkStorageFolderUpdate creates a WAL update for shrinking a storage folder
// on disk.
func shrinkStorageFolderUpdate(index uint16, startingPoint uint32, force bool) walUpdate {
	return walUpdate{
		writeaheadlog.Update{
			Name:         shrinkStorageFolderUpdateName,
			Instructions: encoding.MarshalAll(index, startingPoint, force),
		},
		nil, // no file needed
	}
}

// applyUpdates applies the provided updates one by one.
func (cm *ContractManager) applyUpdates(updates ...walUpdate) error {
	for _, update := range updates {
		var err error
		switch update.Name {
		case addStorageFolderUpdateName:
			err = cm.applyAddStorageFolderUpdate(update)
		case sectorMDUpdateName:
			err = cm.applySectorMetadataUpdate(update)
		case sectorDataUpdateName:
			err = cm.applySectorDataUpdate(update)
		case removeStorageFolderUpdateName:
			err = cm.applyRemoveStorageFolderUpdate(update)
		case shrinkStorageFolderUpdateName:
			err = cm.applyShrinkStorageFolderUpdate(update)
		case growStorageFolderUpdateName:
			err = cm.applyGrowStorageFolderUpdate(update)
		}
		if err != nil {
			return errors.AddContext(err, "applyUpdates:")
		}
	}
	return nil
}

// createAndApplyTransaction will create a transaction from the provided updates
// and try to apply them in order.
func (cm *ContractManager) createAndApplyTransaction(updates ...walUpdate) error {
	// Create the writeaheadlog transaction.
	wUpdates := make([]writeaheadlog.Update, 0, len(updates))
	for _, update := range updates {
		wUpdates = append(wUpdates, update.Update)
	}
	txn, err := cm.staticWal.NewTransaction(wUpdates)
	if err != nil {
		return errors.AddContext(err, "failed to create wal txn")
	}
	// No extra setup is required. Signal that it is done.
	if err := <-txn.SignalSetupComplete(); err != nil {
		return errors.AddContext(err, "failed to signal setup completion")
	}
	// Apply the updates.
	if err := cm.applyUpdates(updates...); err != nil {
		return errors.AddContext(err, "failed to apply updates")
	}
	// Updates are applied. Let the writeaheadlog know.
	if err := txn.SignalUpdatesApplied(); err != nil {
		return errors.AddContext(err, "failed to signal that updates are applied")
	}
	return nil
}

// applyAddStorageFolderUpdate applies an update which adds a storage folder to
// the contract manager.
func (cm *ContractManager) applyAddStorageFolderUpdate(update walUpdate) error {
	if update.Name != addStorageFolderUpdateName {
		return fmt.Errorf("can't call applyAddStorageFolderUpdate on '%v' update", update.Name)
	}
	// Decode the instructions.
	var path string
	var usageLength uint64
	err := encoding.UnmarshalAll(update.Instructions, &path, &usageLength)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal addStorageFolderUpdate instructions")
	}
	return cm.managedAddStorageFolder(&storageFolder{
		path:  path,
		usage: make([]uint64, usageLength),

		availableSectors: make(map[sectorID]uint32),
	})
}

// applySectorDataUpdate applies an update to the sector's data. If no file is
// provided it will try to open the file after decoding the path.
func (cm *ContractManager) applySectorDataUpdate(update walUpdate) error {
	if update.Name != sectorDataUpdateName {
		return fmt.Errorf("can't call applySectorDataUpdate on '%v' update", update.Name)
	}
	// Decode the instructions.
	var path string
	var sectorIndex uint32
	var data []byte
	err := encoding.UnmarshalAll(update.Instructions, &path, &sectorIndex, &data)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal applySectorDataUpdate instructions")
	}
	// Open the file if no file was passed in.
	f := update.f
	if f == nil {
		f, err = cm.dependencies.OpenFile(path, os.O_RDWR, 0700)
		if err != nil {
			return errors.AddContext(err, "applySectorDataUpdate failed to open")
		}
		defer f.Close()
	}
	// Write sector.
	err = writeSector(f, sectorIndex, data)
	if err != nil {
		cm.log.Printf("ERROR: Unable to write sector for folder %v: %v\n", path, err)
		// atomic.AddUint64(&sf.atomicFailedWrites, 1) TODO: move to caller
		return errors.Compose(err, errDiskTrouble)
	}
	return f.Sync()
}

// applySectorMetadataUpdate applies an update to the sector's metadata. If no
// file is provided it will try to open the file after decoding the path.
func (cm *ContractManager) applySectorMetadataUpdate(update walUpdate) error {
	if update.Name != sectorMDUpdateName {
		return fmt.Errorf("can't call applySectorMetadataUpdate on '%v' update", update.Name)
	}
	// Decode the instructions
	var su sectorUpdate
	var path string
	err := encoding.UnmarshalAll(update.Instructions, &path, &su)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal applySectorMDUpdate instructions")
	}
	// Open the file if no file was passed in.
	f := update.f
	if f == nil {
		f, err = cm.dependencies.OpenFile(path, os.O_RDWR, 0700)
		if err != nil {
			return errors.AddContext(err, "applySectorDataUpdate failed to open")
		}
		defer f.Close()
	}
	// Write metadata.
	err = writeSectorMetadata(f, su.Index, su.ID, su.Count)
	if err != nil {
		cm.log.Printf("ERROR: Unable to write sector metadata for folder %v: %v\n", path, err)
		// atomic.AddUint64(&sf.atomicFailedWrites, 1) // TODO: move to caller
		return errors.Compose(err, errDiskTrouble)
	}
	return f.Sync()
}

// applyEmptyStorageFolderUpdate applies an update to empty a sector's storage
// folder.
func (cm *ContractManager) applyRemoveStorageFolderUpdate(update walUpdate) error {
	if update.Name != removeStorageFolderUpdateName {
		return fmt.Errorf("can't call applyEmptyStorageFolderUpdate on '%v' update", update.Name)
	}
	// Decode the instructions
	var index uint16
	var path string
	err := encoding.UnmarshalAll(update.Instructions, &index, &path)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal emptyStorageFolderUpdate instructions")
	}
	// Empty storage folder.
	_, err = cm.managedEmptyStorageFolder(index, 0)
	if err != nil {
		cm.log.Printf("ERROR: Unable to empty storage folder %v: %v\n", index, err)
		// atomic.AddUint64(&sf.atomicFailedWrites, 1) // TODO: move to caller
		return errors.AddContext(err, fmt.Sprintf("failed to empty storage folder at index %v", index))
	}
	// Commit the state.
	cm.commitStorageFolderRemoval(index, path)
	return nil
}

// applyShrinkStorageFolderUpdate applies an update to shrink a sector's storage
// folder.
func (cm *ContractManager) applyShrinkStorageFolderUpdate(update walUpdate) error {
	if update.Name != shrinkStorageFolderUpdateName {
		return fmt.Errorf("can't call applyShrinkStorageFolderUpdate on '%v' update", update.Name)
	}
	// Decode the instructions
	var index uint16
	var newSectorCount uint32
	var force bool
	err := encoding.UnmarshalAll(update.Instructions, &index, &newSectorCount, &force)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal shrinkStorageFolderUpdate instructions")
	}
	// Empty storage folder.
	_, err = cm.managedEmptyStorageFolder(index, newSectorCount)
	if err != nil && !force {
		cm.log.Printf("ERROR: Unable to shrink storage folder %v: %v\n", index, err)
		// atomic.AddUint64(&sf.atomicFailedWrites, 1) // TODO: move to caller
		return errors.AddContext(err, fmt.Sprintf("failed to shrink storage folder at index %v", index))
	}
	// Commit the change to the state.
	cm.commitStorageFolderReduction(index, newSectorCount)
	return nil
}

// growStorageFolderUpdate applies an update to grow a sector's storage
// folder.
func (cm *ContractManager) applyGrowStorageFolderUpdate(update walUpdate) error {
	if update.Name != growStorageFolderUpdateName {
		return fmt.Errorf("can't call applyGrowStorageFolderUpdate on '%v' update", update.Name)
	}
	// Decode the instructions
	var index uint16
	var newSectorCount uint32
	err := encoding.UnmarshalAll(update.Instructions, &index, &newSectorCount)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal shrinkStorageFolderUpdate instructions")
	}
	// Empty storage folder.
	err = cm.managedGrowStorageFolder(index, newSectorCount)
	if err != nil {
		cm.log.Printf("ERROR: Unable to grow storage folder %v: %v\n", index, err)
		// atomic.AddUint64(&sf.atomicFailedWrites, 1) // TODO: move to caller
		return errors.AddContext(err, fmt.Sprintf("failed to grow storage folder at index %v", index))
	}
	// Commit the change to the state.
	cm.commitStorageFolderExtension(index, newSectorCount)
	return nil
}

//func addStorageFolderUpdate(sf *storageFolder) writeaheadlog.Update {
//	panic("not implemented yet")
//	//	wal.appendChange(stateChange{
//	//		UnfinishedStorageFolderAdditions: []savedStorageFolder{sf.savedStorageFolder()},
//	//	})
//}
//
//func modifySectorUpdate(su sectorUpdate) writeaheadlog.Update {
//	panic("not implemented yet")
//}
//
//func (cm *ContractManager) prepareWalTxn(updates ...writeaheadlog.Update) (*writeaheadlog.Transaction, error) {
//	// Create the writeaheadlog transaction.
//	txn, err := cm.wal.NewTransaction(updates)
//	if err != nil {
//		return nil, errors.AddContext(err, "failed to create wal txn")
//	}
//	// No extra setup is required. Signal that it is done.
//	if err := <-txn.SignalSetupComplete(); err != nil {
//		return nil, errors.AddContext(err, "failed to signal setup completion")
//	}
//	return txn, nil
//}

//type (
//	// sectorUpdate is an idempotent update to the sector metadata.
//	sectorUpdate struct {
//		Count  uint16
//		Folder uint16
//		ID     sectorID
//		Index  uint32
//	}
//
//	// stateChange defines an idempotent change to the state that has not yet
//	// been applied to the contract manager. The state change is a single
//	// transaction in the WAL.
//	//
//	// All changes in the stateChange object need to be idempotent, as it's
//	// possible that consecutive unclean shutdowns will result in changes being
//	// committed to the state multiple times.
//	stateChange struct {
//		// These fields relate to adding a storage folder. Adding a storage
//		// folder happens in several stages.
//		//
//		// First the storage folder is added as an
//		// 'UnfinishedStorageFolderAddition', because there is large amount of
//		// I/O preprocessing that is performed when adding a storage folder.
//		// This I/O must be nonblocking and must resume in the event of unclean
//		// or early shutdown.
//		//
//		// When the preprocessing is complete, the storage folder is moved to a
//		// 'StorageFolderAddition', which can be safely applied to the contract
//		// manager but hasn't yet.
//		//
//		// ErroredStorageFolderAdditions are signals to the WAL that an
//		// unfinished storage folder addition has failed and can be cleared
//		// out. The WAL is append-only, which is why an error needs to be
//		// logged instead of just automatically clearning out the unfinished
//		// storage folder addition.
//		ErroredStorageFolderAdditions     []uint16
//		ErroredStorageFolderExtensions    []uint16
//		StorageFolderAdditions            []savedStorageFolder
//		StorageFolderExtensions           []storageFolderExtension
//		StorageFolderRemovals             []storageFolderRemoval
//		StorageFolderReductions           []storageFolderReduction
//		UnfinishedStorageFolderAdditions  []savedStorageFolder
//		UnfinishedStorageFolderExtensions []unfinishedStorageFolderExtension
//
//		// Updates to the sector metadata. Careful ordering of events ensures
//		// that a sector update will not make it into the synced WAL unless the
//		// sector data is already on-disk and synced.
//		SectorUpdates []sectorUpdate
//	}

func (cm *ContractManager) loadWal() error {
	// Try opening the WAL file.
	walFileName := filepath.Join(cm.persistDir, walFile)
	txns, wal, err := writeaheadlog.New(walFileName)
	if err != nil {
		return err
	}
	cm.staticWal = wal
	// Apply the unfinished transactions.
	for _, txn := range txns {
		updates := make([]walUpdate, 0, len(txn.Updates))
		for _, u := range txn.Updates {
			updates = append(updates, walUpdate{u, nil})
		}
		err := cm.applyUpdates(updates...)
		if err != nil && !errors.Contains(err, errBadStorageFolderIndex) {
			return err
		}
		if err := txn.SignalUpdatesApplied(); err != nil {
			return err
		}
	}
	return nil
	//	walFile, err := cm.dependencies.OpenFile(walFileName, os.O_RDONLY, 0600)
	//	if err == nil {
	//		// err == nil indicates that there is a WAL file, which means that the
	//		// previous shutdown was not clean. Re-commit the changes in the WAL to
	//		// bring the program back to consistency.
	//		cm.log.Println("WARN: WAL file detected, performing recovery after unclean shutdown.")
	//		err = wal.recoverWAL(walFile)
	//		if err != nil {
	//			return build.ExtendErr("failed to recover WAL", err)
	//		}
	//		err = walFile.Close()
	//		if err != nil {
	//			return build.ExtendErr("error closing WAL after performing a recovery", err)
	//		}
	//	} else if !os.IsNotExist(err) {
	//		return build.ExtendErr("walFile was not opened successfully", err)
	//	}
	//	// err == os.IsNotExist, suggesting a successful, clean shutdown. No action
	//	// is taken.
	//
	//	// Create the tmp settings file and initialize the first write to it. This
	//	// is necessary before kicking off the sync loop.
	//	wal.fileSettingsTmp, err = wal.cm.dependencies.CreateFile(filepath.Join(wal.cm.persistDir, settingsFileTmp))
	//	if err != nil {
	//		return build.ExtendErr("unable to prepare the settings temp file", err)
	//	}
	//	wal.cm.tg.AfterStop(func() {
	//		wal.mu.Lock()
	//		defer wal.mu.Unlock()
	//		if wal.fileSettingsTmp == nil {
	//			return
	//		}
	//		err := wal.fileSettingsTmp.Close()
	//		if err != nil {
	//			wal.cm.log.Println("ERROR: unable to close settings temporary file")
	//			return
	//		}
	//		err = wal.cm.dependencies.RemoveFile(filepath.Join(wal.cm.persistDir, settingsFileTmp))
	//		if err != nil {
	//			wal.cm.log.Println("ERROR: unable to remove settings temporary file")
	//			return
	//		}
	//	})
	//	ss := cm.savedSettings()
	//	b, err := json.MarshalIndent(ss, "", "\t")
	//	if err != nil {
	//		build.ExtendErr("unable to marshal settings data", err)
	//	}
	//	enc := json.NewEncoder(wal.fileSettingsTmp)
	//	if err := enc.Encode(settingsMetadata.Header); err != nil {
	//		build.ExtendErr("unable to write header to settings temp file", err)
	//	}
	//	if err := enc.Encode(settingsMetadata.Version); err != nil {
	//		build.ExtendErr("unable to write version to settings temp file", err)
	//	}
	//	if _, err = wal.fileSettingsTmp.Write(b); err != nil {
	//		build.ExtendErr("unable to write data settings temp file", err)
	//	}
	//	return nil
}

package siafile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/writeaheadlog"
)

// ApplyUpdates applies a number of writeaheadlog updates to the corresponding
// SiaFile. This method can apply updates from different SiaFiles and should
// only be run before the SiaFiles are loaded from disk right after the startup
// of siad. Otherwise we might run into concurrency issues.
func ApplyUpdates(updates ...writeaheadlog.Update) error {
	for _, u := range updates {
		err := func() error {
			// Decode update.
			path, index, data, err := readUpdate(u)
			if err != nil {
				return err
			}

			// Open the file.
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer f.Close()

			// Write data.
			if n, err := f.WriteAt(data, index); err != nil {
				return err
			} else if n < len(data) {
				return fmt.Errorf("update was only applied partially - %v / %v", n, len(data))
			}
			return nil
		}()
		if err != nil {
			return errors.AddContext(err, "failed to apply update")
		}
	}
	return nil
}

// applyUpdates applies updates to the SiaFile. Only updates that belong to the
// SiaFile on which applyUpdates is called can be applied. Everything else will
// be considered a developer error and cause a panic to avoid corruption.
func (sf *SiaFile) applyUpdates(updates ...writeaheadlog.Update) error {
	// Open the file.
	f, err := os.OpenFile(sf.siaFilePath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// Apply updates.
	for _, u := range updates {
		err := func() error {
			// Decode update.
			path, index, data, err := readUpdate(u)
			if err != nil {
				return err
			}

			// Sanity check path. Update should belong to SiaFile.
			if sf.siaFilePath != path {
				panic(fmt.Sprintf("can't apply update for file %s to SiaFile %s", path, sf.siaFilePath))
			}

			// Write data.
			if n, err := f.WriteAt(data, index); err != nil {
				return err
			} else if n < len(data) {
				return fmt.Errorf("update was only applied partially - %v / %v", n, len(data))
			}
			return nil
		}()
		if err != nil {
			return errors.AddContext(err, "failed to apply update")
		}
	}
	return nil
}

// createUpdate is a helper method which creates a writeaheadlog update for
// writing the specified data to the provided index. It is usually not called
// directly but wrapped into another helper that creates an update for a
// specific part of the SiaFile. e.g. the metadata
func (sf *SiaFile) createUpdate(index int64, data []byte) writeaheadlog.Update {
	if index < 0 {
		panic("index passed to createUpdate should never be negative")
	}
	// Create update
	return writeaheadlog.Update{
		Name:         updateInsertName,
		Instructions: encoding.MarshalAll(sf.siaFilePath, index, data),
	}
}

// readUpdate unmarshals the update's instructions and returns the path, index
// and data encoded in the instructions.
func readUpdate(update writeaheadlog.Update) (path string, index int64, data []byte, err error) {
	if !IsSiaFileUpdate(update) {
		panic("readUpdate can't read non-SiaFile update")
	}
	err = encoding.UnmarshalAll(update.Instructions, &path, &index, &data)
	return
}

// allocateHeaderPage allocates a new page for the metadata and
// publicKeyTable. It returns the necessary writeaheadlog updates to allocate a
// new page by moving the chunk data back by one page and moving the
// publicKeyTable to the end of the newly allocated page.
func (sf *SiaFile) allocateHeaderPage() []writeaheadlog.Update {
	panic("not yet implemented")
}

// createAndApplyTransaction is a helper method that creates a writeaheadlog
// transaction and applies it.
func (sf *SiaFile) createAndApplyTransaction(updates []writeaheadlog.Update) error {
	// Create the writeaheadlog transaction.
	txn, err := sf.wal.NewTransaction(updates)
	if err != nil {
		return errors.AddContext(err, "failed to create wal txn")
	}
	// No extra setup is required. Signal that it is done.
	if err := <-txn.SignalSetupComplete(); err != nil {
		return errors.AddContext(err, "failed to signal setup completion")
	}
	// Apply the updates.
	if err := sf.applyUpdates(updates...); err != nil {
		return errors.AddContext(err, "failed to apply updates")
	}
	// Updates are applied. Let the writeaheadlog know.
	return errors.AddContext(err, "failed to signal that updates are applied")
}

// marshalMetadata marshals the metadata of the SiaFile using json encoding.
func (sf *SiaFile) marshalMetadata() ([]byte, error) {
	// Encode the metadata.
	jsonMD, err := json.Marshal(sf.staticMetadata)
	if err != nil {
		return nil, err
	}
	// Create the update.
	return jsonMD, nil
}

// marshalPubKeyTable marshals the public key table of the SiaFile using Sia
// encoding.
func (sf *SiaFile) marshalPubKeyTable() ([]byte, error) {
	// Create a buffer.
	buf := bytes.NewBuffer(nil)
	// Marshal all the data into the buffer
	for _, pk := range sf.pubKeyTable {
		if err := pk.MarshalSia(buf); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// saveHeader saves the metadata and pubKeyTable of the SiaFile to disk using
// the writeaheadlog. If the metadata and overlap due to growing too large and
// would therefore corrupt if they were written to disk, a new page is
// allocated.
func (sf *SiaFile) saveHeader() error {
	// Create a list of updates which need to be applied to save the metadata.
	updates := make([]writeaheadlog.Update, 0)

	// Marshal the metadata.
	metadata, err := sf.marshalMetadata()
	if err != nil {
		return err
	}
	// Marshal the pubKeyTable.
	pubKeyTable, err := sf.marshalPubKeyTable()
	if err != nil {
		return err
	}

	// If the metadata and the pubKeyTable overlap, we need to allocate a new
	// page for them.
	pubKeyTableOffset := sf.staticMetadata.chunkOffset - int64(len(pubKeyTable))
	for int64(len(metadata)) > pubKeyTableOffset {
		updates = append(updates, sf.allocateHeaderPage()...)
		pubKeyTableOffset = sf.staticMetadata.chunkOffset - int64(len(pubKeyTable))
	}

	// Create updates for the metadata and pubKeyTable.
	updates = append(updates, sf.createUpdate(0, metadata))
	updates = append(updates, sf.createUpdate(sf.staticMetadata.pubKeyTableOffset, pubKeyTable))

	// Apply the updates.
	return sf.createAndApplyTransaction(updates)
}

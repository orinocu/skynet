package renter

// TODO: Derive the account secret key using the wallet seed. Can use:
// `account specifier || wallet seed || host pubkey` I believe.
//
// If we derive the seeds deterministically, that may mean that we can
// regenerate accounts even we fail to load them from disk. When we make a new
// account with a host, we should always query that host for a balance even if
// we think this is a new account, some previous run on siad may have created
// the account for us.
//
// TODO: How long does the host keep an account open? Does it keep the account
// open for the entire period? If not, we should probably adjust that on the
// host side, otherwise renters that go offline for a while are going to lose
// their accounts because the hosts will expire them. Does the renter track the
// expiration date of the accounts? Will it know upload load that the account is
// missing from the host not because of malice but because they expired?

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
)

const (
	// accountSize is the fixed account size in bytes
	accountSize = 1 << 8 // 256 bytes

	// compatV150AccountSize is the size of an account object at v1.5.0
	compatV150AccountSize = 1 << 8
)

var (
	// accountsFilename is the filename of the accounts persistence file
	accountsFilename = "accounts.dat"

	// Metadata
	//
	// Note that the accounts metadata version is set at v1.5.1, even though
	// this was changed in v1.5.0. This is due to the fact the `lastUsed` field
	// was added, and the metadata mistakenly had v1.5.0. A version bump was
	// thus necessary to trigger the compat flow.
	metadataHeader  = types.NewSpecifier("Accounts\n")
	metadataVersion = types.NewSpecifier("v1.5.1\n")
	metadataSize    = 2*types.SpecifierLen + 1 // 1 byte for 'clean' flag

	// compatV150MetadataVersion is the metadata version at v1.5.0
	compatV150MetadataVersion = types.NewSpecifier("v1.5.0\n")

	// Metadata validation errors
	errWrongHeader  = errors.New("wrong header")
	errWrongVersion = errors.New("wrong version")

	// Persistence data validation errors
	errInvalidChecksum = errors.New("invalid checksum")

	// errTestTmpFileRecovery is an error returned when testing the accounts
	// upgrade flow that recovers from an already existing tmp file
	errTestTmpFileRecovery = errors.New("tmp file recovery")
)

type (
	// accountManager tracks the set of accounts known to the renter.
	accountManager struct {
		accounts map[string]*account

		// Utils. The file is global to all accounts, each account looks at a
		// specific offset within the file.
		mu           sync.Mutex
		staticFile   modules.File
		staticRenter *Renter
	}

	// accountsMetadata is the metadata of the accounts persist file
	accountsMetadata struct {
		Header  types.Specifier
		Version types.Specifier
		Clean   bool
	}

	// accountPersistence is the account's persistence object which holds all
	// data that gets persisted for a single account.
	accountPersistence struct {
		AccountID modules.AccountID
		Balance   types.Currency
		HostKey   types.SiaPublicKey
		LastUsed  int64
		SecretKey crypto.SecretKey
	}

	// compatV150AccountPersistence is a compat struct that contains the fields
	// of the persistence object at v1.5.0
	compatV150AccountPersistence struct {
		AccountID modules.AccountID
		Balance   types.Currency
		HostKey   types.SiaPublicKey
		SecretKey crypto.SecretKey
	}
)

// newAccountManager will initialize the account manager for the renter.
func (r *Renter) newAccountManager() error {
	if r.staticAccountManager != nil {
		return errors.New("account manager already exists")
	}

	r.staticAccountManager = &accountManager{
		accounts: make(map[string]*account),

		staticRenter: r,
	}
	return r.staticAccountManager.load()
}

// managedPersist will write the account to the given file at the account's
// offset, without syncing the file.
func (a *account) managedPersist() error {
	a.mu.Lock()
	accountData := accountPersistence{
		AccountID: a.staticID,
		Balance:   a.balance,
		HostKey:   a.staticHostKey,
		LastUsed:  a.lastUsed,
		SecretKey: a.staticSecretKey,
	}
	a.mu.Unlock()
	_, err := a.staticFile.WriteAt(accountData.bytes(), a.staticOffset)
	return errors.AddContext(err, "unable to write the account to disk")
}

// bytes is a helper method on the persistence object that outputs the bytes to
// put on disk, these include the checksum and the marshaled persistence object.
func (ap accountPersistence) bytes() []byte {
	accBytes := encoding.Marshal(ap)
	accBytesMaxSize := accountSize - crypto.HashSize // leave room for checksum
	if len(accBytes) > accBytesMaxSize {
		build.Critical("marshaled object is larger than expected size")
	}

	// Calculate checksum on padded account bytes. Upon load, the padding will
	// be ignored by the unmarshaling.
	accBytesPadded := make([]byte, accBytesMaxSize)
	copy(accBytesPadded, accBytes)
	checksum := crypto.HashBytes(accBytesPadded)

	// create final byte slice of account size
	b := make([]byte, accountSize)
	copy(b[:len(checksum)], checksum[:])
	copy(b[len(checksum):], accBytesPadded)
	return b
}

// loadBytes is a helper method that takes a byte slice, containing a checksum
// and the account bytes, and unmarshals them onto the persistence object if the
// checksum is valid.
func (ap *accountPersistence) loadBytes(b []byte) error {
	// extract checksum and verify it
	checksum := b[:crypto.HashSize]
	accBytes := b[crypto.HashSize:]
	accHash := crypto.HashBytes(accBytes)

	if !bytes.Equal(checksum, accHash[:]) {
		return errInvalidChecksum
	}

	// unmarshal the account bytes onto the persistence object
	return errors.AddContext(encoding.Unmarshal(accBytes, ap), "failed to unmarshal account bytes")
}

// loadBytesCompatV150 is a compat function that can load account bytes of a
// v1.5.0 account on an account persistence object.
func (ap *accountPersistence) loadBytesCompatV150(b []byte) error {
	// Sanity check the length of the byte slice
	if len(b) != compatV150AccountSize {
		build.Critical("Given account bytes are not the expected length")
	}

	checksum := b[:crypto.HashSize]
	accBytes := b[crypto.HashSize:]
	accHash := crypto.HashBytes(accBytes)
	if !bytes.Equal(checksum, accHash[:]) {
		return errInvalidChecksum
	}

	var compat compatV150AccountPersistence
	err := encoding.Unmarshal(accBytes, &compat)
	if err != nil {
		return errors.AddContext(err, "failed to unmarshal account bytes onto compat persistence object")
	}

	// copy over all account persistence fields
	ap.AccountID = compat.AccountID
	ap.Balance = compat.Balance
	ap.HostKey = compat.HostKey
	ap.SecretKey = compat.SecretKey

	// initialize last used at 0, this ensures the renter does not mistakenly
	// penalize the host for expiring his account
	ap.LastUsed = 0

	return nil
}

// managedOpenAccount returns an account for the given host. If it does not
// exist already one is created.
func (am *accountManager) managedOpenAccount(hostKey types.SiaPublicKey) (acc *account, err error) {
	// Check if we already have an account. Due to a race condition around
	// account creation, we need to check that the account was persisted to disk
	// before we can start using it, this happens with the 'staticReady' and
	// 'externActive' variables of the account. See the rest of this functions
	// implementation to understand how they are used in practice.
	am.mu.Lock()
	acc, exists := am.accounts[hostKey.String()]
	if exists {
		am.mu.Unlock()
		<-acc.staticReady
		if acc.externActive {
			return acc, nil
		}
		return nil, errors.New("account creation failed")
	}
	// Open a new account.
	offset := (len(am.accounts) + 1) * accountSize // +1 because the first slot in the file is used for metadata
	aid, sk := modules.NewAccountID()
	acc = &account{
		staticID:        aid,
		staticHostKey:   hostKey,
		staticSecretKey: sk,

		staticFile:   am.staticFile,
		staticOffset: int64(offset),

		staticReady: make(chan struct{}),
	}
	am.accounts[hostKey.String()] = acc
	am.mu.Unlock()
	// Defer a close on 'staticReady'. By default, 'externActive' is false, so
	// if there is an error, the account will be marked as unusable.
	defer close(acc.staticReady)

	// Defer a function to delete the account if the persistence fails. This is
	// technically a race condition, but the alternative is holding the lock on
	// the account mangager while doing an fsync, which is not ideal.
	defer func() {
		if err != nil {
			am.mu.Lock()
			delete(am.accounts, hostKey.String())
			am.mu.Unlock()
		}
	}()

	// Save the file. After the file gets written to disk, perform a sync
	// because we want to ensure that the secret key of the account can be
	// recovered before we start using the account.
	err = acc.managedPersist()
	if err != nil {
		return nil, errors.AddContext(err, "failed to persist account")
	}
	err = acc.staticFile.Sync()
	if err != nil {
		return nil, errors.AddContext(err, "failed to sync accounts file")
	}

	// Mark the account as usable so that anyone who tried to open the account
	// after this function ran will see that the account is persisted correctly.
	acc.mu.Lock()
	acc.externActive = true
	acc.mu.Unlock()
	return acc, nil
}

// managedSaveAndClose is called on shutdown and ensures the account data is
// properly persisted to disk
func (am *accountManager) managedSaveAndClose() error {
	am.mu.Lock()
	defer am.mu.Unlock()

	// Save the account data to disk.
	clean := true
	var persistErrs error
	for _, account := range am.accounts {
		err := account.managedPersist()
		if err != nil {
			clean = false
			persistErrs = errors.Compose(persistErrs, err)
			continue
		}
	}
	// If there was an error saving any of the accounts, the system is not clean
	// and we do not need to update the metadata for the file.
	if !clean {
		return errors.AddContext(persistErrs, "unable to persist all accounts cleanly upon shutdown")
	}

	// Sync the file before updating the header. We want to make sure that the
	// accounts have been put into a clean and finalized state before writing an
	// update to the metadata.
	err := am.staticFile.Sync()
	if err != nil {
		return errors.AddContext(err, "failed to sync accounts file")
	}

	// update the metadata and mark the file as clean
	if err = am.updateMetadata(accountsMetadata{
		Header:  metadataHeader,
		Version: metadataVersion,
		Clean:   true,
	}); err != nil {
		return errors.AddContext(err, "failed to update accounts file metadata")
	}

	// Close the account file.
	return am.staticFile.Close()
}

// managedLoad will pull all of the accounts off of disk and load them into the
// account manager. This should complete before the accountManager is made
// available to other processes.
func (am *accountManager) load() error {
	// Open the accounts file.
	clean, err := am.openFile()
	if err != nil {
		return errors.AddContext(err, "failed to open accounts file")
	}

	// Read the raw account data and decode them into accounts. We start at an
	// offset of 'accountSize' because the first slot is reserved for the
	// metadata.
	for offset := int64(accountSize); ; offset += accountSize {
		// read the account at offset
		acc, err := am.readAccountAt(offset)
		if errors.Contains(err, io.EOF) {
			break
		} else if err != nil {
			am.staticRenter.log.Println("ERROR: could not load account", err)
			continue
		}

		// reset the account balances after an unclean shutdown
		if !clean {
			acc.balance = types.ZeroCurrency
		}
		am.accounts[acc.staticHostKey.String()] = acc
	}

	// Ensure the accounts are saved upon shutdown
	err = am.staticRenter.tg.AfterStop(func() error {
		// Disrupt if the dependency is set to simulate an unclean shutdown.
		if am.staticRenter.deps.Disrupt("InterruptAccountSaveOnShutdown") {
			return nil
		}
		return am.managedSaveAndClose()
	})
	if err != nil {
		return errors.AddContext(err, "unable to schedule a save and close with the thread group")
	}

	// Check for the tmp file possibly left behind by the compat code that
	// upgrades from v150 to v151, if present remove it. We perform this check
	// here to ensure we do not end up with a tmp file on disk should the renter
	// crash after a successful upgrade but before the removal of the file.
	//
	// Disrupt if the dependency is set to prevent cleaning up the tmp accounts
	// file. This simulates a crash that leaves a tmp accounts file on disk.
	if !am.staticRenter.deps.Disrupt("DisableTmpFileCleanup") {
		if err := os.RemoveAll(am.tmpAccountsFilePath()); err != nil {
			am.staticRenter.log.Println("ERROR: failed to remove tmp accounts file, err:", err)
		}
	}

	return nil
}

// checkMetadata will load the metadata from the account file and return whether
// or not the previous shutdown was clean. If the metadata does not match the
// expected metadata, an error will be returned.
//
// NOTE: If we change the version of the file, this is probably the function
// that should handle doing the persist upgrade. Inside of this function there
// would be a call to the upgrade function.
func (am *accountManager) checkMetadata() (bool, error) {
	// Read and decode the metadata.
	buffer := make([]byte, metadataSize)
	_, err := am.staticFile.ReadAt(buffer, 0)
	if err != nil {
		return false, errors.AddContext(err, "failed to read metadata from accounts file")
	}
	var metadata accountsMetadata
	err = encoding.Unmarshal(buffer, &metadata)
	if err != nil {
		return false, errors.AddContext(err, "failed to decode metadata from accounts file")
	}

	// Validate the metadata.
	if metadata.Header != metadataHeader {
		return false, errors.AddContext(errWrongHeader, "failed to verify accounts metadata")
	}
	if metadata.Version != metadataVersion {
		return false, errors.AddContext(errWrongVersion, "failed to verify accounts metadata")
	}
	return metadata.Clean, nil
}

// openFile will open the file of the account manager and set the account
// manager's file variable.
//
// openFile will return 'true' if the previous shutdown was clean, and 'false'
// if the previous shutdown was not clean.
func (am *accountManager) openFile() (bool, error) {
	// Sanity check that the file isn't already opened.
	if am.staticFile != nil {
		am.staticRenter.log.Critical("double open detected on account manager")
		return false, errors.New("file already open")
	}

	// Check the file health.
	path := filepath.Join(am.staticRenter.persistDir, accountsFilename)
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return false, errors.AddContext(statErr, "error calling stat on file")
	}

	// Open the file, create it if it does not exist yet.
	file, err := am.staticRenter.deps.OpenFile(path, os.O_RDWR|os.O_CREATE, defaultFilePerm)
	if err != nil {
		return false, errors.AddContext(err, "error opening account file")
	}
	am.staticFile = file

	// If the stat err was nil, a header already exists. Check that the header
	// matches what we are expecting.
	var cleanClose bool
	if os.IsNotExist(statErr) {
		// If the file didn't previously exist, represent that the file was
		// closed cleanly.
		cleanClose = true
	} else {
		// It's possible the upgrade code has partially overwritten the accounts
		// file (thus changing the version) and has then crashed. To handle this
		// case we want to check if the tmp accounts file is on the system and
		// whether it has a valid checksum, if that is the case we want to
		// trigger the upgrade flow immediately (before the version compare).
		if _, err := os.Stat(am.tmpAccountsFilePath()); !os.IsNotExist(err) {
			validChecksum, err := verifyChecksum(am.tmpAccountsFilePath())
			if err == nil && validChecksum {
				err = am.upgradeFromV150ToV151()
				if err != nil {
					return false, errors.AddContext(err, "failed to upgrade accounts file from v1.5.0 to v1.5.1")
				}
				cleanClose, err = am.checkMetadata()
			}
		}

		cleanClose, err = am.checkMetadata()
		if errors.Contains(err, errWrongVersion) {
			err = am.upgradeFromV150ToV151()
			if err != nil {
				return false, errors.AddContext(err, "failed to upgrade accounts file from v1.5.0 to v1.5.1")
			}
		}
		if err != nil {
			return false, errors.AddContext(err, "error reading account metadata")
		}
	}

	// Whether this is a new file or an existing file, we need to set the header
	// on the metadata. When opening an account, the header should represent an
	// unclean shutdown. This will be flipped to a header that represents a
	// clean shutdown upon closing.
	err = am.updateMetadata(accountsMetadata{
		Header:  metadataHeader,
		Version: metadataVersion,
		Clean:   false,
	})
	if err != nil {
		return false, errors.AddContext(err, "unable to update the account metadata")
	}
	// Sync the metadata to ensure the acounts will load as dirty before any
	// accounts are created.
	err = am.staticFile.Sync()
	if err != nil {
		return false, errors.AddContext(err, "failed to sync accounts file")
	}
	return cleanClose, nil
}

// upgradeFromV150ToV151 attempts to upgrade the accounts file from v1.5.0 to
// v1.5.1
func (am *accountManager) upgradeFromV150ToV151() error {
	// open a temporary accounts file
	tmpFile, err := am.staticRenter.deps.OpenFile(am.tmpAccountsFilePath(), os.O_RDWR|os.O_CREATE, defaultFilePerm)
	if err != nil {
		return errors.AddContext(err, "failed to open tmp file")
	}

	// it might have already existed from an earlier try that ended in a crash
	// in that case we want to verify the checksum and potentially immediately
	// overwrite the accounts file
	validChecksum, err := verifyChecksum(am.tmpAccountsFilePath())
	if err != nil {
		return errors.AddContext(err, "failed to verify checksum in tmp file")
	}

	// if the tmp file does not have a valid checksum
	if !validChecksum {
		// truncate the tmp file to ensure we have an empty file and seek to
		// after the checksum
		err = tmpFile.Truncate(0)
		if err != nil {
			return errors.AddContext(err, "failed to truncate tmp file")
		}
		_, err = tmpFile.Seek(crypto.HashSize, io.SeekStart)
		if err != nil {
			return errors.AddContext(err, "failed to seek in tmp file")
		}

		// make a writer that writes to the tmp file and at the same time builds
		// a hash we'll use for the checksum
		h := crypto.NewHash()
		out := io.MultiWriter(tmpFile, h)

		// read the metadata of the accounts file
		metadata, err := readMetadata(am.staticFile)
		if err != nil {
			return errors.AddContext(err, "failed to read metadata from accounts file")
		}

		// update only the version and write the padded metadata
		metadata.Version = metadataVersion
		paddedMetadata := make([]byte, accountSize)
		copy(paddedMetadata, encoding.Marshal(metadata))
		_, err = out.Write(paddedMetadata)
		if err != nil {
			return errors.AddContext(err, "failed to write the metadata to the tmp accounts file")
		}

		// loop over the accounts file and load them into the persistence object
		// using the compat function, it is important to loop over this file
		// using the compat account size as offset
		for srcOffset := int64(compatV150AccountSize); ; srcOffset += compatV150AccountSize {
			accountBytes := make([]byte, compatV150AccountSize)
			_, err := am.staticFile.ReadAt(accountBytes, srcOffset)
			if errors.Contains(err, io.EOF) {
				break
			}
			if err != nil {
				return errors.AddContext(err, "failed to read accounts data")
			}

			// load the account bytes onto the persistence object
			var accountData accountPersistence
			err = accountData.loadBytesCompatV150(accountBytes)
			if err != nil {
				return errors.AddContext(err, "failed to load account bytes")
			}

			// write the account bytes
			_, err = out.Write(accountData.bytes())
			if err != nil {
				return errors.AddContext(err, "failed to write account bytes")
			}
		}

		// write the checksum
		_, err = tmpFile.WriteAt(h.Sum(nil), 0)
		if err != nil {
			return errors.AddContext(err, "failed to write the hash to the beginning of the tmp accounts file")
		}

		// sync the tmp file
		err = tmpFile.Sync()
		if err != nil {
			return fmt.Errorf("failed to sync the tmp accounts file, err: %v", err)
		}
	}

	// seek to the start of the accounts file
	_, err = am.staticFile.Seek(0, io.SeekStart)
	if err != nil {
		return errors.AddContext(err, "failed to seek in the accounts file")
	}

	// seek to right after the checksum in the tmp file
	_, err = tmpFile.Seek(crypto.HashSize, io.SeekStart)
	if err != nil {
		return errors.AddContext(err, "failed to seek in the tmp file")
	}

	// overwrite the accounts file with the contents of the tmp file
	size, err := io.Copy(am.staticFile, tmpFile)
	if err != nil {
		return errors.AddContext(err, "failed to copy the file contents")
	}
	err = am.staticFile.Truncate(size)
	if err != nil {
		return fmt.Errorf("failed to truncate the accounts file, err: %v", err)
	}

	// sync the accounts file
	err = am.staticFile.Sync()
	if err != nil {
		return errors.AddContext(err, "failed to sync accounts file after upgrading")
	}

	// sanity check the metadata after the upgrade
	_, err = am.checkMetadata()
	if err != nil {
		build.Critical("The metadata is invalid after upgrading")
	}

	// disrupt if we are testing the tmp file recovery flow
	if validChecksum && am.staticRenter.deps.Disrupt("RecoveredFromTmpFile") {
		return errTestTmpFileRecovery
	}
	return nil
}

// readAccountAt tries to read an account object from the account persist file
// at the given offset.
func (am *accountManager) readAccountAt(offset int64) (*account, error) {
	// read account bytes
	accountBytes := make([]byte, accountSize)
	_, err := am.staticFile.ReadAt(accountBytes, offset)
	if err != nil {
		return nil, errors.AddContext(err, "failed to read account bytes")
	}

	// load the account bytes onto the a persistence object
	var accountData accountPersistence
	err = accountData.loadBytes(accountBytes)
	if err != nil {
		return nil, errors.AddContext(err, "failed to load account bytes")
	}

	acc := &account{
		staticID:        accountData.AccountID,
		staticHostKey:   accountData.HostKey,
		staticSecretKey: accountData.SecretKey,

		balance:  accountData.Balance,
		lastUsed: accountData.LastUsed,

		staticReady:  make(chan struct{}),
		externActive: true,

		staticOffset: offset,
		staticFile:   am.staticFile,
	}
	close(acc.staticReady)
	return acc, nil
}

// tmpAccountsFilePath returns the path of the temporary accounts file used in
// the compat code that updates from v150 to v151
func (am *accountManager) tmpAccountsFilePath() string {
	return filepath.Join(am.staticRenter.persistDir, accountsFilename+".tmp")
}

// updateMetadata writes the given metadata to the accounts file.
func (am *accountManager) updateMetadata(meta accountsMetadata) error {
	_, err := am.staticFile.WriteAt(encoding.Marshal(meta), 0)
	return err
}

// readMetadata is a helper function reads and decodes the accounts metadata
// from the given file
func readMetadata(file modules.File) (metadata accountsMetadata, err error) {
	buffer := make([]byte, metadataSize)
	_, err = io.ReadFull(file, buffer)
	if err != nil {
		err = errors.AddContext(err, "failed to read metadata from file")
		return
	}

	err = encoding.Unmarshal(buffer, &metadata)
	if err != nil {
		err = errors.AddContext(err, "failed to decode metadata from file")
		return
	}
	return metadata, nil
}

// verifyChecksum is a helper function that verifies if the file contains a
// checksum and whether or not that checksum is valid
func verifyChecksum(filename string) (bool, error) {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return false, errors.AddContext(err, "faild to read from file")
	}

	if len(buf) < crypto.HashSize {
		return false, nil
	}

	checksum := buf[:crypto.HashSize]
	hash := crypto.HashBytes(buf[crypto.HashSize:])
	return bytes.Equal(checksum, hash[:]), nil
}

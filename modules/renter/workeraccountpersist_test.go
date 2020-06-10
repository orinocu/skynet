package renter

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/siatest/dependencies"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/fastrand"
)

// newRandomAccountPersistence is a helper function that returns an
// accountPersistence object, initialised with random values
func newRandomAccountPersistence() accountPersistence {
	aid, sk := modules.NewAccountID()
	return accountPersistence{
		AccountID: aid,
		Balance:   types.NewCurrency64(fastrand.Uint64n(1e3)),
		HostKey:   types.SiaPublicKey{},
		SecretKey: sk,
		LastUsed:  time.Now().Unix(),
	}
}

// TestAccountSave verifies accounts are properly saved and loaded onto the
// renter when it goes through a graceful shutdown and reboot.
func TestAccountSave(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// create a renter
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := rt.Close()
		if err != nil {
			t.Log(err)
		}
	}()
	r := rt.renter

	// verify accounts file was loaded and set
	if r.staticAccountManager.staticFile == nil {
		t.Fatal("Accounts persistence file not set on the Renter after startup")
	}

	// create a number of test accounts and reload the renter
	accounts := openRandomTestAccountsOnRenter(r)
	r, err = rt.reloadRenter(r)
	if err != nil {
		t.Fatal(err)
	}

	// verify the accounts got reloaded properly
	am := r.staticAccountManager
	am.mu.Lock()
	accountsLen := len(am.accounts)
	am.mu.Unlock()
	if accountsLen != len(accounts) {
		t.Errorf("Unexpected amount of accounts, %v != %v", len(am.accounts), len(accounts))
	}
	for _, account := range accounts {
		reloaded, err := am.managedOpenAccount(account.staticHostKey)
		if err != nil {
			t.Error(err)
		}
		if !account.staticID.SPK().Equals(reloaded.staticID.SPK()) {
			t.Error("Unexpected account ID")
		}
	}
}

// TestAccountUncleanShutdown verifies that accounts are dropped if the accounts
// persist file was not marked as 'clean' on shutdown.
func TestAccountUncleanShutdown(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// create a renter tester
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := rt.Close()
		if err != nil {
			t.Log(err)
		}
	}()
	r := rt.renter

	// create a number accounts
	accounts := openRandomTestAccountsOnRenter(r)
	for _, account := range accounts {
		account.mu.Lock()
		account.balance = types.NewCurrency64(fastrand.Uint64n(1e3))
		account.mu.Unlock()
	}

	// close the renter and reload it with a dependency that interrupts the
	// accounts save on shutdown
	deps := &dependencies.DependencyInterruptAccountSaveOnShutdown{}
	r, err = rt.reloadRenterWithDependency(r, deps)
	if err != nil {
		t.Fatal(err)
	}

	// verify the accounts were saved on disk
	for _, account := range accounts {
		reloaded, err := r.staticAccountManager.managedOpenAccount(account.staticHostKey)
		if err != nil {
			t.Fatal(err)
		}
		if !reloaded.staticID.SPK().Equals(account.staticID.SPK()) {
			t.Fatal("Unexpected reloaded account ID")
		}
		if !reloaded.balance.Equals(account.balance) {
			t.Log(reloaded.balance)
			t.Log(account.balance)
			t.Fatal("Unexpected account balance after reload")
		}
	}

	// reload it to trigger the unclean shutdown
	r, err = rt.reloadRenter(r)
	if err != nil {
		t.Fatal(err)
	}

	// verify the accounts were reloaded but the balances were cleared due to
	// the unclean shutdown
	for _, account := range accounts {
		reloaded, err := r.staticAccountManager.managedOpenAccount(account.staticHostKey)
		if err != nil {
			t.Fatal(err)
		}
		if !account.staticID.SPK().Equals(reloaded.staticID.SPK()) {
			t.Fatal("Unexpected reloaded account ID")
		}
		if !reloaded.balance.IsZero() {
			t.Fatal("Unexpected reloaded account balance")
		}
	}
}

// TestAccountCorrupted verifies accounts that are corrupted are not reloaded
func TestAccountCorrupted(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// create a renter
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := rt.Close()
		if err != nil {
			t.Log(err)
		}
	}()
	r := rt.renter

	// create a number accounts
	accounts := openRandomTestAccountsOnRenter(r)

	// select a random account of which we'll corrupt data on disk
	var corrupted *account
	for _, account := range accounts {
		corrupted = account
		break
	}

	// manually close the renter and corrupt the data at that offset
	err = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	file, err := r.deps.OpenFile(filepath.Join(r.persistDir, accountsFilename), os.O_RDWR, defaultFilePerm)
	if err != nil {
		t.Fatal(err)
	}

	rN := fastrand.Intn(5) + 1
	rOffset := corrupted.staticOffset + int64(fastrand.Intn(accountSize-rN))
	n, err := file.WriteAt(fastrand.Bytes(rN), rOffset)
	if n != rN {
		t.Fatalf("Unexpected amount of bytes written, %v != %v", n, rN)
	}
	if err != nil {
		t.Fatal("Could not write corrupted account data")
	}

	// reopen the renter
	persistDir := filepath.Join(rt.dir, modules.RenterDir)
	r, errChan := New(rt.gateway, rt.cs, rt.wallet, rt.tpool, rt.mux, persistDir)
	if err := <-errChan; err != nil {
		t.Fatal(err)
	}
	err = rt.addRenter(r)

	// verify only the non corrupted accounts got reloaded properly
	am := r.staticAccountManager
	am.mu.Lock()
	// verify the amount of accounts reloaded is one less
	expected := len(accounts) - 1
	if len(am.accounts) != expected {
		t.Errorf("Unexpected amount of accounts, %v != %v", len(am.accounts), expected)
	}
	for _, account := range am.accounts {
		if account.staticID.SPK().Equals(corrupted.staticID.SPK()) {
			t.Error("Corrupted account was not properly skipped")
		}
	}
	am.mu.Unlock()
}

// TestAccountPersistenceToAndFromBytes verifies the functionality of the
// `bytes` and `loadBytes` method on the accountPersistence object
func TestAccountPersistenceToAndFromBytes(t *testing.T) {
	t.Parallel()

	// create a random persistence object and get its bytes
	ap := newRandomAccountPersistence()
	accountBytes := ap.bytes()
	if len(accountBytes) != accountSize {
		t.Fatal("Unexpected account bytes")
	}

	// load the bytes onto a new persistence object and compare for equality
	var uMar accountPersistence
	err := uMar.loadBytes(accountBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ap.AccountID.SPK().Equals(uMar.AccountID.SPK()) {
		t.Fatal("Unexpected AccountID")
	}
	if !ap.Balance.Equals(uMar.Balance) {
		t.Fatal("Unexpected balance")
	}
	if !ap.HostKey.Equals(uMar.HostKey) {
		t.Fatal("Unexpected hostkey")
	}
	if ap.LastUsed != uMar.LastUsed {
		t.Fatal("Unexpected last used")
	}
	if !bytes.Equal(ap.SecretKey[:], uMar.SecretKey[:]) {
		t.Fatal("Unexpected secretkey")
	}

	// corrupt the checksum of the account bytes
	corruptedBytes := accountBytes
	corruptedBytes[fastrand.Intn(crypto.HashSize)] += 1
	err = uMar.loadBytes(corruptedBytes)
	if err != errInvalidChecksum {
		t.Fatalf("Expected error '%v', instead '%v'", errInvalidChecksum, err)
	}

	// corrupt the account data bytes
	corruptedBytes2 := accountBytes
	corruptedBytes2[fastrand.Intn(accountSize-crypto.HashSize)+crypto.HashSize] += 1
	err = uMar.loadBytes(corruptedBytes2)
	if err != errInvalidChecksum {
		t.Fatalf("Expected error '%v', instead '%v'", errInvalidChecksum, err)
	}
}

// TestCompatV150AccountPersistence verifies that the account bytes of an
// account persistence object before it had the `lastUsed` property can be
// loaded into the current persistence object without corrupting it.
func TestCompatV150AccountPersistence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.SkipNow()
	}

	// create a renter tester
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := rt.Close()
		if err != nil {
			t.Fatal(err)
		}
	}()

	// verify it has 0 accounts
	am := rt.renter.staticAccountManager
	am.mu.Lock()
	numAccounts := len(am.accounts)
	am.mu.Unlock()
	if numAccounts != 0 {
		t.Fatalf("Expected 0 accounts to be loaded, however %v were found", numAccounts)
	}

	// close the renter
	err = rt.renter.Close()
	if err != nil {
		t.Fatal(err)
	}

	// copy the compat file to the location of the accounts file
	src := filepath.Join("..", "..", "compatibility", "accountsV150.dat")
	dst := filepath.Join(rt.dir, modules.RenterDir, accountsFilename)
	err = build.CopyFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	// reopen the renter
	r, err := newRenterWithDependency(rt.gateway, rt.cs, rt.wallet, rt.tpool, rt.mux, filepath.Join(rt.dir, modules.RenterDir), &modules.ProductionDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	rt.addRenter(r)

	// verify it can properly load all 163 compat accounts and their lastUsed
	// prop is initialized to 0 - no need to perform any extra validation, the
	// renter will have doen that during bootstrap using the checksum
	am = rt.renter.staticAccountManager
	am.mu.Lock()
	for _, acc := range am.accounts {
		if acc.lastUsed != 0 {
			t.Error("expected `lastUsed` property of a compat account to be initialized to 0", acc.lastUsed)
		}
	}
	numAccounts = len(am.accounts)
	am.mu.Unlock()
	if numAccounts != 163 {
		t.Fatalf("Expected 163 accounts to be loaded, however %v were found", numAccounts)
	}

	// verify the tmp file got cleaned up
	tmp := filepath.Join(rt.dir, modules.RenterDir, "tmp_"+accountsFilename)
	_, err = os.Stat(tmp)
	if !os.IsNotExist(err) {
		t.Fatal("Expected 'NotExist' error, instead err was", err)
	}
}

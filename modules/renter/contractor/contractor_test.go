package contractor

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
)

// newStub is used to test the New function. It implements all of the contractor's
// dependencies.
type newStub struct{}

// consensus set stubs
func (newStub) ConsensusSetSubscribe(modules.ConsensusSetSubscriber, modules.ConsensusChangeID) error {
	return nil
}
func (newStub) Synced() bool { return true }

// wallet stubs
func (newStub) NextAddress() (uc types.UnlockConditions, err error) { return }
func (newStub) StartTransaction() modules.TransactionBuilder        { return nil }

// transaction pool stubs
func (newStub) AcceptTransactionSet([]types.Transaction) error      { return nil }
func (newStub) FeeEstimation() (a types.Currency, b types.Currency) { return }

// hdb stubs
func (newStub) Host(modules.NetAddress) (settings modules.HostDBEntry, ok bool) { return }
func (newStub) RandomHosts(int, []modules.NetAddress) []modules.HostDBEntry     { return nil }

// TestNew tests the New function.
func TestNew(t *testing.T) {
	// Using a stub implementation of the dependencies is fine, as long as its
	// non-nil.
	var stub newStub
	dir := build.TempDir("contractor", "TestNew")

	// Sane values.
	_, err := New(stub, stub, stub, stub, dir)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// Nil consensus set.
	_, err = New(nil, stub, stub, stub, dir)
	if err != errNilCS {
		t.Fatalf("expected %v, got %v", errNilCS, err)
	}

	// Nil wallet.
	_, err = New(stub, nil, stub, stub, dir)
	if err != errNilWallet {
		t.Fatalf("expected %v, got %v", errNilWallet, err)
	}

	// Nil transaction pool.
	_, err = New(stub, stub, nil, stub, dir)
	if err != errNilTpool {
		t.Fatalf("expected %v, got %v", errNilTpool, err)
	}

	// Bad persistDir.
	_, err = New(stub, stub, stub, stub, "")
	if !os.IsNotExist(err) {
		t.Fatalf("expected invalid directory, got %v", err)
	}

	// Corrupted persist file.
	ioutil.WriteFile(filepath.Join(dir, "contractor.json"), []byte{1, 2, 3}, 0666)
	_, err = New(stub, stub, stub, stub, dir)
	if _, ok := err.(*json.SyntaxError); !ok {
		t.Fatalf("expected invalid json, got %v", err)
	}

	// Corrupted logfile.
	os.RemoveAll(filepath.Join(dir, "contractor.log"))
	f, err := os.OpenFile(filepath.Join(dir, "contractor.log"), os.O_CREATE, 0000)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = New(stub, stub, stub, stub, dir)
	if !os.IsPermission(err) {
		t.Fatalf("expected permissions error, got %v", err)
	}
}

// TestContracts tests the Contracts method.
func TestContracts(t *testing.T) {
	c := &Contractor{
		contracts: map[types.FileContractID]modules.RenterContract{
			{1}: {ID: types.FileContractID{1}, NetAddress: "foo"},
			{2}: {ID: types.FileContractID{2}, NetAddress: "bar"},
			{3}: {ID: types.FileContractID{3}, NetAddress: "baz"},
		},
	}
	for _, contract := range c.Contracts() {
		if exp := c.contracts[contract.ID]; exp.NetAddress != contract.NetAddress {
			t.Errorf("contract does not match: expected %v, got %v", exp.NetAddress, contract.NetAddress)
		}
	}
}

// TestAllowance tests the Allowance method.
func TestAllowance(t *testing.T) {
	c := &Contractor{
		allowance: modules.Allowance{
			Funds:  types.NewCurrency64(1),
			Period: 2,
			Hosts:  3,
		},
	}
	a := c.Allowance()
	if a.Funds.Cmp(c.allowance.Funds) != 0 ||
		a.Period != c.allowance.Period ||
		a.Hosts != c.allowance.Hosts {
		t.Fatal("Allowance did not return correct allowance:", a, c.allowance)
	}
}

// stubHostDB mocks the hostDB dependency using zero-valued implementations of
// its methods.
type stubHostDB struct{}

func (stubHostDB) Host(modules.NetAddress) (h modules.HostDBEntry, ok bool)         { return }
func (stubHostDB) RandomHosts(int, []modules.NetAddress) (hs []modules.HostDBEntry) { return }

// TestIntegrationSetAllowance tests the SetAllowance method.
func TestIntegrationSetAllowance(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	// create testing trio
	h, c, m, err := newTestingTrio("TestIntegrationAutoRenew")
	if err != nil {
		t.Fatal(err)
	}

	// bad args
	var a modules.Allowance
	err = c.SetAllowance(a)
	if err == nil {
		t.Error("expected error, got nil")
	}
	a.Hosts = 1
	err = c.SetAllowance(a)
	if err == nil {
		t.Error("expected error, got nil")
	}
	a.Period = 20
	err = c.SetAllowance(a)
	if err == nil {
		t.Error("expected error, got nil")
	}
	a.RenewWindow = 20
	err = c.SetAllowance(a)
	if err == nil {
		t.Error("expected error, got nil")
	}

	// reasonable values; should succeed
	a.Funds = types.SiacoinPrecision.Mul64(100)
	a.RenewWindow = 10
	err = c.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.contracts) != 1 {
		t.Error("expected 1 contract, got", len(c.contracts))
	}

	// set same allowance; should no-op
	err = c.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.contracts) != 1 {
		t.Error("expected 1 contract, got", len(c.contracts))
	}

	// reannounce host on different IP (easier than creating a new host)
	addr := "127.0.0.1:" + c.Contracts()[0].NetAddress.Port()
	err = h.AnnounceAddress(modules.NetAddress(addr))
	if err != nil {
		t.Fatal(err)
	}
	m.AddBlock()

	// wait for hostdb to scan host
	for i := 0; i < 500 && len(c.hdb.RandomHosts(2, nil)) != 2; i++ {
		time.Sleep(time.Millisecond)
	}

	// set allowance with Hosts = 2; should only form one new contract
	a.Hosts = 2
	err = c.SetAllowance(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.contracts) != 2 {
		t.Error("expected 2 contracts, got", len(c.contracts))
	}
}

// testWalletShim is used to test the walletBridge type.
type testWalletShim struct {
	nextAddressCalled bool
	startTxnCalled    bool
}

// These stub implementations for the walletShim interface set their respective
// booleans to true, allowing tests to verify that they have been called.
func (ws *testWalletShim) NextAddress() (types.UnlockConditions, error) {
	ws.nextAddressCalled = true
	return types.UnlockConditions{}, nil
}
func (ws *testWalletShim) StartTransaction() modules.TransactionBuilder {
	ws.startTxnCalled = true
	return nil
}

// TestWalletBridge tests the walletBridge type.
func TestWalletBridge(t *testing.T) {
	shim := new(testWalletShim)
	bridge := walletBridge{shim}
	bridge.NextAddress()
	if !shim.nextAddressCalled {
		t.Error("NextAddress was not called on the shim")
	}
	bridge.StartTransaction()
	if !shim.startTxnCalled {
		t.Error("StartTransaction was not called on the shim")
	}
}

package host

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/host"
	"gitlab.com/NebulousLabs/Sia/modules/host/contractmanager"
	"gitlab.com/NebulousLabs/Sia/node"
	"gitlab.com/NebulousLabs/Sia/node/api"
	"gitlab.com/NebulousLabs/Sia/node/api/client"
	"gitlab.com/NebulousLabs/Sia/siatest"
	"gitlab.com/NebulousLabs/Sia/siatest/dependencies"
)

// TestHostGetPubKey confirms that the pubkey is returned through the API
func TestHostGetPubKey(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Host
	testDir := hostTestDir(t.Name())

	// Create a new server
	hostParams := node.Host(testDir)
	testNode, err := siatest.NewCleanNode(hostParams)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := testNode.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Call HostGet, confirm public key is not a blank key
	hg, err := testNode.HostGet()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hg.PublicKey.Key, []byte{}) {
		t.Fatal("Host has empty pubkey key", hg.PublicKey.Key)
	}

	// Get host pubkey from the server and compare to the pubkey return through
	// the HostGet endpoint
	pk, err := testNode.HostPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !pk.Equals(hg.PublicKey) {
		t.Log("HostGet PubKey:", hg.PublicKey)
		t.Log("Server PubKey:", pk)
		t.Fatal("Public Keys don't match")
	}
}

// TestHostAlertDiskTrouble verifies the host properly registers the disk
// trouble alert, and returns it through the alerts endpoint
func TestHostAlertDiskTrouble(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	groupParams := siatest.GroupParams{
		Miners: 1,
	}

	alert := modules.Alert{
		Cause:    "",
		Module:   "contractmanager",
		Msg:      contractmanager.AlertMSGHostDiskTrouble,
		Severity: modules.SeverityCritical,
	}

	testDir := hostTestDir(t.Name())
	tg, err := siatest.NewGroupFromTemplate(testDir, groupParams)
	if err != nil {
		t.Fatal("Failed to create group:", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a node which won't be able to form a contract due to disk trouble
	depDiskTrouble := dependencies.NewDependencyHostDiskTrouble()
	hostParams := node.Host(filepath.Join(testDir, "/host"))
	hostParams.StorageManagerDeps = depDiskTrouble
	nodes, err := tg.AddNodes(hostParams)
	if err != nil {
		t.Fatal(err)
	}
	h := nodes[0]

	// Add a storage folder and resize it - should trigger disk trouble
	sf := hostTestDir("/some/folder")
	err = h.HostStorageFoldersAddPost(sf, 1<<24)
	if err != nil {
		t.Fatal(err)
	}

	depDiskTrouble.Fail()
	_ = h.HostStorageFoldersResizePost(sf, 1<<23)

	// Test that host registered the alert.
	err = h.IsAlertRegistered(alert)
	if err != nil {
		t.Fatal(err)
	}

	// Test that host reload unregisters the alert
	err = tg.RestartNode(h)
	if err != nil {
		t.Fatal(err)
	}
	err = h.IsAlertUnregistered(alert)
	if err != nil {
		t.Fatal(err)
	}
}

// TestHostAlertInsufficientCollateral verifies the host properly registers the
// insufficient collateral alert, and returns it through the alerts endpoint
func TestHostAlertInsufficientCollateral(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a test group
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	testDir := hostTestDir(t.Name())
	tg, err := siatest.NewGroupFromTemplate(testDir, groupParams)
	if err != nil {
		t.Fatal("Failed to create group:", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Confirm contracts got created
	r := tg.Renters()[0]
	rc, err := r.RenterContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.ActiveContracts) == 0 {
		t.Fatal("No contracts created")
	}

	// Nullify the host's collateral budget
	h := tg.Hosts()[0]
	hS, _ := h.HostGet()
	err = h.HostModifySettingPost(client.HostParamCollateralBudget, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Mine blocks to force contract renewal
	if err = siatest.RenewContractsByRenewWindow(r, tg); err != nil {
		t.Fatal(err)
	}

	// Test that host registered alert.
	alert := modules.Alert{
		Cause:    "",
		Module:   "host",
		Msg:      host.AlertMSGHostInsufficientCollateral,
		Severity: modules.SeverityWarning,
	}

	if err = h.IsAlertRegistered(alert); err != nil {
		t.Fatal(err)
	}

	// Reinstate the host's collateral budget
	err = h.HostModifySettingPost(client.HostParamCollateralBudget, hS.InternalSettings.CollateralBudget)
	if err != nil {
		t.Fatal(err)
	}

	// Test that host unregistered alert.
	if err = h.IsAlertUnregistered(alert); err != nil {
		t.Fatal(err)
	}
}

// TestHostBandwidth confirms that the host module is monitoring bandwidth
func TestHostBandwidth(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	gp := siatest.GroupParams{
		Hosts:   2,
		Renters: 0,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(hostTestDir(t.Name()), gp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	hostNode := tg.Hosts()[0]

	hbw, err := hostNode.HostBandwidthGet()
	if err != nil {
		t.Fatal(err)
	}

	if hbw.Upload != 0 || hbw.Download != 0 {
		t.Fatal("Expected host to have no upload or download bandwidth")
	}

	if _, err := tg.AddNodes(node.RenterTemplate); err != nil {
		t.Fatal(err)
	}

	hbw, err = hostNode.HostBandwidthGet()
	if err != nil {
		t.Fatal(err)
	}

	if hbw.Upload == 0 || hbw.Download == 0 {
		t.Fatal("Expected host to use bandwidth from rpc with new renter node")
	}

	lastUpload := hbw.Upload
	lastDownload := hbw.Download
	renterNode := tg.Renters()[0]

	_, rf, err := renterNode.UploadNewFileBlocking(100, 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}

	hbw, err = hostNode.HostBandwidthGet()
	if err != nil {
		t.Fatal(err)
	}

	if hbw.Upload <= lastUpload || hbw.Download <= lastDownload {
		t.Fatal("Expected host to use more bandwidth from uploaded file")
	}

	lastUpload = hbw.Upload
	lastDownload = hbw.Download

	if _, _, err := renterNode.DownloadToDisk(rf, false); err != nil {
		t.Fatal(err)
	}

	hbw, err = hostNode.HostBandwidthGet()
	if err != nil {
		t.Fatal(err)
	}

	if hbw.Upload <= lastUpload || hbw.Download <= lastDownload {
		t.Fatal("Expected host to use more bandwidth from downloaded file")
	}
}

// TestHostContracts confirms that the host contracts endpoint returns the expected values
func TestHostContracts(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	gp := siatest.GroupParams{
		Hosts:   2,
		Renters: 0,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(hostTestDir(t.Name()), gp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	hostNode := tg.Hosts()[0]
	hc, err := hostNode.HostContractInfoGet()
	if err != nil {
		t.Fatal(err)
	}

	if len(hc.Contracts) != 0 {
		t.Fatal("expected host to have no contracts")
	}

	if _, err := tg.AddNodes(node.RenterTemplate); err != nil {
		t.Fatal(err)
	}

	renterNode := tg.Renters()[0]
	hc, err = hostNode.HostContractInfoGet()
	if err != nil {
		t.Fatal(err)
	}

	if len(hc.Contracts) == 0 {
		t.Fatal("expected host to have new contract")
	}

	if hc.Contracts[0].DataSize != 0 {
		t.Fatal("contract should have 0 datasize")
	}

	prevValidPayout := hc.Contracts[0].ValidProofOutputs[1].Value
	prevMissPayout := hc.Contracts[0].MissedProofOutputs[1].Value
	_, _, err = renterNode.UploadNewFileBlocking(4096, 1, 1, true)
	if err != nil {
		t.Fatal(err)
	}

	hc, err = hostNode.HostContractInfoGet()
	if err != nil {
		t.Fatal(err)
	}

	if hc.Contracts[0].DataSize != 4096 {
		t.Fatal("contract should have 1 sector uploaded")
	}

	if hc.Contracts[0].RevisionNumber != 2 {
		t.Fatal("contract should have 1 revision from upload")
	}

	if hc.Contracts[0].PotentialUploadRevenue.Cmp64(0) != 1 {
		t.Fatal("contract should have upload revenue")
	}

	if hc.Contracts[0].PotentialStorageRevenue.Cmp64(0) != 1 {
		t.Fatal("contract should have storage revenue")
	}

	if hc.Contracts[0].ValidProofOutputs[1].Value.Cmp(prevValidPayout) != 1 {
		t.Fatal("valid payout should be greater than old valid payout")
	}

	if hc.Contracts[0].MissedProofOutputs[1].Value.Cmp(prevMissPayout) != -1 {
		t.Fatal("missed payout should be less than old missed payout")
	}
}

// TestHostValidPrices confirms that the user can't set invalid prices through
// the API
func TestHostValidPrices(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create Host
	testDir := hostTestDir(t.Name())
	hostParams := node.Host(testDir)
	host, err := siatest.NewCleanNode(hostParams)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Get the Host
	hg, err := host.HostGet()
	if err != nil {
		t.Fatal(err)
	}

	// Verify that setting an invalid RPC price will return an error
	rpcPrice := hg.InternalSettings.MaxBaseRPCPrice().Mul64(modules.MaxBaseRPCPriceVsBandwidth)
	err = host.HostModifySettingPost(client.HostParamMinBaseRPCPrice, rpcPrice)
	if err == nil || !strings.Contains(err.Error(), api.ErrInvalidRPCDownloadRatio.Error()) {
		t.Fatalf("Expected Error %v but got %v", api.ErrInvalidRPCDownloadRatio, err)
	}

	// Verify that setting an invalid Sector price will return an error
	sectorPrice := hg.InternalSettings.MaxSectorAccessPrice().Mul64(modules.MaxSectorAccessPriceVsBandwidth)
	err = host.HostModifySettingPost(client.HostParamMinSectorAccessPrice, sectorPrice)
	if err == nil || !strings.Contains(err.Error(), api.ErrInvalidSectorAccessDownloadRatio.Error()) {
		t.Fatalf("Expected Error %v but got %v", api.ErrInvalidSectorAccessDownloadRatio, err)
	}

	// Verify that setting an invalid download price will return an error. Error
	// should be the RPC error since that is the first check
	downloadPrice := hg.InternalSettings.MinDownloadBandwidthPrice.Div64(modules.MaxBaseRPCPriceVsBandwidth)
	err = host.HostModifySettingPost(client.HostParamMinDownloadBandwidthPrice, downloadPrice)
	if err == nil || !strings.Contains(err.Error(), api.ErrInvalidRPCDownloadRatio.Error()) {
		t.Fatalf("Expected Error %v but got %v", api.ErrInvalidRPCDownloadRatio, err)
	}
}

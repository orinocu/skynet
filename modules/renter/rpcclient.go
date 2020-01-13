package renter

import (
	"encoding/json"
	"fmt"
	"sync"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/persist"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/threadgroup"
)

var (
	// errRPCNotAvailable is returned when the requested RPC is not available on
	// the host. This is possible when a host runs an older version, or when it
	// is not fully synced and disables his ephemeral account manager.
	errRPCNotAvailable = errors.New("RPC not available on host")
)

// RPCClient interface lists all possible RPC that can be called on the host
type RPCClient interface {
	UpdatePriceTable() error
	FundEphemeralAccount(id string, amount types.Currency) error
}

// hostRPCClient wraps all necessities to communicate with a host
type hostRPCClient struct {
	staticPaymentProvider modules.PaymentProvider
	staticPeerMux         *modules.PeerMux

	priceTable        modules.RPCPriceTable
	priceTableUpdated types.BlockHeight

	// blockHeight is cached on every client and gets updated by the renter when
	// consensus changes. This to avoid fetching the block height from the
	// renter on every RPC call.
	blockHeight types.BlockHeight

	renter *Renter
	log    *persist.Logger
	tg     *threadgroup.ThreadGroup
	mu     sync.Mutex
}

// newRPCClient returns a new RPC client.
func (r *Renter) newRPCClient(pm *modules.PeerMux, pp modules.PaymentProvider, cbh types.BlockHeight, tg *threadgroup.ThreadGroup, log *persist.Logger) (RPCClient, error) {
	client := hostRPCClient{
		staticPaymentProvider: pp,
		staticPeerMux:         pm,
		blockHeight:           cbh,
		renter:                r,
		log:                   log,
		tg:                    tg,
	}

	if err := client.UpdatePriceTable(); err != nil {
		// Return nil if we weren't able fetch the host's pricing.
		return nil, err
	}
	return &client, nil
}

// UpdateBlockHeight is called by the renter when it processes a consensus
// change. Every time the block height gets updated we potentially also update
// the RPC price table to get the host's latest prices.
func (c *hostRPCClient) UpdateBlockHeight(blockHeight types.BlockHeight) {
	var updatePriceTable bool
	defer func() {
		if updatePriceTable {
			go c.threadedUpdatePriceTable()
		}
	}()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.blockHeight = blockHeight

	// This is more of a sanity check to prevent underflow. This could only be
	// the case if the renter and host's blockheight differ by a large amount of
	// blocks.
	if c.priceTableUpdated > c.priceTable.Expiry {
		updatePriceTable = true
		return
	}

	// Update the price table if the current blockheight has surpassed half of
	// the expiry window. The expiry window is defined as the time (in blocks)
	// since we last updated the RPC price table until its expiry block height.
	window := uint64(c.priceTable.Expiry - c.priceTableUpdated)
	if uint64(c.blockHeight) > uint64(c.priceTableUpdated)+window/2 {
		updatePriceTable = true
		return
	}
}

// UpdatePriceTable performs the updatePriceTableRPC on the host.
func (c *hostRPCClient) UpdatePriceTable() error {
	// Fetch a stream from the mux
	stream := c.staticPeerMux.NewStream()
	defer stream.Close()

	// Write the RPC id on the stream, there's no request object as it's
	// implied from the RPC id.
	if err := stream.WriteObjects(modules.RPCUpdatePriceTable); err != nil {
		return err
	}

	// Receive RPCUpdatePriceTableResponse
	var uptr modules.RPCUpdatePriceTableResponse
	if err := stream.ReadObject(uptr); err != nil {
		return err
	}
	var updated modules.RPCPriceTable
	if err := json.Unmarshal(uptr.PriceTableJSON, &updated); err != nil {
		return err
	}

	// Perform gouging check
	allowance := c.renter.hostContractor.Allowance()
	err := checkPriceTableGouging(allowance, updated)
	if err != nil {
		// TODO: (follow-up) this should negatively affect the host's score
		return err
	}

	// Provide payment for the RPC
	cost := updated.Costs[modules.RPCUpdatePriceTable]
	_, err = c.staticPaymentProvider.ProvidePaymentForRPC(modules.RPCUpdatePriceTable, cost, stream, c.blockHeight)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.priceTable = updated
	c.priceTableUpdated = c.blockHeight
	c.mu.Unlock()
	return nil
}

// FundEphemeralAccount will deposit the given amount into the account with id
// by calling the fundEphemeralAccountRPC on the host.
func (c *hostRPCClient) FundEphemeralAccount(id string, amount types.Currency) error {
	c.mu.Lock()
	pt := c.priceTable
	bh := c.blockHeight
	c.mu.Unlock()

	// Calculate the cost of the RPC
	cost, available := pt.Costs[modules.RPCFundEphemeralAccount]
	if !available {
		return errors.AddContext(errRPCNotAvailable, fmt.Sprintf("Failed to fund ephemeral account %v", id))
	}

	// Get a stream
	stream := c.staticPeerMux.NewStream()
	defer stream.Close()

	// Write the RPC id and RPCFundEphemeralAccountRequest object on the stream.
	if err := stream.WriteObjects(modules.RPCFundEphemeralAccount, modules.RPCFundEphemeralAccountRequest{AccountID: id}); err != nil {
		return err
	}

	// Provide payment for the RPC and await response
	payment := amount.Add(cost)
	_, err := c.staticPaymentProvider.ProvidePaymentForRPC(modules.RPCFundEphemeralAccount, payment, stream, bh)
	if err != nil {
		return err
	}

	// Receive RPCFundEphemeralAccountResponse
	var fundAccResponse modules.RPCFundEphemeralAccountResponse
	if err := stream.ReadObject(fundAccResponse); err != nil {
		return err
	}

	return nil
}

// threadedUpdatePriceTable will update the RPC price table by fetching the
// host's latest prices.
func (c *hostRPCClient) threadedUpdatePriceTable() {
	if err := c.tg.Add(); err != nil {
		return
	}
	defer c.tg.Done()

	err := c.UpdatePriceTable()
	if err != nil {
		c.log.Println("Failed to update the RPC price table", err)
	}
}

// checkPriceTableGouging checks that the host is not gouging the renter during
// a price table update.
func checkPriceTableGouging(allowance modules.Allowance, priceTable modules.RPCPriceTable) error {
	// TODO
	return nil
}

package host

import (
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/siamux"
)

type (
	// registrySubscriptions is a helper type that holds all current
	// subscriptions.
	registrySubscriptions struct {
		mu            sync.Mutex
		subscriptions map[subscriptionID]map[*subscriptionInfo]struct{}
	}
	// subscriptionInfo holds the information required to respond to a
	// subscriber and to correctly charge it.
	subscriptionInfo struct {
		pt *modules.RPCPriceTable
		mu sync.Mutex

		staticStream siamux.Stream
	}

	// subscriptionID is a hash derived from the public key and tweak that a
	// renter would like to subscribe to.
	subscriptionID crypto.Hash
)

// createSubscriptionID is a helper to derive a subscription id.
func createSubscriptionID(pubKey types.SiaPublicKey, tweak crypto.Hash) subscriptionID {
	return subscriptionID(crypto.HashAll(pubKey, tweak))
}

// newRegistrySubscriptions creates a new registrySubscriptions instance.
func newRegistrySubscriptions() *registrySubscriptions {
	return &registrySubscriptions{
		subscriptions: make(map[subscriptionID]map[*subscriptionInfo]struct{}),
	}
}

// subscriptionPeriodCost is a helper that returns the cost of storing a
// provided number of subscriptions for a subscription period.
func subscriptionPeriodCost(pt *modules.RPCPriceTable, numSubscriptions uint64) types.Currency {
	memory := numSubscriptions * modules.SubscriptionEntrySize
	return pt.SubscriptionBaseCost.Add(pt.SubscriptionMemoryCost.Mul64(memory))
}

// AddSubscription adds one of multiple subscription.
func (rs *registrySubscriptions) AddSubscriptions(info *subscriptionInfo, entryIDs ...subscriptionID) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, entryID := range entryIDs {
		if _, exists := rs.subscriptions[entryID]; !exists {
			rs.subscriptions[entryID] = make(map[*subscriptionInfo]struct{})
		}
		rs.subscriptions[entryID][info] = struct{}{}
	}
}

// RemoveSubscriptions removes one or multiple subscriptions.
func (rs *registrySubscriptions) RemoveSubscriptions(info *subscriptionInfo, entryIDs ...subscriptionID) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, entryID := range entryIDs {
		infos, found := rs.subscriptions[entryID]
		if !found {
			continue
		}
		delete(infos, info)

		if len(infos) == 0 {
			delete(rs.subscriptions, entryID)
		}
	}
}

// managedHandleSubscribeRequest handles a new subscription.
func (h *Host) managedHandleSubscribeRequest(info *subscriptionInfo, subs map[subscriptionID]struct{}, pt *modules.RPCPriceTable, pd modules.PaymentDetails) (types.Currency, error) {
	stream := info.staticStream

	// Read a number indicating how many requests to expect.
	var numSubs uint64
	err := modules.RPCRead(stream, &numSubs)
	if err != nil {
		return types.ZeroCurrency, errors.New("failed to read number of requests to expect")
	}

	// Check payment first.
	cost := subscriptionPeriodCost(pt, 1).Mul64(numSubs)
	if pd.Amount().Cmp(cost) < 0 {
		return types.ZeroCurrency, modules.ErrInsufficientPaymentForRPC
	}
	refund := pd.Amount().Sub(cost)

	// Read the requests and apply them.
	ids := make([]subscriptionID, 0, numSubs)
	for i := uint64(0); i < numSubs; i++ {
		var rsr modules.RPCRegistrySubscriptionRequest
		err = modules.RPCRead(stream, &rsr)
		if err != nil {
			return refund, errors.AddContext(err, "failed to read subscription request")
		}
		ids = append(ids, createSubscriptionID(rsr.PubKey, rsr.Tweak))
	}
	// Add the subscriptions.
	h.staticRegistrySubscriptions.AddSubscriptions(info, ids...)
	return refund, nil
}

// managedHandleSubscribeRequest handles a request to unsubscribe.
func (h *Host) managedHandleUnsubscribeRequest(info *subscriptionInfo, subs map[subscriptionID]struct{}, pt *modules.RPCPriceTable, pd modules.PaymentDetails) (types.Currency, error) {
	stream := info.staticStream

	// Read a number indicating how many requests to expect.
	var numUnsubs uint64
	err := modules.RPCRead(stream, &numUnsubs)
	if err != nil {
		return types.ZeroCurrency, errors.New("failed to read number of requests to expect")
	}

	// Check payment first.
	cost := subscriptionPeriodCost(pt, 0).Mul64(numUnsubs) // no need to pay for memory upon deletion
	if pd.Amount().Cmp(cost) < 0 {
		return types.ZeroCurrency, modules.ErrInsufficientPaymentForRPC
	}
	refund := pd.Amount().Sub(cost)

	// Read the requests.
	ids := make([]subscriptionID, 0, numUnsubs)
	for i := uint64(0); i < numUnsubs; i++ {
		var rsr modules.RPCRegistrySubscriptionRequest
		err = modules.RPCRead(stream, &rsr)
		if err != nil {
			return refund, errors.AddContext(err, "failed to read subscription request")
		}
		ids = append(ids, createSubscriptionID(rsr.PubKey, rsr.Tweak))
	}

	// Remove the subscription.
	h.staticRegistrySubscriptions.RemoveSubscriptions(info, ids...)
	return refund, nil
}

// managedHandleExtendSubscriptionRequest handles a request to extend the subscription.
func (h *Host) managedHandleExtendSubscriptionRequest(stream siamux.Stream, subs map[subscriptionID]struct{}, oldDeadline time.Time, pt *modules.RPCPriceTable, pd modules.PaymentDetails) (types.Currency, time.Time, error) {
	// Get new deadline.
	newDeadline := oldDeadline.Add(modules.SubscriptionPeriod)

	// Check payment first.
	cost := subscriptionPeriodCost(pt, uint64(len(subs)))
	if pd.Amount().Cmp(cost) < 0 {
		return types.ZeroCurrency, time.Time{}, modules.ErrInsufficientPaymentForRPC
	}
	refund := pd.Amount().Sub(cost)

	// Set deadline.
	err := stream.SetReadDeadline(newDeadline)
	if err != nil {
		return refund, time.Time{}, errors.AddContext(err, "failed to extend stream deadline")
	}
	return refund, newDeadline, nil
}

// threadedNotifySubscribers handles notifying all subscribers for a certain
// key/tweak combination.
func (h *Host) threadedNotifySubscribers(pubKey types.SiaPublicKey, tweak crypto.Hash) {
	err := h.tg.Add()
	if err != nil {
		return
	}
	defer h.tg.Done()

	id := createSubscriptionID(pubKey, tweak)

	h.staticRegistrySubscriptions.mu.Lock()
	defer h.staticRegistrySubscriptions.mu.Unlock()
	infos, found := h.staticRegistrySubscriptions.subscriptions[id]
	if !found {
		return
	}
	for info := range infos {
		go func(info *subscriptionInfo) {
			// Lock the info while notifying the subscriber.
			info.mu.Lock()
			defer info.mu.Unlock()

			// Notify the caller.
			panic("not implemented yet")
		}(info)
	}
}

// managedRPCRegistrySubscribe handles the RegistrySubscribe rpc.
func (h *Host) managedRPCRegistrySubscribe(stream siamux.Stream) (err error) {
	// read the price table
	pt, err := h.staticReadPriceTableID(stream)
	if err != nil {
		return errors.AddContext(err, "failed to read price table")
	}

	// Process payment.
	pd, err := h.ProcessPayment(stream)
	if err != nil {
		return errors.AddContext(err, "failed to process payment")
	}

	// Check payment.
	if pd.Amount().Cmp(pt.SubscriptionBaseCost) < 0 {
		return modules.ErrInsufficientPaymentForRPC
	}

	// Refund excessive amount.
	if pd.Amount().Cmp(pt.SubscriptionBaseCost) > 0 {
		err = h.staticAccountManager.callRefund(pd.AccountID(), pd.Amount().Sub(pt.SubscriptionBaseCost))
		if err != nil {
			return errors.AddContext(err, "failed to refund excessive initial subscription payment")
		}
	}

	// Set the stream deadline.
	subscriptionTimeExtension := 5 * time.Minute
	deadline := time.Now().Add(subscriptionTimeExtension)
	err = stream.SetReadDeadline(deadline)
	if err != nil {
		return errors.AddContext(err, "failed to set intitial subscription deadline")
	}

	// Keep count of the unique subscriptions to be able to charge accordingly.
	subscriptions := make(map[subscriptionID]struct{})
	info := &subscriptionInfo{
		staticStream: stream,
		pt:           pt,
	}

	// Clean up the subscriptions at the end.
	defer func() {
		entryIDs := make([]subscriptionID, 0, len(subscriptions))
		for entryID := range subscriptions {
			entryIDs = append(entryIDs, entryID)
		}
		h.staticRegistrySubscriptions.RemoveSubscriptions(info, entryIDs...)
	}()

	// The subscription RPC is a request/response loop that continues for as
	// long as the renter keeps paying for it.
	for {
		// Read subscription request.
		var requestType uint8
		err = modules.RPCRead(stream, &requestType)
		if err != nil {
			return errors.AddContext(err, "failed to read request type")
		}

		// Read the price table
		pt, err = h.staticReadPriceTableID(stream)
		if err != nil {
			return errors.AddContext(err, "failed to read price table")
		}

		// Update the subscription info's price table.
		info.mu.Lock()
		info.pt = pt
		info.mu.Unlock()

		// Process payment.
		pd, err := h.ProcessPayment(stream)
		if err != nil {
			return errors.AddContext(err, "failed to process payment")
		}

		// Handle requests.
		var refund types.Currency
		switch requestType {
		case modules.SubscriptionRequestSubscribe:
			refund, err = h.managedHandleSubscribeRequest(info, subscriptions, pt, pd)
		case modules.SubscriptionRequestUnsubscribe:
			refund, err = h.managedHandleUnsubscribeRequest(info, subscriptions, pt, pd)
		case modules.SubscriptionRequestExtend:
			refund, deadline, err = h.managedHandleExtendSubscriptionRequest(stream, subscriptions, deadline, pt, pd)
		default:
			return errors.New("unknown request type")
		}
		// Refund excessive payment before checking the error.
		if !refund.IsZero() {
			err = errors.Compose(err, h.staticAccountManager.callRefund(pd.AccountID(), refund))
		}
		// Check the errors.
		if err != nil {
			return errors.AddContext(err, "failed to handle request")
		}
	}
}

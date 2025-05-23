// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package eth

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"decred.org/dcrdex/dex"
	dexeth "decred.org/dcrdex/dex/networks/eth"
	swapv0 "decred.org/dcrdex/dex/networks/eth/contracts/v0"
	swapv1 "decred.org/dcrdex/dex/networks/eth/contracts/v1"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Check that rpcclient satisfies the ethFetcher interface.
var (
	_ ethFetcher = (*rpcclient)(nil)

	bigZero                    = new(big.Int)
	headerExpirationTime       = time.Minute
	monitorConnectionsInterval = 30 * time.Second
	// failingEndpointsCheckFreq means that endpoints that were never connected
	// will be attempted every (monitorConnectionsInterval * failingEndpointsCheckFreq).
	failingEndpointsCheckFreq = 4
)

type ContextCaller interface {
	CallContext(ctx context.Context, result any, method string, args ...any) error
}

type ethConn struct {
	*ethclient.Client
	endpoint string
	priority uint16
	// swapContract is the current ETH swapContract.
	swapContract swapContract
	// tokens are tokeners for loaded tokens. tokens is not protected by a
	// mutex, as it is expected that the caller will connect and place calls to
	// loadToken sequentially in the same thread during initialization.
	tokens map[uint32]*tokener
	// caller is a client for raw calls not implemented by *ethclient.Client.
	caller          ContextCaller
	txPoolSupported bool

	tipCache struct {
		sync.Mutex
		expiration time.Duration
		lastUpdate time.Time
		hdr        *types.Header
	}
}

func (ec *ethConn) String() string {
	return ec.endpoint
}

// monitorBlocks creates a block header subscription and updates the tipCache.
func (ec *ethConn) monitorBlocks(ctx context.Context, log dex.Logger) {
	c := &ec.tipCache

	// No matter why we exit, revert to manual tip checks.
	defer func() {
		log.Tracef("Exiting block monitor for %s", ec.endpoint)
		c.Lock()
		c.expiration = time.Second * 99 / 10 // 9.9 seconds.
		c.Unlock()
	}()

	h := make(chan *types.Header, 8)
	sub, err := ec.SubscribeNewHead(ctx, h)
	if err != nil {
		log.Errorf("Error connecting to Websockets headers: %w", err)
		return
	}

	defer func() {
		// If a provider does not respond to an unsubscribe request, the unsubscribe function
		// will never return because geth does not use a timeout.
		doneUnsubbing := make(chan struct{})
		go func() {
			sub.Unsubscribe()
			close(doneUnsubbing)
		}()
		select {
		case <-doneUnsubbing:
		case <-time.After(10 * time.Second):
			log.Errorf("Timed out waiting to unsubscribe from %q", ec.endpoint)
		}
	}()

	for {
		select {
		case hdr := <-h:
			c.Lock()
			c.hdr = hdr
			c.lastUpdate = time.Now()
			c.Unlock()
		case err, ok := <-sub.Err():
			if !ok {
				// Subscription cancelled
				return
			}
			if ctx.Err() != nil || err == nil { // Both conditions indicate normal close
				return
			}
			log.Errorf("Header subscription to %s failed with error: %v", ec.endpoint, err)
			log.Infof("Falling back to manual header requests for %s", ec.endpoint)
			return
		case <-ctx.Done():
			return
		}
	}
}

type endpoint struct {
	url      string
	priority uint16
}

func (ec *ethConn) tip(ctx context.Context) (*types.Header, error) {
	cache := &ec.tipCache
	cache.Lock()
	defer cache.Unlock()
	if time.Since(cache.lastUpdate) < cache.expiration && cache.hdr != nil {
		return cache.hdr, nil
	}
	hdr, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	cache.lastUpdate = time.Now()
	cache.hdr = hdr
	return hdr, nil
}

func (ep endpoint) String() string {
	return ep.url
}

var _ fmt.Stringer = endpoint{} // compile error if pointer receiver
var _ fmt.Stringer = (*endpoint)(nil)

type rpcclient struct {
	net dex.Network
	log dex.Logger

	baseChainID    uint32
	genesisChainID uint64
	baseChainName  string

	// endpoints should only be used during connect to know which endpoints
	// to attempt to connect. If we were unable to connect to some of the
	// endpoints, they will not be included in the clients slice.
	endpoints []endpoint
	// neverConnectedEndpoints failed to connect since the initial connect call,
	// so an ethConn has not been created for them.
	neverConnectedEndpoints []endpoint
	healthCheckCounter      int
	tokensLoaded            map[uint32]*VersionedToken
	ethContractVer          uint32
	ethContractAddr         common.Address
	ethContractAddrV1       common.Address

	// the order of clients will change based on the health of the connections.
	clientsMtx sync.RWMutex
	clients    []*ethConn
}

func newRPCClient(baseChainID uint32, chainID uint64, net dex.Network, endpoints []endpoint, ethContractVer uint32, ethContractAddr, ethContractAddrV1 common.Address, log dex.Logger) *rpcclient {
	return &rpcclient{
		baseChainID:       baseChainID,
		genesisChainID:    chainID,
		baseChainName:     strings.ToUpper(dex.BipIDSymbol(baseChainID)),
		net:               net,
		endpoints:         endpoints,
		log:               log,
		ethContractVer:    ethContractVer,
		ethContractAddr:   ethContractAddr,
		ethContractAddrV1: ethContractAddrV1,
		tokensLoaded:      make(map[uint32]*VersionedToken),
	}
}

func (c *rpcclient) clientsCopy() []*ethConn {
	c.clientsMtx.RLock()
	defer c.clientsMtx.RUnlock()

	clients := make([]*ethConn, len(c.clients))
	copy(clients, c.clients)
	return clients
}

func (c *rpcclient) connectToEndpoint(ctx context.Context, endpoint endpoint) (*ethConn, error) {
	var success bool

	client, err := rpc.DialContext(ctx, endpoint.url)
	if err != nil {
		return nil, err
	}

	defer func() {
		// This shouldn't happen as the only possible errors are due to ETHSwap and
		// tokener creation.
		if !success {
			client.Close()
		}
	}()

	ec := &ethConn{
		Client:   ethclient.NewClient(client),
		endpoint: endpoint.url,
		priority: endpoint.priority,
		tokens:   make(map[uint32]*tokener),
		caller:   client,
	}

	chainID, err := ec.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("error checking chain ID from %q: %w", endpoint.url, err)
	}
	if chainID.Uint64() != c.genesisChainID {
		return nil, fmt.Errorf("wrong chain ID from %q. wanted %d, got %d", endpoint.url, c.genesisChainID, chainID)
	}

	// ETHBackend will check rpcclient.blockNumber() once per second. For
	// external http sources, that's an excessive request rate.
	uri, _ := url.Parse(endpoint.url) // err already checked by DialContext
	isWS := uri.Scheme == "ws" || uri.Scheme == "wss"
	// High block tip check frequency for local http.
	ec.tipCache.expiration = time.Second * 9 / 10
	// Websocket endpoints receive headers through a notification feed, so
	// shouldn't make requests unless something seems wrong.
	if isWS {
		ec.tipCache.expiration = headerExpirationTime // time.Minute
	} else if isRemoteURL(uri) {
		// Lower the request rate for non-loopback IPs to avoid running into
		// rate limits.
		ec.tipCache.expiration = time.Second * 99 / 10
	}

	reqModules := []string{"eth", "txpool"}
	if err := dexeth.CheckAPIModules(client, endpoint.url, c.log, reqModules); err != nil {
		c.log.Warnf("Error checking required modules at %q: %v", endpoint, err)
		c.log.Warnf("Will not account for pending transactions in balance calculations at %q", endpoint)
		ec.txPoolSupported = false
	} else {
		ec.txPoolSupported = true
	}

	switch c.ethContractVer {
	case 0:
		es0, err := swapv0.NewETHSwap(c.ethContractAddr, ec.Client)
		if err != nil {
			return nil, err
		}
		ec.swapContract = &swapSourceV0{es0}
	case 1:
		es1, err := swapv1.NewETHSwap(c.ethContractAddr, ec.Client)
		if err != nil {
			return nil, err
		}
		ec.swapContract = &swapSourceV1{es1, c.ethContractAddrV1}
	}

	for assetID, vToken := range c.tokensLoaded {
		tkn, err := newTokener(ctx, assetID, vToken, c.net, ec.Client, c.ethContractAddrV1)
		if err != nil {
			return nil, fmt.Errorf("error constructing ERC20Swap: %w", err)
		}
		ec.tokens[assetID] = tkn
	}

	if isWS {
		go ec.monitorBlocks(ctx, c.log)
	}

	success = true

	return ec, nil
}

type connectionStatus int

const (
	connectionStatusFailed connectionStatus = iota
	connectionStatusOutdated
	connectionStatusConnected
)

func (c *rpcclient) checkConnectionStatus(ctx context.Context, ec *ethConn) connectionStatus {
	hdr, err := ec.tip(ctx)
	if err != nil {
		c.log.Errorf("Failed to get header from %q: %v", ec.endpoint, err)
		return connectionStatusFailed
	}

	if c.headerIsOutdated(hdr) {
		hdrTime := time.Unix(int64(hdr.Time), 0)
		c.log.Warnf("header fetched from %q appears to be outdated (time %s is %v old). "+
			"If you continue to see this message, you might need to check your system clock",
			ec.endpoint, hdrTime, time.Since(hdrTime))
		return connectionStatusOutdated
	}

	return connectionStatusConnected
}

// sortConnectionsByHealth checks the health of the connections and sorts them
// based on their health. It does a best header call to each connection and
// connections with non outdated headers are placed first, ones with outdated
// headers are placed in the middle, and ones that error are placed last.
// Every failingEndpointsCheckFreq health checks, the endpoints that have
// never been successfully connection will be checked. True is returned if
// there is at least one healthy connection.
func (c *rpcclient) sortConnectionsByHealth(ctx context.Context) bool {
	clients := c.clientsCopy()

	healthyConnections := make([]*ethConn, 0, len(clients))
	outdatedConnections := make([]*ethConn, 0, len(clients))
	failingConnections := make([]*ethConn, 0, len(clients))

	categorizeConnection := func(conn *ethConn) {
		status := c.checkConnectionStatus(ctx, conn)
		switch status {
		case connectionStatusConnected:
			healthyConnections = append(healthyConnections, conn)
		case connectionStatusOutdated:
			outdatedConnections = append(outdatedConnections, conn)
		case connectionStatusFailed:
			failingConnections = append(failingConnections, conn)
		}
	}

	for _, ec := range clients {
		categorizeConnection(ec)
	}

	if c.healthCheckCounter == 0 && len(c.neverConnectedEndpoints) > 0 {
		stillUnconnectedEndpoints := make([]endpoint, 0, len(c.neverConnectedEndpoints))

		for _, endpoint := range c.neverConnectedEndpoints {
			ec, err := c.connectToEndpoint(ctx, endpoint)
			if err != nil {
				c.log.Errorf("Error connecting to %q: %v", endpoint, err)
				stillUnconnectedEndpoints = append(stillUnconnectedEndpoints, endpoint)
				continue
			}

			c.log.Infof("Successfully connected to %q", endpoint)

			categorizeConnection(ec)
		}

		c.neverConnectedEndpoints = stillUnconnectedEndpoints
	}

	// Higher priority comes first.
	sort.Slice(healthyConnections, func(i, j int) bool {
		return healthyConnections[i].priority > healthyConnections[j].priority
	})
	sort.Slice(outdatedConnections, func(i, j int) bool {
		return outdatedConnections[i].priority > outdatedConnections[j].priority
	})
	sort.Slice(failingConnections, func(i, j int) bool {
		return failingConnections[i].priority > failingConnections[j].priority
	})

	clientsUpdatedOrder := make([]*ethConn, 0, len(clients))
	clientsUpdatedOrder = append(clientsUpdatedOrder, healthyConnections...)
	clientsUpdatedOrder = append(clientsUpdatedOrder, outdatedConnections...)
	clientsUpdatedOrder = append(clientsUpdatedOrder, failingConnections...)

	c.log.Tracef("Healthy connections: %v", healthyConnections)
	if len(outdatedConnections) > 0 {
		c.log.Warnf("Outdated connections: %v", outdatedConnections)
	}
	if len(failingConnections) > 0 {
		c.log.Warnf("Failing connections: %v", failingConnections)
	}

	c.clientsMtx.Lock()
	defer c.clientsMtx.Unlock()
	c.clients = clientsUpdatedOrder
	c.healthCheckCounter = (c.healthCheckCounter + 1) % failingEndpointsCheckFreq

	return len(healthyConnections) > 0
}

// markConnectionAsFailed moves an connection to the end of the client list.
func (c *rpcclient) markConnectionAsFailed(endpoint string) {
	c.clientsMtx.Lock()
	defer c.clientsMtx.Unlock()

	var index int = -1
	for i, ec := range c.clients {
		if ec.endpoint == endpoint {
			index = i
			break
		}
	}
	if index == -1 {
		c.log.Errorf("Failed to mark client as failed: %q not found", endpoint)
		return
	}

	updatedClients := make([]*ethConn, 0, len(c.clients))
	updatedClients = append(updatedClients, c.clients[:index]...)
	updatedClients = append(updatedClients, c.clients[index+1:]...)
	updatedClients = append(updatedClients, c.clients[index])

	c.clients = updatedClients
}

// monitorConnectionsHealth starts a goroutine that checks the health of all
// connections every 30 seconds.
func (c *rpcclient) monitorConnectionsHealth(ctx context.Context) {
	defer func() {
		for _, ec := range c.clientsCopy() {
			ec.Close()
		}
	}()

	ticker := time.NewTicker(monitorConnectionsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.sortConnectionsByHealth(ctx) {
				c.log.Warnf("No healthy %v RPC connections", c.baseChainName)
			}
		}
	}
}

func (c *rpcclient) withClient(f func(ec *ethConn) error, haltOnNotFound ...bool) (err error) {
	for _, ec := range c.clientsCopy() {
		err = f(ec)
		if err == nil {
			return nil
		}
		if len(haltOnNotFound) > 0 && haltOnNotFound[0] && (errors.Is(err, ethereum.NotFound) || strings.Contains(err.Error(), "not found")) {
			return ethereum.NotFound
		}

		c.log.Errorf("Unpropagated error from %q: %v", ec.endpoint, err)
		c.markConnectionAsFailed(ec.endpoint)
	}

	return fmt.Errorf("all providers failed. last error: %w", err)
}

// connect will attempt to connect to all the endpoints in the endpoints slice.
// If at least one of the connections is successful and is not outdated, the
// function will return without error.
//
// Connections with an outdated block will be marked as outdated, but included
// in the clients slice. If the up-to-date providers start to fail, the outdated
// ones will be checked to see if they are still outdated.
//
// Failed connections will not be included in the clients slice.
func (c *rpcclient) connect(ctx context.Context) (err error) {
	var success bool

	c.clients = make([]*ethConn, 0, len(c.endpoints))
	c.neverConnectedEndpoints = make([]endpoint, 0, len(c.endpoints))

	for _, endpoint := range c.endpoints {
		ec, err := c.connectToEndpoint(ctx, endpoint)
		if err != nil {
			c.log.Errorf("Error connecting to %q: %v", endpoint, err)
			c.neverConnectedEndpoints = append(c.neverConnectedEndpoints, endpoint)
			continue
		}

		defer func() {
			// If all connections are outdated, we will not start, so close any open connections.
			if !success {
				ec.Close()
			}
		}()

		c.clients = append(c.clients, ec)
	}

	success = c.sortConnectionsByHealth(ctx)

	if !success {
		return fmt.Errorf("failed to connect to an up-to-date %v node", c.baseChainName)
	}

	go c.monitorConnectionsHealth(ctx)

	return nil
}

func (c *rpcclient) headerIsOutdated(hdr *types.Header) bool {
	return c.net != dex.Simnet && hdr.Time < uint64(time.Now().Add(-headerExpirationTime).Unix())
}

func (c *rpcclient) loadToken(ctx context.Context, assetID uint32, vToken *VersionedToken) error {
	c.tokensLoaded[assetID] = vToken

	for _, cl := range c.clientsCopy() {
		tkn, err := newTokener(ctx, assetID, vToken, c.net, cl.Client, c.ethContractAddrV1)
		if err != nil {
			return fmt.Errorf("error constructing ERC20Swap: %w", err)
		}
		cl.tokens[assetID] = tkn
	}
	return nil
}

func (c *rpcclient) withTokener(assetID uint32, f func(*tokener) error) error {
	return c.withClient(func(ec *ethConn) error {
		tkn, found := ec.tokens[assetID]
		if !found {
			return fmt.Errorf("no swap source for asset %d", assetID)
		}
		return f(tkn)
	})
}

func (c *rpcclient) withSwapContract(assetID uint32, f func(swapContract) error) error {
	if assetID == c.baseChainID {
		return c.withClient(func(ec *ethConn) error {
			return f(ec.swapContract)
		})
	}
	return c.withTokener(assetID, func(tkn *tokener) error {
		return f(tkn)
	})
}

// bestHeader gets the best header at the time of calling.
func (c *rpcclient) bestHeader(ctx context.Context) (hdr *types.Header, err error) {
	return hdr, c.withClient(func(ec *ethConn) error {
		hdr, err = ec.tip(ctx)
		return err
	})
}

// headerByHeight gets the best header at height.
func (c *rpcclient) headerByHeight(ctx context.Context, height uint64) (hdr *types.Header, err error) {
	return hdr, c.withClient(func(ec *ethConn) error {
		hdr, err = ec.HeaderByNumber(ctx, big.NewInt(int64(height)))
		return err
	})
}

// suggestGasTipCap retrieves the currently suggested priority fee to allow a
// timely execution of a transaction.
func (c *rpcclient) suggestGasTipCap(ctx context.Context) (tipCap *big.Int, err error) {
	return tipCap, c.withClient(func(ec *ethConn) error {
		tipCap, err = ec.SuggestGasTipCap(ctx)
		return err
	})
}

// blockNumber gets the chain length at the time of calling.
func (c *rpcclient) blockNumber(ctx context.Context) (bn uint64, err error) {
	return bn, c.withClient(func(ec *ethConn) error {
		hdr, err := ec.tip(ctx)
		if err == nil {
			bn = hdr.Number.Uint64()
		}
		return err
	})
}

func (c *rpcclient) status(ctx context.Context, assetID uint32, token common.Address, locator []byte) (status *dexeth.SwapStatus, err error) {
	return status, c.withSwapContract(assetID, func(sc swapContract) error {
		status, err = sc.status(ctx, token, locator)
		return err
	})
}

func (c *rpcclient) vector(ctx context.Context, assetID uint32, locator []byte) (vec *dexeth.SwapVector, err error) {
	return vec, c.withSwapContract(assetID, func(sc swapContract) error {
		vec, err = sc.vector(ctx, locator)
		return err
	})
}

func (c *rpcclient) statusAndVector(ctx context.Context, assetID uint32, locator []byte) (status *dexeth.SwapStatus, vec *dexeth.SwapVector, err error) {
	return status, vec, c.withSwapContract(assetID, func(sc swapContract) error {
		status, vec, err = sc.statusAndVector(ctx, locator)
		return err
	})
}

// transaction gets the transaction that hashes to hash from the chain or
// mempool. Errors if tx does not exist.
func (c *rpcclient) transaction(ctx context.Context, hash common.Hash) (tx *types.Transaction, isMempool bool, err error) {
	return tx, isMempool, c.withClient(func(ec *ethConn) error {
		tx, isMempool, err = ec.TransactionByHash(ctx, hash)
		return err
	}, true) // stop on first provider with "not found", because this should be an error if tx does not exist
}

func (c *rpcclient) transactionReceipt(ctx context.Context, txHash common.Hash) (r *types.Receipt, err error) {
	return r, c.withClient(func(ec *ethConn) error {
		r, err = ec.TransactionReceipt(ctx, txHash)
		return err
	})
}

func isNotFoundError(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

// dumbBalance gets the account balance, ignoring the effects of unmined
// transactions.
func (c *rpcclient) dumbBalance(ctx context.Context, ec *ethConn, assetID uint32, addr common.Address) (bal *big.Int, err error) {
	if assetID == c.baseChainID {
		return ec.BalanceAt(ctx, addr, nil)
	}
	tkn := ec.tokens[assetID]
	if tkn == nil {
		return nil, fmt.Errorf("no tokener for asset ID %d", assetID)
	}
	return tkn.balanceOf(ctx, addr)
}

// smartBalance gets the account balance, including the effects of known
// unmined transactions.
func (c *rpcclient) smartBalance(ctx context.Context, ec *ethConn, assetID uint32, addr common.Address) (bal *big.Int, err error) {
	tip, err := c.blockNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("blockNumber error: %v", err)
	}

	// We need to subtract and pending outgoing value, but ignore any pending
	// incoming value since that can't be spent until mined. So we can't using
	// PendingBalanceAt or BalanceAt by themselves.
	// We'll iterate tx pool transactions and subtract any value and fees being
	// sent from this account. The rpc.Client doesn't expose the
	// txpool_contentFrom => (*TxPool).ContentFrom RPC method, for whatever
	// reason, so we'll have to use CallContext and copy the mimic the
	// internal RPCTransaction type.
	var txs map[string]map[string]*RPCTransaction
	if err := ec.caller.CallContext(ctx, &txs, "txpool_contentFrom", addr); err != nil {
		return nil, fmt.Errorf("contentFrom error: %w", err)
	}

	if assetID == c.baseChainID {
		ethBalance, err := ec.BalanceAt(ctx, addr, big.NewInt(int64(tip)))
		if err != nil {
			return nil, err
		}
		outgoingEth := new(big.Int)
		for _, group := range txs { // 2 groups, pending and queued
			for _, tx := range group {
				outgoingEth.Add(outgoingEth, tx.Value.ToInt())
				gas := new(big.Int).SetUint64(uint64(tx.Gas))
				if tx.GasPrice != nil && tx.GasPrice.ToInt().Cmp(bigZero) > 0 {
					outgoingEth.Add(outgoingEth, new(big.Int).Mul(gas, tx.GasPrice.ToInt()))
				} else if tx.GasFeeCap != nil {
					outgoingEth.Add(outgoingEth, new(big.Int).Mul(gas, tx.GasFeeCap.ToInt()))
				} else {
					return nil, fmt.Errorf("cannot find fees for tx %s", tx.Hash)
				}
			}
		}
		return ethBalance.Sub(ethBalance, outgoingEth), nil
	}

	// For tokens, we'll do something similar, but with checks for pending txs
	// that transfer tokens or pay to the swap contract.
	// Can't use withTokener because we need to use the same ethConn due to
	// txPoolSupported being used to decide between {smart/dumb}Balance.
	tkn := ec.tokens[assetID]
	if tkn == nil {
		return nil, fmt.Errorf("no tokener for asset ID %d", assetID)
	}
	bal, err = tkn.balanceOf(ctx, addr)
	if err != nil {
		return nil, err
	}
	for _, group := range txs {
		for _, rpcTx := range group {
			to := *rpcTx.To
			if to == tkn.tokenAddr {
				if sent := tkn.transferred(rpcTx.Input); sent != nil {
					bal.Sub(bal, sent)
				}
			}
			if to == tkn.contractAddr {
				if swapped := tkn.swapped(rpcTx.Input); swapped != nil {
					bal.Sub(bal, swapped)
				}
			}
		}
	}
	return bal, nil
}

// accountBalance gets the account balance. If txPool functions are supported by the
// client, it will include the effects of unmined transactions, otherwise it will not.
func (c *rpcclient) accountBalance(ctx context.Context, assetID uint32, addr common.Address) (bal *big.Int, err error) {
	return bal, c.withClient(func(ec *ethConn) error {
		if ec.txPoolSupported {
			bal, err = c.smartBalance(ctx, ec, assetID, addr)
		} else {
			bal, err = c.dumbBalance(ctx, ec, assetID, addr)
		}
		return err
	})

}

type RPCTransaction struct {
	Value     *hexutil.Big    `json:"value"`
	Gas       hexutil.Uint64  `json:"gas"`
	GasPrice  *hexutil.Big    `json:"gasPrice"`
	GasFeeCap *hexutil.Big    `json:"maxFeePerGas,omitempty"`
	Hash      common.Hash     `json:"hash"`
	To        *common.Address `json:"to"`
	Input     hexutil.Bytes   `json:"input"`
	// BlockHash        *common.Hash      `json:"blockHash"`
	// BlockNumber      *hexutil.Big      `json:"blockNumber"`
	// From             common.Address    `json:"from"`
	// GasTipCap        *hexutil.Big      `json:"maxPriorityFeePerGas,omitempty"`
	// Nonce            hexutil.Uint64    `json:"nonce"`
	// TransactionIndex *hexutil.Uint64   `json:"transactionIndex"`
	// Type             hexutil.Uint64    `json:"type"`
	// Accesses         *types.AccessList `json:"accessList,omitempty"`
	// ChainID          *hexutil.Big      `json:"chainId,omitempty"`
	// V                *hexutil.Big      `json:"v"`
	// R                *hexutil.Big      `json:"r"`
	// S                *hexutil.Big      `json:"s"`
}

func isRemoteURL(uri *url.URL) bool {
	host := uri.Hostname()
	ip := net.ParseIP(host)
	if ip == nil {
		ips, _ := net.LookupIP(host)
		if len(ips) == 0 {
			return true
		}
		ip = ips[0]
	}
	return !ip.IsLoopback()
}

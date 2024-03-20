package faucet

import (
	"context"
	"fmt"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/iotaledger/hive.go/app/daemon"
	"github.com/iotaledger/hive.go/core/safemath"
	"github.com/iotaledger/hive.go/ierrors"
	"github.com/iotaledger/hive.go/log"
	"github.com/iotaledger/hive.go/runtime/event"
	"github.com/iotaledger/hive.go/runtime/syncutils"
	"github.com/iotaledger/hive.go/runtime/timeutil"
	"github.com/iotaledger/inx-app/pkg/httpserver"
	iotago "github.com/iotaledger/iota.go/v4"
	"github.com/iotaledger/iota.go/v4/api"
	"github.com/iotaledger/iota.go/v4/builder"
)

var (
	// ErrOperationAborted is returned when the operation was aborted e.g. by a shutdown signal.
	ErrOperationAborted = ierrors.New("operation was aborted")
	// ErrNothingToProcess is returned when there is no need to sweep or send funds.
	ErrNothingToProcess = ierrors.New("nothing to process")

	// EmptyBasicOutput is used to calculate the storage deposit of the faucet remainder output.
	EmptyBasicOutput = &iotago.BasicOutput{
		Amount: 0,
		Mana:   0,
		UnlockConditions: iotago.BasicOutputUnlockConditions{
			&iotago.AddressUnlockCondition{
				Address: &iotago.RestrictedAddress{
					Address:             &iotago.Ed25519Address{},
					AllowedCapabilities: iotago.AddressCapabilitiesBitMaskWithCapabilities(iotago.WithAddressCanReceiveMana(true)),
				},
			},
		},
		Features: iotago.BasicOutputFeatures{},
	}
)

type (
	// IsNodeHealthyFunc is a function to query if the used node is synced.
	IsNodeHealthyFunc func() bool
	// FetchTransactionMetadataFunc is a function to fetch the required metadata of a transaction.
	// This returns nil if the transaction is not found.
	FetchTransactionMetadataFunc func(transactionID iotago.TransactionID) (*api.TransactionMetadataResponse, error)
	// CollectUnlockableFaucetOutputsFunc is a function to collect the unlockable outputs of the faucet.
	CollectUnlockableFaucetOutputsFunc func() ([]UTXOBasicOutput, error)
	// CollectUnlockableFaucetOutputsAndBalanceFunc is a function to collect the unlockable outputs and the balance of the faucet.
	CollectUnlockableFaucetOutputsAndBalanceFunc func() ([]UTXOBasicOutput, iotago.BaseToken, error)
	// ComputeUnlockableAddressBalanceFunc is a function to compute the unlockable balance of an address.
	ComputeUnlockableAddressBalanceFunc func(address iotago.Address) (iotago.BaseToken, error)
	// GetLatestSlotFunc is a function to get the latest known slot in the network.
	GetLatestSlotFunc func() iotago.SlotIndex
	// SubmitTransactionPayloadFunc is a function which creates a signed transaction payload and sends it to a block issuer.
	SubmitTransactionPayloadFunc func(ctx context.Context, builder *builder.TransactionBuilder, storedManaOutputIndex int, numPoWWorkers ...int) (iotago.ApplicationPayload, iotago.BlockID, error)
)

type UTXOBasicOutput struct {
	OutputID iotago.OutputID
	Output   *iotago.BasicOutput
}

// Events are the events issued by the faucet.
type Events struct {
	// Fired when a faucet block is issued.
	IssuedBlock *event.Event1[iotago.BlockID]
	// SoftError is triggered when a soft error is encountered.
	SoftError *event.Event1[error]
}

// queueItem is an item for the faucet requests queue.
type queueItem struct {
	Bech32          string
	BaseTokenAmount iotago.BaseToken
	Address         iotago.Address
}

// pendingTransaction holds info about a sent transaction that is pending.
type pendingTransaction struct {
	BlockID        iotago.BlockID
	TransactionID  iotago.TransactionID
	QueuedItems    []*queueItem
	ConsumedInputs iotago.OutputIDs
}

// InfoResponse defines the response of a GET RouteFaucetInfo REST API call.
type InfoResponse struct {
	// Whether the faucet is healthy.
	IsHealthy bool `json:"isHealthy"`
	// The bech32 address of the faucet.
	Address string `json:"address"`
	// The remaining balance of faucet.
	Balance iotago.BaseToken `json:"balance"`
	// The name of the token of the faucet.
	TokenName string `json:"tokenName"`
	// The Bech32 human readable part of the faucet.
	Bech32HRP iotago.NetworkPrefix `json:"bech32Hrp"`
}

// EnqueueRequest defines the request for a POST RouteFaucetEnqueue REST API call.
type EnqueueRequest struct {
	// The bech32 address.
	Address string `json:"address"`
}

// EnqueueResponse defines the response of a POST RouteFaucetEnqueue REST API call.
type EnqueueResponse struct {
	// The bech32 address.
	Address string `json:"address"`
	// The number of waiting requests in the queue.
	WaitingRequests int `json:"waitingRequests"`
}

// Faucet is used to issue transaction to users that requested funds via a REST endpoint.
type Faucet struct {
	// lock used to secure the state of the faucet.
	syncutils.RWMutex
	// the logger used to log events.
	log.Logger
	// used to access the global daemon.
	daemon daemon.Daemon

	// used to determine the health status of the node.
	isNodeHealthyFunc IsNodeHealthyFunc
	// used to fetch metadata of a transaction from the node.
	fetchTransactionMetadataFunc FetchTransactionMetadataFunc
	// used to collect the unlockable outputs and the balance of the faucet.
	// write lock must be acquired outside because we read from queueMap and we want to set the faucet balance without modifications to the map.
	collectUnlockableFaucetOutputsAndBalanceFuncWithoutLocking CollectUnlockableFaucetOutputsAndBalanceFunc
	// used to compute the unlockable balance of an address.
	computeUnlockableAddressBalanceFunc ComputeUnlockableAddressBalanceFunc
	// used to get the latest known slot in the network.
	getLatestSlotFunc GetLatestSlotFunc
	// used to create a signed transaction payload and send it to a block issuer.
	submitTransactionPayloadFunc SubmitTransactionPayloadFunc

	// the api Provider.
	apiProvider iotago.APIProvider
	// the address of the faucet.
	address iotago.Address
	// used to sign the faucet transactions.
	addressSigner iotago.AddressSigner
	// holds the faucet options.
	opts *Options

	// events of the faucet.
	Events *Events

	// faucetBalance is the remaining balance of the faucet if all requests would be processed.
	faucetBalance iotago.BaseToken
	// queue of new requests.
	queue chan *queueItem
	// map with all queued requests per address (bech32).
	queueMap map[string]*queueItem
	// flushQueue is used to signal to stop an ongoing batching of faucet requests.
	flushQueue chan struct{}
	// pendingTransaction is the currently sent transaction that is still pending.
	pendingTransaction *pendingTransaction
}

// the default options applied to the faucet.
var defaultOptions = []Option{
	WithTokenName("TestToken"),
	WithBaseTokenAmount(10_000_000),          // 10 IOTA
	WithBaseTokenAmountSmall(1_000_000),      // 1 IOTA
	WithBaseTokenAmountMaxTarget(20_000_000), // 20 IOTA
	WithManaAmount(1000),
	WithManaAmountMinFaucet(1000000),
	WithTagMessage("FAUCET"),
	WithBatchTimeout(2 * time.Second),
}

// Options define options for the faucet.
type Options struct {
	// the logger used to log events.
	logger                   log.Logger
	tokenName                string
	baseTokenAmount          iotago.BaseToken
	baseTokenAmountSmall     iotago.BaseToken
	baseTokenAmountMaxTarget iotago.BaseToken
	manaAmount               iotago.Mana
	manaAmountMinFaucet      iotago.Mana
	tagMessage               []byte
	batchTimeout             time.Duration
	powWorkerCount           int
}

// applies the given Option.
func (so *Options) apply(opts ...Option) {
	for _, opt := range opts {
		opt(so)
	}
}

// WithLogger enables logging within the faucet.
func WithLogger(logger log.Logger) Option {
	return func(opts *Options) {
		opts.logger = logger
	}
}

// WithTokenName sets the name of the token.
func WithTokenName(name string) Option {
	return func(opts *Options) {
		opts.tokenName = name
	}
}

// WithBaseTokenAmount defines the amount of funds the requester receives.
func WithBaseTokenAmount(baseTokenAmount iotago.BaseToken) Option {
	return func(opts *Options) {
		opts.baseTokenAmount = baseTokenAmount
	}
}

// WithBaseTokenAmountSmall defines the amount of funds the requester receives
// if the target address has more funds than the faucet amount and less than maximum.
func WithBaseTokenAmountSmall(baseTokenAmountSmall iotago.BaseToken) Option {
	return func(opts *Options) {
		opts.baseTokenAmountSmall = baseTokenAmountSmall
	}
}

// WithBaseTokenAmountMaxTarget defines the maximum allowed amount of funds on the target address.
// If there are more funds already, the faucet request is rejected.
func WithBaseTokenAmountMaxTarget(baseTokenAmountMaxTarget iotago.BaseToken) Option {
	return func(opts *Options) {
		opts.baseTokenAmountMaxTarget = baseTokenAmountMaxTarget
	}
}

// WithManaAmount defines the amount of mana the requester receives.
func WithManaAmount(manaAmount iotago.Mana) Option {
	return func(opts *Options) {
		opts.manaAmount = manaAmount
	}
}

// WithManaAmountMinFaucet defines the minimum amount of mana the faucet
// needs to hold before mana payouts become active.
func WithManaAmountMinFaucet(manaAmountMinFaucet iotago.Mana) Option {
	return func(opts *Options) {
		opts.manaAmountMinFaucet = manaAmountMinFaucet
	}
}

// WithTagMessage defines the faucet transaction tag payload.
func WithTagMessage(tagMessage string) Option {
	return func(opts *Options) {
		opts.tagMessage = []byte(tagMessage)
	}
}

// WithBatchTimeout sets the maximum duration for collecting faucet batches.
func WithBatchTimeout(timeout time.Duration) Option {
	return func(opts *Options) {
		opts.batchTimeout = timeout
	}
}

// WithPoWWorkerCount sets the amount of workers used for calculating PoW when sending payloads to the block issuer.
func WithPoWWorkerCount(powWorkerCount int) Option {
	return func(opts *Options) {
		opts.powWorkerCount = powWorkerCount
	}
}

// Option is a function setting a faucet option.
type Option func(opts *Options)

// New creates a new faucet instance.
func New(
	daemon daemon.Daemon,
	isNodeHealthyFunc IsNodeHealthyFunc,
	fetchTransactionMetadataFunc FetchTransactionMetadataFunc,
	collectUnlockableFaucetOutputsFunc CollectUnlockableFaucetOutputsFunc,
	computeUnlockableAddressBalanceFunc ComputeUnlockableAddressBalanceFunc,
	getLatestSlotFunc GetLatestSlotFunc,
	submitTransactionPayloadFunc SubmitTransactionPayloadFunc,
	apiProvider iotago.APIProvider,
	address iotago.Address,
	addressSigner iotago.AddressSigner,
	opts ...Option) *Faucet {
	options := &Options{}
	options.apply(defaultOptions...)
	options.apply(opts...)

	faucet := &Faucet{
		daemon:                              daemon,
		isNodeHealthyFunc:                   isNodeHealthyFunc,
		fetchTransactionMetadataFunc:        fetchTransactionMetadataFunc,
		computeUnlockableAddressBalanceFunc: computeUnlockableAddressBalanceFunc,
		getLatestSlotFunc:                   getLatestSlotFunc,
		submitTransactionPayloadFunc:        submitTransactionPayloadFunc,
		apiProvider:                         apiProvider,
		address:                             address,
		addressSigner:                       addressSigner,
		opts:                                options,

		Events: &Events{
			IssuedBlock: event.New1[iotago.BlockID](),
			SoftError:   event.New1[error](),
		},
	}

	// write lock must be acquired outside because we read from queueMap and we want to set the faucet balance without modifications to the map
	faucet.collectUnlockableFaucetOutputsAndBalanceFuncWithoutLocking = func() ([]UTXOBasicOutput, iotago.BaseToken, error) {
		// get all outputs of the faucet
		unspentOutputs, err := collectUnlockableFaucetOutputsFunc()
		if err != nil {
			return nil, 0, err
		}

		// get the total faucet balance
		var balance iotago.BaseToken
		for _, output := range unspentOutputs {
			balance += output.Output.BaseTokenAmount()
		}

		// calculate total balance of all pending requests
		var pendingRequestsBalance iotago.BaseToken
		for _, pendingRequest := range faucet.queueMap {
			pendingRequestsBalance += pendingRequest.BaseTokenAmount
		}

		// subtract the storage deposit for a simple basic output, so we can simplify our logic for remainder handling
		minStorageDeposit, err := faucet.apiProvider.CommittedAPI().StorageScoreStructure().MinDeposit(EmptyBasicOutput)
		if err != nil {
			return nil, 0, err
		}

		if balance >= minStorageDeposit {
			balance -= minStorageDeposit
		} else {
			balance = 0
		}

		if balance >= pendingRequestsBalance {
			balance -= pendingRequestsBalance
		} else {
			balance = 0
		}

		return unspentOutputs, balance, nil
	}

	faucet.Logger = options.logger
	faucet.init()

	return faucet
}

func (f *Faucet) init() {
	f.faucetBalance = 0
	f.queue = make(chan *queueItem, 5000)
	f.queueMap = make(map[string]*queueItem)
	f.flushQueue = make(chan struct{})
	f.pendingTransaction = nil
}

// IsHealthy returns the health status of the faucet.
func (f *Faucet) IsHealthy() bool {
	return f.isNodeHealthyFunc()
}

// Address returns the deposit address of the faucet.
func (f *Faucet) Address() iotago.Address {
	return f.address
}

// Info returns the used faucet address and remaining balance.
func (f *Faucet) Info() (*InfoResponse, error) {
	protocolParams := f.apiProvider.CommittedAPI().ProtocolParameters()

	return &InfoResponse{
		IsHealthy: f.isNodeHealthyFunc(),
		Address:   f.address.Bech32(protocolParams.Bech32HRP()),
		Balance:   f.faucetBalance,
		TokenName: f.opts.tokenName,
		Bech32HRP: protocolParams.Bech32HRP(),
	}, nil
}

// Enqueue adds a new faucet request to the queue.
func (f *Faucet) Enqueue(bech32Addr string) (*EnqueueResponse, error) {
	addr, err := f.parseBech32Address(bech32Addr)
	if err != nil {
		return nil, err
	}

	if !f.isNodeHealthyFunc() {
		return nil, ierrors.Wrap(echo.ErrInternalServerError, "Faucet node is not synchronized/healthy. Please try again later!")
	}

	if exists := f.isAlreadyinQueue(bech32Addr); exists {
		return nil, ierrors.Wrap(httpserver.ErrInvalidParameter, "Address is already in the queue.")
	}

	baseTokenAmount := f.opts.baseTokenAmount
	balance, err := f.computeUnlockableAddressBalanceFunc(addr)
	if err == nil && balance >= f.opts.baseTokenAmount {
		baseTokenAmount = f.opts.baseTokenAmountSmall

		if balance >= f.opts.baseTokenAmountMaxTarget {
			return nil, ierrors.Wrap(httpserver.ErrInvalidParameter, "You already have enough funds on your address.")
		}
	}

	// we already need to lock here to have the correct faucet balance
	// and we need to add the request to the queueMap
	f.Lock()
	defer f.Unlock()

	if baseTokenAmount > f.faucetBalance {
		return nil, ierrors.Wrap(echo.ErrInternalServerError, "Faucet does not have enough funds to process your request. Please try again later!")
	}

	request := &queueItem{
		Bech32:          bech32Addr,
		BaseTokenAmount: baseTokenAmount,
		Address:         addr,
	}

	select {
	case f.queue <- request:
		f.faucetBalance -= baseTokenAmount
		f.queueMap[bech32Addr] = request

		return &EnqueueResponse{
			Address:         bech32Addr,
			WaitingRequests: len(f.queueMap),
		}, nil

	default:
		// queue is full

		return nil, ierrors.Wrap(echo.ErrInternalServerError, "Faucet queue is full. Please try again later!")
	}
}

// FlushRequests stops current batching of faucet requests.
func (f *Faucet) FlushRequests() {
	f.flushQueue <- struct{}{}
}

// logSoftError logs a soft error and triggers the event.
func (f *Faucet) logSoftError(err error) {
	f.LogWarn(err.Error())
	f.Events.SoftError.Trigger(err)
}

// parseBech32Address parses a bech32 address.
func (f *Faucet) parseBech32Address(bech32Addr string) (iotago.Address, error) {
	hrp, bech32Address, err := iotago.ParseBech32(bech32Addr)
	if err != nil {
		return nil, ierrors.Wrap(httpserver.ErrInvalidParameter, "Invalid bech32 address provided!")
	}

	protocolParams := f.apiProvider.CommittedAPI().ProtocolParameters()
	if hrp != protocolParams.Bech32HRP() {
		return nil, ierrors.Wrapf(httpserver.ErrInvalidParameter, "Invalid bech32 address provided! Address does not start with \"%s\".", protocolParams.Bech32HRP())
	}

	return bech32Address, nil
}

// isAlreadyinQueue checks if the given address is already in the queue.
func (f *Faucet) isAlreadyinQueue(bech32Addr string) bool {
	f.RLock()
	defer f.RUnlock()

	_, exists := f.queueMap[bech32Addr]

	return exists
}

// clearRequestWithoutLocking clear the old request from the map.
// this is necessary to be able to send a new request to the same address.
// write lock must be acquired outside.
func (f *Faucet) clearRequestWithoutLocking(request *queueItem) {
	delete(f.queueMap, request.Bech32)
}

// clearRequestsWithoutLocking clears the old requests from the map.
// this is necessary to be able to send new requests to the same addresses.
// write lock must be acquired outside.
func (f *Faucet) clearRequestsWithoutLocking(batchedRequests []*queueItem) {
	for _, request := range batchedRequests {
		f.clearRequestWithoutLocking(request)
	}
}

// readdRequestsWithoutLocking adds old requests back to the queue.
// write lock must be acquired outside.
func (f *Faucet) readdRequestsWithoutLocking(batchedRequests []*queueItem) {
	for _, request := range batchedRequests {
		select {
		case f.queue <- request:
		default:
			// queue full => no way to readd it, delete it from the map as well so user are able to send a new request
			f.clearRequestWithoutLocking(request)
		}
	}
}

// setPendingTransactionWithoutLocking sets the pending transaction.
// write lock must be acquired outside.
func (f *Faucet) setPendingTransactionWithoutLocking(pending *pendingTransaction) {
	f.pendingTransaction = pending
}

// clearPendingTransactionWithoutLocking removes tracking of a pending transaction.
// write lock must be acquired outside.
func (f *Faucet) clearPendingTransactionWithoutLocking() {
	f.pendingTransaction = nil
}

// clearPendingRequestsWithoutLocking clears the old requests from the map
// and removes tracking of a pending transaction.
// write lock must be acquired outside.
func (f *Faucet) clearPendingRequestsWithoutLocking() {
	f.clearRequestsWithoutLocking(f.pendingTransaction.QueuedItems)
	f.clearPendingTransactionWithoutLocking()
}

// readdPendingRequestsWithoutLocking adds old requests back to the queue
// and removes tracking of a pending transaction.
// write lock must be acquired outside.
func (f *Faucet) readdPendingRequestsWithoutLocking() {
	f.readdRequestsWithoutLocking(f.pendingTransaction.QueuedItems)
	f.clearPendingTransactionWithoutLocking()
}

// collectRequests collects faucet requests until the maximum amount or a timeout is reached.
// locking not required.
func (f *Faucet) collectRequests(ctx context.Context) ([]*queueItem, error) {
	batchedRequests := []*queueItem{}

CollectValues:
	for len(batchedRequests) < iotago.MaxOutputsCount {
		select {
		case <-ctx.Done():
			// faucet was stopped
			return nil, ErrOperationAborted

		case <-time.After(f.opts.batchTimeout):
			// timeout was reached => stop collecting requests
			break CollectValues

		case <-f.flushQueue:
			// flush signal => stop collecting requests
			for len(batchedRequests) < iotago.MaxOutputsCount {
				// collect all pending requests
				select {
				case request := <-f.queue:
					batchedRequests = append(batchedRequests, request)

				default:
					// no pending requests
					break CollectValues
				}
			}

			break CollectValues

		case request := <-f.queue:
			batchedRequests = append(batchedRequests, request)
		}
	}

	return batchedRequests, nil
}

// processRequestsWithoutLocking processes all possible requests considering the maximum transaction size and the remaining funds of the faucet.
// write lock must be acquired outside.
func (f *Faucet) processRequestsWithoutLocking(collectedRequestsCounter int, balance iotago.BaseToken, batchedRequests []*queueItem) []*queueItem {
	processedBatchedRequests := []*queueItem{}
	unprocessedBatchedRequests := []*queueItem{}
	nodeHealthy := f.isNodeHealthyFunc()

	for i := range batchedRequests {
		request := batchedRequests[i]

		if !nodeHealthy {
			// request can't be processed because the node is not healthy => re-add it to the queue
			unprocessedBatchedRequests = append(unprocessedBatchedRequests, request)

			continue
		}

		if collectedRequestsCounter >= iotago.MaxOutputsCount-1 {
			// request can't be processed in this transaction => re-add it to the queue
			unprocessedBatchedRequests = append(unprocessedBatchedRequests, request)

			continue
		}

		if balance < request.BaseTokenAmount {
			// not enough funds to process this request => ignore the request
			f.clearRequestWithoutLocking(request)

			continue
		}

		// request can be processed in this transaction
		balance -= request.BaseTokenAmount
		collectedRequestsCounter++
		processedBatchedRequests = append(processedBatchedRequests, request)
	}

	f.readdRequestsWithoutLocking(unprocessedBatchedRequests)

	return processedBatchedRequests
}

// createTransactionBuilder creates a transaction builder with all inputs and batched requests.
func (f *Faucet) createTransactionBuilder(api iotago.API, unspentOutputs []UTXOBasicOutput, batchedRequests []*queueItem) (*builder.TransactionBuilder, iotago.OutputIDs, int) {
	txBuilder := builder.NewTransactionBuilder(api, f.addressSigner)
	txBuilder.AddTaggedDataPayload(&iotago.TaggedData{Tag: f.opts.tagMessage, Data: nil})

	var outputCount int
	var remainderAmount int64
	var remainderOutputIndex int

	// collect all unspent output of the faucet address
	consumedInputs := []iotago.OutputID{}
	for _, unspentOutput := range unspentOutputs {
		outputCount++
		remainderAmount += int64(unspentOutput.Output.Amount)
		txBuilder.AddInput(&builder.TxInput{UnlockTarget: f.address, InputID: unspentOutput.OutputID, Input: unspentOutput.Output})
		consumedInputs = append(consumedInputs, unspentOutput.OutputID)
	}

	manaPayoutPerOutput := func() iotago.Mana {
		// we don't know the exact slot for the transaction yet, but we use the latest slot for the estimation.
		// this is no problem, because we issue the transaction immediately afterwards, so the commitment for block issuance should be older anyway.
		// also we only use the stored mana in the calculation, so we don't have the influence of mana generation.
		// because of the bigger "manaAmountMinFaucet" threshold, there is also a lot of wiggle room.
		availableManaInputs, err := txBuilder.CalculateAvailableManaInputs(f.getLatestSlotFunc())
		if err != nil {
			f.logSoftError(ierrors.Wrap(err, "failed to calculate available mana balance"))

			return 0
		}

		totalManaPayouts, err := safemath.SafeMul(iotago.Mana(len(batchedRequests)), f.opts.manaAmount)
		if err != nil {
			f.logSoftError(ierrors.Wrap(err, "failed to calculate required total mana for payouts"))

			return 0
		}

		unboundStoredManaRemainder, err := safemath.SafeSub(availableManaInputs.UnboundStoredMana, totalManaPayouts)
		if err != nil {
			f.logSoftError(ierrors.Wrapf(err, "not enough mana left in the faucet to do the payouts: %d < %d", availableManaInputs.UnboundStoredMana, totalManaPayouts))

			// underflow => not enough mana left in the faucet
			return 0
		}

		if unboundStoredManaRemainder <= f.opts.manaAmountMinFaucet {
			f.logSoftError(ierrors.Errorf("not enough mana left in the faucet: %d < %d", unboundStoredManaRemainder, f.opts.manaAmountMinFaucet))

			// not enough mana left in the faucet
			return 0
		}

		return f.opts.manaAmount
	}()

	// add all requests as outputs
	for _, req := range batchedRequests {
		outputCount++

		if outputCount >= iotago.MaxOutputsCount-1 {
			// do not collect further requests
			// the last slot is for the remainder
			break
		}

		if remainderAmount == 0 {
			// do not collect further requests
			break
		}

		baseTokenAmount := req.BaseTokenAmount
		if remainderAmount < int64(baseTokenAmount) {
			// not enough funds left
			baseTokenAmount = iotago.BaseToken(remainderAmount)
		}
		remainderAmount -= int64(baseTokenAmount)

		txBuilder.AddOutput(&iotago.BasicOutput{
			Amount: baseTokenAmount,
			Mana:   manaPayoutPerOutput,
			UnlockConditions: iotago.BasicOutputUnlockConditions{
				&iotago.AddressUnlockCondition{Address: req.Address},
			},
		})
		remainderOutputIndex++
	}

	if remainderAmount > 0 {
		txBuilder.AddOutput(&iotago.BasicOutput{
			Amount: iotago.BaseToken(remainderAmount),
			UnlockConditions: iotago.BasicOutputUnlockConditions{
				&iotago.AddressUnlockCondition{Address: f.address},
			},
		})
	}

	return txBuilder, consumedInputs, remainderOutputIndex
}

// sendFaucetBlockWithoutLocking creates a faucet transaction payload and sends it to the block issuer.
// write lock must be acquired outside.
func (f *Faucet) sendFaucetBlockWithoutLocking(ctx context.Context, unspentOutputs []UTXOBasicOutput, batchedRequests []*queueItem) error {
	api := f.apiProvider.CommittedAPI()

	txBuilder, consumedInputs, remainderOutputIndex := f.createTransactionBuilder(api, unspentOutputs, batchedRequests)

	blockPayload, blockID, err := f.submitTransactionPayloadFunc(ctx, txBuilder, remainderOutputIndex, f.opts.powWorkerCount)
	if err != nil {
		return ierrors.Errorf("submit faucet transaction payload failed, error: %w", err)
	}

	signedTx, ok := blockPayload.(*iotago.SignedTransaction)
	if !ok {
		return ierrors.Errorf("submitted faucet transaction payload is not a SignedTransaction, got instead: %T", blockPayload)
	}

	transactionID, err := signedTx.Transaction.ID()
	if err != nil {
		return ierrors.Errorf("send faucet block failed, error: %w", err)
	}

	f.setPendingTransactionWithoutLocking(&pendingTransaction{
		BlockID:        blockID,
		QueuedItems:    batchedRequests,
		ConsumedInputs: consumedInputs,
		TransactionID:  transactionID,
	})

	f.Events.IssuedBlock.Trigger(blockID)

	return nil
}

// computeAndSetInitialFaucetBalance computes the faucet balance minus the storage deposit for a single basic output.
func (f *Faucet) computeAndSetInitialFaucetBalance() error {
	f.Lock()
	defer f.Unlock()

	_, balance, err := f.collectUnlockableFaucetOutputsAndBalanceFuncWithoutLocking()
	if err != nil {
		return err
	}

	f.faucetBalance = balance

	return nil
}

// collectRequestsAndSendFaucetBlock collects the requests and sends a faucet block.
func (f *Faucet) collectRequestsAndSendFaucetBlock(ctx context.Context) error {
	f.LogDebug("entering collectRequestsAndSendFaucetBlock...")
	defer f.LogDebug("leaving collectRequestsAndSendFaucetBlock...")

	f.RLock()
	pendingTx := f.pendingTransaction
	f.RUnlock()

	// check if there is a pending transaction before issuing the next one
	if pendingTx != nil {
		f.LogDebugf("skip processing of new requests because a pending tx was found, blockID: %s, txID: %s", f.pendingTransaction.BlockID, f.pendingTransaction.TransactionID)

		select {
		case <-ctx.Done():
			// faucet was stopped
			return nil
		case <-time.After(time.Second):
			// cooldown
			return nil
		}
	}

	// first collect requests
	batchedRequests, err := f.collectRequests(ctx)
	if err != nil {
		if ierrors.Is(err, ErrOperationAborted) {
			return nil
		}
		if IsCriticalError(err) != nil {
			// error is a critical error
			// => stop the faucet
			return err
		}
		f.logSoftError(err)

		return nil
	}

	f.LogDebugf("collected %d requests", len(batchedRequests))

	// write lock must be acquired outside
	processRequestsWithoutLocking := func() ([]UTXOBasicOutput, []*queueItem, error) {
		unspentOutputs, balance, err := f.collectUnlockableFaucetOutputsAndBalanceFuncWithoutLocking()
		if err != nil {
			return nil, nil, err
		}
		f.faucetBalance = balance

		if len(unspentOutputs) < 2 && len(batchedRequests) == 0 {
			// no need to sweep or send funds
			return nil, nil, ErrNothingToProcess
		}

		processableRequests := f.processRequestsWithoutLocking(len(unspentOutputs), balance, batchedRequests)

		return unspentOutputs, processableRequests, nil
	}

	// we need to acquire a write lock here to be able to modify the requests in the queue
	f.Lock()
	defer f.Unlock()

	unspentOutputs, processableRequests, err := processRequestsWithoutLocking()
	if err != nil {
		if !ierrors.Is(err, ErrNothingToProcess) {
			if IsCriticalError(err) != nil {
				// error is a critical error
				// => stop the faucet
				return err
			}
			f.logSoftError(err)
		}
		// readd the all collected requests back to the queue
		f.readdRequestsWithoutLocking(batchedRequests)

		return nil
	}

	f.LogDebugf("determined %d available unspent outputs and %d processable requests", len(unspentOutputs), len(processableRequests))
	for i, unspentOutput := range unspentOutputs {
		f.LogDebugf("	unspent output %d, outputID: %s, amount: %d, mana: %d", i, unspentOutput.OutputID.ToHex(), unspentOutput.Output.Amount, unspentOutput.Output.Mana)
	}
	for i, processableRequest := range processableRequests {
		f.LogDebugf("	processable request %d, address: %s, amount: %d", i, processableRequest.Bech32, processableRequest.BaseTokenAmount)
	}

	if err := f.sendFaucetBlockWithoutLocking(ctx, unspentOutputs, processableRequests); err != nil {
		if IsCriticalError(err) != nil {
			// error is a critical error
			// => stop the faucet
			return err
		}
		// readd the non-processed requests back to the queue
		f.readdRequestsWithoutLocking(processableRequests)
		f.logSoftError(err)
	}

	return nil
}

// RunFaucetLoop collects unspent outputs on the faucet address and batches the requests from the queue.
func (f *Faucet) RunFaucetLoop(ctx context.Context) error {
	// set initial faucet balance
	if err := f.computeAndSetInitialFaucetBalance(); err != nil {
		return CriticalError(ierrors.Errorf("reading faucet address balance failed: %s, error: %w", f.address.Bech32(f.apiProvider.CommittedAPI().ProtocolParameters().Bech32HRP()), err))
	}

	checkPendingTxTicker := time.NewTicker(5 * time.Second)
	defer timeutil.CleanupTicker(checkPendingTxTicker)

	for {
		select {
		case <-ctx.Done():
			// faucet was stopped
			return nil

		case <-checkPendingTxTicker.C:
			// check periodically for pending transaction state
			f.checkPendingTransactionState()

		default:
			if err := f.collectRequestsAndSendFaucetBlock(ctx); err != nil {
				return err
			}
		}
	}
}

// checkPendingTransactionState checks if a pending transaction was orphaned or another error occurred.
// If a problem is found, all requests are readded to the queue.
func (f *Faucet) checkPendingTransactionState() {
	f.LogDebug("entering checkPendingTransactionState...")
	defer f.LogDebug("leaving checkPendingTransactionState...")

	//nolint:nonamedreturns // easier to read in this case
	checkPendingTransaction := func(pendingTx *pendingTransaction) (clearPending bool, readdPending bool, logMessage string, softError error) {
		if pendingTx == nil {
			// no pending transaction so there is no need for additional checks
			return false, false, "no pending transaction found", nil
		}

		metadata, err := f.fetchTransactionMetadataFunc(pendingTx.TransactionID)
		if err != nil {
			// an error occurred => re-add the items to the queue and delete the pending transaction
			return false, true, "", ierrors.Errorf("failed to fetch metadata of the pending transaction, blockID: %s, txID: %s", pendingTx.BlockID, pendingTx.TransactionID)
		}

		if metadata == nil {
			// metadata unknown, this can only happen if the block was orphaned.
			// => re-add the items to the queue and delete the pending transaction
			return false, true, "", ierrors.Errorf("metadata of the pending transaction is unknown, blockID: %s, txID: %s", pendingTx.BlockID, pendingTx.TransactionID)
		}

		switch metadata.TransactionState {
		case api.TransactionStateUnknown:
			// transaction is not known, so the block must have been filtered
			// => re-add the items to the queue and delete the pending transaction
			return false, true, "", ierrors.Errorf("metadata of the pending transaction is no transaction, blockID: %s, txID: %s", pendingTx.BlockID, pendingTx.TransactionID)

		case api.TransactionStatePending:
			// transaction is still pending
			// => do nothing
			return false, false, fmt.Sprintf("transaction still pending, blockID: %s, txID: %s", pendingTx.BlockID, pendingTx.TransactionID), nil

		case api.TransactionStateAccepted, api.TransactionStateCommitted, api.TransactionStateFinalized:
			// transaction was accepted
			// => delete the requests and the pending transaction
			return true, false, fmt.Sprintf("transaction successful, blockID: %s, txID: %s", pendingTx.BlockID, pendingTx.TransactionID), nil

		case api.TransactionStateFailed:
			// transaction failed
			// => re-add the items to the queue and delete the pending transaction
			return false, true, "", ierrors.Errorf("transaction failed, blockID: %s, txID: %s, reason: %d", pendingTx.BlockID, pendingTx.TransactionID, metadata.TransactionFailureReason)

		default:
			// unknown transaction state
			panic(ierrors.Errorf("unknown transaction state: %d", metadata.TransactionState))
		}
	}

	f.RLock()
	pendingTx := f.pendingTransaction
	f.RUnlock()

	clearPending, readdPending, logMessage, softError := checkPendingTransaction(pendingTx)
	if !(clearPending || readdPending) {
		// no pending transaction or transaction is still pending
		if softError != nil {
			f.logSoftError(ierrors.Wrap(softError, "checkPendingTransactionState failed"))
		}

		if logMessage != "" {
			f.LogDebugf("checkPendingTransactionState: %s", logMessage)
		}

		return
	}

	// we need to acquire a write lock here and check again if there is a pending transaction.
	f.Lock()
	defer f.Unlock()

	if pendingTx != f.pendingTransaction {
		// the pending transaction changed, check again
		clearPending, readdPending, logMessage, softError = checkPendingTransaction(f.pendingTransaction)
	}

	if softError != nil {
		f.logSoftError(ierrors.Wrap(softError, "checkPendingTransactionState failed"))
	}

	if logMessage != "" {
		f.LogDebugf("checkPendingTransactionState: %s", logMessage)
	}

	if clearPending {
		f.clearPendingRequestsWithoutLocking()
		return
	}
	if readdPending {
		f.readdPendingRequestsWithoutLocking()
	}
}

// ApplyAcceptedTransaction applies an accepted transaction to the faucet.
// If there is a pending transaction, it is checked if the transaction was confirmed or conflicting.
// If a conflict is found, all requests are readded to the queue.
func (f *Faucet) ApplyAcceptedTransaction(createdOutputs map[iotago.OutputID]struct{}, consumedOutputs map[iotago.OutputID]struct{}) {
	f.LogDebug("entering ApplyAcceptedTransaction...")
	defer f.LogDebug("leaving ApplyAcceptedTransaction...")

	//nolint:nonamedreturns // easier to read in this case
	checkPendingTransaction := func(pendingTx *pendingTransaction) (clearPending bool, readdPending bool, logMessage string) {
		if pendingTx == nil {
			// no pending transaction so there is no need for additional checks
			return false, false, "no pending transaction found"
		}

		// check if the pending transaction was confirmed.
		// we can easily check this by searching for output index 0.
		txOutputIndexZero := iotago.UTXOInput{
			TransactionID:          pendingTx.TransactionID,
			TransactionOutputIndex: 0,
		}
		txOutputIDIndexZero := txOutputIndexZero.OutputID()

		// if this output was created, the rest of the outputs were created as well because transactions are atomic.
		if _, created := createdOutputs[txOutputIDIndexZero]; created {
			// transaction was confirmed
			// => delete the requests and the pending transaction
			return true, false, "transaction successful"
		}

		// check if the inputs of the pending transaction were affected by the ledger update.
		for _, consumedInput := range pendingTx.ConsumedInputs {
			if _, spent := consumedOutputs[consumedInput]; spent {
				// a referenced input of the pending transaction was spent, so it is affected by this ledger update.
				// since the output index 0 of the pending transaction was not created,
				// it means that the transaction was conflicting with another one.
				// => readd the items to the queue and delete the pending transaction
				return false, true, "transaction conflicting, inputs consumed in another transaction"
			}
		}

		return false, false, ""
	}

	f.RLock()
	pendingTx := f.pendingTransaction
	f.RUnlock()

	clearPending, readdPending, logMessage := checkPendingTransaction(pendingTx)
	if !(clearPending || readdPending) {
		// no pending transaction or transaction is not affected by the update
		if logMessage != "" {
			f.LogDebugf("ApplyAcceptedTransaction: %s", logMessage)
		}

		return
	}

	// we need to acquire a write lock here and check again if there is a pending transaction.
	f.Lock()
	defer f.Unlock()

	if pendingTx != f.pendingTransaction {
		// the pending transaction changed, check again
		clearPending, readdPending, logMessage = checkPendingTransaction(f.pendingTransaction)
	}

	if logMessage != "" {
		f.LogDebugf("ApplyAcceptedTransaction: %s", logMessage)
	}

	if clearPending {
		f.clearPendingRequestsWithoutLocking()
		return
	}
	if readdPending {
		f.readdPendingRequestsWithoutLocking()
	}
}

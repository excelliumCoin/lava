package lavasession

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/gogo/status"
	"github.com/lavanet/lava/utils"
	"google.golang.org/grpc/codes"
)

// created with NewConsumerSessionManager
type ConsumerSessionManager struct {
	rpcEndpoint    *RPCEndpoint // used to filter out endpoints
	lock           sync.RWMutex
	pairing        map[string]*ConsumerSessionsWithProvider // key == provider address
	currentEpoch   uint64
	numberOfResets uint64
	// pairingAddresses for Data reliability
	pairingAddresses       []string // contains all addresses from the initial pairing.
	pairingAddressesLength uint64

	validAddresses        []string            // contains all addresses that are currently valid
	addedToPurgeAndReport map[string]struct{} // list of purged providers to report for QoS unavailability. (easier to search maps.)

	// pairingPurge - contains all pairings that are unwanted this epoch, keeps them in memory in order to avoid release.
	// (if a consumer session still uses one of them or we want to report it.)
	pairingPurge map[string]*ConsumerSessionsWithProvider
}

func (csm *ConsumerSessionManager) RPCEndpoint() RPCEndpoint {
	return *csm.rpcEndpoint
}

// Update the provider pairing list for the ConsumerSessionManager
func (csm *ConsumerSessionManager) UpdateAllProviders(epoch uint64, pairingList []*ConsumerSessionsWithProvider) error {
	pairingListLength := len(pairingList)

	csm.lock.Lock()         // start by locking the class lock.
	defer csm.lock.Unlock() // we defer here so in case we return an error it will unlock automatically.

	if epoch <= csm.atomicReadCurrentEpoch() { // sentry shouldn't update an old epoch or current epoch
		return utils.LavaFormatError("trying to update provider list for older epoch", nil, &map[string]string{"epoch": strconv.FormatUint(epoch, 10), "currentEpoch": strconv.FormatUint(csm.atomicReadCurrentEpoch(), 10)})
	}
	// Update Epoch.
	csm.atomicWriteCurrentEpoch(epoch)

	// Reset States
	// csm.validAddresses length is reset in setValidAddressesToDefaultValue
	csm.pairingAddresses = make([]string, pairingListLength)
	csm.addedToPurgeAndReport = make(map[string]struct{}, 0)
	csm.pairingAddressesLength = uint64(pairingListLength)
	csm.numberOfResets = 0

	// Reset the pairingPurge.
	// This happens only after an entire epoch. so its impossible to have session connected to the old purged list
	csm.pairingPurge = csm.pairing
	csm.pairing = make(map[string]*ConsumerSessionsWithProvider, pairingListLength)
	for idx, provider := range pairingList {
		csm.pairingAddresses[idx] = provider.PublicLavaAddress
		csm.pairing[provider.PublicLavaAddress] = provider
	}
	csm.setValidAddressesToDefaultValue() // the starting point is that valid addresses are equal to pairing addresses.

	return nil
}

func (csm *ConsumerSessionManager) setValidAddressesToDefaultValue() {
	csm.validAddresses = make([]string, len(csm.pairingAddresses))
	copy(csm.validAddresses, csm.pairingAddresses)
}

// reads cs.currentEpoch atomically
func (csm *ConsumerSessionManager) atomicWriteCurrentEpoch(epoch uint64) {
	atomic.StoreUint64(&csm.currentEpoch, epoch)
}

// reads cs.currentEpoch atomically
func (csm *ConsumerSessionManager) atomicReadCurrentEpoch() (epoch uint64) {
	return atomic.LoadUint64(&csm.currentEpoch)
}

// validate if reset is needed for valid addresses list.
func (csm *ConsumerSessionManager) shouldResetValidAddresses() (reset bool, numberOfResets uint64) {
	csm.lock.RLock() // lock read to validate length
	defer csm.lock.RUnlock()
	numberOfResets = csm.numberOfResets
	if len(csm.validAddresses) == 0 {
		reset = true
	}
	return
}

// reset the valid addresses list and increase numberOfResets
func (csm *ConsumerSessionManager) resetValidAddresses() uint64 {
	csm.lock.Lock() // lock write
	defer csm.lock.Unlock()
	if len(csm.validAddresses) == 0 { // re verify it didn't change while waiting for lock.
		utils.LavaFormatWarning("Provider pairing list is empty, resetting state.", nil, nil)
		csm.setValidAddressesToDefaultValue()
		csm.numberOfResets += 1
	}
	// if len(csm.validAddresses) != 0 meaning we had a reset (or an epoch change), so we need to return the numberOfResets which is currently in csm
	return csm.numberOfResets
}

// validating we still have providers, otherwise reset valid addresses list
func (csm *ConsumerSessionManager) validatePairingListNotEmpty() uint64 {
	reset, numberOfResets := csm.shouldResetValidAddresses()
	if reset {
		numberOfResets = csm.resetValidAddresses()
	}
	return numberOfResets
}

// GetSession will return a ConsumerSession, given cu needed for that session.
// The user can also request specific providers to not be included in the search for a session.
func (csm *ConsumerSessionManager) GetSession(ctx context.Context, cuNeededForSession uint64, initUnwantedProviders map[string]struct{}) (
	consumerSession *SingleConsumerSession, epoch uint64, providerPublicAddress string, reportedProviders []byte, errRet error,
) {
	numberOfResets := csm.validatePairingListNotEmpty() // if pairing list is empty we reset the state.

	if initUnwantedProviders == nil { // verify initUnwantedProviders is not nil
		initUnwantedProviders = make(map[string]struct{})
	}
	// providers that we don't try to connect this iteration.
	tempIgnoredProviders := &ignoredProviders{
		providers:    initUnwantedProviders,
		currentEpoch: csm.atomicReadCurrentEpoch(),
	}

	for {
		// Get a valid consumerSessionWithProvider
		consumerSessionWithProvider, providerAddress, sessionEpoch, err := csm.getValidConsumerSessionsWithProvider(tempIgnoredProviders, cuNeededForSession)
		if err != nil {
			if PairingListEmptyError.Is(err) {
				return nil, 0, "", nil, err
			} else if MaxComputeUnitsExceededError.Is(err) {
				// This provider doesn't have enough compute units for this session, we block it for this session and continue to another provider.
				utils.LavaFormatError("Max Compute Units Exceeded For provider", err, &map[string]string{"providerAddress": providerAddress})
				tempIgnoredProviders.providers[providerAddress] = struct{}{}
				continue
			} else {
				utils.LavaFormatFatal("Unsupported Error", err, nil)
			}
		}

		// Get a valid Endpoint from the provider chosen
		connected, endpoint, err := consumerSessionWithProvider.fetchEndpointConnectionFromConsumerSessionWithProvider(ctx, sessionEpoch)
		if err != nil {
			// verify err is AllProviderEndpointsDisabled and report.
			if AllProviderEndpointsDisabledError.Is(err) {
				err = csm.blockProvider(providerAddress, true, sessionEpoch) // reporting and blocking provider this epoch
				if err != nil {
					if !EpochMismatchError.Is(err) {
						// only acceptable error is EpochMismatchError so if different, throw fatal
						utils.LavaFormatFatal("Unsupported Error", err, nil)
					}
				}
				continue
			} else {
				utils.LavaFormatFatal("Unsupported Error", err, nil)
			}
		} else if !connected {
			// If failed to connect we ignore this provider for this get session request only
			// and try again getting a random provider to pick from
			tempIgnoredProviders.providers[providerAddress] = struct{}{}
			continue
		}

		// we get the reported providers here after we try to connect, so if any provider did'nt respond he will already be added to the list.
		reportedProviders, err = csm.GetReportedProviders(sessionEpoch)
		if err != nil {
			// if failed to GetReportedProviders just log the error and continue.
			utils.LavaFormatError("Failed Unmarshal Error in GetReportedProviders", err, nil)
		}

		// Get session from endpoint or create new or continue. if more than 10 connections are open.
		consumerSession, pairingEpoch, err := consumerSessionWithProvider.getConsumerSessionInstanceFromEndpoint(endpoint, numberOfResets)
		if err != nil {
			utils.LavaFormatDebug("Error on consumerSessionWithProvider.getConsumerSessionInstanceFromEndpoint", &map[string]string{"Error": err.Error()})
			if MaximumNumberOfSessionsExceededError.Is(err) {
				// we can get a different provider, adding this provider to the list of providers to skip on.
				tempIgnoredProviders.providers[providerAddress] = struct{}{}
			} else if MaximumNumberOfBlockListedSessionsError.Is(err) {
				// provider has too many block listed sessions. we block it until the next epoch.
				err = csm.blockProvider(providerAddress, false, sessionEpoch)
				if err != nil {
					return nil, 0, "", nil, err
				}
			} else {
				utils.LavaFormatFatal("Unsupported Error", err, nil)
			}
			continue
		}

		if pairingEpoch != sessionEpoch {
			// pairingEpoch and SessionEpoch must be the same, we validate them here if they are different we raise an error and continue with pairingEpoch
			utils.LavaFormatError("sessionEpoch and pairingEpoch mismatch", nil, &map[string]string{"sessionEpoch": strconv.FormatUint(sessionEpoch, 10), "pairingEpoch": strconv.FormatUint(pairingEpoch, 10)})
			sessionEpoch = pairingEpoch
		}

		// If we successfully got a consumerSession we can apply the current CU to the consumerSessionWithProvider.UsedComputeUnits
		err = consumerSessionWithProvider.addUsedComputeUnits(cuNeededForSession)
		if err != nil {
			utils.LavaFormatDebug("consumerSessionWithProvider.addUsedComputeUnit", &map[string]string{"Error": err.Error()})
			if MaxComputeUnitsExceededError.Is(err) {
				tempIgnoredProviders.providers[providerAddress] = struct{}{}
				// We must unlock the consumer session before continuing.
				consumerSession.lock.Unlock()
				continue
			} else {
				utils.LavaFormatFatal("Unsupported Error", err, nil)
			}
		} else {
			consumerSession.LatestRelayCu = cuNeededForSession // set latestRelayCu
			// Successfully created/got a consumerSession.
			return consumerSession, sessionEpoch, providerAddress, reportedProviders, nil
		}
		utils.LavaFormatFatal("Unreachable Error", UnreachableCodeError, nil)
	}
}

// Get a valid provider address.
func (csm *ConsumerSessionManager) getValidProviderAddress(ignoredProvidersList map[string]struct{}) (address string, err error) {
	// cs.Lock must be Rlocked here.
	ignoredProvidersListLength := len(ignoredProvidersList)
	validAddressesLength := len(csm.validAddresses)
	totalValidLength := validAddressesLength - ignoredProvidersListLength
	if totalValidLength <= 0 {
		utils.LavaFormatDebug("Pairing list empty", &map[string]string{"Provider list": fmt.Sprintf("%v", csm.validAddresses), "IgnoredProviderList": fmt.Sprintf("%v", ignoredProvidersList)})
		err = PairingListEmptyError
		return
	}
	validAddressIndex := rand.Intn(totalValidLength) // get the N'th valid provider index, only valid providers will increase the addressIndex counter
	validAddressesCounter := 0                       // this counter will try to reach the addressIndex
	for index := 0; index < validAddressesLength; index++ {
		if _, ok := ignoredProvidersList[csm.validAddresses[index]]; !ok { // not ignored -> yes valid
			if validAddressesCounter == validAddressIndex {
				return csm.validAddresses[index], nil
			}
			validAddressesCounter += 1
		}
	}
	return "", UnreachableCodeError // should not reach here
}

func (csm *ConsumerSessionManager) getValidConsumerSessionsWithProvider(ignoredProviders *ignoredProviders, cuNeededForSession uint64) (consumerSessionWithProvider *ConsumerSessionsWithProvider, providerAddress string, currentEpoch uint64, err error) {
	csm.lock.RLock()
	defer csm.lock.RUnlock()
	currentEpoch = csm.atomicReadCurrentEpoch() // reading the epoch here while locked, to get the epoch of the pairing.
	if ignoredProviders.currentEpoch < currentEpoch {
		utils.LavaFormatDebug("ignoredProviders epoch is not the current epoch, resetting ignoredProviders", &map[string]string{"ignoredProvidersEpoch": strconv.FormatUint(ignoredProviders.currentEpoch, 10), "currentEpoch": strconv.FormatUint(currentEpoch, 10)})
		ignoredProviders.providers = make(map[string]struct{}) // reset the old providers as epochs changed so we have a new pairing list.
		ignoredProviders.currentEpoch = currentEpoch
	}

	providerAddress, err = csm.getValidProviderAddress(ignoredProviders.providers)
	if err != nil {
		utils.LavaFormatError("could not get a provider address", err, nil)
		return nil, "", 0, err
	}
	consumerSessionWithProvider = csm.pairing[providerAddress]
	if err := consumerSessionWithProvider.validateComputeUnits(cuNeededForSession); err != nil { // checking if we even have enough compute units for this provider.
		return nil, providerAddress, 0, err // provider address is used to add to temp ignore upon error
	}
	return
}

// removes a given address from the valid addresses list.
func (csm *ConsumerSessionManager) removeAddressFromValidAddresses(address string) error {
	// cs Must be Locked here.
	for idx, addr := range csm.validAddresses {
		if addr == address {
			// remove the index from the valid list.
			csm.validAddresses = append(csm.validAddresses[:idx], csm.validAddresses[idx+1:]...)
			return nil
		}
	}
	return AddressIndexWasNotFoundError
}

// Blocks a provider making him unavailable for pick this epoch, will also report him as unavailable if reportProvider is set to true.
// Validates that the sessionEpoch is equal to cs.currentEpoch otherwise doesn't take effect.
func (csm *ConsumerSessionManager) blockProvider(address string, reportProvider bool, sessionEpoch uint64) error {
	// find Index of the address
	if sessionEpoch != csm.atomicReadCurrentEpoch() { // we read here atomically so cs.currentEpoch cant change in the middle, so we can save time if epochs mismatch
		return EpochMismatchError
	}

	csm.lock.Lock() // we lock RW here because we need to make sure nothing changes while we verify validAddresses/addedToPurgeAndReport
	defer csm.lock.Unlock()
	if sessionEpoch != csm.atomicReadCurrentEpoch() { // After we lock we need to verify again that the epoch didn't change while we waited for the lock.
		return EpochMismatchError
	}

	err := csm.removeAddressFromValidAddresses(address)
	if err != nil {
		if AddressIndexWasNotFoundError.Is(err) {
			// in case index wasnt found just continue with the method
			utils.LavaFormatError("address was not found in valid addresses", err, &map[string]string{"address": address, "validAddresses": fmt.Sprintf("%v", csm.validAddresses)})
		} else {
			return err
		}
	}

	if reportProvider { // Report provider flow
		if _, ok := csm.addedToPurgeAndReport[address]; !ok { // verify it doesn't exist already
			csm.addedToPurgeAndReport[address] = struct{}{}
		}
	}

	return nil
}

// Verify the consumerSession is locked when getting to this function, if its not locked throw an error
func (csm *ConsumerSessionManager) verifyLock(consumerSession *SingleConsumerSession) error {
	if consumerSession.lock.TryLock() { // verify.
		// if we managed to lock throw an error for misuse.
		defer consumerSession.lock.Unlock()
		return LockMisUseDetectedError
	}
	return nil
}

// A Session can be created but unused if consumer found the response in the cache.
// So we need to unlock the session and decrease the cu that were applied
func (csm *ConsumerSessionManager) OnSessionUnUsed(consumerSession *SingleConsumerSession) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionUnUsed, consumerSession.lock must be locked before accessing this method, additional info:")
	}
	cuToDecrease := consumerSession.LatestRelayCu
	consumerSession.LatestRelayCu = 0                            // making sure no one uses it in a wrong way
	parentConsumerSessionsWithProvider := consumerSession.Client // must read this pointer before unlocking
	// finished with consumerSession here can unlock.
	consumerSession.lock.Unlock()                                                    // we unlock before we change anything in the parent ConsumerSessionsWithProvider
	err := parentConsumerSessionsWithProvider.decreaseUsedComputeUnits(cuToDecrease) // change the cu in parent
	if err != nil {
		return err
	}
	return nil
}

// Report session failure, mark it as blocked from future usages, report if timeout happened.
func (csm *ConsumerSessionManager) OnSessionFailure(consumerSession *SingleConsumerSession, errorReceived error) error {
	// consumerSession must be locked when getting here.
	code := status.Code(errorReceived)

	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionFailure, consumerSession.lock must be locked before accessing this method, additional info:")
	}

	// consumer Session should be locked here. so we can just apply the session failure here.
	if consumerSession.BlockListed {
		// if consumer session is already blocklisted return an error.
		return sdkerrors.Wrapf(SessionIsAlreadyBlockListedError, "trying to report a session failure of a blocklisted consumer session")
	}

	consumerSession.QoSInfo.TotalRelays++
	consumerSession.ConsecutiveNumberOfFailures += 1 // increase number of failures for this session

	// if this session failed more than MaximumNumberOfFailuresAllowedPerConsumerSession times or session went out of sync we block it.
	if consumerSession.ConsecutiveNumberOfFailures > MaximumNumberOfFailuresAllowedPerConsumerSession || code == codes.Code(SessionOutOfSyncError.ABCICode()) {
		utils.LavaFormatDebug("Blocking consumer session", &map[string]string{"id": strconv.FormatInt(consumerSession.SessionId, 10)})
		consumerSession.BlockListed = true // block this session from future usages
	}
	cuToDecrease := consumerSession.LatestRelayCu
	consumerSession.LatestRelayCu = 0 // making sure no one uses it in a wrong way

	parentConsumerSessionsWithProvider := consumerSession.Client // must read this pointer before unlocking
	// finished with consumerSession here can unlock.
	consumerSession.lock.Unlock() // we unlock before we change anything in the parent ConsumerSessionsWithProvider

	err := parentConsumerSessionsWithProvider.decreaseUsedComputeUnits(cuToDecrease) // change the cu in parent
	if err != nil {
		return err
	}

	// check if need to block & report
	var blockProvider, reportProvider bool
	if ReportAndBlockProviderError.Is(errorReceived) {
		blockProvider = true
		reportProvider = true
	} else if BlockProviderError.Is(errorReceived) {
		blockProvider = true
	}
	if blockProvider {
		publicProviderAddress, pairingEpoch := parentConsumerSessionsWithProvider.getPublicLavaAddressAndPairingEpoch()
		err = csm.blockProvider(publicProviderAddress, reportProvider, pairingEpoch)
		if err != nil {
			if EpochMismatchError.Is(err) {
				return nil // no effects this epoch has been changed
			}
			return err
		}
	}
	return nil
}

// get a session from the pool except specific providers, which also validates the epoch.
func (csm *ConsumerSessionManager) GetSessionFromAllExcept(ctx context.Context, bannedAddresses map[string]struct{}, cuNeeded uint64, bannedAddressesEpoch uint64) (consumerSession *SingleConsumerSession, epoch uint64, providerPublicAddress string, reportedProviders []byte, err error) {
	// if bannedAddressesEpoch != current epoch, we just return GetSession. locks...
	if bannedAddressesEpoch != csm.atomicReadCurrentEpoch() {
		utils.LavaFormatDebug("Getting session ignores banned addresses due to epoch mismatch", &map[string]string{"bannedAddresses": fmt.Sprintf("%+v", bannedAddresses), "bannedAddressesEpoch": strconv.FormatUint(bannedAddressesEpoch, 10), "currentEpoch": strconv.FormatUint(csm.atomicReadCurrentEpoch(), 10)})
		return csm.GetSession(ctx, cuNeeded, nil)
	} else {
		return csm.GetSession(ctx, cuNeeded, bannedAddresses)
	}
}

// On a successful DataReliability session we don't need to increase and update any field, we just need to unlock the session.
func (csm *ConsumerSessionManager) OnDataReliabilitySessionDone(consumerSession *SingleConsumerSession,
	latestServicedBlock int64,
	specComputeUnits uint64,
	currentLatency time.Duration,
	expectedBH int64,
	numOfProviders int,
	providersCount uint64,
) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnDataReliabilitySessionDone, consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()               // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.ConsecutiveNumberOfFailures = 0   // reset failures.
	consumerSession.LatestBlock = latestServicedBlock // update latest serviced block
	consumerSession.CalculateQoS(specComputeUnits, currentLatency, expectedBH-latestServicedBlock, numOfProviders, int64(providersCount))
	return nil
}

// On a successful session this function will update all necessary fields in the consumerSession. and unlock it when it finishes
func (csm *ConsumerSessionManager) OnSessionDone(
	consumerSession *SingleConsumerSession,
	epoch uint64,
	latestServicedBlock int64,
	specComputeUnits uint64,
	currentLatency time.Duration,
	expectedBH int64,
	numOfProviders int,
	providersCount uint64,
) error {
	// release locks, update CU, relaynum etc..
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionDone, consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()                    // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.CuSum += consumerSession.LatestRelayCu // add CuSum to current cu usage.
	consumerSession.LatestRelayCu = 0                      // reset cu just in case
	consumerSession.RelayNum += RelayNumberIncrement       // increase relayNum
	consumerSession.ConsecutiveNumberOfFailures = 0        // reset failures.
	consumerSession.LatestBlock = latestServicedBlock      // update latest serviced block
	// calculate QoS
	consumerSession.CalculateQoS(specComputeUnits, currentLatency, expectedBH-latestServicedBlock, numOfProviders, int64(providersCount))
	return nil
}

// Get the reported providers currently stored in the session manager.
func (csm *ConsumerSessionManager) GetReportedProviders(epoch uint64) ([]byte, error) {
	csm.lock.RLock()
	defer csm.lock.RUnlock()
	if epoch != csm.atomicReadCurrentEpoch() {
		return []byte{}, nil // if epochs are not equal, we will return an empty list.
	}
	keys := make([]string, 0, len(csm.addedToPurgeAndReport))
	for k := range csm.addedToPurgeAndReport {
		keys = append(keys, k)
	}
	bytes, err := json.Marshal(keys)

	return bytes, err
}

// Data Reliability Section:

// Atomically read csm.pairingAddressesLength for data reliability.
func (csm *ConsumerSessionManager) GetAtomicPairingAddressesLength() uint64 {
	return atomic.LoadUint64(&csm.pairingAddressesLength)
}

func (csm *ConsumerSessionManager) getDataReliabilityProviderIndex(unAllowedAddress string, index uint64) (cswp *ConsumerSessionsWithProvider, providerAddress string, epoch uint64, err error) {
	csm.lock.RLock()
	defer csm.lock.RUnlock()
	currentEpoch := csm.atomicReadCurrentEpoch()
	pairingAddressesLength := csm.GetAtomicPairingAddressesLength()
	if index >= pairingAddressesLength {
		utils.LavaFormatInfo(DataReliabilityIndexOutOfRangeError.Error(), &map[string]string{"index": strconv.FormatUint(index, 10), "pairingAddressesLength": strconv.FormatUint(pairingAddressesLength, 10)})
		return nil, "", currentEpoch, DataReliabilityIndexOutOfRangeError
	}
	providerAddress = csm.pairingAddresses[index]
	if providerAddress == unAllowedAddress {
		return nil, "", currentEpoch, DataReliabilityIndexRequestedIsOriginalProviderError
	}
	// if address is valid return the ConsumerSessionsWithProvider
	return csm.pairing[providerAddress], providerAddress, currentEpoch, nil
}

func (csm *ConsumerSessionManager) getEndpointFromConsumerSessionWithProviderForDR(ctx context.Context, consumerSessionWithProvider *ConsumerSessionsWithProvider, sessionEpoch uint64, providerAddress string) (endpoint *Endpoint, err error) {
	var connected bool
	for idx := 0; idx < MaxConsecutiveConnectionAttempts; idx++ { // try to connect to the endpoint 3 times
		connected, endpoint, err = consumerSessionWithProvider.fetchEndpointConnectionFromConsumerSessionWithProvider(ctx, sessionEpoch)
		if err != nil {
			// verify err is AllProviderEndpointsDisabled and report.
			if AllProviderEndpointsDisabledError.Is(err) {
				err = csm.blockProvider(providerAddress, true, sessionEpoch) // reporting and blocking provider this epoch
				if err != nil {
					if !EpochMismatchError.Is(err) {
						// only acceptable error is EpochMismatchError so if different, throw fatal
						utils.LavaFormatFatal("Unsupported Error", err, nil)
					}
				}
				break // all endpoints are disabled, no reason to continue with this provider.
			} else {
				utils.LavaFormatFatal("Unsupported Error", err, nil)
			}
		}
		if connected {
			// if we are connected we can stop trying and return the endpoint
			break
		} else {
			continue
		}
	}
	if !connected { // if we are not connected at the end
		// failed to get an endpoint connection from that provider. return an error.
		return nil, utils.LavaFormatError("Not Connected", FailedToConnectToEndPointForDataReliabilityError, &map[string]string{"provider": providerAddress})
	}
	return endpoint, nil
}

// Get a Data Reliability Session
func (csm *ConsumerSessionManager) GetDataReliabilitySession(ctx context.Context, originalProviderAddress string, index int64, sessionEpoch uint64) (singleConsumerSession *SingleConsumerSession, providerAddress string, epoch uint64, err error) {
	consumerSessionWithProvider, providerAddress, currentEpoch, err := csm.getDataReliabilityProviderIndex(originalProviderAddress, uint64(index))
	if err != nil {
		return nil, "", 0, err
	}
	if sessionEpoch != currentEpoch { // validate we are in the same epoch.
		return nil, "", currentEpoch, DataReliabilityEpochMismatchError
	}

	// after choosing a provider, try to see if it already has an existing data reliability session.
	err = consumerSessionWithProvider.verifyDataReliabilitySessionWasNotAlreadyCreated()
	if err != nil {
		return nil, "", currentEpoch, err
	}

	// We can get an endpoint now and create a data reliability session.
	endpoint, err := csm.getEndpointFromConsumerSessionWithProviderForDR(ctx, consumerSessionWithProvider, currentEpoch, providerAddress)
	if err != nil {
		return nil, "", currentEpoch, err
	}

	// get data reliability session from endpoint
	consumerSession, pairingEpoch, err := consumerSessionWithProvider.getDataReliabilitySingleConsumerSession(endpoint)
	if err != nil {
		return nil, "", currentEpoch, err
	}

	if currentEpoch != pairingEpoch { // validate they are the same, if not print an error and set currentEpoch to pairingEpoch.
		utils.LavaFormatError("currentEpoch and pairingEpoch mismatch", nil, &map[string]string{"sessionEpoch": strconv.FormatUint(currentEpoch, 10), "pairingEpoch": strconv.FormatUint(pairingEpoch, 10)})
		currentEpoch = pairingEpoch
	}

	return consumerSession, providerAddress, currentEpoch, nil
}

// On a successful Subscribe relay
func (csm *ConsumerSessionManager) OnSessionDoneIncreaseRelayAndCu(consumerSession *SingleConsumerSession) error {
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnSessionDoneIncreaseRelayAndCu consumerSession.lock must be locked before accessing this method")
	}

	defer consumerSession.lock.Unlock()                    // we need to be locked here, if we didn't get it locked we try lock anyway
	consumerSession.CuSum += consumerSession.LatestRelayCu // add CuSum to current cu usage.
	consumerSession.LatestRelayCu = 0                      // reset cu just in case
	consumerSession.RelayNum += RelayNumberIncrement       // increase relayNum
	consumerSession.ConsecutiveNumberOfFailures = 0        // reset failures.
	return nil
}

// On a failed DataReliability session we don't decrease the cu unlike a normal session, we just unlock and verify if we need to block this session or provider.
func (csm *ConsumerSessionManager) OnDataReliabilitySessionFailure(consumerSession *SingleConsumerSession, errorReceived error) error {
	// consumerSession must be locked when getting here.
	if err := csm.verifyLock(consumerSession); err != nil {
		return sdkerrors.Wrapf(err, "OnDataReliabilitySessionFailure consumerSession.lock must be locked before accessing this method")
	}
	// consumer Session should be locked here. so we can just apply the session failure here.
	if consumerSession.BlockListed {
		// if consumer session is already blocklisted return an error.
		return sdkerrors.Wrapf(SessionIsAlreadyBlockListedError, "trying to report a session failure of a blocklisted client session")
	}
	consumerSession.QoSInfo.TotalRelays++
	consumerSession.ConsecutiveNumberOfFailures += 1 // increase number of failures for this session

	// if this session failed more than MaximumNumberOfFailuresAllowedPerConsumerSession times we block list it.
	if consumerSession.ConsecutiveNumberOfFailures > MaximumNumberOfFailuresAllowedPerConsumerSession {
		consumerSession.BlockListed = true // block this session from future usages
	} else if SessionOutOfSyncError.Is(errorReceived) { // this is an error that we must block the session due to.
		consumerSession.BlockListed = true
	}

	var blockProvider, reportProvider bool
	if ReportAndBlockProviderError.Is(errorReceived) {
		blockProvider = true
		reportProvider = true
	} else if BlockProviderError.Is(errorReceived) {
		blockProvider = true
	}

	parentConsumerSessionsWithProvider := consumerSession.Client
	consumerSession.lock.Unlock()

	if blockProvider {
		publicProviderAddress, pairingEpoch := parentConsumerSessionsWithProvider.getPublicLavaAddressAndPairingEpoch()
		err := csm.blockProvider(publicProviderAddress, reportProvider, pairingEpoch)
		if err != nil {
			if EpochMismatchError.Is(err) {
				return nil // no effects this epoch has been changed
			}
			return err
		}
	}

	return nil
}

func NewConsumerSessionManager(rpcEndpoint *RPCEndpoint) *ConsumerSessionManager {
	csm := ConsumerSessionManager{}
	csm.rpcEndpoint = rpcEndpoint
	return &csm
}

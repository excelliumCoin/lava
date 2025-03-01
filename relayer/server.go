package relayer

import (
	"bytes"
	context "context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/status"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"golang.org/x/exp/slices"

	btcSecp256k1 "github.com/btcsuite/btcd/btcec"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/version"
	"github.com/lavanet/lava/relayer/chainproxy"
	"github.com/lavanet/lava/relayer/chainproxy/rpcclient"
	"github.com/lavanet/lava/relayer/chainsentry"
	"github.com/lavanet/lava/relayer/lavasession"
	"github.com/lavanet/lava/relayer/performance"
	"github.com/lavanet/lava/relayer/sentry"
	"github.com/lavanet/lava/relayer/sigs"
	"github.com/lavanet/lava/utils"
	conflicttypes "github.com/lavanet/lava/x/conflict/types"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
	"github.com/spf13/pflag"
	tenderbytes "github.com/tendermint/tendermint/libs/bytes"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

const (
	RETRY_INCORRECT_SEQUENCE      = 5
	TimeWaitInitializeChainSentry = 10
	RetryInitAttempts             = 10
)

var (
	g_privKey               *btcSecp256k1.PrivateKey
	g_sessions              map[string]*UserSessions
	g_sessions_mutex        utils.LavaMutex
	g_votes                 map[string]*voteData
	g_votes_mutex           utils.LavaMutex
	g_sentry                *sentry.Sentry
	g_serverChainID         string
	g_txFactory             tx.Factory
	g_chainProxy            chainproxy.ChainProxy
	g_chainSentry           *chainsentry.ChainSentry
	g_rewardsSessions       map[uint64][]*RelaySession // map[epochHeight][]*rewardableSessions
	g_rewardsSessions_mutex utils.LavaMutex
	g_serverID              uint64
	g_askForRewards_mutex   sync.Mutex
)

type UserSessionsEpochData struct {
	UsedComputeUnits uint64
	MaxComputeUnits  uint64
	DataReliability  *pairingtypes.VRFData
	VrfPk            utils.VrfPubKey
}

type UserSessions struct {
	Sessions      map[uint64]*RelaySession
	Subs          map[string]*subscription // key: subscriptionID
	IsBlockListed bool
	user          string
	dataByEpoch   map[uint64]*UserSessionsEpochData
	Lock          utils.LavaMutex
}

type RelaySession struct {
	userSessionsParent *UserSessions
	CuSum              uint64
	UniqueIdentifier   uint64
	Lock               utils.LavaMutex
	Proof              *pairingtypes.RelayRequest // saves last relay request of a session as proof
	RelayNum           uint64
	PairingEpoch       uint64
}

func (rs *RelaySession) atomicReadRelayNum() uint64 {
	return atomic.LoadUint64(&rs.RelayNum)
}

type subscription struct {
	id                   string
	sub                  *rpcclient.ClientSubscription
	subscribeRepliesChan chan interface{}
}

// TODO Perform payment stuff here
func (s *subscription) disconnect() {
	s.sub.Unsubscribe()
}

func (r *RelaySession) GetPairingEpoch() uint64 {
	return atomic.LoadUint64(&r.PairingEpoch)
}

func (r *RelaySession) SetPairingEpoch(epoch uint64) {
	atomic.StoreUint64(&r.PairingEpoch, epoch)
}

type voteData struct {
	RelayDataHash []byte
	Nonce         int64
	CommitHash    []byte
}
type relayServer struct {
	pairingtypes.UnimplementedRelayerServer
}

func askForRewards(staleEpochHeight int64) {
	g_askForRewards_mutex.Lock()
	defer g_askForRewards_mutex.Unlock()
	staleEpochs := []uint64{uint64(staleEpochHeight)}
	g_rewardsSessions_mutex.Lock()
	if len(g_rewardsSessions) > sentry.StaleEpochDistance+1 {
		utils.LavaFormatWarning("Some epochs were not rewarded, catching up and asking for rewards...", nil, &map[string]string{
			"requested epoch":      strconv.FormatInt(staleEpochHeight, 10),
			"provider block":       strconv.FormatInt(g_sentry.GetBlockHeight(), 10),
			"rewards to claim len": strconv.FormatInt(int64(len(g_rewardsSessions)), 10),
		})

		// go over all epochs and look for stale unhandled epochs
		for epoch := range g_rewardsSessions {
			if epoch < uint64(staleEpochHeight) {
				staleEpochs = append(staleEpochs, epoch)
			}
		}
	}
	g_rewardsSessions_mutex.Unlock()

	relays := []*pairingtypes.RelayRequest{}
	reliability := false
	sessionsToDelete := make([]*RelaySession, 0)

	for _, staleEpoch := range staleEpochs {
		g_rewardsSessions_mutex.Lock()
		staleEpochSessions, ok := g_rewardsSessions[staleEpoch]
		g_rewardsSessions_mutex.Unlock()
		if !ok {
			continue
		}

		for _, session := range staleEpochSessions {
			session.Lock.Lock() // TODO:: is it ok to lock session without g_sessions_mutex?
			if session.Proof == nil {
				// this can happen if the data reliability created a session, we dont save a proof on data reliability message

				if session.UniqueIdentifier != 0 {
					utils.LavaFormatError("Missing proof, cannot get rewards for this session, deleting it", nil, &map[string]string{
						"UniqueIdentifier": strconv.FormatUint(session.UniqueIdentifier, 10),
					})
				}
				session.Lock.Unlock()
				continue
			}

			relay := session.Proof
			relays = append(relays, relay)
			sessionsToDelete = append(sessionsToDelete, session)

			userSessions := session.userSessionsParent
			session.Lock.Unlock()
			userSessions.Lock.Lock()
			userAccAddr, err := sdk.AccAddressFromBech32(userSessions.user)
			if err != nil {
				utils.LavaFormatError("get rewards invalid user address", err, &map[string]string{
					"address": userSessions.user,
				})
			}

			userSessionsEpochData, ok := userSessions.dataByEpoch[staleEpoch]
			if !ok {
				utils.LavaFormatError("get rewards Missing epoch data for this user", err, &map[string]string{
					"address":         userSessions.user,
					"requested epoch": strconv.FormatUint(staleEpoch, 10),
				})
				userSessions.Lock.Unlock()
				continue
			}

			if relay.BlockHeight != int64(staleEpoch) {
				utils.LavaFormatError("relay proof is under incorrect epoch in relay rewards", err, &map[string]string{
					"relay epoch":     strconv.FormatInt(relay.BlockHeight, 10),
					"requested epoch": strconv.FormatUint(staleEpoch, 10),
				})
			}

			if userSessionsEpochData.DataReliability != nil {
				relay.DataReliability = userSessionsEpochData.DataReliability
				userSessionsEpochData.DataReliability = nil
				reliability = true
			}
			userSessions.Lock.Unlock()

			g_sentry.AddExpectedPayment(sentry.PaymentRequest{CU: relay.CuSum, BlockHeightDeadline: relay.BlockHeight, Amount: sdk.Coin{}, Client: userAccAddr, UniqueIdentifier: relay.SessionId})
			g_sentry.UpdateCUServiced(relay.CuSum)
		}

		g_rewardsSessions_mutex.Lock()
		delete(g_rewardsSessions, staleEpoch) // All rewards handles for that epoch
		g_rewardsSessions_mutex.Unlock()
	}

	userSessionObjsToDelete := make([]string, 0)
	for _, session := range sessionsToDelete {
		session.Lock.Lock()
		userSessions := session.userSessionsParent
		sessionID := session.UniqueIdentifier
		session.Lock.Unlock()
		userSessions.Lock.Lock()
		delete(userSessions.Sessions, sessionID)
		if len(userSessions.Sessions) == 0 {
			userSessionObjsToDelete = append(userSessionObjsToDelete, userSessions.user)
		}
		userSessions.Lock.Unlock()
	}

	g_sessions_mutex.Lock()
	for _, user := range userSessionObjsToDelete {
		delete(g_sessions, user)
	}
	g_sessions_mutex.Unlock()
	if len(relays) == 0 {
		// no rewards to ask for
		return
	}

	utils.LavaFormatInfo("asking for rewards", &map[string]string{
		"account":     g_sentry.Acc,
		"reliability": fmt.Sprintf("%t", reliability),
	})

	myWriter := bytes.Buffer{}
	hasSequenceError := false
	success := false
	idx := -1
	sequenceNumberParsed := 0
	summarizedTransactionResult := ""
	for ; idx < RETRY_INCORRECT_SEQUENCE && !success; idx++ {
		msg := pairingtypes.NewMsgRelayPayment(g_sentry.Acc, relays, strconv.FormatUint(g_serverID, 10))
		g_sentry.ClientCtx.Output = &myWriter
		if hasSequenceError { // a retry
			// if sequence number error happened it means that we already sent a tx this block.
			// we need to wait a block for the tx to be approved,
			// only then we can ask for a new sequence number continue and try again.
			var seq uint64
			if sequenceNumberParsed != 0 {
				utils.LavaFormatInfo("Sequence Number extracted from transaction error, retrying", &map[string]string{"sequence": strconv.Itoa(sequenceNumberParsed)})
				seq = uint64(sequenceNumberParsed)
			} else {
				var err error
				_, seq, err = g_sentry.ClientCtx.AccountRetriever.GetAccountNumberSequence(g_sentry.ClientCtx, g_sentry.ClientCtx.GetFromAddress())
				if err != nil {
					utils.LavaFormatError("failed to get correct sequence number for account, give up", err, nil)
					break // give up
				}
			}
			g_txFactory = g_txFactory.WithSequence(seq)
			myWriter.Reset()
			utils.LavaFormatInfo("Retrying with sequence number:", &map[string]string{
				"SeqNum": strconv.FormatUint(seq, 10),
			})
		}
		var transactionResult string
		err := sentry.CheckProfitabilityAndBroadCastTx(g_sentry.ClientCtx, g_txFactory, msg)
		if err != nil {
			utils.LavaFormatWarning("Sending CheckProfitabilityAndBroadCastTx failed", err, &map[string]string{
				"msg": fmt.Sprintf("%+v", msg),
			})
			transactionResult = err.Error() // incase we got an error the tx result is basically the error
		} else {
			transactionResult = myWriter.String()
		}

		var returnCode int
		summarizedTransactionResult, returnCode = parseTransactionResult(transactionResult)

		if returnCode == 0 { // if we get some other code which isn't 0 then keep retrying
			success = true
		} else if strings.Contains(transactionResult, "account sequence") {
			hasSequenceError = true
			sequenceNumberParsed, err = findSequenceNumber(transactionResult)
			if err != nil {
				utils.LavaFormatWarning("Failed findSequenceNumber", err, &map[string]string{"sequence": transactionResult})
			}
			summarizedTransactionResult = transactionResult
		}
	}

	if !success {
		utils.LavaFormatError(fmt.Sprintf("askForRewards ERROR, transaction results: \n%s\n", summarizedTransactionResult), nil, nil)
	} else {
		utils.LavaFormatInfo(fmt.Sprintf("askForRewards SUCCESS!, transaction results: %s\n", summarizedTransactionResult), nil)
	}
}

// extract requested sequence number from tx error.
func findSequenceNumber(sequence string) (int, error) {
	re := regexp.MustCompile(`expected (\d+), got (\d+)`)
	match := re.FindStringSubmatch(sequence)
	if match == nil || len(match) < 2 {
		return 0, utils.LavaFormatWarning("Failed to parse sequence number from error", nil, &map[string]string{"sequence": sequence})
	}
	return strconv.Atoi(match[1]) // atoi return 0 upon error, so it will be ok when sequenceNumberParsed uses it
}

func parseTransactionResult(transactionResult string) (string, int) {
	transactionResult = strings.ReplaceAll(transactionResult, ": ", ":")
	transactionResults := strings.Split(transactionResult, "\n")
	summarizedResult := ""
	for _, str := range transactionResults {
		if strings.Contains(str, "raw_log:") || strings.Contains(str, "txhash:") || strings.Contains(str, "code:") {
			summarizedResult = summarizedResult + str + ", "
		}
	}

	re := regexp.MustCompile(`code:(\d+)`) // extracting code from transaction result (in format code:%d)
	match := re.FindStringSubmatch(transactionResult)
	if match == nil || len(match) < 2 {
		return summarizedResult, 1 // not zero
	}
	retCode, err := strconv.Atoi(match[1]) // extract return code.
	if err != nil {
		return summarizedResult, 1 // not zero
	}
	return summarizedResult, retCode
}

func getRelayUser(in *pairingtypes.RelayRequest) (tenderbytes.HexBytes, error) {
	pubKey, err := sigs.RecoverPubKeyFromRelay(*in)
	if err != nil {
		return nil, err
	}

	return pubKey.Address(), nil
}

func isSupportedSpec(in *pairingtypes.RelayRequest) bool {
	return in.ChainID == g_serverChainID
}

func validateRequestedBlockHeight(blockHeight uint64) bool {
	return (blockHeight == g_sentry.GetCurrentEpochHeight() || blockHeight == g_sentry.GetPrevEpochHeight())
}

func getOrCreateSession(ctx context.Context, userAddr string, req *pairingtypes.RelayRequest) (*RelaySession, error) {
	userSessions := getOrCreateUserSessions(userAddr)

	userSessions.Lock.Lock()
	if userSessions.IsBlockListed {
		userSessions.Lock.Unlock()
		return nil, utils.LavaFormatError("User blocklisted!", nil, &map[string]string{
			"userAddr": userAddr,
		})
	}

	var sessionEpoch uint64
	session, ok := userSessions.Sessions[req.SessionId]
	userSessions.Lock.Unlock()

	if !ok {
		vrf_pk, maxcuRes, err := g_sentry.GetVrfPkAndMaxCuForUser(ctx, userAddr, req.ChainID, req.BlockHeight)
		if err != nil {
			return nil, utils.LavaFormatError("failed to get the Max allowed compute units for the user!", err, &map[string]string{
				"userAddr": userAddr,
			})
		}

		isValidBlockHeight := validateRequestedBlockHeight(uint64(req.BlockHeight))
		if !isValidBlockHeight {
			return nil, utils.LavaFormatError("User requested with invalid block height", err, &map[string]string{
				"req.BlockHeight": strconv.FormatInt(req.BlockHeight, 10),
			})
		}

		sessionEpoch = uint64(req.BlockHeight)

		userSessions.Lock.Lock()
		session = &RelaySession{userSessionsParent: userSessions, RelayNum: 0, UniqueIdentifier: req.SessionId, PairingEpoch: sessionEpoch}
		utils.LavaFormatInfo("new session for user", &map[string]string{
			"userAddr":            userAddr,
			"created for epoch":   strconv.FormatUint(sessionEpoch, 10),
			"request blockheight": strconv.FormatInt(req.BlockHeight, 10),
			"req.SessionId":       strconv.FormatUint(req.SessionId, 10),
		})
		userSessions.Sessions[req.SessionId] = session
		getOrCreateDataByEpoch(userSessions, sessionEpoch, maxcuRes, vrf_pk, userAddr)
		userSessions.Lock.Unlock()

		g_rewardsSessions_mutex.Lock()
		if _, ok := g_rewardsSessions[sessionEpoch]; !ok {
			g_rewardsSessions[sessionEpoch] = make([]*RelaySession, 0)
		}
		g_rewardsSessions[sessionEpoch] = append(g_rewardsSessions[sessionEpoch], session)
		g_rewardsSessions_mutex.Unlock()
	}

	return session, nil
}

// Must lock UserSessions before using this func
func getOrCreateDataByEpoch(userSessions *UserSessions, sessionEpoch uint64, maxcuRes uint64, vrf_pk *utils.VrfPubKey, userAddr string) *UserSessionsEpochData {
	if _, ok := userSessions.dataByEpoch[sessionEpoch]; !ok {
		userSessions.dataByEpoch[sessionEpoch] = &UserSessionsEpochData{UsedComputeUnits: 0, MaxComputeUnits: maxcuRes, VrfPk: *vrf_pk}
		utils.LavaFormatInfo("new user sessions in epoch", &map[string]string{
			"userAddr":          userAddr,
			"maxcuRes":          strconv.FormatUint(maxcuRes, 10),
			"saved under epoch": strconv.FormatUint(sessionEpoch, 10),
			"sentry epoch":      strconv.FormatUint(g_sentry.GetCurrentEpochHeight(), 10),
		})
	}
	return userSessions.dataByEpoch[sessionEpoch]
}

func getOrCreateUserSessions(userAddr string) *UserSessions {
	g_sessions_mutex.Lock()
	userSessions, ok := g_sessions[userAddr]
	if !ok {
		userSessions = &UserSessions{dataByEpoch: map[uint64]*UserSessionsEpochData{}, Sessions: map[uint64]*RelaySession{}, user: userAddr, Subs: make(map[string]*subscription)}
		g_sessions[userAddr] = userSessions
	}
	g_sessions_mutex.Unlock()
	return userSessions
}

func updateSessionCu(sess *RelaySession, userSessions *UserSessions, serviceApi *spectypes.ServiceApi, request *pairingtypes.RelayRequest, pairingEpoch uint64) error {
	sess.Lock.Lock()
	relayNum := sess.RelayNum
	cuSum := sess.CuSum
	sess.Lock.Unlock()

	if relayNum+1 != request.RelayNum {
		utils.LavaFormatError("consumer requested incorrect relaynum, expected it to increment by 1", nil, &map[string]string{
			"request.SessionId": strconv.FormatUint(request.SessionId, 10),
			"expected":          strconv.FormatUint(relayNum+1, 10),
			"received":          strconv.FormatUint(request.RelayNum, 10),
		})
	}

	// Check that relaynum gets incremented by user
	if relayNum+1 > request.RelayNum {
		return utils.LavaFormatError("consumer requested a smaller relay num than expected, trying to overwrite past usage", lavasession.SessionOutOfSyncError, &map[string]string{
			"request.SessionId": strconv.FormatUint(request.SessionId, 10),
			"expected":          strconv.FormatUint(relayNum+1, 10),
			"received":          strconv.FormatUint(request.RelayNum, 10),
		})
	}

	sess.Lock.Lock()
	sess.RelayNum++
	sess.Lock.Unlock()

	// utils.LavaFormatDebug("updateSessionCu", &map[string]string{
	// 	"serviceApi.Name":   serviceApi.Name,
	// 	"request.SessionId": strconv.FormatUint(request.SessionId, 10),
	// })
	//
	// TODO: do we worry about overflow here?
	if cuSum >= request.CuSum {
		return utils.LavaFormatError("bad CU sum", lavasession.SessionOutOfSyncError, &map[string]string{
			"request.SessionId": strconv.FormatUint(request.SessionId, 10),
			"cuSum":             strconv.FormatUint(cuSum, 10),
			"request.CuSum":     strconv.FormatUint(request.CuSum, 10),
		})
	}
	if cuSum+serviceApi.ComputeUnits != request.CuSum {
		return utils.LavaFormatError("bad CU sum", lavasession.SessionOutOfSyncError, &map[string]string{
			"request.SessionId":       strconv.FormatUint(request.SessionId, 10),
			"cuSum":                   strconv.FormatUint(cuSum, 10),
			"request.CuSum":           strconv.FormatUint(request.CuSum, 10),
			"serviceApi.ComputeUnits": strconv.FormatUint(serviceApi.ComputeUnits, 10),
		})
	}

	userSessions.Lock.Lock()
	epochData := userSessions.dataByEpoch[pairingEpoch]

	if epochData.UsedComputeUnits+serviceApi.ComputeUnits > epochData.MaxComputeUnits {
		userSessions.Lock.Unlock()
		return utils.LavaFormatError("client cu overflow", nil, &map[string]string{
			"request.SessionId":          strconv.FormatUint(request.SessionId, 10),
			"epochData.MaxComputeUnits":  strconv.FormatUint(epochData.MaxComputeUnits, 10),
			"epochData.UsedComputeUnits": strconv.FormatUint(epochData.UsedComputeUnits, 10),
			"serviceApi.ComputeUnits":    strconv.FormatUint(request.CuSum, 10),
		})
	}

	epochData.UsedComputeUnits += serviceApi.ComputeUnits
	userSessions.Lock.Unlock()

	sess.Lock.Lock()
	sess.CuSum = request.CuSum
	sess.Lock.Unlock()

	return nil
}

func processUnsubscribeEthereum(subscriptionID string, userSessions *UserSessions) {
	if sub, ok := userSessions.Subs[subscriptionID]; ok {
		sub.disconnect()
		delete(userSessions.Subs, subscriptionID)
	}
}

func processUnsubscribeTendermint(apiName string, subscriptionID string, userSessions *UserSessions) {
	if apiName == "unsubscribe" {
		if sub, ok := userSessions.Subs[subscriptionID]; ok {
			sub.disconnect()
			delete(userSessions.Subs, subscriptionID)
		}
	} else {
		for subscriptionID, sub := range userSessions.Subs {
			sub.disconnect()
			delete(userSessions.Subs, subscriptionID)
		}
	}
}

func processUnsubscribe(apiName string, userAddr sdk.AccAddress, reqParams interface{}) error {
	userSessions := getOrCreateUserSessions(userAddr.String())
	userSessions.Lock.Lock()
	defer userSessions.Lock.Unlock()
	switch p := reqParams.(type) {
	case []interface{}:
		subscriptionID, ok := p[0].(string)
		if !ok {
			return fmt.Errorf("processUnsubscribe - p[0].(string) - type assertion failed, type:" + fmt.Sprintf("%s", p[0]))
		}
		processUnsubscribeEthereum(subscriptionID, userSessions)
	case map[string]interface{}:
		subscriptionID := ""
		if apiName == "unsubscribe" {
			var ok bool
			subscriptionID, ok = p["query"].(string)
			if !ok {
				return fmt.Errorf("processUnsubscribe - p['query'].(string) - type assertion failed, type:" + fmt.Sprintf("%s", p["query"]))
			}
		}
		processUnsubscribeTendermint(apiName, subscriptionID, userSessions)
	}
	return nil
}

func (s *relayServer) initRelay(ctx context.Context, request *pairingtypes.RelayRequest) (sdk.AccAddress, chainproxy.NodeMessage, *UserSessions, *RelaySession, error) {
	// client blockheight can only be at at prev epoch but not earlier
	if request.BlockHeight < int64(g_sentry.GetPrevEpochHeight()) {
		return nil, nil, nil, nil, utils.LavaFormatError("user reported very old lava block height", nil, &map[string]string{
			"current lava block":   strconv.FormatInt(g_sentry.GetBlockHeight(), 10),
			"requested lava block": strconv.FormatInt(request.BlockHeight, 10),
		})
	}

	// Checks
	if g_sentry.Acc != request.Provider {
		return nil, nil, nil, nil, utils.LavaFormatError("User is trying to communicate with the wrong provider address.", nil, &map[string]string{
			"ProviderWhoGotTheRequest": g_sentry.Acc,
			"ProviderInTheRequest":     request.Provider,
		})
	}

	user, err := getRelayUser(request)
	if err != nil {
		return nil, nil, nil, nil, utils.LavaFormatError("get relay user", err, &map[string]string{})
	}
	userAddr, err := sdk.AccAddressFromHex(user.String())
	if err != nil {
		return nil, nil, nil, nil, utils.LavaFormatError("get relay acc address", err, &map[string]string{})
	}

	if !isSupportedSpec(request) {
		return nil, nil, nil, nil, utils.LavaFormatError("spec not supported by server", err, &map[string]string{"request.chainID": request.ChainID, "chainID": g_serverChainID})
	}

	var nodeMsg chainproxy.NodeMessage
	authorizeAndParseMessage := func(ctx context.Context, userAddr sdk.AccAddress, request *pairingtypes.RelayRequest, blockHeightToAuthorize uint64) (*pairingtypes.QueryVerifyPairingResponse, chainproxy.NodeMessage, error) {
		// TODO: cache this client, no need to run the query every time
		authorisedUserResponse, err := g_sentry.IsAuthorizedConsumer(ctx, userAddr.String(), blockHeightToAuthorize)
		if err != nil {
			return nil, nil, utils.LavaFormatError("user not authorized or error occurred", err, &map[string]string{"userAddr": userAddr.String(), "block": strconv.FormatUint(blockHeightToAuthorize, 10), "userRequest": fmt.Sprintf("%+v", request)})
		}
		// Parse message, check valid api, etc
		nodeMsg, err := g_chainProxy.ParseMsg(request.ApiUrl, request.Data, request.ConnectionType)
		if err != nil {
			return nil, nil, utils.LavaFormatError("failed parsing request message", err, &map[string]string{"apiInterface": g_sentry.ApiInterface, "request URL": request.ApiUrl, "request data": string(request.Data), "userAddr": userAddr.String()})
		}
		return authorisedUserResponse, nodeMsg, nil
	}
	var authorisedUserResponse *pairingtypes.QueryVerifyPairingResponse
	authorisedUserResponse, nodeMsg, err = authorizeAndParseMessage(ctx, userAddr, request, uint64(request.BlockHeight))
	if err != nil {
		return nil, nil, nil, nil, utils.LavaFormatError("failed authorizing user request", err, nil)
	}
	var relaySession *RelaySession
	var userSessions *UserSessions
	if request.DataReliability != nil {
		if request.RelayNum > lavasession.DataReliabilitySessionId {
			return nil, nil, nil, nil, utils.LavaFormatError("request's relay num is larger than the data reliability session ID", nil, &map[string]string{"relayNum": strconv.FormatUint(request.RelayNum, 10), "DataReliabilitySessionId": strconv.Itoa(lavasession.DataReliabilitySessionId)})
		}
		if request.CuSum != lavasession.DataReliabilityCuSum {
			return nil, nil, nil, nil, utils.LavaFormatError("request's CU sum is not equal to the data reliability CU sum", nil, &map[string]string{"cuSum": strconv.FormatUint(request.CuSum, 10), "DataReliabilityCuSum": strconv.Itoa(lavasession.DataReliabilityCuSum)})
		}
		userSessions = getOrCreateUserSessions(userAddr.String())
		vrf_pk, maxcuRes, err := g_sentry.GetVrfPkAndMaxCuForUser(ctx, userAddr.String(), request.ChainID, request.BlockHeight)
		if err != nil {
			return nil, nil, nil, nil, utils.LavaFormatError("failed to get vrfpk and maxCURes for data reliability!", err, &map[string]string{
				"userAddr": userAddr.String(),
			})
		}

		userSessions.Lock.Lock()
		if epochData, ok := userSessions.dataByEpoch[uint64(request.BlockHeight)]; ok {
			// data reliability message
			if epochData.DataReliability != nil {
				userSessions.Lock.Unlock()
				return nil, nil, nil, nil, utils.LavaFormatError("Simulation: dataReliability can only be used once per client per epoch", nil,
					&map[string]string{"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(), "dataReliability": fmt.Sprintf("%v", epochData.DataReliability)})
			}
		}
		userSessions.Lock.Unlock()
		// data reliability is not session dependant, its always sent with sessionID 0 and if not we don't care
		if vrf_pk == nil {
			return nil, nil, nil, nil, utils.LavaFormatError("dataReliability Triggered with vrf_pk == nil", nil,
				&map[string]string{"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String()})
		}
		// verify the providerSig is ineed a signature by a valid provider on this query
		valid, err := s.VerifyReliabilityAddressSigning(ctx, userAddr, request)
		if err != nil {
			return nil, nil, nil, nil, utils.LavaFormatError("VerifyReliabilityAddressSigning invalid", err,
				&map[string]string{"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(), "dataReliability": fmt.Sprintf("%v", request.DataReliability)})
		}
		if !valid {
			return nil, nil, nil, nil, utils.LavaFormatError("invalid DataReliability Provider signing", nil,
				&map[string]string{"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(), "dataReliability": fmt.Sprintf("%v", request.DataReliability)})
		}
		// verify data reliability fields correspond to the right vrf
		valid = utils.VerifyVrfProof(request, *vrf_pk, uint64(request.BlockHeight))
		if !valid {
			return nil, nil, nil, nil, utils.LavaFormatError("invalid DataReliability fields, VRF wasn't verified with provided proof", nil,
				&map[string]string{"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(), "dataReliability": fmt.Sprintf("%v", request.DataReliability)})
		}

		vrfIndex, vrfErr := utils.GetIndexForVrf(request.DataReliability.VrfValue, uint32(g_sentry.GetProvidersCount()), g_sentry.GetReliabilityThreshold())
		if vrfErr != nil {
			dataReliabilityMarshalled, err := json.Marshal(request.DataReliability)
			if err != nil {
				dataReliabilityMarshalled = []byte{}
			}
			return nil, nil, nil, nil, utils.LavaFormatError("Provider identified vrf value in data reliability request does not meet threshold", vrfErr,
				&map[string]string{
					"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(),
					"dataReliability": string(dataReliabilityMarshalled), "relayEpochStart": strconv.FormatInt(request.BlockHeight, 10),
					"vrfIndex":   strconv.FormatInt(vrfIndex, 10),
					"self Index": strconv.FormatInt(authorisedUserResponse.Index, 10),
				})
		}
		if authorisedUserResponse.Index != vrfIndex {
			dataReliabilityMarshalled, err := json.Marshal(request.DataReliability)
			if err != nil {
				dataReliabilityMarshalled = []byte{}
			}
			return nil, nil, nil, nil, utils.LavaFormatError("Provider identified invalid vrfIndex in data reliability request, the given index and self index are different", nil,
				&map[string]string{
					"requested epoch": strconv.FormatInt(request.BlockHeight, 10), "userAddr": userAddr.String(),
					"dataReliability": string(dataReliabilityMarshalled), "relayEpochStart": strconv.FormatInt(request.BlockHeight, 10),
					"vrfIndex":   strconv.FormatInt(vrfIndex, 10),
					"self Index": strconv.FormatInt(authorisedUserResponse.Index, 10),
				})
		}
		utils.LavaFormatInfo("Simulation: server got valid DataReliability request", nil)

		userSessions.Lock.Lock()
		getOrCreateDataByEpoch(userSessions, uint64(request.BlockHeight), maxcuRes, vrf_pk, userAddr.String())
		userSessions.dataByEpoch[uint64(request.BlockHeight)].DataReliability = request.DataReliability
		userSessions.Lock.Unlock()
	} else {
		relaySession, err = getOrCreateSession(ctx, userAddr.String(), request)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if relaySession == nil {
			return nil, nil, nil, nil, utils.LavaFormatError("getOrCreateSession has a RelaySession nil without an error", nil, nil)
		}
		relaySession.Lock.Lock()
		pairingEpoch := relaySession.GetPairingEpoch()

		if request.BlockHeight != int64(pairingEpoch) {
			relaySession.Lock.Unlock()
			return nil, nil, nil, nil, utils.LavaFormatError("request blockheight mismatch to session epoch", nil,
				&map[string]string{
					"pairingEpoch": strconv.FormatUint(pairingEpoch, 10), "userAddr": userAddr.String(),
					"relay blockheight": strconv.FormatInt(request.BlockHeight, 10),
				})
		}

		userSessions = relaySession.userSessionsParent
		relaySession.Lock.Unlock()

		// Validate
		if request.SessionId == 0 {
			return nil, nil, nil, nil, utils.LavaFormatError("SessionID cannot be 0 for non-data reliability requests", nil,
				&map[string]string{
					"pairingEpoch": strconv.FormatUint(pairingEpoch, 10), "userAddr": userAddr.String(),
					"relay request": fmt.Sprintf("%v", request),
				})
		}
		// Update session
		err = updateSessionCu(relaySession, userSessions, nodeMsg.GetServiceApi(), request, pairingEpoch)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		relaySession.Lock.Lock()

		// Make a shallow copy of relay request and save it as session proof
		relaySession.Proof = request.ShallowCopy()

		relaySession.Lock.Unlock()
	}
	if userSessions == nil {
		return nil, nil, nil, nil, utils.LavaFormatError("relay Init has a nil UserSession", nil, &map[string]string{"userSessions": fmt.Sprintf("%+v", userSessions)})
	}
	return userAddr, nodeMsg, userSessions, relaySession, nil
}

func (s *relayServer) onRelayFailure(userSessions *UserSessions, relaySession *RelaySession, nodeMsg chainproxy.NodeMessage) error {
	if userSessions == nil || relaySession == nil { // verify sessions are not nil
		return utils.LavaFormatError("relayFailure had a UserSession Or RelaySession nil", nil, &map[string]string{"userSessions": fmt.Sprintf("%+v", userSessions), "relaySession": fmt.Sprintf("%+v", relaySession)})
	}
	// deal with relaySession
	computeUnits := nodeMsg.GetServiceApi().ComputeUnits
	relaySession.Lock.Lock()
	pairingEpoch := relaySession.PairingEpoch
	relaySession.RelayNum -= 1
	relaySession.CuSum -= computeUnits
	var retError error
	if int64(relaySession.RelayNum) < 0 || int64(relaySession.CuSum) < 0 { // relayNumber must be greater than zero.
		utils.LavaFormatError("consumer RelayNumber or CuSum are negative values", nil, &map[string]string{
			"RelayNum": strconv.FormatUint(relaySession.RelayNum, 10),
			"CuSum":    strconv.FormatUint(relaySession.CuSum, 10),
		})
		relaySession.RelayNum = 0
		relaySession.CuSum = 0
		retError = lavasession.SessionOutOfSyncError
	}
	relaySession.Lock.Unlock()
	// deal with userSessions
	userSessions.Lock.Lock()
	userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits -= computeUnits
	if int64(userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits) < 0 {
		// if the provider lost sync with the consumer itself, and not just a session. we blockList the consumer.
		userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits = 0
		userSessions.IsBlockListed = true
		retError = utils.LavaFormatError("userSessions Out of sync, Blocking consumer",
			fmt.Errorf("userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits reached negative value"),
			&map[string]string{
				"consumer_address": userSessions.user,
				"userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits": strconv.FormatUint(userSessions.dataByEpoch[pairingEpoch].UsedComputeUnits, 10),
			})
	}
	userSessions.Lock.Unlock()
	return retError
}

func (s *relayServer) handleRelayErrorStatus(err error) error {
	if err == nil {
		return nil
	}
	if lavasession.SessionOutOfSyncError.Is(err) {
		err = status.Error(codes.Code(lavasession.SessionOutOfSyncError.ABCICode()), err.Error())
	}
	return err
}

func (s *relayServer) Relay(ctx context.Context, request *pairingtypes.RelayRequest) (*pairingtypes.RelayReply, error) {
	utils.LavaFormatDebug("Provider got relay request", &map[string]string{
		"request.SessionId":   strconv.FormatUint(request.SessionId, 10),
		"request.relayNumber": strconv.FormatUint(request.RelayNum, 10),
		"request.cu":          strconv.FormatUint(request.CuSum, 10),
	})
	userAddr, nodeMsg, userSessions, relaySession, err := s.initRelay(ctx, request)
	if err != nil {
		return nil, s.handleRelayErrorStatus(err)
	}

	reply, err := s.TryRelay(ctx, request, userAddr, nodeMsg)
	if err != nil && request.DataReliability == nil { // we ignore data reliability because its not checking/adding cu/relaynum.
		// failed to send relay. we need to adjust session state. cuSum and relayNumber.
		relayFailureError := s.onRelayFailure(userSessions, relaySession, nodeMsg)
		if relayFailureError != nil {
			err = sdkerrors.Wrapf(relayFailureError, "On relay failure: "+err.Error())
		}
		utils.LavaFormatError("TryRelay Failed", err, &map[string]string{
			"request.SessionId": strconv.FormatUint(request.SessionId, 10),
			"request.userAddr":  userAddr.String(),
		})
	} else {
		utils.LavaFormatDebug("Provider Finished Relay Successfully", &map[string]string{
			"request.SessionId":   strconv.FormatUint(request.SessionId, 10),
			"request.relayNumber": strconv.FormatUint(request.RelayNum, 10),
		})
	}
	return reply, s.handleRelayErrorStatus(err)
}

func (s *relayServer) TryRelay(ctx context.Context, request *pairingtypes.RelayRequest, userAddr sdk.AccAddress, nodeMsg chainproxy.NodeMessage) (*pairingtypes.RelayReply, error) {
	// Send
	var reqMsg *chainproxy.JsonrpcMessage
	var reqParams interface{}
	switch msg := nodeMsg.GetMsg().(type) {
	case *chainproxy.JsonrpcMessage:
		reqMsg = msg
		reqParams = reqMsg.Params
	default:
		reqMsg = nil
	}
	latestBlock := int64(0)
	finalizedBlockHashes := map[int64]interface{}{}
	var requestedBlockHash []byte = nil
	finalized := false
	if g_sentry.GetSpecDataReliabilityEnabled() {
		// Add latest block and finalized data
		var requestedBlockHashStr string
		var err error
		latestBlock, finalizedBlockHashes, requestedBlockHashStr, err = g_chainSentry.GetLatestBlockData(request.RequestBlock)
		if err != nil {
			return nil, utils.LavaFormatError("Could not guarantee data reliability", err, &map[string]string{"requestedBlock": strconv.FormatInt(request.RequestBlock, 10), "latestBlock": strconv.FormatInt(latestBlock, 10)})
		}
		if requestedBlockHashStr == "" {
			// avoid using cache, but can still service
			utils.LavaFormatWarning("no hash data for requested block", nil, &map[string]string{"requestedBlock": strconv.FormatInt(request.RequestBlock, 10), "latestBlock": strconv.FormatInt(latestBlock, 10)})
		} else {
			requestedBlockHash = []byte(requestedBlockHashStr)
		}
		request.RequestBlock = sentry.ReplaceRequestedBlock(request.RequestBlock, latestBlock)

		// TODO: uncomment when we add chain tracker
		// if request.RequestBlock > latestBlock {
		// 	// consumer asked for a block that is newer than our state tracker, we cant sign this for DR
		// 	return nil, utils.LavaFormatError("Requested a block that is too new", err, &map[string]string{"requestedBlock": strconv.FormatInt(request.RequestBlock, 10), "latestBlock": strconv.FormatInt(latestBlock, 10)})
		// }

		finalized = g_sentry.IsFinalizedBlock(request.RequestBlock, latestBlock)
	}
	cache := g_chainProxy.GetCache()
	// TODO: handle cache on fork for dataReliability = false
	var reply *pairingtypes.RelayReply = nil
	var err error = nil
	if requestedBlockHash != nil || finalized {
		reply, err = cache.GetEntry(ctx, request, g_sentry.ApiInterface, requestedBlockHash, g_sentry.ChainID, finalized)
	}
	if err != nil || reply == nil {
		if err != nil && performance.NotConnectedError.Is(err) {
			utils.LavaFormatWarning("cache not connected", err, nil)
		}
		// cache miss or invalid
		reply, _, _, err = nodeMsg.Send(ctx, nil)
		if err != nil {
			return nil, utils.LavaFormatError("Sending nodeMsg failed", err, nil)
		}
		if requestedBlockHash != nil || finalized {
			err := cache.SetEntry(ctx, request, g_sentry.ApiInterface, requestedBlockHash, g_sentry.ChainID, userAddr.String(), reply, finalized)
			if err != nil && !performance.NotInitialisedError.Is(err) {
				utils.LavaFormatWarning("error updating cache with new entry", err, nil)
			}
		}
	}

	apiName := nodeMsg.GetServiceApi().Name
	if reqMsg != nil && strings.Contains(apiName, "unsubscribe") {
		err := processUnsubscribe(apiName, userAddr, reqParams)
		if err != nil {
			return nil, err
		}
	}
	// TODO: verify that the consumer still listens, if it took to much time to get the response we cant update the CU.

	jsonStr, err := json.Marshal(finalizedBlockHashes)
	if err != nil {
		return nil, utils.LavaFormatError("failed unmarshaling finalizedBlockHashes", err,
			&map[string]string{"finalizedBlockHashes": fmt.Sprintf("%v", finalizedBlockHashes)})
	}

	reply.FinalizedBlocksHashes = jsonStr
	reply.LatestBlock = latestBlock

	getSignaturesFromRequest := func(request pairingtypes.RelayRequest) error {
		// request is a copy of the original request, but won't modify it
		// update relay request requestedBlock to the provided one in case it was arbitrary
		sentry.UpdateRequestedBlock(&request, reply)
		// Update signature,
		sig, err := sigs.SignRelayResponse(g_privKey, reply, &request)
		if err != nil {
			return utils.LavaFormatError("failed signing relay response", err,
				&map[string]string{"request": fmt.Sprintf("%v", request), "reply": fmt.Sprintf("%v", reply)})
		}
		reply.Sig = sig

		if g_sentry.GetSpecDataReliabilityEnabled() {
			// update sig blocks signature
			sigBlocks, err := sigs.SignResponseFinalizationData(g_privKey, reply, &request, userAddr)
			if err != nil {
				return utils.LavaFormatError("failed signing finalization data", err,
					&map[string]string{"request": fmt.Sprintf("%v", request), "reply": fmt.Sprintf("%v", reply), "userAddr": userAddr.String()})
			}
			reply.SigBlocks = sigBlocks
		}
		return nil
	}
	err = getSignaturesFromRequest(*request)
	if err != nil {
		return nil, err
	}

	// return reply to user
	return reply, nil
}

func (s *relayServer) RelaySubscribe(request *pairingtypes.RelayRequest, srv pairingtypes.Relayer_RelaySubscribeServer) error {
	utils.LavaFormatInfo("Provider got relay request subscribe", &map[string]string{
		"request.SessionId": strconv.FormatUint(request.SessionId, 10),
	})
	_, nodeMsg, userSessions, relaySession, err := s.initRelay(context.Background(), request)
	if err != nil {
		return err
	}

	err = s.TryRelaySubscribe(request, srv, nodeMsg, userSessions)
	if err != nil && request.DataReliability == nil { // we ignore data reliability because its not checking/adding cu/relaynum.
		// failed to send relay. we need to adjust session state. cuSum and relayNumber.
		relayFailureError := s.onRelayFailure(userSessions, relaySession, nodeMsg)
		if relayFailureError != nil {
			err = sdkerrors.Wrapf(relayFailureError, "Relay Error: "+err.Error())
		}
	}
	return err
}

func (s *relayServer) TryRelaySubscribe(request *pairingtypes.RelayRequest, srv pairingtypes.Relayer_RelaySubscribeServer, nodeMsg chainproxy.NodeMessage, userSessions *UserSessions) error {
	var reply *pairingtypes.RelayReply
	var clientSub *rpcclient.ClientSubscription
	var subscriptionID string
	subscribeRepliesChan := make(chan interface{})
	reply, subscriptionID, clientSub, err := nodeMsg.Send(context.Background(), subscribeRepliesChan)
	if err != nil {
		return utils.LavaFormatError("Subscription failed", err, nil)
	}

	userSessions.Lock.Lock()
	if _, ok := userSessions.Subs[subscriptionID]; ok {
		return utils.LavaFormatError("SubscriptiodID: "+subscriptionID+"exists", nil, nil)
	}
	userSessions.Subs[subscriptionID] = &subscription{
		id:                   subscriptionID,
		sub:                  clientSub,
		subscribeRepliesChan: subscribeRepliesChan,
	}
	userSessions.Lock.Unlock()

	err = srv.Send(reply) // this reply contains the RPC ID
	if err != nil {
		utils.LavaFormatError("Error getting RPC ID", err, nil)
	}

	for {
		select {
		case <-clientSub.Err():
			utils.LavaFormatError("client sub", err, nil)
			// delete this connection from the subs map
			userSessions.Lock.Lock()
			if sub, ok := userSessions.Subs[subscriptionID]; ok {
				sub.disconnect()
				delete(userSessions.Subs, subscriptionID)
			}
			userSessions.Lock.Unlock()
			return err
		case subscribeReply := <-subscribeRepliesChan:
			data, err := json.Marshal(subscribeReply)
			if err != nil {
				utils.LavaFormatError("client sub unmarshal", err, nil)
				userSessions.Lock.Lock()
				if sub, ok := userSessions.Subs[subscriptionID]; ok {
					sub.disconnect()
					delete(userSessions.Subs, subscriptionID)
				}
				userSessions.Lock.Unlock()
				return err
			}

			err = srv.Send(
				&pairingtypes.RelayReply{
					Data: data,
				},
			)
			if err != nil {
				// usually triggered when client closes connection
				if strings.Contains(err.Error(), "Canceled desc = context canceled") {
					utils.LavaFormatWarning("Client closed connection", err, nil)
				} else {
					utils.LavaFormatError("srv.Send", err, nil)
				}
				userSessions.Lock.Lock()
				if sub, ok := userSessions.Subs[subscriptionID]; ok {
					sub.disconnect()
					delete(userSessions.Subs, subscriptionID)
				}
				userSessions.Lock.Unlock()
				return err
			}

			utils.LavaFormatInfo("Sending data", &map[string]string{"data": string(data)})
		}
	}
}

func (relayServ *relayServer) VerifyReliabilityAddressSigning(ctx context.Context, consumer sdk.AccAddress, request *pairingtypes.RelayRequest) (valid bool, err error) {
	queryHash := utils.CalculateQueryHash(*request)
	if !bytes.Equal(queryHash, request.DataReliability.QueryHash) {
		return false, utils.LavaFormatError("query hash mismatch on data reliability message", nil,
			&map[string]string{"queryHash": string(queryHash), "request QueryHash": string(request.DataReliability.QueryHash)})
	}

	// validate consumer signing on VRF data
	valid, err = sigs.ValidateSignerOnVRFData(consumer, *request.DataReliability)
	if err != nil {
		return false, utils.LavaFormatError("failed to Validate Signer On VRF Data", err,
			&map[string]string{"consumer": consumer.String(), "request.DataReliability": fmt.Sprintf("%v", request.DataReliability)})
	}
	if !valid {
		return false, nil
	}
	// validate provider signing on query data
	pubKey, err := sigs.RecoverProviderPubKeyFromVrfDataAndQuery(request)
	if err != nil {
		return false, utils.LavaFormatError("failed to Recover Provider PubKey From Vrf Data And Query", err,
			&map[string]string{"consumer": consumer.String(), "request": fmt.Sprintf("%v", request)})
	}
	providerAccAddress, err := sdk.AccAddressFromHex(pubKey.Address().String()) // consumer signer
	if err != nil {
		return false, utils.LavaFormatError("failed converting signer to address", err,
			&map[string]string{"consumer": consumer.String(), "PubKey": pubKey.Address().String()})
	}
	return g_sentry.IsAuthorizedPairing(ctx, consumer.String(), providerAccAddress.String(), uint64(request.BlockHeight)) // return if this pairing is authorised
}

func SendVoteCommitment(voteID string, vote *voteData) {
	msg := conflicttypes.NewMsgConflictVoteCommit(g_sentry.Acc, voteID, vote.CommitHash)
	myWriter := bytes.Buffer{}
	g_sentry.ClientCtx.Output = &myWriter
	err := tx.GenerateOrBroadcastTxWithFactory(g_sentry.ClientCtx, g_txFactory, msg)
	if err != nil {
		utils.LavaFormatError("failed to send vote commitment", err, nil)
	}
}

func SendVoteReveal(voteID string, vote *voteData) {
	msg := conflicttypes.NewMsgConflictVoteReveal(g_sentry.Acc, voteID, vote.Nonce, vote.RelayDataHash)
	myWriter := bytes.Buffer{}
	g_sentry.ClientCtx.Output = &myWriter
	err := tx.GenerateOrBroadcastTxWithFactory(g_sentry.ClientCtx, g_txFactory, msg)
	if err != nil {
		utils.LavaFormatError("failed to send vote Reveal", err, nil)
	}
}

func voteEventHandler(ctx context.Context, voteID string, voteDeadline uint64, voteParams *sentry.VoteParams) {
	// got a vote event, handle the cases here

	if !voteParams.GetCloseVote() {
		// meaning we dont close a vote, so we should check stuff
		if voteParams != nil {
			// chainID is sent only on new votes
			chainID := voteParams.ChainID
			if chainID != g_serverChainID {
				// not our chain ID
				return
			}
		}
		nodeHeight := uint64(g_sentry.GetBlockHeight())
		if voteDeadline < nodeHeight {
			// its too late to vote
			utils.LavaFormatError("Vote Event received but it's too late to vote", nil,
				&map[string]string{"deadline": strconv.FormatUint(voteDeadline, 10), "nodeHeight": strconv.FormatUint(nodeHeight, 10)})
			return
		}
	}
	g_votes_mutex.Lock()
	defer g_votes_mutex.Unlock()
	vote, ok := g_votes[voteID]
	if ok {
		// we have an existing vote with this ID
		if voteParams != nil {
			if voteParams.GetCloseVote() {
				// we are closing the vote, so its okay we have this voteID
				utils.LavaFormatInfo("Received Vote termination event for vote, cleared entry",
					&map[string]string{"voteID": voteID})
				delete(g_votes, voteID)
				return
			}
			// expected to start a new vote but found an existing one
			utils.LavaFormatError("new vote Request for vote had existing entry", nil,
				&map[string]string{"voteParams": fmt.Sprintf("%+v", voteParams), "voteID": voteID, "voteData": fmt.Sprintf("%+v", vote)})
			return
		}
		utils.LavaFormatInfo(" Received Vote Reveal for vote, sending Reveal for result",
			&map[string]string{"voteID": voteID, "voteData": fmt.Sprintf("%+v", vote)})
		SendVoteReveal(voteID, vote)
		return
	} else {
		// new vote
		if voteParams == nil {
			utils.LavaFormatError("vote reveal Request didn't have a vote entry", nil,
				&map[string]string{"voteID": voteID})
			return
		}
		if voteParams.GetCloseVote() {
			utils.LavaFormatError("vote closing received but didn't have a vote entry", nil,
				&map[string]string{"voteID": voteID})
			return
		}
		// try to find this provider in the jury
		found := slices.Contains(voteParams.Voters, g_sentry.Acc)
		if !found {
			utils.LavaFormatInfo("new vote initiated but not for this provider to vote", nil)
			// this is a new vote but not for us
			return
		}
		// we need to send a commit, first we need to use the chainProxy and get the response
		// TODO: implement code that verified the requested block is finalized and if its not waits and tries again
		nodeMsg, err := g_chainProxy.ParseMsg(voteParams.ApiURL, voteParams.RequestData, voteParams.ConnectionType)
		if err != nil {
			utils.LavaFormatError("vote Request did not pass the api check on chain proxy", err,
				&map[string]string{"voteID": voteID, "chainID": voteParams.ChainID})
			return
		}
		reply, _, _, err := nodeMsg.Send(ctx, nil)
		if err != nil {
			utils.LavaFormatError("vote relay send has failed", err,
				&map[string]string{"ApiURL": voteParams.ApiURL, "RequestData": string(voteParams.RequestData)})
			return
		}
		nonce := rand.Int63()
		replyDataHash := sigs.HashMsg(reply.Data)
		commitHash := conflicttypes.CommitVoteData(nonce, replyDataHash)

		vote = &voteData{RelayDataHash: replyDataHash, Nonce: nonce, CommitHash: commitHash}
		g_votes[voteID] = vote
		utils.LavaFormatInfo("Received Vote start, sending commitment for result", &map[string]string{"voteID": voteID, "voteData": fmt.Sprintf("%+v", vote)})
		SendVoteCommitment(voteID, vote)
		return
	}
}

func Server(
	ctx context.Context,
	clientCtx client.Context,
	txFactory tx.Factory,
	listenAddr string,
	nodeUrl string,
	chainID string,
	apiInterface string,
	flagSet *pflag.FlagSet,
) {
	utils.LavaFormatInfo("lavad Binary Version: "+version.Version, nil)
	//
	// ctrl+c
	ctx, cancel := context.WithCancel(ctx)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	defer func() {
		signal.Stop(signalChan)
		cancel()
	}()

	// Init random seed
	rand.Seed(time.Now().UnixNano())
	g_serverID = uint64(rand.Int63())

	//

	// Start newSentry
	newSentry := sentry.NewSentry(clientCtx, txFactory, chainID, false, voteEventHandler, askForRewards, apiInterface, nil, flagSet, g_serverID)
	err := newSentry.Init(ctx)
	if err != nil {
		utils.LavaFormatError("sentry init failure to initialize", err, &map[string]string{"apiInterface": apiInterface, "ChainID": chainID})
		return
	}
	go newSentry.Start(ctx)
	for newSentry.GetSpecHash() == nil {
		time.Sleep(1 * time.Second)
	}
	g_sentry = newSentry
	g_sessions = map[string]*UserSessions{}
	g_votes = map[string]*voteData{}
	g_rewardsSessions = map[uint64][]*RelaySession{}
	g_serverChainID = chainID
	// allow more gas
	g_txFactory = txFactory.WithGas(1000000)

	//
	// Info
	utils.LavaFormatInfo("Server starting", &map[string]string{"listenAddr": listenAddr, "ChainID": newSentry.GetChainID(), "node": nodeUrl, "spec": newSentry.GetSpecName(), "api Interface": apiInterface})

	//
	// Keys
	keyName, err := sigs.GetKeyName(clientCtx)
	if err != nil {
		utils.LavaFormatFatal("provider failure to getKeyName", err, &map[string]string{"apiInterface": apiInterface, "ChainID": chainID})
	}

	privKey, err := sigs.GetPrivKey(clientCtx, keyName)
	if err != nil {
		utils.LavaFormatFatal("provider failure to getPrivKey", err, &map[string]string{"apiInterface": apiInterface, "ChainID": chainID})
	}
	g_privKey = privKey
	serverKey, _ := clientCtx.Keyring.Key(keyName)
	utils.LavaFormatInfo("Server loaded keys", &map[string]string{"PublicKey": serverKey.GetPubKey().Address().String()})
	//
	// Node
	// get portal logs
	pLogs, err := chainproxy.NewPortalLogs()
	if err != nil {
		utils.LavaFormatFatal("provider failure to NewPortalLogs", err, &map[string]string{"apiInterface": apiInterface, "ChainID": chainID})
	}
	numberOfNodeParallelConnections, err := flagSet.GetUint(chainproxy.ParallelConnectionsFlag)
	if err != nil {
		utils.LavaFormatFatal("error fetching chainproxy.ParallelConnectionsFlag", err, nil)
	}

	chainProxy, err := chainproxy.GetChainProxy(nodeUrl, numberOfNodeParallelConnections, newSentry, pLogs)
	if err != nil {
		utils.LavaFormatFatal("provider failure to GetChainProxy", err, &map[string]string{"apiInterface": apiInterface, "ChainID": chainID})
	}
	chainProxy.Start(ctx)
	g_chainProxy = chainProxy

	if g_sentry.GetSpecDataReliabilityEnabled() {
		// Start chain sentry
		chainSentry := chainsentry.NewChainSentry(clientCtx, chainProxy, chainID)
		var chainSentryInitError error
		errMapInfo := &map[string]string{"apiInterface": apiInterface, "ChainID": chainID, "nodeUrl": nodeUrl}
		for attempt := 0; attempt < RetryInitAttempts; attempt++ {
			chainSentryInitError = chainSentry.Init(ctx)
			if chainSentryInitError != nil {
				if chainsentry.ErrorFailedToFetchLatestBlock.Is(chainSentryInitError) { // we allow ErrorFailedToFetchLatestBlock. to retry
					utils.LavaFormatWarning(fmt.Sprintf("chainSentry Init failed. Attempt Number: %d/%d, Retrying in %d seconds",
						attempt+1, RetryInitAttempts, TimeWaitInitializeChainSentry), nil, nil)
					time.Sleep(TimeWaitInitializeChainSentry * time.Second)
					continue
				} else { // other errors are currently fatal.
					utils.LavaFormatFatal("Provider Init failure", chainSentryInitError, errMapInfo)
				}
			}
			// break when chainSentry was initialized successfully
			break
		}
		if chainSentryInitError != nil {
			utils.LavaFormatFatal("provider failure initializing chainSentry - nodeUrl might be unreachable or offline", chainSentryInitError, errMapInfo)
		}

		chainSentry.Start(ctx)
		g_chainSentry = chainSentry
	}

	//
	// GRPC
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		utils.LavaFormatFatal("provider failure setting up listener", err, &map[string]string{"listenAddr": listenAddr, "ChainID": chainID})
	}
	s := grpc.NewServer()

	wrappedServer := grpcweb.WrapServer(s)
	handler := func(resp http.ResponseWriter, req *http.Request) {
		// Set CORS headers
		resp.Header().Set("Access-Control-Allow-Origin", "*")
		resp.Header().Set("Access-Control-Allow-Headers", "Content-Type,x-grpc-web")

		wrappedServer.ServeHTTP(resp, req)
	}

	httpServer := http.Server{
		Handler: h2c.NewHandler(http.HandlerFunc(handler), &http2.Server{}),
	}

	go func() {
		select {
		case <-ctx.Done():
			utils.LavaFormatInfo("Provider Server ctx.Done", nil)
		case <-signalChan:
			utils.LavaFormatInfo("Provider Server signalChan", nil)
		}

		shutdownCtx, shutdownRelease := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownRelease()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			utils.LavaFormatFatal("Provider failed to shutdown", err, &map[string]string{})
		}
	}()

	Server := &relayServer{}

	pairingtypes.RegisterRelayerServer(s, Server)

	cacheAddr, err := flagSet.GetString(performance.CacheFlagName)
	if err != nil {
		utils.LavaFormatError("Failed To Get Cache Address flag", err, &map[string]string{"flags": fmt.Sprintf("%v", flagSet)})
	} else if cacheAddr != "" {
		cache, err := performance.InitCache(ctx, cacheAddr)
		if err != nil {
			utils.LavaFormatError("Failed To Connect to cache at address", err, &map[string]string{"address": cacheAddr})
		} else {
			utils.LavaFormatInfo("cache service connected", &map[string]string{"address": cacheAddr})
			chainProxy.SetCache(cache)
		}
	}

	utils.LavaFormatInfo("Server listening", &map[string]string{"Address": lis.Addr().String()})
	// serve is blocking, until terminated
	if err := httpServer.Serve(lis); !errors.Is(err, http.ErrServerClosed) {
		utils.LavaFormatFatal("provider failed to serve", err, &map[string]string{"Address": lis.Addr().String(), "ChainID": chainID})
	}
	// in case we stop serving, claim rewards
	askForRewards(int64(g_sentry.GetCurrentEpochHeight()))
}

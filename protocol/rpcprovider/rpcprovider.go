package rpcprovider

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/lavanet/lava/protocol/chainlib"
	"github.com/lavanet/lava/protocol/chaintracker"
	"github.com/lavanet/lava/protocol/rpcprovider/reliabilitymanager"
	"github.com/lavanet/lava/protocol/rpcprovider/rewardserver"
	"github.com/lavanet/lava/protocol/statetracker"
	"github.com/lavanet/lava/relayer/lavasession"
	"github.com/lavanet/lava/relayer/performance"
	"github.com/lavanet/lava/relayer/sigs"
	"github.com/lavanet/lava/utils"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	"github.com/spf13/viper"
)

const (
	EndpointsConfigName       = "endpoints"
	ChainTrackerDefaultMemory = 100
)

var (
	Yaml_config_properties = []string{"network-address", "chain-id", "api-interface", "node-url"}
	NumFieldsInConfig      = len(Yaml_config_properties)
)

type ProviderStateTrackerInf interface {
	RegisterChainParserForSpecUpdates(ctx context.Context, chainParser chainlib.ChainParser)
	RegisterReliabilityManagerForVoteUpdates(ctx context.Context, reliabilityManager *reliabilitymanager.ReliabilityManager)
	RegisterForEpochUpdates(ctx context.Context, epochUpdatable statetracker.EpochUpdatable)
	QueryVerifyPairing(ctx context.Context, consumer string, blockHeight uint64)
	TxRelayPayment(ctx context.Context, relayRequests []*pairingtypes.RelayRequest)
}

type RPCProvider struct {
	providerStateTracker ProviderStateTrackerInf
	rpcProviderServers   map[string]*RPCProviderServer
}

func (rpcp *RPCProvider) Start(ctx context.Context, txFactory tx.Factory, clientCtx client.Context, rpcProviderEndpoints []*lavasession.RPCProviderEndpoint, cache *performance.Cache, parallelConnections uint) (err error) {
	// single state tracker
	providerStateTracker := statetracker.ProviderStateTracker{}
	rpcp.providerStateTracker, err = providerStateTracker.New(ctx, txFactory, clientCtx)
	if err != nil {
		return err
	}
	rpcp.rpcProviderServers = make(map[string]*RPCProviderServer, len(rpcProviderEndpoints))
	// single reward server
	rewardServer := rewardserver.NewRewardServer(&providerStateTracker)

	keyName, err := sigs.GetKeyName(clientCtx)
	if err != nil {
		utils.LavaFormatFatal("failed getting key name from clientCtx", err, nil)
	}
	privKey, err := sigs.GetPrivKey(clientCtx, keyName)
	if err != nil {
		utils.LavaFormatFatal("failed getting private key from key name", err, &map[string]string{"keyName": keyName})
	}
	clientKey, _ := clientCtx.Keyring.Key(keyName)

	var addr sdk.AccAddress
	err = addr.Unmarshal(clientKey.GetPubKey().Address())
	if err != nil {
		utils.LavaFormatFatal("failed unmarshaling public address", err, &map[string]string{"keyName": keyName, "pubkey": clientKey.GetPubKey().Address().String()})
	}
	utils.LavaFormatInfo("RPCProvider pubkey: "+addr.String(), nil)
	utils.LavaFormatInfo("RPCProvider setting up endpoints", &map[string]string{"length": strconv.Itoa(len(rpcProviderEndpoints))})
	for _, rpcProviderEndpoint := range rpcProviderEndpoints {
		providerSessionManager := lavasession.NewProviderSessionManager(rpcProviderEndpoint, &providerStateTracker)
		key := rpcProviderEndpoint.Key()
		rpcp.providerStateTracker.RegisterForEpochUpdates(ctx, providerSessionManager)
		chainParser, err := chainlib.NewChainParser(rpcProviderEndpoint.ApiInterface)
		if err != nil {
			return err
		}
		providerStateTracker.RegisterChainParserForSpecUpdates(ctx, chainParser)

		chainProxy, err := chainlib.GetChainProxy(ctx, parallelConnections, rpcProviderEndpoint)
		if err != nil {
			utils.LavaFormatFatal("failed creating chain proxy", err, &map[string]string{"parallelConnections": strconv.FormatUint(uint64(parallelConnections), 10), "rpcProviderEndpoint": fmt.Sprintf("%+v", rpcProviderEndpoint)})
		}

		_, avergaeBlockTime, blocksToFinalization, blocksInFinalizationData := chainParser.ChainBlockStats()
		blocksToSaveChainTracker := uint64(blocksToFinalization + blocksInFinalizationData)
		chainTrackerConfig := chaintracker.ChainTrackerConfig{
			ServerAddress:     rpcProviderEndpoint.NodeUrl,
			BlocksToSave:      blocksToSaveChainTracker,
			AverageBlockTime:  avergaeBlockTime, // divide here to make the querying more often so we don't miss block changes by that much
			ServerBlockMemory: ChainTrackerDefaultMemory + blocksToSaveChainTracker,
		}
		chainFetcher := chainlib.NewChainFetcher(ctx, chainProxy)
		chainTracker := chaintracker.New(ctx, chainFetcher, chainTrackerConfig)
		reliabilityManager := reliabilitymanager.NewReliabilityManager(chainTracker)
		providerStateTracker.RegisterReliabilityManagerForVoteUpdates(ctx, reliabilityManager)

		rpcp.rpcProviderServers[key] = &RPCProviderServer{}
		utils.LavaFormatInfo("RPCProvider Listening", &map[string]string{"endpoints": lavasession.PrintRPCProviderEndpoint(rpcProviderEndpoint)})
		rpcp.rpcProviderServers[key].ServeRPCRequests(ctx, rpcProviderEndpoint, chainParser, rewardServer, providerSessionManager, reliabilityManager, privKey, cache, chainProxy)
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	<-signalChan
	return nil
}

func ParseEndpoints(viper_endpoints *viper.Viper, geolocation uint64) (endpoints []*lavasession.RPCProviderEndpoint, err error) {
	err = viper_endpoints.UnmarshalKey(EndpointsConfigName, &endpoints)
	if err != nil {
		utils.LavaFormatFatal("could not unmarshal endpoints", err, &map[string]string{"viper_endpoints": fmt.Sprintf("%v", viper_endpoints.AllSettings())})
	}
	for _, endpoint := range endpoints {
		endpoint.Geolocation = geolocation
	}
	return
}

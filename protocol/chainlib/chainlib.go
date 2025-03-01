package chainlib

import (
	"context"
	"fmt"
	"time"

	"github.com/lavanet/lava/protocol/chainlib/chainproxy/rpcclient"
	"github.com/lavanet/lava/protocol/common"
	"github.com/lavanet/lava/relayer/lavasession"
	"github.com/lavanet/lava/relayer/metrics"
	"github.com/lavanet/lava/relayer/parser"
	pairingtypes "github.com/lavanet/lava/x/pairing/types"
	spectypes "github.com/lavanet/lava/x/spec/types"
)

const (
	DefaultTimeout           = 5 * time.Second
	TimePerCU                = uint64(100 * time.Millisecond)
	MinimumTimePerRelayDelay = time.Second
	AverageWorldLatency      = 200 * time.Millisecond
)

func NewChainParser(apiInterface string) (chainParser ChainParser, err error) {
	switch apiInterface {
	case spectypes.APIInterfaceJsonRPC:
		return NewJrpcChainParser()
	case spectypes.APIInterfaceTendermintRPC:
		return NewTendermintRpcChainParser()
	case spectypes.APIInterfaceRest:
		return NewRestChainParser()
	case spectypes.APIInterfaceGrpc:
		return NewGrpcChainParser()
	}
	return nil, fmt.Errorf("chainParser for apiInterface (%s) not found", apiInterface)
}

func NewChainListener(ctx context.Context, listenEndpoint *lavasession.RPCEndpoint, relaySender RelaySender, rpcConsumerLogs *common.RPCConsumerLogs) (ChainListener, error) {
	switch listenEndpoint.ApiInterface {
	case spectypes.APIInterfaceJsonRPC:
		return NewJrpcChainListener(ctx, listenEndpoint, relaySender, rpcConsumerLogs), nil
	case spectypes.APIInterfaceTendermintRPC:
		return NewTendermintRpcChainListener(ctx, listenEndpoint, relaySender, rpcConsumerLogs), nil
	case spectypes.APIInterfaceRest:
		return NewRestChainListener(ctx, listenEndpoint, relaySender, rpcConsumerLogs), nil
	case spectypes.APIInterfaceGrpc:
		return NewGrpcChainListener(ctx, listenEndpoint, relaySender, rpcConsumerLogs), nil
	}
	return nil, fmt.Errorf("chainListener for apiInterface (%s) not found", listenEndpoint.ApiInterface)
}

type ChainParser interface {
	ParseMsg(url string, data []byte, connectionType string) (ChainMessage, error)
	SetSpec(spec spectypes.Spec)
	DataReliabilityParams() (enabled bool, dataReliabilityThreshold uint32)
	ChainBlockStats() (allowedBlockLagForQosSync int64, averageBlockTime time.Duration, blockDistanceForFinalizedData uint32, blocksInFinalizationProof uint32)
}

type ChainMessage interface {
	GetServiceApi() *spectypes.ServiceApi
	GetInterface() *spectypes.ApiInterface
	RequestedBlock() int64
	GetRPCMessage() parser.RPCInput
}

type RelaySender interface {
	SendRelay(
		ctx context.Context,
		url string,
		req string,
		connectionType string,
		dappID string,
		analytics *metrics.RelayMetrics,
	) (*pairingtypes.RelayReply, *pairingtypes.Relayer_RelaySubscribeClient, error)
}

type ChainListener interface {
	Serve(ctx context.Context)
}

type ChainProxy interface {
	SendNodeMsg(ctx context.Context, ch chan interface{}, chainMessage ChainMessage) (relayReply *pairingtypes.RelayReply, subscriptionID string, relayReplyServer *rpcclient.ClientSubscription, err error) // has to be thread safe, reuse code within ParseMsg as common functionality
}

func GetChainProxy(ctx context.Context, nConns uint, rpcProviderEndpoint *lavasession.RPCProviderEndpoint) (ChainProxy, error) {
	switch rpcProviderEndpoint.ApiInterface {
	case spectypes.APIInterfaceJsonRPC:
		return NewJrpcChainProxy(ctx, nConns, rpcProviderEndpoint)
	case spectypes.APIInterfaceTendermintRPC:
		return NewtendermintRpcChainProxy(ctx, nConns, rpcProviderEndpoint)
	case spectypes.APIInterfaceRest:
		return NewRestChainProxy(ctx, nConns, rpcProviderEndpoint)
	case spectypes.APIInterfaceGrpc:
		return NewGrpcChainProxy(ctx, nConns, rpcProviderEndpoint)
	}
	return nil, fmt.Errorf("chain proxy for apiInterface (%s) not found", rpcProviderEndpoint.ApiInterface)
}

func LocalNodeTimePerCu(cu uint64) time.Duration {
	return time.Duration(cu * TimePerCU)
}

package chainproxy

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/lavanet/lava/protocol/chainlib/chainproxy/rpcclient"
	"github.com/lavanet/lava/relayer/parser"
	"github.com/lavanet/lava/utils"
	tenderminttypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
)

type TendermintrpcMessage struct {
	JsonrpcMessage
	Path string
}

func (cp TendermintrpcMessage) GetParams() interface{} {
	return cp.Params
}

func (cp TendermintrpcMessage) GetResult() json.RawMessage {
	return cp.Result
}

func (cp TendermintrpcMessage) ParseBlock(inp string) (int64, error) {
	return parser.ParseDefaultBlockParameter(inp)
}

func GetTendermintRPCError(jsonError *rpcclient.JsonError) (*tenderminttypes.RPCError, error) {
	var rpcError *tenderminttypes.RPCError
	if jsonError != nil {
		errData, ok := (jsonError.Data).(string)
		if !ok {
			return nil, utils.LavaFormatError("(rpcMsg.Error.Data).(string) conversion failed", nil, &map[string]string{"data": fmt.Sprintf("%v", jsonError.Data)})
		}
		rpcError = &tenderminttypes.RPCError{
			Code:    jsonError.Code,
			Message: jsonError.Message,
			Data:    errData,
		}
	}
	return rpcError, nil
}

func ConvertErrorToRPCError(err error) *tenderminttypes.RPCError {
	var rpcError *tenderminttypes.RPCError
	unmarshalError := json.Unmarshal([]byte(err.Error()), &rpcError)
	if unmarshalError != nil {
		utils.LavaFormatWarning("Failed unmarshalling error tendermintrpc", unmarshalError, &map[string]string{"err": err.Error()})
		rpcError = &tenderminttypes.RPCError{
			Code:    -1, // TODO get code from error
			Message: "Rpc Error",
			Data:    err.Error(),
		}
	}
	return rpcError
}

type jsonrpcId interface {
	isJSONRPCID()
}

// JSONRPCStringID a wrapper for JSON-RPC string IDs
type JSONRPCStringID string

func (JSONRPCStringID) isJSONRPCID()      {}
func (id JSONRPCStringID) String() string { return string(id) }

// JSONRPCIntID a wrapper for JSON-RPC integer IDs
type JSONRPCIntID int

func (JSONRPCIntID) isJSONRPCID()      {}
func (id JSONRPCIntID) String() string { return fmt.Sprintf("%d", id) }

func IdFromRawMessage(rawID json.RawMessage) (jsonrpcId, error) {
	var idInterface interface{}
	err := json.Unmarshal(rawID, &idInterface)
	if err != nil {
		return nil, utils.LavaFormatError("failed to unmarshal id from response", err, &map[string]string{"id": fmt.Sprintf("%v", rawID)})
	}

	switch id := idInterface.(type) {
	case string:
		return JSONRPCStringID(id), nil
	case float64:
		// json.Unmarshal uses float64 for all numbers
		return JSONRPCIntID(int(id)), nil
	default:
		typ := reflect.TypeOf(id)
		return nil, utils.LavaFormatError("failed to unmarshal id not a string or float", err, &map[string]string{"id": fmt.Sprintf("%v", rawID), "id type": fmt.Sprintf("%v", typ)})
	}
}

type RPCResponse struct {
	JSONRPC string                    `json:"jsonrpc"`
	ID      jsonrpcId                 `json:"id,omitempty"`
	Result  json.RawMessage           `json:"result,omitempty"`
	Error   *tenderminttypes.RPCError `json:"error,omitempty"`
}

func ConvertTendermintMsg(rpcMsg *rpcclient.JsonrpcMessage) (*RPCResponse, error) {
	// Return an error if the message was not sent
	if rpcMsg == nil {
		return nil, ErrFailedToConvertMessage
	}
	rpcError, err := GetTendermintRPCError(rpcMsg.Error)
	if err != nil {
		return nil, err
	}

	jsonid, err := IdFromRawMessage(rpcMsg.ID)
	if err != nil {
		return nil, err
	}
	msg := &RPCResponse{
		JSONRPC: rpcMsg.Version,
		ID:      jsonid,
		Result:  rpcMsg.Result,
		Error:   rpcError,
	}

	return msg, nil
}

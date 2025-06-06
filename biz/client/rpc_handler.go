package client

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/VaalaCat/frp-panel/conf"
	"github.com/VaalaCat/frp-panel/pb"
	"github.com/VaalaCat/frp-panel/services/app"
	"github.com/VaalaCat/frp-panel/utils/logger"
	"google.golang.org/protobuf/proto"
)

func HandleServerMessage(appInstance app.Application, req *pb.ServerMessage) *pb.ClientMessage {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("\n--------------------\ncatch panic !!! \nhandle server message error: %v, stack: %s\n--------------------\n", err, debug.Stack())
		}
	}()
	c := context.Background()
	logger.Logger(c).Infof("client get a server message, clientId: [%s], event: [%s], sessionId: [%s]", req.GetClientId(), req.GetEvent().String(), req.GetSessionId())
	switch req.Event {
	case pb.Event_EVENT_UPDATE_FRPC:
		return app.WrapperServerMsg(appInstance, req, UpdateFrpcHander)
	case pb.Event_EVENT_REMOVE_FRPC:
		return app.WrapperServerMsg(appInstance, req, RemoveFrpcHandler)
	case pb.Event_EVENT_START_FRPC:
		return app.WrapperServerMsg(appInstance, req, StartFRPCHandler)
	case pb.Event_EVENT_STOP_FRPC:
		return app.WrapperServerMsg(appInstance, req, StopFRPCHandler)
	case pb.Event_EVENT_START_STREAM_LOG:
		return app.WrapperServerMsg(appInstance, req, StartSteamLogHandler)
	case pb.Event_EVENT_STOP_STREAM_LOG:
		return app.WrapperServerMsg(appInstance, req, StopSteamLogHandler)
	case pb.Event_EVENT_START_PTY_CONNECT:
		return app.WrapperServerMsg(appInstance, req, StartPTYConnect)
	case pb.Event_EVENT_GET_PROXY_INFO:
		return app.WrapperServerMsg(appInstance, req, GetProxyConfig)
	case pb.Event_EVENT_CREATE_WORKER:
		return app.WrapperServerMsg(appInstance, req, CreateWorker)
	case pb.Event_EVENT_REMOVE_WORKER:
		return app.WrapperServerMsg(appInstance, req, RemoveWorker)
	case pb.Event_EVENT_GET_WORKER_STATUS:
		return app.WrapperServerMsg(appInstance, req, GetWorkerStatus)
	case pb.Event_EVENT_INSTALL_WORKERD:
		return app.WrapperServerMsg(appInstance, req, InstallWorkerd)
	case pb.Event_EVENT_PING:
		rawData, _ := proto.Marshal(conf.GetVersion().ToProto())
		return &pb.ClientMessage{
			Event: pb.Event_EVENT_PONG,
			Data:  rawData,
		}
	default:
	}

	return &pb.ClientMessage{
		Event: pb.Event_EVENT_ERROR,
		Data:  []byte("unknown event"),
	}
}

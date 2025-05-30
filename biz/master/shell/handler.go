package shell

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/VaalaCat/frp-panel/common"
	"github.com/VaalaCat/frp-panel/pb"
	"github.com/VaalaCat/frp-panel/services/app"
	"github.com/VaalaCat/frp-panel/services/rpc"
	"github.com/VaalaCat/frp-panel/utils/logger"
	"github.com/fatedier/golib/log"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/samber/lo"
	"github.com/sourcegraph/conc"
	"google.golang.org/protobuf/proto"
)

func PTYHandler(appInstance app.Application) func(*gin.Context) {
	return func(ctx *gin.Context) {
		ptyHandler(ctx, appInstance)
	}
}

func ptyHandler(c *gin.Context, appInstance app.Application) {
	connectionErrorLimit := 10
	keepalivePingTimeout := 10 * time.Second

	upgrader := getUpgrader(c)
	webConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Logger(c).WithError(err).Infof("websocket connect error")
		c.JSON(http.StatusBadRequest, common.Err("websocket connect error"))
		return
	}

	clientID := c.Param("clientID")
	if len(clientID) == 0 {
		logger.Logger(c).Errorf("invalid client id")
		webConn.Close()
		return
	}

	var (
		initHeight    = c.Query("height")
		initWidth     = c.Query("width")
		initWidthInt  = 0
		initHeightInt = 0
	)

	if initHeight != "" {
		initHeightInt, err = strconv.Atoi(initHeight)
		if err != nil {
			logger.Logger(c).WithError(err).Infof("invalid height")
			webConn.Close()
			return
		}
	}

	if initWidth != "" {
		initWidthInt, err = strconv.Atoi(initWidth)
		if err != nil {
			logger.Logger(c).WithError(err).Infof("invalid width")
			webConn.Close()
			return
		}
	}

	cliMsg, err := rpc.CallClient(app.NewContext(c, appInstance), clientID, pb.Event_EVENT_START_PTY_CONNECT, &pb.CommonRequest{})
	if err != nil {
		logger.Logger(c).WithError(err).Errorf("start pty connect error")
		webConn.Close()
		return
	}

	commonResp := &pb.CommonResponse{}
	if err := proto.Unmarshal(cliMsg.GetData(), commonResp); err != nil {
		logger.Logger(c).WithError(err).Errorf("cannot unmarshal")
		webConn.Close()
		return
	}

	sessionID := string(commonResp.GetData())

	cliConn, ok := appInstance.GetShellPTYMgr().Load(sessionID)
	if !ok {
		logger.Logger(c).Errorf("cannot get client, session id: [%s]", sessionID)
		c.JSON(http.StatusInternalServerError, common.Err("cannot get client"))
		return
	}

	cliConn.Send(&pb.PTYServerMessage{
		Height: lo.ToPtr(int32(initHeightInt)),
		Width:  lo.ToPtr(int32(initWidthInt)),
	})

	defer func() {
		logger.Logger(c).Info("gracefully stopping spawned tty...")
		if err := cliConn.Send(&pb.PTYServerMessage{Data: []byte("bye!"), Done: true}); err != nil {
			logger.Logger(c).Warnf("failed to send close message: %s", err)
		}

		appInstance.GetShellPTYMgr().SetSessionDone(sessionID)
		if err := webConn.Close(); err != nil {
			logger.Logger(c).Warnf("failed to close webscoket connection: %s", err)
		}
	}()

	var connectionClosed bool
	var wg conc.WaitGroup

	// this is a keep-alive loop that ensures connection does not hang-up itself
	lastPongTime := time.Now()
	webConn.SetPongHandler(func(msg string) error {
		lastPongTime = time.Now()
		return nil
	})

	wg.Go(func() {
		defer func() {
			if err := cliConn.Send(&pb.PTYServerMessage{Data: []byte("bye!"), Done: true}); err != nil {
				logger.Logger(c).Warnf("failed to send close message: %s", err)
			}

			appInstance.GetShellPTYMgr().SetSessionDone(sessionID)
			if err := webConn.Close(); err != nil {
				logger.Logger(c).Warnf("failed to close webscoket connection: %s", err)
			}
		}()
		for {
			if err := webConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
				logger.Logger(c).Warn("failed to write ping message")
				return
			}
			time.Sleep(keepalivePingTimeout / 2)
			if time.Since(lastPongTime) > keepalivePingTimeout {
				logger.Logger(c).Warn("failed to get response from ping, triggering disconnect now...")
				return
			}
			logger.Logger(c).Debug("received response from ping successfully")
		}
	})

	// client >> xterm.js
	wg.Go(func() {
		errorCounter := 0
		for {
			// consider the connection closed/errored out so that the socket handler
			// can be terminated - this frees up memory so the service doesn't get
			// overloaded
			if errorCounter > connectionErrorLimit {
				break
			}
			cliMsg, err := cliConn.Recv()
			if err != nil {
				logger.Logger(c).Warnf("failed to read from client sender: %s", err)
				if err := webConn.WriteMessage(websocket.BinaryMessage, []byte("bye!")); err != nil {
					logger.Logger(c).Warnf("failed to send termination message from client sender to xterm.js: %s", err)
				}
				if err := cliConn.Send(&pb.PTYServerMessage{Data: []byte("bye!"), Done: true}); err != nil {
					logger.Logger(c).Warnf("failed to send termination message from client sender to client: %s", err)
				}
				if err := webConn.Close(); err != nil {
					logger.Logger(c).Warnf("failed to close webscoket connection: %s", err)
				}
				return
			}

			readLength := len(cliMsg.GetData())

			if err := webConn.WriteMessage(websocket.BinaryMessage, []byte(cliMsg.GetData())); err != nil {
				logger.Logger(c).Warnf("failed to send %v bytes from client sender to xterm.js", readLength)
				errorCounter++
				continue
			}
			logger.Logger(c).Tracef("sent message of size %v bytes from client sender to xterm.js", readLength)
			errorCounter = 0
		}
	})

	// client << xterm.js
	wg.Go(func() {
		for {
			// data processing
			messageType, data, err := webConn.ReadMessage()
			if err != nil {
				if !connectionClosed {
					logger.Logger(c).Warnf("failed to get next reader: %s", err)
				}
				if err := cliConn.Send(&pb.PTYServerMessage{Data: []byte("bye!"), Done: true}); err != nil {
					logger.Logger(c).Warnf("failed to send termination message from xterm.js to client: %s", err)
				}
				if err := webConn.Close(); err != nil {
					logger.Logger(c).Warnf("failed to close webscoket connection: %s", err)
				}
				return
			}
			payload := struct {
				Data   *string `json:"data,omitempty"`
				Height *uint16 `json:"height,omitempty"`
				Width  *uint16 `json:"width,omitempty"`
			}{}
			json.Unmarshal(data, &payload)

			msg := &pb.PTYServerMessage{}
			if payload.Data != nil {
				msg.Data = []byte(*payload.Data)
			}
			if payload.Height != nil {
				msg.Height = lo.ToPtr(int32(*payload.Height))
			}
			if payload.Width != nil {
				msg.Width = lo.ToPtr(int32(*payload.Width))
			}

			err = cliConn.Send(msg)
			if err != nil {
				logger.Logger(c).Warn(fmt.Sprintf("failed to write bytes to client: %s", err))
				continue
			}
			logger.Logger(c).Tracef("messageType [%v] bytes written to client...", messageType)
		}
	})

	wg.Wait()
	log.Info("closing conn...")
	connectionClosed = true
}

func getUpgrader(c *gin.Context) websocket.Upgrader {
	return websocket.Upgrader{
		// cross origin domain
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		// 处理 Sec-WebSocket-Protocol Header
		Subprotocols: []string{c.GetHeader("Sec-WebSocket-Protocol")},
	}
}

package shared

import (
	"context"
	"crypto/tls"
	"embed"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	bizcommon "github.com/VaalaCat/frp-panel/biz/common"
	bizmaster "github.com/VaalaCat/frp-panel/biz/master"
	"github.com/VaalaCat/frp-panel/biz/master/shell"
	"github.com/VaalaCat/frp-panel/biz/master/streamlog"
	bizserver "github.com/VaalaCat/frp-panel/biz/server"
	"github.com/VaalaCat/frp-panel/conf"
	"github.com/VaalaCat/frp-panel/defs"
	"github.com/VaalaCat/frp-panel/models"
	"github.com/VaalaCat/frp-panel/pb"
	"github.com/VaalaCat/frp-panel/services/api"
	"github.com/VaalaCat/frp-panel/services/app"
	"github.com/VaalaCat/frp-panel/services/dao"
	"github.com/VaalaCat/frp-panel/services/master"
	"github.com/VaalaCat/frp-panel/services/mux"
	"github.com/VaalaCat/frp-panel/services/rbac"
	"github.com/VaalaCat/frp-panel/services/rpc"
	"github.com/VaalaCat/frp-panel/services/watcher"
	"github.com/VaalaCat/frp-panel/services/workerd"
	"github.com/VaalaCat/frp-panel/utils"
	"github.com/VaalaCat/frp-panel/utils/logger"
	"github.com/VaalaCat/frp-panel/utils/wsgrpc"
	"github.com/casbin/casbin/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/websocket"
	"go.uber.org/fx"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func NewLogHookManager() app.StreamLogHookMgr {
	return &bizcommon.HookMgr{}
}

func NewPTYManager() app.ShellPTYMgr {
	return shell.NewPTYMgr()
}

func NewBaseApp(param struct {
	fx.In

	Cfg     conf.Config `name:"originConfig"`
	CliMgr  app.ClientsManager
	HookMgr app.StreamLogHookMgr
	PtyMgr  app.ShellPTYMgr
}) app.Application {
	appInstance := app.NewApp()
	appInstance.SetConfig(param.Cfg)
	appInstance.SetClientsManager(param.CliMgr)
	appInstance.SetStreamLogHookMgr(param.HookMgr)
	appInstance.SetShellPTYMgr(param.PtyMgr)
	appInstance.SetClientRecvMap(&sync.Map{})
	return appInstance
}

func NewClientsManager() app.ClientsManager {
	return rpc.NewClientsManager()
}

func NewPatchedConfig(param struct {
	fx.In

	AppInstance app.Application
	CommonArgs  CommonArgs
}) conf.Config {
	patchedCfg := patchConfig(param.AppInstance, param.CommonArgs)
	param.AppInstance.SetConfig(patchedCfg)

	return patchedCfg
}

func NewContext(appInstance app.Application) *app.Context {
	return app.NewContext(context.Background(), appInstance)
}

func NewClientLogManager() app.ClientLogManager {
	return streamlog.NewClientLogManager()
}

func NewDBManager(ctx *app.Context, appInstance app.Application) app.DBManager {
	logger.Logger(ctx).Infof("start to init database, type: %s", appInstance.GetConfig().DB.Type)
	mgr := models.NewDBManager(appInstance.GetConfig().DB.Type)
	appInstance.SetDBManager(mgr)

	if appInstance.GetConfig().IsDebug {
		appInstance.GetDBManager().SetDebug(true)
	}

	switch appInstance.GetConfig().DB.Type {
	case defs.DBTypeSQLite3:
		if err := utils.EnsureDirectoryExists(appInstance.GetConfig().DB.DSN); err != nil {
			logger.Logger(ctx).WithError(err).Warnf("ensure directory failed, data location: [%s], keep data in current directory",
				appInstance.GetConfig().DB.DSN)
			tmpCfg := appInstance.GetConfig()
			tmpCfg.DB.DSN = filepath.Base(appInstance.GetConfig().DB.DSN)
			appInstance.SetConfig(tmpCfg)
			logger.Logger(ctx).Infof("new data location: [%s]", appInstance.GetConfig().DB.DSN)
		}

		if sqlitedb, err := gorm.Open(sqlite.Open(appInstance.GetConfig().DB.DSN), &gorm.Config{}); err != nil {
			logger.Logger(ctx).Panic(err)
		} else {
			appInstance.GetDBManager().SetDB(defs.DBTypeSQLite3, defs.DBRoleDefault, sqlitedb)
			logger.Logger(ctx).Infof("init database success, data location: [%s]", appInstance.GetConfig().DB.DSN)
		}
	case defs.DBTypeMysql:
		if mysqlDB, err := gorm.Open(mysql.Open(appInstance.GetConfig().DB.DSN), &gorm.Config{}); err != nil {
			logger.Logger(ctx).Panic(err)
		} else {
			appInstance.GetDBManager().SetDB(defs.DBTypeMysql, defs.DBRoleDefault, mysqlDB)
			logger.Logger(ctx).Infof("init database success, data type: [%s]", "mysql")
		}
	case defs.DBTypePostgres:
		if postgresDB, err := gorm.Open(postgres.Open(appInstance.GetConfig().DB.DSN), &gorm.Config{}); err != nil {
			logger.Logger(ctx).Panic(err)
		} else {
			appInstance.GetDBManager().SetDB(defs.DBTypePostgres, defs.DBRoleDefault, postgresDB)
			logger.Logger(ctx).Infof("init database success, data type: [%s]", "postgres")
		}
	default:
		logger.Logger(ctx).Panicf("currently unsupported database type: %s", appInstance.GetConfig().DB.Type)
	}

	memoryDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		logger.Logger(ctx).Panic(err)
	}
	appInstance.GetDBManager().SetDB(defs.DBTypeSQLite3, defs.DBRoleRam, memoryDB)
	logger.Logger(ctx).Infof("init memory database success")

	appInstance.GetDBManager().Init()
	return mgr
}

func NewMasterTLSConfig(ctx *app.Context) *tls.Config {
	return dao.NewQuery(ctx).InitCert(conf.GetCertTemplate(ctx.GetApp().GetConfig()))
}

func NewMasterRouter(fs embed.FS, appInstance app.Application) *gin.Engine {
	return bizmaster.NewRouter(fs, appInstance)
}

func NewListenerOptions(ctx *app.Context, cfg conf.Config) conf.LisOpt {
	return conf.GetListener(ctx, cfg)
}

func NewTLSMasterService(appInstance app.Application, masterTLSConfig *tls.Config) master.MasterService {
	return master.NewMasterService(appInstance, credentials.NewTLS(masterTLSConfig))
}

func NewHTTPMasterService(appInstance app.Application) master.MasterService {
	return master.NewMasterService(appInstance, insecure.NewCredentials())
}

func NewServerMasterCli(appInstance app.Application) app.MasterClient {
	return rpc.NewMasterCli(appInstance)
}

func NewClientMasterCli(appInstance app.Application) app.MasterClient {
	return rpc.NewMasterCli(appInstance)
}

func NewMux(param struct {
	fx.In

	MasterService master.MasterService `name:"tlsMasterService"`
	Router        *gin.Engine          `name:"masterRouter"`
	LisOpt        conf.LisOpt
	TLSCfg        *tls.Config
}) mux.MuxServer {
	return mux.NewMux(param.MasterService.GetServer(), param.Router, param.LisOpt.MuxLis, param.TLSCfg)
}

func NewHTTPMux(param struct {
	fx.In

	MasterService master.MasterService `name:"httpMasterService"`
	Router        *gin.Engine          `name:"masterRouter"`
	LisOpt        conf.LisOpt
}) mux.MuxServer {
	return mux.NewMux(param.MasterService.GetServer(), param.Router, param.LisOpt.ApiLis, nil)
}

func NewWatcher() watcher.Client {
	return watcher.NewClient()
}

func NewWSListener(ctx *app.Context, cfg conf.Config) *wsgrpc.WSListener {
	return wsgrpc.NewWSListener("ws-listener", "wsgrpc", 100)
}

func NewWSGrpcHandler(ctx *app.Context, ws *wsgrpc.WSListener, upgrader *websocket.Upgrader) gin.HandlerFunc {
	return wsgrpc.GinWSHandler(ws, upgrader)
}

func NewWSUpgrader(ctx *app.Context, cfg conf.Config) *websocket.Upgrader {
	return &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
}

func NewServerRouter(appInstance app.Application) *gin.Engine {
	return bizserver.NewRouter(appInstance)
}

func NewServerAPI(param struct {
	fx.In
	Ctx          *app.Context
	ServerRouter *gin.Engine `name:"serverRouter"`
}) app.Service {
	l, err := net.Listen("tcp", conf.ServerAPIListenAddr(param.Ctx.GetApp().GetConfig()))
	if err != nil {
		logger.Logger(param.Ctx).WithError(err).Fatalf("failed to listen addr: %v", conf.ServerAPIListenAddr(param.Ctx.GetApp().GetConfig()))
		return nil
	}

	return api.NewApiService(l, param.ServerRouter, true)
}

func NewServerCred(appInstance app.Application) credentials.TransportCredentials {
	cfg := appInstance.GetConfig()
	clientID := cfg.Client.ID
	clientSecret := cfg.Client.Secret
	ctx := context.Background()

	cred, err := utils.TLSClientCertNoValidate(rpc.GetClientCert(appInstance, clientID, clientSecret, pb.ClientType_CLIENT_TYPE_FRPS))
	if err != nil {
		logger.Logger(ctx).WithError(err).Fatal("new tls client cert failed")
	}
	logger.Logger(ctx).Infof("new tls server cert success")

	return cred
}

func NewClientCred(appInstance app.Application) credentials.TransportCredentials {
	cfg := appInstance.GetConfig()
	clientID := cfg.Client.ID
	clientSecret := cfg.Client.Secret
	ctx := context.Background()

	cred, err := utils.TLSClientCertNoValidate(rpc.GetClientCert(appInstance, clientID, clientSecret, pb.ClientType_CLIENT_TYPE_FRPC))
	if err != nil {
		logger.Logger(ctx).WithError(err).Fatal("new tls client cert failed")
	}
	logger.Logger(ctx).Infof("new tls client cert success")

	return cred
}

func NewDefaultServerConfig(ctx *app.Context) conf.Config {
	appInstance := ctx.GetApp()

	logger.Logger(ctx).Infof("init default internal server")

	dao.NewQuery(ctx).InitDefaultServer(appInstance.GetConfig().Master.APIHost)
	defaultServer, err := dao.NewQuery(ctx).GetDefaultServer()

	if err != nil {
		logger.Logger(ctx).WithError(err).Fatal("get default server failed")
	}

	tmpCfg := appInstance.GetConfig()
	tmpCfg.Client.ID = defaultServer.ServerID
	tmpCfg.Client.Secret = defaultServer.ConnectSecret
	appInstance.SetConfig(tmpCfg)

	return tmpCfg
}

const splitter = "\n--------------------------------------------\n"

func NewConfigPrinter(param struct {
	fx.In

	Ctx    *app.Context
	Config conf.Config
}) {
	var (
		ctx    = param.Ctx
		config = param.Config
	)
	logger.Logger(ctx).Infof("%srunning config is: %s%s", splitter, config.PrintStr(), splitter)
	logger.Logger(ctx).Infof("%scurrent version: \n%s%s", splitter, conf.GetVersion().String(), splitter)
}

func NewAutoJoin(param struct {
	fx.In

	Role       defs.AppRole
	Ctx        *app.Context
	Cfg        conf.Config `name:"argsPatchedConfig"`
	CommonArgs CommonArgs
}) conf.Config {
	var (
		ctx          = param.Ctx
		clientID     = param.Cfg.Client.ID
		clientSecret = param.Cfg.Client.Secret
		autoJoin     = false
		appInstance  = param.Ctx.GetApp()
	)

	appInstance.SetConfig(param.Cfg)

	if param.Role != defs.AppRole_Client {
		return param.Cfg
	}

	// 用户不输入clientID和clientSecret时，使用autoJoin
	if len(clientSecret) == 0 || len(clientID) == 0 {
		if param.CommonArgs.JoinToken != nil && len(*param.CommonArgs.JoinToken) > 0 {
			autoJoin = true
		} else {
			if len(clientSecret) == 0 {
				logger.Logger(ctx).Fatal("client secret cannot be empty")
			}

			if len(clientID) == 0 {
				logger.Logger(ctx).Fatal("client id cannot be empty")
			}
		}
	}

	if autoJoin {
		logger.Logger(ctx).Infof("start to try join master, clientID: [%s], clientSecret: [%s]", clientID, clientSecret)
		cli, err := JoinMaster(param.Cfg, param.CommonArgs)
		if err != nil {
			logger.Logger(ctx).Fatalf("join master failed: %s", err.Error())
		}
		logger.Logger(ctx).Infof("join master success, clientID: [%s], clientInfo: [%s]", cli.GetId(), cli.String())
		tmpCfg := appInstance.GetConfig()
		tmpCfg.Client.ID = cli.GetId()
		tmpCfg.Client.Secret = cli.GetSecret()
		appInstance.SetConfig(tmpCfg)
	}
	return appInstance.GetConfig()
}

func NewPermissionManager(param struct {
	fx.In

	Enforcer    *casbin.Enforcer
	AppInstance app.Application
}) app.PermissionManager {
	permMgr := rbac.NewPermManager(param.Enforcer)
	param.AppInstance.SetPermManager(permMgr)
	return permMgr
}

func NewEnforcer(param struct {
	fx.In

	Ctx         *app.Context
	DBmanager   app.DBManager
	AppInstance app.Application
}) *casbin.Enforcer {
	e, err := rbac.InitializeCasbin(param.Ctx, param.DBmanager.GetDefaultDB())
	if err != nil {
		logger.Logger(param.Ctx).WithError(err).Fatal("initialize casbin failed")
	}
	param.AppInstance.SetEnforcer(e)
	return e
}

func NewWorkersManager(lx fx.Lifecycle, mgr app.WorkerExecManager, appInstance app.Application) app.WorkersManager {
	if !appInstance.GetConfig().Client.Features.EnableFunctions {
		return nil
	}

	workerMgr := workerd.NewWorkersManager()
	appInstance.SetWorkersManager(workerMgr)

	lx.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			workerMgr.StopAllWorkers(app.NewContext(ctx, appInstance))
			logger.Logger(ctx).Info("stop all workers")
			return nil
		},
	})

	return workerMgr
}

func NewWorkerExecManager(cfg conf.Config, appInstance app.Application) app.WorkerExecManager {
	if !appInstance.GetConfig().Client.Features.EnableFunctions {
		return nil
	}

	workerdBinPath := cfg.Client.Worker.WorkerdBinaryPath

	if err := os.MkdirAll(cfg.Client.Worker.WorkerdWorkDir, os.ModePerm); err != nil {
		logger.Logger(context.Background()).WithError(err).Fatalf("create work dir failed, path: [%s]", cfg.Client.Worker.WorkerdWorkDir)
	}

	mgr := workerd.NewExecManager(workerdBinPath,
		[]string{"serve", "--watch", "--verbose"})
	appInstance.SetWorkerExecManager(mgr)
	return mgr
}

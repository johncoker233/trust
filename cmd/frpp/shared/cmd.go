package shared

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/VaalaCat/frp-panel/conf"
	"github.com/VaalaCat/frp-panel/defs"
	"github.com/VaalaCat/frp-panel/pb"
	"github.com/VaalaCat/frp-panel/services/app"
	"github.com/VaalaCat/frp-panel/services/rpc"
	"github.com/VaalaCat/frp-panel/utils"
	"github.com/VaalaCat/frp-panel/utils/logger"
	"github.com/joho/godotenv"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/fx"
)

type CommonArgs struct {
	ClientSecret *string
	ClientID     *string
	RpcUrl       *string
	ApiUrl       *string

	RpcHost   *string
	ApiHost   *string
	RpcPort   *int
	ApiPort   *int
	ApiScheme *string
	JoinToken *string
}

func BuildCommand(fs embed.FS) *cobra.Command {
	cfg := conf.NewConfig()

	logger.UpdateLoggerOpt(
		cfg.Logger.FRPLoggerLevel,
		cfg.Logger.DefaultLoggerLevel,
	)

	return NewRootCmd(
		NewMasterCmd(cfg, fs),
		NewClientCmd(cfg),
		NewServerCmd(cfg),
		NewJoinCmd(),
		NewInstallServiceCmd(),
		NewUninstallServiceCmd(),
		NewStartServiceCmd(),
		NewStopServiceCmd(),
		NewRestartServiceCmd(),
		NewVersionCmd(),
	)
}

func AddCommonFlags(commonCmd *cobra.Command) {
	commonCmd.Flags().StringP("secret", "s", "", "client secret")
	commonCmd.Flags().StringP("id", "i", "", "client id")
	commonCmd.Flags().String("rpc-url", "", "rpc url, master rpc url, scheme can be grpc/ws/wss://hostname:port")
	commonCmd.Flags().String("api-url", "", "api url, master api url, scheme can be http/https://hostname:port")
	commonCmd.Flags().StringP("join-token", "j", "", "your token from master, auto join with out webui")

	// deprecated start
	commonCmd.Flags().StringP("app", "a", "", "app secret")
	commonCmd.Flags().StringP("rpc-host", "r", "", "deprecated, use --rpc-url instead, rpc host, canbe ip or domain")
	commonCmd.Flags().StringP("api-host", "t", "", "deprecated, use --api-url instead, api host, canbe ip or domain")
	commonCmd.Flags().IntP("rpc-port", "c", 0, "deprecated, use --rpc-url instead, rpc port, master rpc port, scheme is grpc")
	commonCmd.Flags().IntP("api-port", "p", 0, "deprecated, use --api-url instead, api port, master api port, scheme is http/https")
	commonCmd.Flags().StringP("api-scheme", "e", "", "deprecated, use --api-url instead, api scheme, master api scheme, scheme is http/https")
	// deprecated end
}

func GetCommonArgs(cmd *cobra.Command) CommonArgs {
	var commonArgs CommonArgs

	if clientSecret, err := cmd.Flags().GetString("secret"); err == nil {
		commonArgs.ClientSecret = &clientSecret
	}

	if clientID, err := cmd.Flags().GetString("id"); err == nil {
		commonArgs.ClientID = &clientID
	}

	if rpcURL, err := cmd.Flags().GetString("rpc-url"); err == nil {
		commonArgs.RpcUrl = &rpcURL
	}

	if apiURL, err := cmd.Flags().GetString("api-url"); err == nil {
		commonArgs.ApiUrl = &apiURL
	}

	if rpcHost, err := cmd.Flags().GetString("rpc-host"); err == nil {
		commonArgs.RpcHost = &rpcHost
	}

	if apiHost, err := cmd.Flags().GetString("api-host"); err == nil {
		commonArgs.ApiHost = &apiHost
	}

	if rpcPort, err := cmd.Flags().GetInt("rpc-port"); err == nil {
		commonArgs.RpcPort = &rpcPort
	}

	if apiPort, err := cmd.Flags().GetInt("api-port"); err == nil {
		commonArgs.ApiPort = &apiPort
	}

	if apiScheme, err := cmd.Flags().GetString("api-scheme"); err == nil {
		commonArgs.ApiScheme = &apiScheme
	}

	if joinToken, err := cmd.Flags().GetString("join-token"); err == nil {
		commonArgs.JoinToken = &joinToken
	}

	return commonArgs
}

func NewJoinCmd() *cobra.Command {
	joinCmd := &cobra.Command{
		Use:   "join [-j join token] [-r rpc host] [-p api port] [-e api scheme]",
		Short: "join to master with token, save param to config",
		Run: func(cmd *cobra.Command, args []string) {
			ctx := context.Background()
			commonArgs := GetCommonArgs(cmd)

			warnDepParam(cmd)

			cli, err := JoinMaster(conf.NewConfig(), commonArgs)
			if err != nil {
				logger.Logger(ctx).Fatalf("join master failed: %s", err.Error())
			}
			saveConfig(ctx, cli, commonArgs)
		},
	}

	AddCommonFlags(joinCmd)

	return joinCmd
}

func NewMasterCmd(cfg conf.Config, fs embed.FS) *cobra.Command {
	return &cobra.Command{
		Use:   "master",
		Short: "run frp-panel manager",
		Run: func(cmd *cobra.Command, args []string) {

			warnDepParam(cmd)

			opts := []fx.Option{
				commonMod,
				masterMod,
				serverMod,
				fx.Supply(
					CommonArgs{},
					fx.Annotate(cfg, fx.ResultTags(`name:"originConfig"`)),
					fs,
					defs.AppRole_Master,
				),
				fx.Provide(fx.Annotate(NewDefaultServerConfig, fx.ResultTags(`name:"defaultServerConfig"`))),
				fx.Invoke(NewConfigPrinter),
				fx.Invoke(runMaster),
				fx.Invoke(runServer),
			}

			if !cfg.IsDebug {
				opts = append(opts, fx.NopLogger)
			}

			run := func() {
				masterApp := fx.New(opts...)
				masterApp.Run()
				if err := masterApp.Err(); err != nil {
					logger.Logger(context.Background()).Fatalf("masterApp FX Application Error: %v", err)
				}
			}

			if srv, err := utils.CreateSystemService(args, run); err != nil {
				run()
			} else {
				srv.Run()
			}
		},
	}
}

func NewClientCmd(cfg conf.Config) *cobra.Command {
	clientCmd := &cobra.Command{
		Use:   "client [-s client secret] [-i client id] [-a app secret] [-t api host] [-r rpc host] [-c rpc port] [-p api port]",
		Short: "run managed frpc",
		Run: func(cmd *cobra.Command, args []string) {
			commonArgs := GetCommonArgs(cmd)

			warnDepParam(cmd)

			opts := []fx.Option{
				clientMod,
				commonMod,
				fx.Supply(
					commonArgs,
					fx.Annotate(cfg, fx.ResultTags(`name:"originConfig"`)),
					defs.AppRole_Client,
				),
				fx.Invoke(NewConfigPrinter),
				fx.Invoke(runClient),
			}

			if !cfg.IsDebug {
				opts = append(opts, fx.NopLogger)
			}

			run := func() {
				clientApp := fx.New(opts...)
				clientApp.Run()
				if err := clientApp.Err(); err != nil {
					logger.Logger(context.Background()).Fatalf("clientApp FX Application Error: %v", err)
				}
			}
			if srv, err := utils.CreateSystemService(args, run); err != nil {
				run()
			} else {
				srv.Run()
			}
		},
	}

	AddCommonFlags(clientCmd)

	return clientCmd
}

func NewServerCmd(cfg conf.Config) *cobra.Command {
	serverCmd := &cobra.Command{
		Use:   "server [-s client secret] [-i client id] [-a app secret] [-r rpc host] [-c rpc port] [-p api port]",
		Short: "run managed frps",
		Run: func(cmd *cobra.Command, args []string) {
			commonArgs := GetCommonArgs(cmd)

			warnDepParam(cmd)

			opts := []fx.Option{
				serverMod,
				commonMod,
				fx.Supply(
					commonArgs,
					fx.Annotate(cfg, fx.ResultTags(`name:"originConfig"`)),
					defs.AppRole_Server,
				),
				fx.Invoke(runServer),
			}

			if !cfg.IsDebug {
				opts = append(opts, fx.NopLogger)
			}

			run := func() {
				serverApp := fx.New(opts...)
				serverApp.Run()
				if err := serverApp.Err(); err != nil {
					logger.Logger(context.Background()).Fatalf("serverApp FX Application Error: %v", err)
				}
			}
			if srv, err := utils.CreateSystemService(args, run); err != nil {
				run()
			} else {
				srv.Run()
			}
		},
	}

	AddCommonFlags(serverCmd)

	return serverCmd
}

func NewInstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "install",
		Short:                 "install frp-panel as service",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			utils.ControlSystemService(args, "install", func() {})
		},
	}
}

func NewUninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "uninstall",
		Short:                 "uninstall frp-panel service",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			utils.ControlSystemService(args, "uninstall", func() {})
		},
	}
}

func NewStartServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "start",
		Short:                 "start frp-panel service",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			utils.ControlSystemService(args, "start", func() {})
		},
	}
}

func NewStopServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "stop",
		Short:                 "stop frp-panel service",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			utils.ControlSystemService(args, "stop", func() {})
		},
	}
}

func NewRestartServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "restart",
		Short:                 "restart frp-panel service",
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		Run: func(cmd *cobra.Command, args []string) {
			utils.ControlSystemService(args, "restart", func() {})
		},
	}
}

func NewVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version info of frp-panel",
		Long:  `All software has versions. This is frp-panel's`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(conf.GetVersion().String())
		},
	}
}

func patchConfig(appInstance app.Application, commonArgs CommonArgs) conf.Config {
	c := context.Background()
	tmpCfg := appInstance.GetConfig()

	if commonArgs.RpcHost != nil && len(*commonArgs.RpcHost) > 0 {
		tmpCfg.Master.RPCHost = *commonArgs.RpcHost
		tmpCfg.Master.APIHost = *commonArgs.RpcHost
	}

	if commonArgs.ApiHost != nil && len(*commonArgs.RpcHost) > 0 {
		tmpCfg.Master.APIHost = *commonArgs.ApiHost
	}

	if commonArgs.RpcPort != nil && *commonArgs.RpcPort > 0 {
		tmpCfg.Master.RPCPort = *commonArgs.RpcPort
	}
	if commonArgs.ApiPort != nil && *commonArgs.ApiPort > 0 {
		tmpCfg.Master.APIPort = *commonArgs.ApiPort
	}
	if commonArgs.ApiScheme != nil && len(*commonArgs.ApiScheme) > 0 {
		tmpCfg.Master.APIScheme = *commonArgs.ApiScheme
	}
	if commonArgs.ClientID != nil && len(*commonArgs.ClientID) > 0 {
		tmpCfg.Client.ID = *commonArgs.ClientID
	}
	if commonArgs.ClientSecret != nil && len(*commonArgs.ClientSecret) > 0 {
		tmpCfg.Client.Secret = *commonArgs.ClientSecret
	}

	if commonArgs.ApiUrl != nil && len(*commonArgs.ApiUrl) > 0 {
		tmpCfg.Client.APIUrl = *commonArgs.ApiUrl
	}
	if commonArgs.RpcUrl != nil && len(*commonArgs.RpcUrl) > 0 {
		tmpCfg.Client.RPCUrl = *commonArgs.RpcUrl
	}

	if lo.FromPtrOr(commonArgs.RpcPort, 0) != 0 || lo.FromPtrOr(commonArgs.ApiPort, 0) != 0 ||
		lo.FromPtrOr(commonArgs.ApiScheme, "") != "" ||
		lo.FromPtrOr(commonArgs.RpcHost, "") != "" || lo.FromPtrOr(commonArgs.ApiHost, "") != "" {
		logger.Logger(c).Warnf("deprecatedenv configs !!! pls use api url and rpc url \n\n rpc host: %s, rpc port: %d, api host: %s, api port: %d, api scheme: %s, \n\n args: %s",
			tmpCfg.Master.RPCHost, tmpCfg.Master.RPCPort,
			tmpCfg.Master.APIHost, tmpCfg.Master.APIPort,
			tmpCfg.Master.APIScheme, utils.MarshalForJson(tmpCfg))
	} else if len(tmpCfg.Client.APIUrl) > 0 || len(tmpCfg.Client.RPCUrl) > 0 {
		logger.Logger(c).Infof("env config, api url: %s, rpc url: %s", tmpCfg.Client.APIUrl, tmpCfg.Client.RPCUrl)
	}

	return tmpCfg
}

func warnDepParam(cmd *cobra.Command) {
	if appSecret, _ := cmd.Flags().GetString("app"); len(appSecret) != 0 {
		logger.Logger(context.Background()).Errorf(
			"\n⚠️\n\n-a / -app / APP_SECRET 参数已停止使用，请删除该参数重新启动\n\n" +
				"The -a / -app / APP_SECRET parameter is deprecated. Please remove it and restart.\n\n")
	}
}

func SetMasterCommandIfNonePresent(rootCmd *cobra.Command) {
	cmd, _, err := rootCmd.Find(os.Args[1:])
	if err == nil && cmd.Use == rootCmd.Use && cmd.Flags().Parse(os.Args[1:]) != pflag.ErrHelp {
		args := append([]string{"master"}, os.Args[1:]...)
		rootCmd.SetArgs(args)
	}
}

func SetClientCommandIfNonePresent(rootCmd *cobra.Command) {
	cmd, _, err := rootCmd.Find(os.Args[1:])
	if err == nil && cmd.Use == rootCmd.Use && cmd.Flags().Parse(os.Args[1:]) != pflag.ErrHelp {
		args := append([]string{"client"}, os.Args[1:]...)
		rootCmd.SetArgs(args)
	}
}

func JoinMaster(cfg conf.Config, joinArgs CommonArgs) (*pb.Client, error) {
	c := context.Background()
	if err := checkPullParams(joinArgs); err != nil {
		logger.Logger(c).Errorf("check pull params failed: %s", err.Error())
		return nil, err
	}

	var clientID string

	if cliID := joinArgs.ClientID; cliID == nil || len(*cliID) == 0 {
		clientID = utils.GetHostnameWithIP()
	} else {
		clientID = *cliID
	}

	clientID = utils.MakeClientIDPermited(clientID)

	logger.Logger(c).Infof("join master with param, clientId:[%s] joinArgs:[%s]", clientID, utils.MarshalForJson(joinArgs))

	// 检测是否存在已有的client
	clientResp, err := rpc.GetClient(cfg, clientID, *joinArgs.JoinToken)
	if err != nil || clientResp == nil || clientResp.GetStatus().GetCode() != pb.RespCode_RESP_CODE_SUCCESS {
		logger.Logger(c).Infof("client [%s] not found, try to init client", clientID)

		// 创建短期client
		initResp, err := rpc.InitClient(cfg, clientID, *joinArgs.JoinToken, true)
		if err != nil {
			logger.Logger(c).Errorf("init client failed: %s", err.Error())
			return nil, err
		}
		if initResp == nil {
			logger.Logger(c).Errorf("init resp is nil")
			return nil, err
		}
		if initResp.GetStatus().GetCode() != pb.RespCode_RESP_CODE_SUCCESS {
			logger.Logger(c).Errorf("init client failed with status: %s", initResp.GetStatus().GetMessage())
			return nil, err
		}

		clientID = initResp.GetClientId()
		clientResp, err = rpc.GetClient(cfg, clientID, *joinArgs.JoinToken)
		if err != nil {
			logger.Logger(c).Errorf("get client failed: %s", err.Error())
			return nil, err
		}
	}

	if clientResp == nil {
		logger.Logger(c).Errorf("client resp is nil")
		return nil, err
	}
	if clientResp.GetStatus().GetCode() != pb.RespCode_RESP_CODE_SUCCESS {
		logger.Logger(c).Errorf("client resp code is not success: %s", clientResp.GetStatus().GetMessage())
		return nil, err
	}

	client := clientResp.GetClient()
	if client == nil {
		logger.Logger(c).Errorf("client is nil")
		return nil, err
	}

	return client, nil
}

func saveConfig(ctx context.Context, cli *pb.Client, joinArgs CommonArgs) {
	if err := utils.EnsureDirectoryExists(defs.SysEnvPath); err != nil {
		logger.Logger(ctx).Errorf("ensure directory failed: %s", err.Error())
		return
	}

	envMap, err := godotenv.Read(defs.SysEnvPath)
	if err != nil {
		envMap = make(map[string]string)
		logger.Logger(ctx).Warnf("read env file failed, try to create: %s", err.Error())
	}

	envMap[defs.EnvClientID] = cli.GetId()
	envMap[defs.EnvClientSecret] = cli.GetSecret()
	envMap[defs.EnvClientAPIUrl] = *joinArgs.ApiUrl
	envMap[defs.EnvClientRPCUrl] = *joinArgs.RpcUrl

	if err = godotenv.Write(envMap, defs.SysEnvPath); err != nil {
		logger.Logger(ctx).Errorf("write env file failed: %s", err.Error())
		return
	}
	logger.Logger(ctx).Infof("config saved to env file: %s, you can use `frp-panel client` without args to run client,\n\nconfig is: [%v]",
		defs.SysEnvPath, envMap)
}

func checkPullParams(joinArgs CommonArgs) error {
	if joinToken := joinArgs.JoinToken; joinToken != nil && len(*joinToken) == 0 {
		return errors.New("join token is empty")
	}

	var (
		apiUrlAvaliable = joinArgs.ApiUrl != nil && len(*joinArgs.ApiUrl) > 0
		rpcUrlAvaliable = joinArgs.RpcUrl != nil && len(*joinArgs.RpcUrl) > 0
	)

	if !apiUrlAvaliable {
		return errors.New("api url is empty")
	}

	if !rpcUrlAvaliable {
		return errors.New("rpc url is empty")
	}

	return nil
}

func NewRootCmd(cmds ...*cobra.Command) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "frp-panel",
		Short: "frp-panel is a frp panel QwQ",
	}

	rootCmd.AddCommand(cmds...)

	return rootCmd
}

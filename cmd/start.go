package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/FunctionX/cosmos-firewall/config"
	"github.com/FunctionX/cosmos-firewall/internal/handler"
	"github.com/FunctionX/cosmos-firewall/internal/middleware"
	"github.com/FunctionX/cosmos-firewall/internal/node"
	"github.com/FunctionX/cosmos-firewall/internal/types"
	"github.com/FunctionX/cosmos-firewall/logger"
)

func start() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start firewall",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := viper.ReadInConfig(); err != nil {
				return err
			}
			cfg := config.DefaultConfig()
			if err := viper.Unmarshal(cfg); err != nil {
				return err
			}
			if err := cfg.ValidateBasic(); err != nil {
				return err
			}
			return Run(cfg)
		},
	}
	cmd.Flags().String(flagRpcAddress, config.DefaultJSONRPCAddress, "the service rpc address")
	cmd.Flags().String(flagGrpcAddress, config.DefaultGRPCAddress, "the service grpc address")
	cmd.Flags().String(flagRestAddress, config.DefaultRESTAddress, "the service rest address")
	cmd.Flags().Bool(flagForward, false, "the forward flag")
	cmd.Flags().String(flagJsonRpc, "", "the chain json rpc flag")
	cmd.Flags().String(flagGrpc, "", "the chain grpc flag")
	cmd.Flags().String(flagRest, "", "the chain rest flag")
	cmd.Flags().Uint64(flagMinimumGasLimit, config.DefaultMinGasLimit, "the chain minimum gas limit")
	cmd.Flags().String(flagMinimumFee, "", "the chain minimum fee")
	cmd.Flags().Uint64(flagMaxMemo, 256, "the chain max memo")
	cmd.Flags().StringSlice(flagWhiteRouters, []string{""}, "the chain white routers")
	cmd.Flags().Int(flagExtensionOptions, 0, "the chain extension options")
	cmd.Flags().Int(flagNonCriticalExtensionOptions, 0, "the chain non critical extension options")
	cmd.Flags().Int(flagGranter, 0, "the chain granter")
	cmd.Flags().Int(flagPayer, 0, "the chain payer")
	cmd.Flags().Int(flagSignerInfos, 1, "the chain signer infos")
	cmd.Flags().Int(flagMinimumSignatures, 1, "the chain minimum signatures")
	cmd.Flags().StringSlice(flagPublicKeyTypeUrl, []string{""}, "the chain public key type url")
	return cmd
}

func Run(config *config.Config) (err error) {
	var jsonrpcNodes, grpcNodes, restNodes *node.Node
	if config.Redirect.Enable {
		light := config.Redirect.Nodes[string(types.LightNode)]
		fullNode := config.Redirect.Nodes[string(types.FullNode)]
		archiveNode := config.Redirect.Nodes[string(types.ArchiveNode)]
		jsonrpcNodes, err = node.NewJSONRPCNode(light.JSONRPCNode, fullNode.JSONRPCNode, archiveNode.JSONRPCNode, config.Redirect.TimeoutSecond, config.Redirect.CheckNodeSecond)
		if err != nil {
			return err
		}
		grpcNodes, err = node.NewGRPCNode(light.GRPCNode, fullNode.GRPCNode, archiveNode.GRPCNode, config.Redirect.TimeoutSecond, config.Redirect.CheckNodeSecond)
		if err != nil {
			return err
		}
		restNodes, err = node.NewRESTNode(light.RESTNode, fullNode.RESTNode, archiveNode.RESTNode, config.Redirect.TimeoutSecond, config.Redirect.CheckNodeSecond)
		if err != nil {
			return err
		}
	}

	validator := middleware.NewValidator(config)
	ctx, cancelFn := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)
	ListenForQuitSignals(cancelFn)
	g.Go(func() error {
		return RunJSONRPCServer(ctx, validator, jsonrpcNodes)
	})
	g.Go(func() error {
		return RunRESTServer(ctx, validator, restNodes)
	})
	g.Go(func() error {
		return RunGRPCServer(ctx, validator, grpcNodes)
	})
	return g.Wait()
}

func ListenForQuitSignals(cancelFn context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		cancelFn()
		logger.Info("caught signal", "signal", sig.String())
	}()
}

func RunGRPCServer(ctx context.Context, validator middleware.Validator, node *node.Node) error {
	logger.Infof("start GRPC server listening on %v", validator.Cfg.GRPCAddress)
	var director middleware.Director
	if node != nil {
		go func() {
			node.CheckNode()
			ticker := time.NewTicker(time.Duration(node.CheckNodeSecond) * time.Second)
			for range ticker.C {
				node.CheckNode()
			}
		}()
		director = middleware.NewRedirect(node).StreamDirector
	}
	grpcSrv := grpc.NewServer(grpc.CustomCodec(types.Codec()), grpc.UnknownServiceHandler(handler.TransparentHandler(ctx, validator, director))) //nolint:staticcheck
	addr, err := net.Listen("tcp", validator.Cfg.GRPCAddress)
	if err != nil {
		return err
	}
	errCh := make(chan error)
	go func() {
		errCh <- grpcSrv.Serve(addr)
	}()
	select {
	case <-ctx.Done():
		logger.Info("stopping GRPC  server...", "address", validator.Cfg.RestAddress)
		grpcSrv.Stop()
		return nil
	case err = <-errCh:
		logger.Error("failed to start GRPC server", "err", err)
		return err
	}
}

func RunRESTServer(ctx context.Context, validator middleware.Validator, node *node.Node) error {
	logger.Infof("start REST server listening on %v", validator.Cfg.RestAddress)
	var director middleware.Director
	if node != nil {
		go func() {
			node.CheckNode()
			ticker := time.NewTicker(time.Duration(node.CheckNodeSecond) * time.Second)
			for range ticker.C {
				node.CheckNode()
			}
		}()
		director = middleware.NewRedirect(node).HttpDirector
	}
	srv := &http.Server{Addr: validator.Cfg.RestAddress, Handler: handler.RestHandler(ctx, validator, director)}
	errCh := make(chan error)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		logger.Info("stopping REST  server...", "address", validator.Cfg.RestAddress)
		return srv.Shutdown(ctx)
	case err := <-errCh:
		logger.Error("failed to start REST server", "err", err)
		return err
	}
}

func RunJSONRPCServer(ctx context.Context, validator middleware.Validator, node *node.Node) error {
	logger.Infof("start JSON-RPC server listening on %v", validator.Cfg.RPCAddress)
	var director middleware.Director
	if node != nil {
		go func() {
			node.CheckNode()
			ticker := time.NewTicker(time.Duration(node.CheckNodeSecond) * time.Second)
			for range ticker.C {
				node.CheckNode()
			}
		}()
		director = middleware.NewRedirect(node).HttpDirector
	}
	srv := &http.Server{Addr: validator.Cfg.RPCAddress, Handler: handler.JSONRPCHandler(ctx, validator, director)}
	errCh := make(chan error)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		logger.Info("stopping JSON-RPC  server...", "address", validator.Cfg.RPCAddress)
		return srv.Shutdown(ctx)
	case err := <-errCh:
		logger.Error("failed to start JSON-RPC server", "err", err)
		return err
	}
}

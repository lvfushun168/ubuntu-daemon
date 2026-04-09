package app

import (
	"context"
	"log"

	"openclaw/dameon/internal/cloud"
	"openclaw/dameon/internal/config"
	"openclaw/dameon/internal/gateway"
	"openclaw/dameon/internal/imagegen"
	"openclaw/dameon/internal/manager"
	"openclaw/dameon/internal/router"
	"openclaw/dameon/internal/runner"
	"openclaw/dameon/internal/store"
	"openclaw/dameon/internal/video"
)

type App struct {
	cloudClient *cloud.Client
}

func New(cfg *config.Config, logger *log.Logger) (*App, error) {
	stateStore := store.NewFileStore(cfg.Store.StateFile)
	execRunner := runner.NewExecRunner()
	configManager := manager.NewConfigManager(cfg, stateStore, execRunner)
	remoteRunner := runner.NewRemoteCommandRunner(cfg.RemoteCommand, execRunner)
	gatewayAdapter := gateway.NewAdapter(cfg, logger, stateStore)
	videoService := video.NewService(cfg, logger, gatewayAdapter)
	imageService := imagegen.NewService(cfg, logger, gatewayAdapter)

	messageRouter := router.New(logger, nil, configManager, remoteRunner, gatewayAdapter, videoService, imageService, cfg.DaemonVersion)
	cloudClient := cloud.NewClient(cfg, logger, messageRouter)
	messageRouter.SetSender(cloudClient)

	return &App{cloudClient: cloudClient}, nil
}

func (a *App) Run(ctx context.Context) error {
	return a.cloudClient.Run(ctx)
}

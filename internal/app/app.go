package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/rousoftware/asgard/internal/api"
	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/backup"
	"github.com/rousoftware/asgard/internal/config"
	"github.com/rousoftware/asgard/internal/deploy"
	"github.com/rousoftware/asgard/internal/dockerx"
	"github.com/rousoftware/asgard/internal/importer"
	"github.com/rousoftware/asgard/internal/mcpserver"
	"github.com/rousoftware/asgard/internal/networking"
	"github.com/rousoftware/asgard/internal/oauth"
	"github.com/rousoftware/asgard/internal/operations"
	"github.com/rousoftware/asgard/internal/proxy"
	"github.com/rousoftware/asgard/internal/secrets"
	"github.com/rousoftware/asgard/internal/store"
)

type App struct {
	Config     config.Config
	Store      *store.Store
	Auth       *auth.Service
	Docker     *dockerx.Engine
	Networks   *networking.Manager
	Operations *operations.Manager
	API        *api.Server
	collector  *dockerx.Collector
	http       *http.Server
}

func New(cfg config.Config) (*App, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	fail := func(err error) (*App, error) { database.Close(); return nil, err }
	signer, err := auth.LoadOrCreateSigner(filepath.Join(cfg.DataDir, "keys", "jwt_ed25519.pem"), cfg.PublicURL)
	if err != nil {
		return fail(fmt.Errorf("load signing key: %w", err))
	}
	authService := auth.New(database, signer, cfg.PublicURL, cfg.SecureCookies, cfg.AccessTTL, cfg.RefreshTTL)
	engine, err := dockerx.New(cfg.DockerHost)
	if err != nil {
		return fail(fmt.Errorf("create Docker client: %w", err))
	}
	proxyGenerator := &proxy.Generator{Store: database, Dir: cfg.TraefikDynamicDir}
	ops := operations.New(database, cfg.OperationWorkers)
	deployer := &deploy.Deployer{Store: database, Docker: engine, Proxy: proxyGenerator, EdgeNetwork: cfg.EdgeNetwork, DataDir: cfg.DataDir, DataVolume: cfg.DataVolume}
	ops.Register("deployment.create", deployer.Handle)
	ops.Register("deployment.rollback", deployer.Handle)
	backupManager := &backup.Manager{Store: database, Docker: engine, BackupsDir: cfg.BackupsDir, DataVolume: cfg.DataVolume, HelperImage: cfg.HelperImage}
	ops.Register("backup.create", backupManager.HandleCreate)
	ops.Register("backup.restore", backupManager.HandleRestore)
	secretBox, err := secrets.LoadOrCreate(filepath.Join(cfg.DataDir, "keys", "secrets.key"))
	if err != nil {
		return fail(fmt.Errorf("load secret key: %w", err))
	}
	projectImporter := &importer.Importer{Store: database, ProjectsDir: cfg.ProjectsDir, DataDir: cfg.DataDir, Domain: cfg.Domain, Secrets: secretBox}
	networkManager := &networking.Manager{Store: database, Docker: engine, EdgeNetwork: cfg.EdgeNetwork}
	oauthServer := oauth.New(database, authService, cfg.PublicURL)
	mcpServer := mcpserver.New(mcpserver.Dependencies{Config: cfg, Store: database, Docker: engine, Networks: networkManager, Operations: ops, Importer: projectImporter, Proxy: proxyGenerator, Secrets: secretBox})
	server := api.New(api.Dependencies{Config: cfg, Store: database, Auth: authService, Docker: engine, Networks: networkManager, Operations: ops, Importer: projectImporter, Proxy: proxyGenerator, OAuth: oauthServer, MCP: mcpServer.Handler, Secrets: secretBox})
	collector := &dockerx.Collector{Engine: engine, Store: database, Interval: cfg.MetricsInterval}
	return &App{Config: cfg, Store: database, Auth: authService, Docker: engine, Networks: networkManager, Operations: ops, API: server, collector: collector}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := a.Networks.ReconcileAll(ctx); err != nil {
		slog.Warn("shared network reconciliation completed with errors", "error", err)
	}
	if err := a.Operations.Start(ctx); err != nil {
		return err
	}
	a.collector.Start(ctx)
	a.http = &http.Server{Addr: a.Config.ListenAddr, Handler: a.API.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("Asgard listening", "address", a.Config.ListenAddr, "public_url", a.Config.PublicURL)
		errCh <- a.http.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = a.http.Shutdown(shutdownCtx)
		a.collector.Stop()
		a.Operations.Wait()
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (a *App) Close() error { a.collector.Stop(); _ = a.Docker.Close(); return a.Store.Close() }

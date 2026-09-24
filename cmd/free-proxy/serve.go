package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/masteralanlab/free-proxy/internal/api"
	"github.com/masteralanlab/free-proxy/internal/config"
	"github.com/masteralanlab/free-proxy/internal/ipinfo"
	"github.com/masteralanlab/free-proxy/internal/logging"
	"github.com/masteralanlab/free-proxy/internal/netx"
	"github.com/masteralanlab/free-proxy/internal/providers/vpngate"
	"github.com/masteralanlab/free-proxy/internal/proxy"
	"github.com/masteralanlab/free-proxy/internal/security"
	"github.com/masteralanlab/free-proxy/internal/services"
	"github.com/masteralanlab/free-proxy/internal/store"
	"github.com/masteralanlab/free-proxy/internal/tunnel"
	"github.com/masteralanlab/free-proxy/internal/updater"
)

// buildDeps wires the entire application graph (the Go analogue of lifespan).
func buildDeps(ctx context.Context, cfg *config.Config, repos *store.Repos, auth *security.AuthService, logs *logging.Store) *api.Deps {
	runner := netx.SystemCommandRunner{}

	coordinator := services.NewCoordinator()
	jobs := services.NewJobService(repos.Jobs, ctx)

	provider := vpngate.NewProvider()
	ipClient := ipinfo.New(config.IPInfoAPIURL, config.RequestTimeout)
	ipInfo := services.NewIpInfoService(ipClient, repos.Nodes, repos.IPCache)
	diagnostics := services.NewDiagnosticsService(cfg, runner)
	discovery := services.NewDiscoveryService(provider, repos.Nodes)

	tunnelMgr := tunnel.NewManager(cfg)
	router := netx.NewPolicyRouter(runner, netx.PolicyRouterConfig{
		Table: cfg.PolicyRoutingTable, Interface: cfg.TunnelInterface,
		DevicePrefix: cfg.ProbeDevicePrefix,
		SetupRetries: config.RoutingSetupRetries, RetryInterval: config.RoutingRetryInterval,
		StrictRPF: config.RoutingStrictRPFilter,
	})

	adminCfg := auth.Store.Config()
	// The persisted proxy password is deliberately non-recoverable. Give the
	// in-process health checker an ephemeral credential so it exercises the same
	// authenticated listener as every other client without weakening loopback
	// authentication. These values exist only for this process lifetime.
	healthUsername := security.RandomCredential(32)
	healthPassword := security.RandomCredential(32)
	connector := proxy.NewSocketConnector(cfg.TunnelInterface, config.ProxyDNSServer, config.ProxyConnectTimeout)
	// Both of these run on every accepted proxy connection, so neither may go to
	// the database or to scrypt unconditionally — see ProxyAuthenticator.
	proxyAuth := security.NewProxyAuthenticator(repos.App)
	proxyGateway := proxy.New(proxy.Options{
		Host: "0.0.0.0", Port: adminCfg.ProxyPort,
		MaxConnections: config.ProxyMaxConnections,
		ConnectTimeout: config.ProxyConnectTimeout, IdleTimeout: config.ProxyIdleTimeout,
		// Fails closed on a database read error. The internal health credential
		// remains usable so monitoring does not trigger a false recovery.
		AuthRequired: proxyAuth.Required,
		Authenticate: proxyAuth.Authenticate,
		InternalAuthenticate: func(username, password string) bool {
			healthUserOK := subtle.ConstantTimeCompare([]byte(username), []byte(healthUsername))
			healthPasswordOK := subtle.ConstantTimeCompare([]byte(password), []byte(healthPassword))
			return healthUserOK&healthPasswordOK == 1
		},
		// Listener binds all interfaces; non-loopback clients are gated at runtime
		// by the admin toggle (default off) and still require proxy auth.
		ExternalAllowed: func() bool { return auth.Store.Config().ProxyExternalAllowed() },
	}, connector)

	pool := services.NewProxyPoolService(repos.Nodes, repos.Settings)
	gateway := services.NewGatewayService(cfg, repos.Nodes, repos.Settings, tunnelMgr, router, proxyGateway, pool, coordinator, runner)
	autoSwitch := services.NewAutoSwitchService(repos.Nodes, repos.Settings, pool, gateway)
	// The reconnect backs off, so it can still be waiting when the process is
	// asked to stop. It gets the lifetime context rather than a Background one,
	// so shutdown ends the wait instead of letting it reconnect into a teardown
	// that is already under way.
	gateway.SetUnexpectedExitHandler(func(nodeID string, uptime time.Duration) {
		autoSwitch.HandleUnexpectedExit(ctx, nodeID, uptime)
	})

	healthChecker := netx.NewHealthChecker(adminCfg.ProxyHost, adminCfg.ProxyPort, healthUsername, healthPassword, config.ProxyConnectTimeout)
	health := services.NewHealthService(healthChecker, repos.Nodes, repos.Settings, gateway, autoSwitch)
	settingsSvc := services.NewSettingsService(repos.Nodes, repos.Settings, pool, gateway, autoSwitch, coordinator)

	tunAlloc, _ := netx.NewTunAllocator(cfg.ProbeDevicePrefix, cfg.TestTunStart, cfg.TestTunEnd)
	probe := services.NewProbeService(repos.Nodes, tunnelMgr, tunAlloc, runner, ipInfo, repos.Probes, coordinator)
	maintenance := services.NewMaintenanceService(repos.Nodes, repos.Settings, repos.Probes, repos.Jobs,
		discovery, probe, pool, gateway, autoSwitch, coordinator)
	liveness := services.NewLivenessService(repos.Nodes, gateway)

	// The updater is given the version this binary was stamped with — what it
	// compares against the published releases — and the file `install` writes
	// to while it finishes an upgrade this process will not be alive to see.
	selfUpdate := updater.New(cfg.UpdateRepo, version, filepath.Join(cfg.LogsDir(), "update.log"))

	return &api.Deps{
		Cfg: cfg, Version: version, Repos: repos, Auth: auth, Logs: logs,
		Coordinator: coordinator, Jobs: jobs, Discovery: discovery, Probe: probe,
		Gateway: gateway, Pool: pool, Settings: settingsSvc, Health: health,
		Diagnostics: diagnostics, Maintenance: maintenance, AutoSwitch: autoSwitch,
		Liveness:         liveness,
		Updater:          selfUpdate,
		MaintenanceMon:   services.NewMaintenanceMonitor(maintenance, gateway),
		ActiveLatencyMon: services.NewActiveLatencyMonitor(repos.Nodes, gateway, runner),
		HealthMon:        services.NewHealthMonitor(health, gateway),
		LivenessMon:      services.NewLivenessMonitor(liveness),
	}
}

// runServe assembles the app, starts background workers, and serves HTTP until
// the context is cancelled (SIGINT/SIGTERM).
func runServe(ctx context.Context, cfg *config.Config, hostOverride string, portOverride int) error {
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}
	logs, err := logging.Configure(cfg.LogsDir())
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		return err
	}

	repos := store.NewRepos(db)
	if err := prepareAppSettings(ctx, cfg, repos); err != nil {
		return err
	}
	adminStore, err := security.NewAdminConfigStore(cfg, repos.App)
	if err != nil {
		return err
	}
	auth := security.NewAuthService(cfg, adminStore, security.NewSessionManager(config.SessionTTL))
	adminCfg := adminStore.Config()

	deps := buildDeps(ctx, cfg, repos, auth, logs)
	if err := deps.Jobs.Initialize(ctx); err != nil {
		slog.Warn("job init failed", "module", "serve", "err", err)
	}

	// Startup preflight (advisory).
	for _, check := range deps.Diagnostics.StartupPreflight(ctx).Checks {
		if !check.OK {
			slog.Warn("preflight check failed", "module", "serve", "check", check.Name, "detail", check.Detail)
		}
	}

	if err := deps.Gateway.Start(ctx); err != nil {
		slog.Error("proxy gateway did not start", "module", "serve", "err", err)
	}

	// Background upkeep is no longer switchable. Maintenance is what keeps the
	// pool worth selecting from — without it the node list ages into a set of
	// exits that no longer answer — so an install that turns it off is an install
	// that stops working a few hours later.
	go deps.HealthMon.Run(ctx)
	go deps.ActiveLatencyMon.Run(ctx)
	go deps.MaintenanceMon.Run(ctx)
	go deps.LivenessMon.Run(ctx)

	// Bind all interfaces; external web access is gated at runtime by the admin
	// toggle (default on) — see api.ExternalAccessGuard. A --host flag still wins.
	host := firstNonEmptyStr(hostOverride, "0.0.0.0")
	port := adminCfg.Port
	if portOverride != 0 {
		port = portOverride
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	slog.Info("web control plane starting", "module", "serve", "addr", addr, "path", "/"+adminCfg.SecretPath+"/")

	srv := &http.Server{Addr: addr, Handler: api.NewServer(deps), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	deps.Gateway.DisconnectOnly(shutdownCtx)
	return nil
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

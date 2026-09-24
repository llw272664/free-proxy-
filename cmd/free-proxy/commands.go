package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/masteralanlab/free-proxy/internal/config"
	"github.com/masteralanlab/free-proxy/internal/naming"
	"github.com/masteralanlab/free-proxy/internal/netx"
	"github.com/masteralanlab/free-proxy/internal/platform"
	"github.com/masteralanlab/free-proxy/internal/providers/vpngate"
	"github.com/masteralanlab/free-proxy/internal/security"
	"github.com/masteralanlab/free-proxy/internal/services"
	"github.com/masteralanlab/free-proxy/internal/store"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var host string
	var port int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web/API control plane, proxy gateway, and background workers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runServe(ctx, cfg, host, port)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Web bind address override")
	cmd.Flags().IntVar(&port, "port", 0, "Web bind port override")
	return cmd
}

func discoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover",
		Short: "Fetch nodes from the configured provider and store them",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			db, repos, err := openAppStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			provider := vpngate.NewProvider()
			svc := services.NewDiscoveryService(provider, repos.Nodes)
			res, err := svc.Discover(cmd.Context())
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(res)
		},
	}
}

func credentialsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Print the current web management address, path, username, and password",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			db, repos, err := openAppStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			adminStore, err := security.NewAdminConfigStore(cfg, repos.App)
			if err != nil {
				return err
			}
			c := adminStore.Config()
			out := cmd.OutOrStdout()
			url, note := adminURL(cmd.Context(), c)
			path := "/" + c.SecretPath + "/"
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"url": url, "path": path, "port": c.Port,
					"username": c.Username, "password": c.Password,
				})
			}
			fmt.Fprintf(out, "URL:      %s\n", url)
			if note != "" {
				fmt.Fprintf(out, "          %s\n", note)
			}
			fmt.Fprintf(out, "Path:     %s\n", path)
			fmt.Fprintf(out, "Username: %s\n", c.Username)
			if c.Password != "" {
				fmt.Fprintf(out, "Password: %s\n", c.Password)
			} else {
				// Reached only when the binary was replaced without re-running
				// the installer: `install` resets a hash-only password once.
				fmt.Fprintln(out, "Password: [set before this version; not recoverable]")
				fmt.Fprintln(out, "          Run `free-proxy install` (or the install script) — it resets the")
				fmt.Fprintln(out, "          password once and prints the new one.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the credentials as JSON")
	return cmd
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Print local configuration and database schema status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			db, _, err := openAppStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			tables, _ := store.SchemaTables(context.Background(), db)
			payload := map[string]any{
				"environment":  cfg.Environment,
				"data_dir":     cfg.DataDir,
				"database_url": cfg.DatabaseURL,
				"web":          fmt.Sprintf("%s:%d", cfg.WebHost, cfg.WebPort),
				"proxy":        fmt.Sprintf("%s:%d", cfg.ProxyHost, cfg.ProxyPort),
				"tables":       tables,
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(payload)
		},
	}
}

func preflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Run startup environment checks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			db, _, err := openAppStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			diag := services.NewDiagnosticsService(cfg, nil)
			result := diag.StartupPreflight(cmd.Context())
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		},
	}
}

func logsCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the most recent structured application log entries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(cfg.LogsDir())
			if err != nil {
				return nil
			}
			var files []string
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					files = append(files, e.Name())
				}
			}
			if len(files) == 0 {
				return nil
			}
			sort.Strings(files)
			data, err := os.ReadFile(filepath.Join(cfg.LogsDir(), files[len(files)-1]))
			if err != nil {
				return err
			}
			all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if lines < len(all) {
				all = all[len(all)-lines:]
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.Join(all, "\n"))
			return nil
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 100, "Number of log lines to print")
	return cmd
}

func adminConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin-config",
		Short: "Update web administration credentials and listener settings",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			db, repos, err := openAppStore(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			adminStore, err := security.NewAdminConfigStore(cfg, repos.App)
			if err != nil {
				return err
			}
			prev := adminStore.Config()
			updated := prev
			f := cmd.Flags()
			if v, _ := f.GetString("username"); v != "" {
				updated.Username = v
			}
			if v, _ := f.GetString("password"); v != "" {
				if updated.PasswordHash, err = security.HashPassword(v); err != nil {
					return err
				}
				updated.Password = v
			}
			if v, _ := f.GetString("secret-path"); v != "" {
				updated.SecretPath = v
			}
			if v, _ := f.GetInt("port"); v != 0 {
				updated.Port = v
			}
			if v, _ := f.GetInt("proxy-port"); v != 0 {
				updated.ProxyPort = v
			}
			if err := adminStore.Update(updated); err != nil {
				return err
			}
			// A running service holds its own snapshot of this configuration, so
			// nothing here reaches it until it restarts — credentials included.
			fmt.Fprintln(cmd.OutOrStdout(), "Administration configuration updated; restart the service for it to take effect (systemctl restart free-proxy)")
			fmt.Fprintln(cmd.OutOrStdout(), "Run `free-proxy credentials` to print the current address, path, username, and password")
			return nil
		},
	}
	cmd.Flags().String("username", "", "Administrator username")
	cmd.Flags().String("password", "", "Administrator password")
	cmd.Flags().String("secret-path", "", "Secret URL path segment")
	cmd.Flags().Int("port", 0, "Web bind port")
	cmd.Flags().Int("proxy-port", 0, "Proxy bind port")
	return cmd
}

func databaseUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "database-upgrade",
		Short: "Upgrade the application database to the latest migration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.EnsureDirectories(); err != nil {
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
			fmt.Fprintln(cmd.OutOrStdout(), "Database upgraded to head")
			return nil
		},
	}
}

func doctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check (and optionally install) required system dependencies",
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := platform.RunChecks()
			// Naming checks need the resolved config. A config that fails to
			// load is reported by `serve` itself; doctor still prints the
			// dependency checks rather than aborting on it.
			if cfg, err := loadConfig(); err == nil {
				checks = append(checks, platform.NamingChecks(cmd.Context(),
					cfg.TunnelInterface, cfg.ProbeDevicePrefix, cfg.PolicyRoutingTable)...)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: configuration could not be loaded, skipping naming checks: %v\n", err)
			}
			out := cmd.OutOrStdout()
			for _, c := range checks {
				mark := "OK  "
				if !c.OK {
					mark = "FAIL"
				}
				fmt.Fprintf(out, "[%s] %-12s %s\n", mark, c.Name, c.Detail)
			}
			missing := platform.MissingPackages(checks)
			if fix && len(missing) > 0 {
				if os.Geteuid() != 0 {
					return fmt.Errorf("--fix requires root")
				}
				if err := platform.Install(cmd.Context(), missing); err != nil {
					return err
				}
				fmt.Fprintln(out, "Dependencies installed; re-run `doctor` to verify.")
				return nil
			}
			if platform.CriticalMissing(checks) {
				return fmt.Errorf("critical dependencies missing (run `free-proxy install-deps` as root)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "Install missing dependencies")
	return cmd
}

func installDepsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install-deps",
		Short: "Install required system dependencies (openvpn, iproute2, ...)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return fmt.Errorf("install-deps requires root")
			}
			return platform.Install(cmd.Context(), platform.RecommendedPackages)
		},
	}
}

func installCmd() *cobra.Command {
	var rotateAdmin bool
	var quiet bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Free Proxy on this machine: binary, dependencies, env file, service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := platform.RequireInstallSupport(); err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			if err := platform.InstallSelf(); err != nil {
				return fmt.Errorf("install binary: %w", err)
			}
			fmt.Fprintf(out, "Binary installed at %s\n", platform.BinPath)
			if missing := platform.MissingPackages(platform.RunChecks()); len(missing) > 0 {
				if err := platform.Install(ctx, missing); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: dependency install failed: %v\nFix the package manager and re-run `free-proxy doctor --fix`.\n", err)
				}
			}
			if err := platform.WriteDefaultEnv(); err != nil {
				return fmt.Errorf("write environment: %w", err)
			}
			// Upgrades keep their existing env file, so an install that still
			// claims the shared tun0 / table 100 has to be moved explicitly.
			migrated, err := platform.MigrateLegacyNaming()
			if err != nil {
				return fmt.Errorf("migrate network naming: %w", err)
			}
			if len(migrated) > 0 {
				fmt.Fprintln(out, "Moved shared network identifiers into this project's private namespace:")
				for _, change := range migrated {
					fmt.Fprintf(out, "  %s\n", change)
				}
			}
			// A previous run killed before it could tear down leaves policy
			// entries pointing at the old device. They are inert now, but would
			// hijack whatever creates that device name next.
			if orphans := netx.CleanupOrphanedRules(ctx, nil, naming.LegacyTunnelInterface); len(orphans) > 0 {
				fmt.Fprintf(out, "Removed stale policy entries left by an earlier release (device %s is gone):\n", naming.LegacyTunnelInterface)
				for _, entry := range orphans {
					fmt.Fprintf(out, "  %s\n", entry)
				}
			}
			// The first install creates database-backed random admin credentials.
			// Updates preserve them unless rotation was explicitly requested.
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := cfg.EnsureDirectories(); err != nil {
				return err
			}
			// Advisory: makes our table id visible as taken to other tools.
			if err := platform.RegisterRoutingTable(cfg.PolicyRoutingTable); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not register routing table alias: %v\n", err)
			}
			db, repos, err := openAppStore(ctx, cfg)
			if err != nil {
				return fmt.Errorf("open settings database: %w", err)
			}
			defer db.Close()
			adminStore, err := security.NewAdminConfigStore(cfg, repos.App)
			if err != nil {
				return fmt.Errorf("admin config: %w", err)
			}
			if err := platform.PruneDatabaseSettingsEnv(); err != nil {
				return fmt.Errorf("prune migrated environment: %w", err)
			}
			admin := adminStore.Config()
			password := admin.Password
			// Installs from before passwords were stored recoverably have a hash
			// and nothing else, so their owner has no way to learn the password
			// again. Reset it once, here: this run restarts the service anyway,
			// which is the only cost a password change carries.
			passwordReset := !rotateAdmin && password == ""
			switch {
			case rotateAdmin:
				admin, password, err = adminStore.Rotate()
				if err != nil {
					return fmt.Errorf("rotate admin credentials: %w", err)
				}
			case passwordReset:
				admin, password, err = adminStore.ResetPassword()
				if err != nil {
					return fmt.Errorf("reset admin password: %w", err)
				}
			}
			if err := platform.InstallService(ctx); err != nil {
				return fmt.Errorf("install service: %w", err)
			}
			fmt.Fprintln(out, "Free Proxy installed and started.")
			// An update triggered from the console runs this command with its
			// output going to a file. Credentials are unchanged there, so the
			// only thing printing them would achieve is a second copy of the
			// password on disk. A reset still prints: that password is new, and
			// this is the one place it can be read.
			if quiet && !rotateAdmin && !passwordReset {
				fmt.Fprintln(out, "Management path, username and password are unchanged.")
				fmt.Fprintln(out, "Run `free-proxy credentials` to print them.")
				return nil
			}
			switch {
			case rotateAdmin:
				fmt.Fprintln(out, "Management path and admin login were explicitly rotated:")
			case passwordReset:
				fmt.Fprintln(out, "Your previous password was stored as a hash only and could not be read back,")
				fmt.Fprintln(out, "so this update reset it once. The management path and username are unchanged:")
			default:
				fmt.Fprintln(out, "Management path and admin login (preserved on future updates):")
			}
			url, note := adminURL(ctx, admin)
			fmt.Fprintf(out, "  URL:       %s\n", url)
			if note != "" {
				fmt.Fprintf(out, "             %s\n", note)
			}
			fmt.Fprintf(out, "  Path:      /%s/\n", admin.SecretPath)
			fmt.Fprintf(out, "  Username:  %s\n", admin.Username)
			// Every branch above leaves a readable password behind, so there is
			// no longer an install that cannot show one.
			fmt.Fprintf(out, "  Password:  %s\n", password)
			fmt.Fprintln(out, "Future updates keep this path, username, and password unchanged.")
			fmt.Fprintln(out, "Forgot them later? Run: free-proxy credentials")
			return nil
		},
	}
	cmd.Flags().BoolVar(&rotateAdmin, "rotate-admin", false, "Generate a new random management path, username, and password")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Do not print unchanged credentials (used by the console's updater)")
	return cmd
}

// adminURL renders the management URL with a real address in it, plus a note to
// print underneath when that address is not internet-reachable. The listener
// binds a wildcard, so the stored host is "0.0.0.0" — useless to paste into a
// browser; netx.ResolveAdvertiseHost picks the address a human actually needs.
func adminURL(ctx context.Context, c security.AdminConfig) (url, note string) {
	addr := netx.ResolveAdvertiseHost(ctx, c.Host)
	host := addr.Host
	switch {
	case host == "":
		host = "<your-server-ip>"
		note = "(no address could be detected on this host)"
	case !addr.Public:
		note = "(private address — reachable from this network only)"
	}
	return fmt.Sprintf("http://%s/%s/", net.JoinHostPort(host, strconv.Itoa(c.Port)), c.SecretPath), note
}

func openAppStore(ctx context.Context, cfg *config.Config) (*sql.DB, *store.Repos, error) {
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	if err = store.Migrate(db); err != nil {
		db.Close()
		return nil, nil, err
	}
	repos := store.NewRepos(db)
	if err = prepareAppSettings(ctx, cfg, repos); err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, repos, nil
}

func prepareAppSettings(ctx context.Context, cfg *config.Config, repos *store.Repos) error {
	var proxyHash string
	var err error
	if cfg.ProxyPassword != "" {
		proxyHash, err = security.HashPassword(cfg.ProxyPassword)
		if err != nil {
			return err
		}
	}
	if err = repos.App.InitializeFromLegacyEnv(ctx, cfg, proxyHash); err != nil {
		return err
	}
	settings, err := repos.App.Get(ctx)
	if err != nil {
		return err
	}
	store.ApplyToConfig(cfg, settings)
	return nil
}

func uninstallCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove Free Proxy from this machine",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := platform.RequireInstallSupport(); err != nil {
				return err
			}
			if err := platform.Uninstall(cmd.Context(), purge); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Free Proxy removed.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&purge, "purge-data", false, "Also delete "+platform.DataDir+" (database, logs, configs)")
	return cmd
}

// Command gamefleet serves the GameFleet machine API, console and image
// registry front.
//
//	gamefleet [-config FILE]                   serve
//	gamefleet user NAME                        make a console user; reads the password from stdin
//	gamefleet cluster [-config FILE] add          add a cluster whose connection JSON is on stdin
//	gamefleet key [-config FILE] [-sha256] NAME SCOPES
//	                                           add an API key whose value, or with
//	                                           -sha256 its SHA-256 in hex, is on stdin
//	gamefleet var [-config FILE] GAME NAME    set a game server runtime setting whose
//	                                           value is on stdin
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xuhuanhello/gamefleet/internal/allocate"
	"github.com/xuhuanhello/gamefleet/internal/api"
	"github.com/xuhuanhello/gamefleet/internal/auth"
	"github.com/xuhuanhello/gamefleet/internal/buildinfo"
	"github.com/xuhuanhello/gamefleet/internal/clusters"
	"github.com/xuhuanhello/gamefleet/internal/config"
	"github.com/xuhuanhello/gamefleet/internal/health"
	"github.com/xuhuanhello/gamefleet/internal/install"
	"github.com/xuhuanhello/gamefleet/internal/logbook"
	"github.com/xuhuanhello/gamefleet/internal/logs"
	"github.com/xuhuanhello/gamefleet/internal/redact"
	"github.com/xuhuanhello/gamefleet/internal/registry"
	"github.com/xuhuanhello/gamefleet/internal/release"
	"github.com/xuhuanhello/gamefleet/internal/sites"
	"github.com/xuhuanhello/gamefleet/internal/store"
	"github.com/xuhuanhello/gamefleet/internal/update"
	"github.com/xuhuanhello/gamefleet/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "user" {
		if err := newUser(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "gamefleet user:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "cluster" {
		if err := addCluster(os.Args[2:], os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "gamefleet cluster:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "key" {
		if err := addKey(os.Args[2:], os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "gamefleet key:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		if err := backup(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "gamefleet backup:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "var" {
		if err := setVar(os.Args[2:], os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, "gamefleet var:", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "/etc/gamefleet/config.json", "path to the configuration file")
	flag.Parse()

	// The logs API reads GameFleet's own newest lines from the book.
	book := logbook.New(20000, time.Now)
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, book), nil))
	if err := run(*configPath, log, book); err != nil {
		log.Error("gamefleet stopped", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger, book *logbook.Book) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	upstream, _ := url.Parse(cfg.Registry.URL) // checked by config.Validate

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := sites.New()
	keys := auth.NewKeys(db)
	fleets := clusters.New(db, dir, keys, clusters.ConnectClient, cfg.Registry.Host, log)
	if err := fleets.Load(ctx); err != nil {
		return err
	}
	releases := release.New(db, registry.NewClient(upstream), cfg.Registry.Host, dir, cfg.DrainRetention.Duration, log)
	svc := allocate.New(releases, dir)
	backups := filepath.Join(filepath.Dir(cfg.Database), "backups")
	monitor := health.New(time.Now, func() (time.Time, error) {
		_, at, err := store.NewestDailyBackup(backups)
		return at, err
	})
	monitor.WatchOffsite(func() (time.Time, error) { return store.LastOffsite(backups) })
	monitor.Watch([]string{"release", "clusters"}, func() []string {
		var names []string
		for _, s := range dir.All() {
			names = append(names, s.Name)
		}
		return names
	})
	releases.SetMonitor(monitor)
	fleets.SetMonitor(monitor)
	users, err := auth.New(cfg.Users)
	if err != nil {
		return err
	}
	// install.sh points GameFleet at the updater's directory; without one,
	// GameFleet only says when a release is out.
	releasesRepo := "xuhuanhello/gamefleet"
	if v, ok := os.LookupEnv("GAMEFLEET_RELEASES"); ok {
		releasesRepo = v
	}
	updates := update.New(update.Options{
		Current: buildinfo.Version, Repository: releasesRepo, Dir: os.Getenv("GAMEFLEET_UPDATE_DIR"),
		Backup: db.Backup, BackupDir: backups, Log: log,
	})
	console, err := web.New(web.Options{Host: cfg.Registry.Host, Sites: dir, Clusters: fleets, Auth: users, Keys: keys,
		Store: db, Releases: releases, Updates: updates, Monitor: monitor, Log: log})
	if err != nil {
		return err
	}
	installer, err := install.Handler("https://"+cfg.Registry.Host, fleets, log)
	if err != nil {
		return err
	}

	reader := logs.New(dir, book, db, redact.New(), time.Now)
	machines := api.Handler(svc, releases, keys, monitor, reader, log)
	mux := http.NewServeMux()
	mux.Handle("/v2/", registry.NewProxy(upstream, keys, log))
	mux.Handle("/api/", machines)
	mux.Handle("/healthz", machines)
	mux.Handle("/status", machines)
	mux.Handle("/install/", installer)
	mux.Handle("/", console.Handler())

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	go releases.Run(ctx)
	go fleets.Run(ctx)
	log.Info("gamefleet listening", "version", buildinfo.Version, "addr", cfg.Listen, "clusters", len(dir.All()), "users", len(cfg.Users))

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// newUser prints a console user for config.json and the otpauth URI to add to
// an authenticator app. The password comes from the first line of stdin.
// backup makes the daily backup of the database, in a process of its own:
// the systemd timer gamefleet-backup runs it with docker compose exec.
func backup(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/gamefleet/config.json", "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	path, err := store.DailyBackup(ctx, cfg.Database, filepath.Join(filepath.Dir(cfg.Database), "backups"), time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, path)
	return nil
}

func newUser(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("user", flag.ContinueOnError)
	otpauth := flags.String("otpauth", "", "write the authenticator link to this file, mode 0600, and print only the user")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: gamefleet user [-otpauth FILE] NAME < password-file")
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	password := strings.TrimRight(line, "\r\n")
	if len(password) < 12 {
		return errors.New("the password needs at least 12 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	user := config.User{Name: flags.Arg(0), PasswordHash: hash, TOTPSecret: auth.NewTOTPSecret()}
	if err := user.Validate(); err != nil {
		return err
	}
	uri := auth.OTPAuthURI("GameFleet", user.Name, user.TOTPSecret)
	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	if *otpauth != "" {
		if err := os.WriteFile(*otpauth, []byte(uri+"\n"), 0o600); err != nil {
			return err
		}
		return out.Encode(user)
	}
	return out.Encode(map[string]any{"user": user, "otpauth": uri})
}

// addKey stores an API key whose value already exists, such as a registry
// password callers hold. The value, or with -sha256 its hash, is the first
// line of stdin. The hash form keeps the key itself on the caller's machine.
// setVar sets a runtime setting from stdin, so a secret such as Nakama's
// http_key can come straight from another machine without being shown.
func setVar(args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("var", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/gamefleet/config.json", "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: gamefleet var [-config FILE] GAME NAME < value-file")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	value, err := io.ReadAll(io.LimitReader(stdin, 16<<10+2))
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	// A file or a command's output ends in a line break that is not part of the value.
	return clusters.SaveVar(context.Background(), db, flags.Arg(0), flags.Arg(1),
		strings.TrimRight(string(value), "\r\n"), "gamefleet var")
}

func addKey(args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("key", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/gamefleet/config.json", "path to the configuration file")
	hashed := flags.Bool("sha256", false, "stdin holds the key's SHA-256 in hex, not the key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: gamefleet key [-config FILE] [-sha256] NAME SCOPE[,SCOPE] < key-file")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	scopes := strings.Split(flags.Arg(1), ",")
	value := strings.TrimSpace(line)
	if *hashed {
		return auth.NewKeys(db).AddHash(context.Background(), flags.Arg(0), scopes, strings.ToLower(value), "gamefleet key")
	}
	return auth.NewKeys(db).Add(context.Background(), flags.Arg(0), scopes, value, "gamefleet key")
}

// addCluster adds a cluster that was not installed by GameFleet's install
// script, for example the first one. Stdin holds the same connection JSON
// the install script sends. GameFleet must be restarted to use the cluster.
func addCluster(args []string, stdin io.Reader) error {
	flags := flag.NewFlagSet("cluster", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/gamefleet/config.json", "path to the configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || flags.Arg(0) != "add" {
		return errors.New("usage: gamefleet cluster [-config FILE] add < connection.json")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	var c clusters.Connection
	decoder := json.NewDecoder(io.LimitReader(stdin, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return fmt.Errorf("read the connection JSON: %w", err)
	}
	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	svc := clusters.New(db, sites.New(), auth.NewKeys(db), clusters.ConnectClient, cfg.Registry.Host, log)
	return svc.Add(context.Background(), c, "gamefleet cluster")
}

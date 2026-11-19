// Command relay is a headless, agent-first social media scheduler (a lightweight, self-hosted alternative to tools like Postiz or Buffer).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Seeyko/relay/internal/api"
	"github.com/Seeyko/relay/internal/blob"
	"github.com/Seeyko/relay/internal/core"
	"github.com/Seeyko/relay/internal/mcp"
	"github.com/Seeyko/relay/internal/secret"
	"github.com/Seeyko/relay/internal/store"
	"github.com/Seeyko/relay/internal/ui"

	// connectors register themselves
	_ "github.com/Seeyko/relay/internal/providers/meta"
	_ "github.com/Seeyko/relay/internal/providers/tiktok"
	_ "github.com/Seeyko/relay/internal/providers/website"
	_ "github.com/Seeyko/relay/internal/providers/x"
	_ "github.com/Seeyko/relay/internal/providers/youtube"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func dur(k string, def time.Duration, log *slog.Logger) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Warn("invalid duration, using default", "var", k, "value", v)
		return def
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Used by the Docker HEALTHCHECK (the image has no curl).
		resp, err := http.Get("http://127.0.0.1" + env("RELAY_ADDR", ":8080") + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token := os.Getenv("RELAY_API_TOKEN")
	if len(token) < 16 {
		return errors.New("RELAY_API_TOKEN must be set (at least 16 characters)")
	}
	box, err := secret.New(os.Getenv("RELAY_ENCRYPTION_KEY"))
	if err != nil {
		return err
	}
	dataDir := env("RELAY_DATA_DIR", "./data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "relay.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	svc := &core.Service{
		Store:         st,
		Box:           box,
		MediaDir:      filepath.Join(dataDir, "media"),
		BaseURL:       os.Getenv("RELAY_BASE_URL"),
		HTTP:          &http.Client{Timeout: 5 * time.Minute},
		WebhookURL:    os.Getenv("RELAY_WEBHOOK_URL"),
		WebhookSecret: os.Getenv("RELAY_WEBHOOK_SECRET"),
		Log:           log,
		Location:      time.UTC,
	}
	if ep := os.Getenv("RELAY_S3_ENDPOINT"); ep != "" {
		b := &blob.S3{
			Endpoint:  ep,
			Bucket:    os.Getenv("RELAY_S3_BUCKET"),
			Region:    env("RELAY_S3_REGION", "auto"),
			AccessKey: os.Getenv("RELAY_S3_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("RELAY_S3_SECRET_ACCESS_KEY"),
			HTTP:      svc.HTTP,
		}
		if err := b.Validate(); err != nil {
			return fmt.Errorf("RELAY_S3_*: %w", err)
		}
		svc.Blob = b
		svc.MediaPublicURL = os.Getenv("RELAY_S3_PUBLIC_URL")
		log.Info("media stored in bucket", "endpoint", ep, "bucket", b.Bucket)
	}
	if tz := os.Getenv("RELAY_TIMEZONE"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return fmt.Errorf("RELAY_TIMEZONE: %w", err)
		}
		svc.Location = loc
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bootstrapX(ctx, svc, log)

	server := mcp.NewServer("relay", version, mcp.BuildTools(svc))
	server.ExtraInstructions = os.Getenv("RELAY_AGENT_INSTRUCTIONS")
	draft := os.Getenv("RELAY_DRAFT_TOKEN")
	if draft != "" && (len(draft) < 16 || draft == token) {
		return errors.New("RELAY_DRAFT_TOKEN must be at least 16 characters and differ from RELAY_API_TOKEN")
	}
	a := &api.API{Svc: svc, MCP: server, Token: token, DraftToken: draft, Log: log, Version: version}
	if os.Getenv("RELAY_UI") != "off" && svc.BaseURL != "" {
		dash, err := ui.New(svc.BaseURL, st, token, log)
		if err != nil {
			log.Warn("dashboard disabled", "err", err)
		} else {
			a.UI = dash
		}
	}
	srv := &http.Server{Addr: env("RELAY_ADDR", ":8080"), Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}

	sched := &core.Scheduler{
		Svc:          svc,
		Tick:         dur("RELAY_TICK", 30*time.Second, log),
		CollectEvery: dur("RELAY_COLLECT_INTERVAL", time.Hour, log),
	}
	go sched.Run(ctx)

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("relay listening", "addr", srv.Addr, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// bootstrapX connects the X account from environment variables on first start.
func bootstrapX(ctx context.Context, svc *core.Service, log *slog.Logger) {
	ck, cs := os.Getenv("X_CONSUMER_KEY"), env("X_CONSUMER_SECRET", os.Getenv("X_SECRET_KEY"))
	at, as := os.Getenv("X_ACCESS_TOKEN"), os.Getenv("X_ACCESS_TOKEN_SECRET")
	if ck == "" || cs == "" || at == "" || as == "" {
		return
	}
	accs, err := svc.Store.ListAccounts(ctx)
	if err != nil {
		return
	}
	for _, a := range accs {
		if a.Provider == "x" {
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	acc, err := svc.ConnectAccount(cctx, "x", "", map[string]string{"consumer_key": ck, "consumer_secret": cs, "access_token": at, "access_token_secret": as})
	if err != nil {
		log.Warn("could not connect X account from env", "err", err)
		return
	}
	log.Info("connected X account from env", "account", acc.ID, "handle", acc.Handle)
}

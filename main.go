// Command mot-router runs a local router and dashboard for the free LLM
// providers it proxies (OpenCode Zen, Token Harbor, Cline), exposing both
// Anthropic and OpenAI compatible endpoints.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/server"
	"github.com/rian/mot-router/internal/store"
	"github.com/rian/mot-router/internal/zen"
)

const (
	defaultPort  = 8787
	defaultModel = "muse-spark-1.3-contributor-free"
)

func main() {
	port := flag.Int("port", envInt("PORT", defaultPort), "port to listen on")
	host := flag.String("host", env("HOST", "127.0.0.1"), "host to bind")
	dataDir := flag.String("data", env("DATA_DIR", "./data"), "directory for the SQLite database")
	flag.Parse()

	cfg := server.Config{
		Host:            *host,
		Port:            *port,
		DefaultModel:    env("MOT_MODEL", defaultModel),
		DefaultProvider: env("MOT_PROVIDER", "opencode-zen"),
	}

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}

	st, err := store.Open(absData)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	zc := zen.New(
		env("ZEN_BASE_URL", zen.DefaultBaseURL),
		env("ZEN_UA", zen.DefaultUserAgent),
		os.Getenv("OPENCODE_API_KEY"),
	)

	// The upstream gate is probed rather than assumed: every gate failure returns
	// the same 403, so the only reliable way to learn the required request shape
	// is to test it. The cache refreshes in the background.
	gc := gate.NewCache(gate.NewProber(zc.BaseURL, env("MOT_GATE_PROBE_MODEL", "muse-spark-1.3-contributor-free")), 0)

	srvAPI := server.New(cfg, st, zc, gc)

	srv := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler: srvAPI.Handler(),
		// No write timeout: generations stream for as long as the model needs.
	}

	// Probe once at startup so the router adapts before the first client
	// request. It runs in the background: a slow or unreachable upstream must not
	// delay the server from accepting connections, and the fallback profile
	// covers that window.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if p := srvAPI.ProbeGate(ctx); p != nil {
			log.Printf("gate profile: %s (tools: %s)", p.Verdict, strings.Join(p.RequiredTools, ", "))
		}
	}()

	go func() {
		banner(cfg)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func banner(cfg server.Config) {
	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	log.Printf("Mot Router listening on %s", base)
	log.Printf("  anthropic  %s/v1/messages", base)
	log.Printf("  openai     %s/v1/chat/completions", base)
	log.Printf("  dashboard  %s/", base)
	log.Printf("  model      %s", cfg.DefaultModel)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

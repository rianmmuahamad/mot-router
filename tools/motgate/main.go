// Command motgate is the user-facing portal for mot-router: an admin console
// that issues per-user keys with an expiry, a per-user status page, and an
// inference endpoint that fronts mot-router through the guard.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rian/motgate/internal/server"
	"github.com/rian/motgate/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	listenHost := env("MOTGATE_HOST", "127.0.0.1")
	listenPort := envInt("MOTGATE_PORT", 8790)
	dataDir := env("MOTGATE_DATA", "./data-motgate")
	publicURL := env("MOTGATE_PUBLIC_URL", "http://127.0.0.1:8790")
	upstream := env("MOTGATE_UPSTREAM", "http://127.0.0.1:8791")
	adminPassword := env("MOTGATE_ADMIN_PASSWORD", "")
	defaultHours := envInt("MOTGATE_DEFAULT_HOURS", 24)

	flag.StringVar(&listenHost, "host", listenHost, "host to bind")
	flag.IntVar(&listenPort, "port", listenPort, "port to listen on")
	flag.StringVar(&dataDir, "data", dataDir, "directory for the SQLite database")
	flag.StringVar(&publicURL, "public-url", publicURL, "public base URL handed to users")
	flag.StringVar(&upstream, "upstream", upstream, "guard base URL")
	flag.StringVar(&adminPassword, "admin-password", adminPassword, "admin password")
	flag.IntVar(&defaultHours, "default-hours", defaultHours, "default access duration in hours")
	flag.Parse()

	if adminPassword == "" {
		log.Println("MOTGATE_ADMIN_PASSWORD is required (or -admin-password).")
		log.Println("Generate one with: openssl rand -hex 16")
		os.Exit(2)
	}
	if defaultHours < 1 {
		defaultHours = 24
	}

	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if err := st.RecoverQuotaRequests(context.Background()); err != nil {
		log.Fatalf("recover pending quota requests: %v", err)
	}
	defer st.Close()

	srv, err := server.New(server.Config{
		ModelCatalogURL: env("MOTGATE_MODEL_CATALOG_URL", "http://127.0.0.1:8787"),
		PublicURL:       publicURL,
		Upstream:        upstream,
		AdminPassword:   adminPassword,
		DefaultHours:    defaultHours,
	}, st)
	if err != nil {
		log.Fatalf("build server: %v", err)
	}

	addr := net.JoinHostPort(listenHost, strconv.Itoa(listenPort))
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// No write timeout: generations stream for as long as the model needs.
	}

	log.Printf("motgate listening on http://%s", addr)
	log.Printf("  admin      %s/admin", publicURL)
	log.Printf("  upstream   %s", upstream)
	if upstream == "http://127.0.0.1:8787" {
		log.Printf("  note       upstream points straight at mot-router; the guard is bypassed")
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

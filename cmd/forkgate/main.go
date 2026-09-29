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
	"syscall"
	"time"

	"github.com/yushui2022/ForkGate/internal/api"
	"github.com/yushui2022/ForkGate/internal/branches"
	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/proxy"
	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/secrets"
	"github.com/yushui2022/ForkGate/internal/store"
)

func main() {
	proxyAddr := flag.String("proxy-addr", envOr("FORKGATE_PROXY_ADDR", "127.0.0.1:3128"), "explicit proxy listen address")
	controlAddr := flag.String("control-addr", envOr("FORKGATE_CONTROL_ADDR", "127.0.0.1:7070"), "control API listen address")
	dataDir := flag.String("data-dir", envOr("FORKGATE_DATA_DIR", filepath.Join("data", "forkgate")), "data directory")
	adminToken := flag.String("admin-token", os.Getenv("FORKGATE_ADMIN_TOKEN"), "control API bearer token (required)")
	flag.Parse()

	if *adminToken == "" {
		log.Fatal("FORKGATE_ADMIN_TOKEN or -admin-token is required")
	}
	allowNonLoopback := os.Getenv("FORKGATE_ALLOW_NON_LOOPBACK") == "1"
	for name, addr := range map[string]string{"proxy": *proxyAddr, "control": *controlAddr} {
		if !isLoopbackAddr(addr) && !allowNonLoopback {
			log.Fatalf("%s listener must use a loopback address during Phase 0: %s", name, addr)
		}
	}
	if allowNonLoopback {
		log.Printf("WARNING: non-loopback listeners explicitly enabled; use only on a private test network")
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("create data directory: %v", err)
	}
	dbPath := filepath.Join(*dataDir, "forkgate.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer db.Close()

	ca, err := mitm.LoadOrCreate(filepath.Join(*dataDir, "ca"))
	if err != nil {
		log.Fatalf("load CA: %v", err)
	}

	p := proxy.New(db, ca)
	control := api.New(db, ca, *adminToken)
	branchManager := branches.New(db)
	branchesEnabled := os.Getenv("FORKGATE_BRANCHES_ENABLED") == "1"
	if branchesEnabled {
		control.SetBranchManager(branchManager)
		p.SetBranchManager(branchManager)
		log.Printf("branch identity enforcement enabled")
	} else {
		log.Printf("branch identity enforcement disabled: FORKGATE_BRANCHES_ENABLED is not 1")
	}
	if rawMasterKey := os.Getenv("FORKGATE_MASTER_KEY"); rawMasterKey != "" {
		masterKey, err := secrets.ParseMasterKey(rawMasterKey)
		if err != nil {
			log.Fatalf("load SecretGuard master key: %v", err)
		}
		guard := secretguard.New()
		service, err := secrets.New(context.Background(), db, guard, masterKey)
		if err != nil {
			for i := range masterKey {
				masterKey[i] = 0
			}
			log.Fatalf("load SecretGuard rules: %v", err)
		}
		if branchesEnabled {
			if err := branchManager.SetStagingKey(masterKey); err != nil {
				log.Fatalf("enable branch staging: %v", err)
			}
		}
		for i := range masterKey {
			masterKey[i] = 0
		}
		p.SetGuard(guard)
		control.SetSecretService(service)
		log.Printf("SecretGuard enabled")
	} else if branchesEnabled {
		log.Fatal("FORKGATE_MASTER_KEY is required when FORKGATE_BRANCHES_ENABLED=1")
	} else if records, err := db.SecretRecords(context.Background()); err != nil {
		log.Fatalf("inspect SecretGuard rules: %v", err)
	} else if len(records) > 0 {
		log.Fatal("FORKGATE_MASTER_KEY is required to load persisted SecretGuard rules")
	} else {
		log.Printf("SecretGuard disabled: FORKGATE_MASTER_KEY is not set")
	}

	proxyServer := &http.Server{
		Addr:              *proxyAddr,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}
	controlServer := &http.Server{
		Addr:              *controlAddr,
		Handler:           control.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Printf("data plane listening on http://%s", *proxyAddr)
		if err := proxyServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	go func() {
		log.Printf("control plane listening on http://%s", *controlAddr)
		if err := controlServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("control server: %w", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		log.Fatal(err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = proxyServer.Shutdown(shutdownCtx)
	_ = p.Close()
	_ = controlServer.Shutdown(shutdownCtx)
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

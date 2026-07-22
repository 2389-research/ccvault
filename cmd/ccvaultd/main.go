// ABOUTME: ccvaultd — the group-mode server binary
// ABOUTME: Long-lived SSH server that accepts pushes and serves queries

package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/internal/remote/server"
)

const buildVersion = "0.1.0"

func main() {
	var (
		dataDir  = flag.String("data", "/var/lib/ccvaultd", "Data directory (holds ccvault.db, host key, cache)")
		addr     = flag.String("addr", ":2222", "listen address")
		authFile = flag.String("authorized-keys", "/etc/ccvaultd/authorized_keys", "SSH authorized_keys file")
		hostKey  = flag.String("host-key", "", "Path to SSH host key (default: <data>/ssh_host_ed25519_key)")
	)
	flag.Parse()

	if *hostKey == "" {
		*hostKey = filepath.Join(*dataDir, "ssh_host_ed25519_key")
	}

	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	database, err := db.Open(*dataDir)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	authKeys, err := server.LoadAuthorizedKeys(*authFile)
	if err != nil {
		log.Fatalf("load authorized_keys: %v", err)
	}

	hostSigner, err := server.LoadOrGenerateHostKey(*hostKey)
	if err != nil {
		log.Fatalf("host key: %v", err)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("ccvaultd %s listening on %s", buildVersion, *addr)

	srv := server.New(database, authKeys, hostSigner, buildVersion)

	// SIGHUP → reload authorized_keys
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			if err := authKeys.Reload(); err != nil {
				log.Printf("reload authorized_keys: %v", err)
			} else {
				log.Printf("reloaded authorized_keys from %s", *authFile)
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-term
		log.Println("shutdown requested")
		cancel()
		_ = ln.Close()
	}()

	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

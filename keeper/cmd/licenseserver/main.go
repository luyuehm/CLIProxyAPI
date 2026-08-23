package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"cpa-usage-keeper/internal/licenseserver"
)

func main() {
	port := flag.String("port", "8443", "listen port")
	keysFile := flag.String("keys", "license_keys.json", "path to license keys definition file")
	stateFile := flag.String("state", "license_state.json", "path to activation state file")
	leaseHours := flag.Int("lease-hours", 24, "lease duration in hours")
	flag.Parse()

	listenAddr := ":" + *port

	// Resolve relative paths from the executable directory so Docker/absolute runs
	// find keys/state even when started from another cwd.
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		if !filepath.IsAbs(*keysFile) {
			*keysFile = filepath.Join(exeDir, *keysFile)
		}
		if !filepath.IsAbs(*stateFile) {
			*stateFile = filepath.Join(exeDir, *stateFile)
		}
	}

	store, err := licenseserver.NewStore(*keysFile, *stateFile)
	if err != nil {
		log.Fatalf("Failed to initialize license store: %v", err)
	}

	leaseDuration := time.Duration(*leaseHours) * time.Hour
	fmt.Printf("License Server starting\n")
	fmt.Printf("  Listen:    %s\n", listenAddr)
	fmt.Printf("  Keys:      %s\n", *keysFile)
	fmt.Printf("  State:     %s\n", *stateFile)
	fmt.Printf("  Lease:     %v\n", leaseDuration)
	fmt.Printf("  Grace:     %v\n", licenseserver.GracePeriod)

	if err := licenseserver.StartServer(store, listenAddr, leaseDuration); err != nil {
		log.Fatalf("License server failed: %v", err)
	}
}
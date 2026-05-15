package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/servermgr"
	"github.com/deux2k5/dcs-ais-traffic/internal/shipcache"
	"github.com/deux2k5/dcs-ais-traffic/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	// Detect first boot before config creates the file.
	_, statErr := os.Stat("config.toml")
	firstBoot := os.IsNotExist(statErr)

	// Load config (with auto-migration from v1 format).
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("config loaded: %d server(s) configured", len(cfg.Servers))

	// On first boot, offer to create a desktop shortcut.
	if firstBoot && runtime.GOOS == "windows" {
		offerDesktopShortcut()
	}

	// Create shared ship metadata cache.
	cache := shipcache.New("shipcache.json")
	log.Printf("ship cache: %d vessels loaded", cache.Size())

	// Create the server manager (owns AIS client + all server instances).
	mgr := servermgr.New(cfg, cache)

	// Deploy hooks to all configured servers on startup (keeps Lua in sync
	// with binary version after self-update).
	mgr.DeployAllHooks()

	// Start all configured server instances (TCP listeners + coordinators).
	mgr.StartAll()

	// Initial AIS subscription based on whatever theatres are already known.
	mgr.RefreshAIS()

	// Start web server.
	webServer := web.NewServer(cfg, mgr)
	go func() {
		if err := webServer.Start(); err != nil {
			log.Printf("[WEB] server error: %v", err)
			os.Exit(1)
		}
	}()

	cfg.RLock()
	webPort := cfg.Web.Port
	serverCount := len(cfg.Servers)
	cfg.RUnlock()

	log.Printf("DCS AIS Traffic running (web: http://localhost:%d, %d server(s))", webPort, serverCount)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down...")
	mgr.StopAll()
	cache.Stop()
}

// offerDesktopShortcut prompts the user (if running interactively) to create
// a desktop shortcut pointing to this exe.
func offerDesktopShortcut() {
	// Only prompt if stdin is a real terminal (not a service or redirected).
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}

	fmt.Print("Create a desktop shortcut? [Y/n] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "" && answer != "y" && answer != "yes" {
		return
	}

	exePath, _ := filepath.Abs(os.Args[0])
	exeDir := filepath.Dir(exePath)
	desktop := filepath.Join(os.Getenv("USERPROFILE"), "Desktop")
	lnkPath := filepath.Join(desktop, "DCS AIS Traffic.lnk")

	ps := fmt.Sprintf(
		`$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.WorkingDirectory = '%s'; $s.IconLocation = '%s,0'; $s.Save()`,
		lnkPath, exePath, exeDir, exePath,
	)

	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	if err := cmd.Run(); err != nil {
		log.Printf("failed to create desktop shortcut: %v", err)
		return
	}
	fmt.Println("Desktop shortcut created!")
}

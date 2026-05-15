package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/deux2k5/dcs-ais-traffic/internal/config"
	"github.com/deux2k5/dcs-ais-traffic/internal/coordinator"
	"github.com/deux2k5/dcs-ais-traffic/internal/dcscomm"
	"github.com/deux2k5/dcs-ais-traffic/internal/shipcache"
	"github.com/deux2k5/dcs-ais-traffic/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	// Detect first boot before config creates the file.
	_, statErr := os.Stat("config.toml")
	firstBoot := os.IsNotExist(statErr)

	// Load config.
	cfg, err := config.Load("config.toml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Always start with AIS disabled — user must enable via dashboard.
	cfg.Lock()
	cfg.AIS.Enabled = false
	cfg.Unlock()

	log.Println("config loaded")

	// On first boot, offer to create a desktop shortcut.
	if firstBoot && runtime.GOOS == "windows" {
		offerDesktopShortcut()
	}

	// Create ship metadata cache, coordinator and DCS comm server.
	cache := shipcache.New("shipcache.json")
	log.Printf("ship cache: %d vessels loaded", cache.Size())

	cfg.RLock()
	hookPort := cfg.DCS.HookPort
	cfg.RUnlock()

	// Use a closure to break the circular dependency: the DCS server needs the
	// coordinator's handler, and the coordinator needs the DCS server. The
	// closure captures the coord pointer which is set before any connections
	// arrive.
	var coord *coordinator.Coordinator
	dcsServer := dcscomm.NewServer(hookPort, func(msg dcscomm.InboundMessage) {
		coord.OnHookMessage(msg)
	})
	coord = coordinator.New(cfg, dcsServer, cache)

	// Start TCP server for DCS hook.
	go func() {
		if err := dcsServer.Start(); err != nil {
			// Listener close during shutdown is expected — don't treat it as fatal.
			var opErr *net.OpError
			if errors.As(err, &opErr) && opErr.Op == "accept" {
				log.Println("[DCS] TCP server stopped")
				return
			}
			log.Printf("[DCS] TCP server error: %v", err)
			os.Exit(1)
		}
	}()

	// Start web server.
	webServer := web.NewServer(cfg, coord, dcsServer)
	go func() {
		if err := webServer.Start(); err != nil {
			log.Printf("[WEB] server error: %v", err)
			os.Exit(1)
		}
	}()

	// Start coordinator loop.
	go coord.Start()

	cfg.RLock()
	webPort := cfg.Web.Port
	cfg.RUnlock()

	log.Printf("DCS AIS Traffic running (web: http://localhost:%d, hook: :%d)", webPort, hookPort)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down...")
	coord.Stop()
	dcsServer.Stop()
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

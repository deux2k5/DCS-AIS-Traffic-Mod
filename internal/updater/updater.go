package updater

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	githubRepo = "deux2k5/DCS-AIS-Traffic-Mod"
	apiURL     = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
)

// Release holds the GitHub API response fields we care about.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is a single file attached to a release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Status is returned by the update endpoint.
type Status struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// CheckLatest queries GitHub for the newest release and returns its tag + zip URL.
func CheckLatest() (*Release, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dcs-ais-traffic-updater")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse release: %w", err)
	}
	return &rel, nil
}

// Apply downloads the release zip, extracts the exe and lua hook, writes
// a restart batch script, launches it, and exits the current process.
// This function does not return on success.
func Apply(rel *Release) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("self-update only supported on Windows")
	}

	// Find the zip asset.
	var zipURL string
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".zip") {
			zipURL = a.BrowserDownloadURL
			break
		}
	}
	if zipURL == "" {
		return fmt.Errorf("no zip asset found in release %s", rel.TagName)
	}

	log.Printf("[UPDATE] downloading %s", zipURL)

	// Download to temp file.
	tmpZip, err := os.CreateTemp("", "ais-update-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmpZip.Name())

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(zipURL)
	if err != nil {
		tmpZip.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(tmpZip, resp.Body); err != nil {
		tmpZip.Close()
		return fmt.Errorf("saving zip: %w", err)
	}
	tmpZip.Close()

	log.Printf("[UPDATE] downloaded %s, extracting", filepath.Base(tmpZip.Name()))

	// Create staging directory.
	stageDir, err := os.MkdirTemp("", "ais-update-stage-*")
	if err != nil {
		return err
	}

	// Extract zip.
	if err := extractZip(tmpZip.Name(), stageDir); err != nil {
		return fmt.Errorf("extracting zip: %w", err)
	}

	// Find the exe and lua hook in the extracted contents.
	exePath, _ := filepath.Abs(os.Args[0])
	exeDir := filepath.Dir(exePath)

	newExe := filepath.Join(stageDir, "DCS AIS Traffic.exe")
	newLua := filepath.Join(stageDir, "lua", "AISTrafficHook.lua")

	if _, err := os.Stat(newExe); err != nil {
		return fmt.Errorf("new exe not found in zip: %w", err)
	}

	// Build restart batch script.
	batPath := filepath.Join(os.TempDir(), "ais-update-restart.bat")
	luaCopyLine := ""
	if _, err := os.Stat(newLua); err == nil {
		luaDst := filepath.Join(exeDir, "lua", "AISTrafficHook.lua")
		luaCopyLine = fmt.Sprintf("copy /y \"%s\" \"%s\"\r\n", newLua, luaDst)
	}

	bat := fmt.Sprintf(
		"@echo off\r\n"+
			"timeout /t 2 /nobreak >nul\r\n"+
			"taskkill /f /im \"DCS AIS Traffic.exe\" >nul 2>&1\r\n"+
			"timeout /t 1 /nobreak >nul\r\n"+
			"copy /y \"%s\" \"%s\"\r\n"+
			"%s"+
			"cd /d \"%s\"\r\n"+
			"start \"\" \"%s\"\r\n"+
			"timeout /t 3 /nobreak >nul\r\n"+
			"rmdir /s /q \"%s\"\r\n"+
			"del \"%%~f0\"\r\n",
		newExe, exePath,
		luaCopyLine,
		exeDir, exePath,
		stageDir,
	)

	if err := os.WriteFile(batPath, []byte(bat), 0644); err != nil {
		return fmt.Errorf("writing restart script: %w", err)
	}

	log.Printf("[UPDATE] launching restart script, switching to %s", rel.TagName)

	// Launch the batch script detached and exit.
	cmd := exec.Command("cmd.exe", "/c", "start", "/b", batPath)
	cmd.Dir = exeDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching restart script: %w", err)
	}

	// Give the batch script a moment, then exit.
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
	return nil // unreachable
}

func extractZip(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target := filepath.Join(dest, f.Name)

		// Prevent zip slip.
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}

		os.MkdirAll(filepath.Dir(target), 0755)

		out, err := os.Create(target)
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

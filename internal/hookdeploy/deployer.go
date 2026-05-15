package hookdeploy

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed AISTrafficHook.lua
var hookTemplate string

// markerPrefix identifies files managed by this deployer. The full marker
// includes a content hash so we can detect when the hook needs updating.
const markerPrefix = "-- AIS-TRAFFIC-MANAGED"

// hookFilename is the name used inside Scripts/Hooks/.
const hookFilename = "AISTrafficHook.lua"

// portRe matches the TCP_PORT assignment line in the Lua source.
var portRe = regexp.MustCompile(`(AIS\.TCP_PORT\s*=\s*)(\d+)`)

// Deploy writes the Lua hook to <savedGamesPath>/Scripts/Hooks/AISTrafficHook.lua
// with the TCP port patched to the given value. Creates intermediate directories
// if they don't exist. Returns an error if savedGamesPath doesn't exist.
func Deploy(savedGamesPath string, port int) error {
	// Validate saved games path exists.
	info, err := os.Stat(savedGamesPath)
	if err != nil {
		return fmt.Errorf("saved games path %q: %w", savedGamesPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("saved games path %q is not a directory", savedGamesPath)
	}

	// Build the hook content with the correct port.
	content := portRe.ReplaceAllString(hookTemplate, fmt.Sprintf("${1}%d", port))

	// Compute a hash of the final content for the ownership marker.
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))[:12]
	marker := fmt.Sprintf("%s v%s\n", markerPrefix, hash)

	// Prepend the marker to the content.
	final := marker + content

	// Ensure the Scripts/Hooks/ directory exists.
	hooksDir := filepath.Join(savedGamesPath, "Scripts", "Hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("creating hooks directory: %w", err)
	}

	// Write the file.
	dest := filepath.Join(hooksDir, hookFilename)
	if err := os.WriteFile(dest, []byte(final), 0644); err != nil {
		return fmt.Errorf("writing hook file: %w", err)
	}

	return nil
}

// Remove deletes the hook file only if it was deployed by this tool (has the
// ownership marker). Returns nil if the file doesn't exist or isn't managed.
func Remove(savedGamesPath string) error {
	dest := filepath.Join(savedGamesPath, "Scripts", "Hooks", hookFilename)

	data, err := os.ReadFile(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to remove
		}
		return err
	}

	// Safety check: only delete if we own this file.
	if !strings.HasPrefix(string(data), markerPrefix) {
		return fmt.Errorf("hook file exists but is not managed by AIS Traffic (no ownership marker); refusing to delete")
	}

	return os.Remove(dest)
}

// IsDeployed checks whether the hook is deployed, managed by us, and configured
// for the given port. Returns (deployed, nil) on success. If the file exists but
// isn't managed or has the wrong port, returns (false, nil).
func IsDeployed(savedGamesPath string, port int) (bool, error) {
	dest := filepath.Join(savedGamesPath, "Scripts", "Hooks", hookFilename)

	data, err := os.ReadFile(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	content := string(data)

	// Check ownership marker.
	if !strings.HasPrefix(content, markerPrefix) {
		return false, nil
	}

	// Check port matches.
	match := portRe.FindStringSubmatch(content)
	if match == nil {
		return false, nil
	}
	deployedPort := match[2]
	if deployedPort != fmt.Sprintf("%d", port) {
		return false, nil
	}

	// Check content hash matches current template (detects outdated hooks).
	patched := portRe.ReplaceAllString(hookTemplate, fmt.Sprintf("${1}%d", port))
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(patched)))[:12]
	expectedMarker := fmt.Sprintf("%s v%s", markerPrefix, hash)

	firstLine := strings.SplitN(content, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, expectedMarker) {
		return false, nil // outdated version
	}

	return true, nil
}

// HookPath returns the full path where the hook would be deployed.
func HookPath(savedGamesPath string) string {
	return filepath.Join(savedGamesPath, "Scripts", "Hooks", hookFilename)
}

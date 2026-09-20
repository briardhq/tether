//go:build windows

package device

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DriverlessIDs is the hardware id of every attached adapter this table knows that Windows has no
// driver for — the `VID_10C4&PID_EA60` form, which is what a driver update is matched on.
//
// It is what the install verb asks before offering to fetch anything, and it is empty in the
// normal case: a stick whose driver is in the box, or already installed, needs none of this.
func DriverlessIDs() []string {
	_, unusable, err := adapters()
	if err != nil {
		return nil
	}
	var ids []string
	for _, n := range unusable {
		// A device that has a port and is merely stopped does not want a driver; it wants
		// enabling, and fetching one for it would be the wrong answer loudly.
		if n.port != "" {
			continue
		}
		ids = append(ids, hardwareID(n.device.Vendor, n.device.Product))
	}
	return ids
}

// InstallDrivers fetches and installs the drivers for those hardware ids, and returns when the
// adapter has one or when it is clear it will not get one.
//
// ⚠️ **It runs through a one-shot scheduled task as SYSTEM, and that is not belt and braces.**
// Measured: `AcceptEula` and `CreateUpdateInstaller` return E_ACCESSDENIED to an ordinary
// elevated administrator, so an install verb that is merely elevated cannot do this directly.
// zigbee-herdsman is not the precedent here — PSWindowsUpdate is, whose `Invoke-WUJob` exists to
// do exactly this and by the same means.
func InstallDrivers(ids []string) error {
	dir := filepath.Join(programData(), "tether")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	script := filepath.Join(dir, "install-driver.ps1")
	result := filepath.Join(dir, "install-driver.status")
	// The script reports through a file because a scheduled task's own exit code says only that
	// PowerShell ran, not what it concluded, and the difference between "no driver was offered"
	// and "the task would not start" is the whole of what the caller has to tell an operator.
	body := "$ErrorActionPreference = 'Stop'\ntry {\n" + driverScript(ids) +
		"\n    'ok' | Set-Content -Encoding ascii " + quotePS(result) +
		"\n} catch {\n    \"failed: $($_.Exception.Message)\" | Set-Content -Encoding ascii " + quotePS(result) + "\n}\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", script, err)
	}
	defer os.Remove(script)
	_ = os.Remove(result)

	const task = "briard-tether-driver"
	defer exec.Command("schtasks", "/delete", "/tn", task, "/f").Run()
	create := exec.Command("schtasks", "/create", "/tn", task, "/ru", "SYSTEM", "/rl", "HIGHEST",
		"/sc", "once", "/st", "00:00", "/f",
		"/tr", `powershell -NoProfile -ExecutionPolicy Bypass -File "`+script+`"`)
	if out, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("creating the installer task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("schtasks", "/run", "/tn", task).CombinedOutput(); err != nil {
		return fmt.Errorf("running the installer task: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Measured at about a minute start to finish; the cap is generous because the alternative
	// to waiting is reporting a failure that is actually a slow download.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		b, err := os.ReadFile(result)
		if err != nil {
			continue
		}
		defer os.Remove(result)
		if text := strings.TrimSpace(string(b)); text != "ok" {
			return fmt.Errorf("%s", text)
		}
		return nil
	}
	return fmt.Errorf("the driver install did not finish within five minutes")
}

func programData() string {
	if dir := os.Getenv("ProgramData"); dir != "" {
		return dir
	}
	return `C:\ProgramData`
}

// quotePS wraps a path for PowerShell as a single-quoted literal, where the only escape is a
// doubled quote — so a path is never re-read as an expression.
func quotePS(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

package tui

import (
	"os/exec"
	"runtime"
)

// openBrowser hands a URL to the platform's handler without a shell, so the URL is never interpreted.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // Reap the launcher; its exit status says nothing about the browser.
	return nil
}

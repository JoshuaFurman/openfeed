// Command openfeed is a TUI-first RSS reader.
package main

import (
	"fmt"
	"os"

	"github.com/JoshuaFurman/openfeed/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "openfeed:", err)
		os.Exit(1)
	}
}

// run is separated from main so that every failure path returns an error
// instead of calling os.Exit, which would skip deferred cleanup.
//
// This is temporary scaffolding for v0.1: it reports what config.Load resolved
// so the package can be exercised before the store and TUI exist.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Println("config:  ", cfg.FilePath())
	fmt.Println("database:", cfg.DatabasePath())
	fmt.Println("log:     ", cfg.LogPath())
	fmt.Println("feeds:   ", len(cfg.Feeds))
	for _, f := range cfg.Feeds {
		fmt.Printf("  %-20s %s\n", f.Name, f.URL)
	}
	return nil
}

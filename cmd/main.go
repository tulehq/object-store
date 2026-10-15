// Command tule-object-store serves an S3-compatible API over objects stored
// on the local filesystem.
package main

import (
	"fmt"
	"log"

	"tule.dev/object-store/internal/app"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (err error) {
	cfg, cfgErr := app.LoadConfig()
	if cfgErr != nil {
		return fmt.Errorf("failed to load config: %w", cfgErr)
	}

	a, appErr := app.New(cfg)
	if appErr != nil {
		return fmt.Errorf("failed to create application: %w", appErr)
	}

	if runErr := a.Run(); runErr != nil {
		return fmt.Errorf("app exited with error: %w", runErr)
	}

	return
}

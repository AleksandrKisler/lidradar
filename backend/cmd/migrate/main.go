// Command migrate applies LidRadar database migrations.
package main

import (
	"context"
	"os"

	"lidradar/backend/platform/bootstrap"
)

func main() {
	os.Exit(bootstrap.Run(context.Background(), "lidradar-migrate", os.Stderr, bootstrap.Complete))
}

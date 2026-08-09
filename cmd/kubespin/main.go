// Command kubespin provisions and manages Kubernetes clusters across
// EKS, GKE, and AKS.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/GitOpsHub/kubespin/internal/cli"
)

func main() {
	// Provisioning runs for tens of minutes; a cancellable context lets an
	// interrupted run release its lease instead of leaving a cluster wedged.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.NewRootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "kubespin: %v\n", err)
		os.Exit(1)
	}
}

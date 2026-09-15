// SPDX-License-Identifier: GPL-3.0-only
package main

import (
	"context"
	"inductor/internal/inductor"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(inductor.Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

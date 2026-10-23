//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

func prepareConsole() {}
func serviceContext(_ string) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
func isMaintenance(s string) bool {
	return s == "start" || s == "stop" || s == "restart" || s == "uninstall"
}
func maintenance(_ []string) error { return errors.New("installation_management_windows_only") }

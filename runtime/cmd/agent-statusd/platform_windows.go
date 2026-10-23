//go:build windows

package main

import (
	"agent-status-runtime/internal/install"
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"os/signal"
)

func prepareConsole() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		for _, arg := range os.Args[2:] {
			if arg == "--background" || arg == "--background=true" {
				return
			}
		}
	}
	attach := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	_, _, _ = attach.Call(^uintptr(0))
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil {
		os.Stdout = os.NewFile(uintptr(h), "stdout")
	}
	if h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE); err == nil {
		os.Stderr = os.NewFile(uintptr(h), "stderr")
	}
}
func serviceContext(dir string) (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	event, err := windows.CreateEvent(nil, 1, 0, windows.StringToUTF16Ptr(install.EventName(dir)))
	if err == nil {
		go func() {
			defer windows.CloseHandle(event)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				result, _ := windows.WaitForSingleObject(event, 250)
				if result == windows.WAIT_OBJECT_0 {
					cancel()
					return
				}
			}
		}()
	}
	return ctx, cancel
}
func isMaintenance(s string) bool {
	return s == "start" || s == "stop" || s == "restart" || s == "uninstall"
}
func maintenance(args []string) error {
	if len(args) != 1 {
		return errors.New("maintenance_commands_take_no_arguments")
	}
	switch args[0] {
	case "start":
		return install.Start()
	case "stop":
		return install.Stop()
	case "restart":
		if err := install.Stop(); err != nil {
			return err
		}
		return install.Start()
	case "uninstall":
		return install.Uninstall()
	}
	return errors.New("unknown_command")
}

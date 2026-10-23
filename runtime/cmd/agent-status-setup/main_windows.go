//go:build windows

package main

import (
	"agent-status-runtime/internal/install"
	"embed"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

//go:embed payload
var files embed.FS

func main() {
	body := "Agent Status Runtime 已安装，并在后台运行。登录后将自动启动。"
	payload, err := files.ReadFile("payload/agent-statusd.exe")
	if err == nil {
		err = install.Install(payload)
	}
	flags := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
	if err != nil {
		body = "安装失败：" + err.Error()
		flags = windows.MB_OK | windows.MB_ICONERROR
	}
	if len(os.Args) < 2 || os.Args[1] != "--silent" {
		_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr(body), windows.StringToUTF16Ptr("Agent Status Runtime"), flags)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

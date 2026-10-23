//go:build windows

package install

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func Paths() (string, string, error) {
	local := os.Getenv("LOCALAPPDATA")
	config, err := os.UserConfigDir()
	if err != nil || !filepath.IsAbs(local) || !filepath.IsAbs(config) {
		return "", "", errors.New("user_directories_unavailable")
	}
	return filepath.Join(local, "Programs", "AgentStatusRuntime"), filepath.Join(config, "AgentStatusRuntime"), nil
}
func EventName(dir string) string {
	full, _ := filepath.Abs(dir)
	h := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(full))))
	return "Local\\AgentStatusRuntime-stop-" + hex.EncodeToString(h[:16])
}
func TaskName() string {
	user, _ := windows.GetCurrentProcessToken().GetTokenUser()
	if user == nil {
		return "AgentStatusRuntime"
	}
	target, _, _ := Paths()
	h := sha256.Sum256([]byte(strings.ToLower(target)))
	return "AgentStatusRuntime-" + user.User.Sid.String() + "-" + hex.EncodeToString(h[:6])
}
func commandOutput(b []byte) string {
	if len(b) == 0 || utf8.Valid(b) {
		return strings.TrimSpace(string(b))
	}
	decoded := make([]uint16, len(b))
	n, err := windows.MultiByteToWideChar(windows.GetACP(), 0, &b[0], int32(len(b)), &decoded[0], int32(len(decoded)))
	if err != nil {
		return "system_command_failed"
	}
	return strings.TrimSpace(windows.UTF16ToString(decoded[:n]))
}
func command(args ...string) error {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scheduled_task_failed (%s): %w: %s", args[0], err, commandOutput(output))
	}
	return nil
}
func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func TaskXML(exe, dir, sid string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + escape(sid) + `</UserId></LogonTrigger><TimeTrigger><Repetition><Interval>PT1M</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition><StartBoundary>` + time.Now().Format("2006-01-02T15:04:05") + `</StartBoundary><Enabled>true</Enabled></TimeTrigger></Triggers><Principals><Principal id="Author"><UserId>` + escape(sid) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Enabled>false</Enabled></Settings><Actions Context="Author"><Exec><Command>` + escape(exe) + `</Command><Arguments>serve --background --data-dir &quot;` + escape(dir) + `&quot;</Arguments><WorkingDirectory>` + escape(filepath.Dir(exe)) + `</WorkingDirectory></Exec></Actions></Task>`
}
func Register(exe, dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "agent-status-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	document := strings.Replace(TaskXML(exe, dir, user.User.Sid.String()), `encoding="UTF-8"`, `encoding="UTF-16"`, 1)
	units := utf16.Encode([]rune(document))
	encoded := []byte{0xff, 0xfe}
	for _, v := range units {
		encoded = append(encoded, byte(v), byte(v>>8))
	}
	_, err = f.Write(encoded)
	_ = f.Close()
	if err != nil {
		return err
	}
	return command("/Create", "/TN", TaskName(), "/XML", f.Name(), "/F")
}

type connection struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
	Epoch string `json:"epoch"`
}

func load(dir string) (connection, error) {
	var c connection
	b, err := os.ReadFile(filepath.Join(dir, "private", "connection.json"))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}
func process(pid int, exe string) (windows.Handle, error) {
	if pid <= 0 {
		return 0, errors.New("invalid_runtime_pid")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	b := make([]uint16, 32768)
	n := uint32(len(b))
	err = windows.QueryFullProcessImageName(h, 0, &b[0], &n)
	if err != nil || !strings.EqualFold(filepath.Clean(windows.UTF16ToString(b[:n])), filepath.Clean(exe)) {
		windows.CloseHandle(h)
		return 0, errors.New("runtime_process_path_mismatch")
	}
	return h, nil
}
func StopProcess(dir, exe string) error {
	c, err := load(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	h, err := process(c.PID, exe)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	event, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, windows.StringToUTF16Ptr(EventName(dir)))
	if err == nil {
		_ = windows.SetEvent(event)
		_ = windows.CloseHandle(event)
	}
	result, _ := windows.WaitForSingleObject(h, 5000)
	if result == uint32(windows.WAIT_TIMEOUT) {
		if err := windows.TerminateProcess(h, 1); err != nil {
			return err
		}
		_, _ = windows.WaitForSingleObject(h, 5000)
	}
	return nil
}
func Start() error {
	release, err := deploymentLock()
	if err != nil {
		return err
	}
	defer release()
	if err := command("/Change", "/TN", TaskName(), "/ENABLE"); err != nil {
		return err
	}
	return command("/Run", "/TN", TaskName())
}
func Stop() error {
	release, lockErr := deploymentLock()
	if lockErr != nil {
		return lockErr
	}
	defer release()
	exe, dir, err := Paths()
	if err != nil {
		return err
	}
	if err = command("/Change", "/TN", TaskName(), "/DISABLE"); err != nil {
		return err
	}
	return StopProcess(dir, filepath.Join(exe, "agent-statusd.exe"))
}
func Healthy(dir string) error {
	c, err := load(dir)
	if err != nil {
		return err
	}
	u, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(c.URL, "http://"))
	if err != nil || !u.IP.IsLoopback() {
		return errors.New("invalid_runtime_address")
	}
	req, err := http.NewRequest("GET", c.URL+"/v3/info", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var info struct {
		Schema int    `json:"schema"`
		Epoch  string `json:"epoch"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info)
	if err != nil || resp.StatusCode != 200 || info.Schema != 3 || info.Epoch != c.Epoch {
		return errors.New("runtime_health_check_failed")
	}
	req, err = http.NewRequest("GET", c.URL+"/v3/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	healthResponse, err := client.Do(req)
	if err != nil {
		return err
	}
	defer healthResponse.Body.Close()
	var health struct {
		LastScanAt time.Time `json:"last_scan_at"`
	}
	if json.NewDecoder(io.LimitReader(healthResponse.Body, 4<<20)).Decode(&health) != nil || healthResponse.StatusCode != 200 || health.LastScanAt.IsZero() {
		return errors.New("runtime_not_initialized")
	}
	return nil
}

func deploymentLock() (func(), error) {
	runtime.LockOSThread()
	name := "Local" + string(rune(92)) + TaskName() + "-install"
	mutex, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(name))
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		runtime.UnlockOSThread()
		return nil, err
	}
	state, err := windows.WaitForSingleObject(mutex, 0)
	if err != nil || state == uint32(windows.WAIT_TIMEOUT) {
		windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
		return nil, errors.New("installation_in_progress")
	}
	return func() { _ = windows.ReleaseMutex(mutex); _ = windows.CloseHandle(mutex); runtime.UnlockOSThread() }, nil
}
func deploy(exe string, payload []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".runtime-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(payload); err == nil {
		err = tmp.Chmod(0700)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}
func Install(payload []byte) (result error) {
	release, err := deploymentLock()
	if err != nil {
		return err
	}
	defer release()
	if len(payload) < 2 || string(payload[:2]) != "MZ" {
		return errors.New("installer_payload_missing")
	}
	target, dir, err := Paths()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(target, 0700); err != nil {
		return err
	}
	exe := filepath.Join(target, "agent-statusd.exe")
	generationPath := filepath.Join(target, "generation")
	oldPayload, _ := os.ReadFile(exe)
	oldGeneration, _ := os.ReadFile(generationPath)
	existing := command("/Query", "/TN", TaskName()) == nil
	registered := false
	defer func() {
		if result == nil || !registered {
			return
		}
		_ = StopProcess(dir, exe)
		if existing && len(oldPayload) > 0 {
			if restoreErr := deploy(exe, oldPayload); restoreErr != nil {
				result = fmt.Errorf("%w; restore: %v", result, restoreErr)
				return
			}
			if len(oldGeneration) > 0 {
				_ = os.WriteFile(generationPath, oldGeneration, 0600)
			}
			if restoreErr := Start(); restoreErr != nil {
				result = fmt.Errorf("%w; restart: %v", result, restoreErr)
			}
		} else {
			_ = command("/Delete", "/TN", TaskName(), "/F")
			_ = os.Remove(exe)
			_ = os.Remove(generationPath)
			_ = os.Remove(target)
		}
	}()
	if err = Register(exe, dir); err != nil {
		return err
	}
	registered = true
	if err = StopProcess(dir, exe); err != nil {
		return err
	}
	if err = deploy(exe, payload); err != nil {
		return err
	}
	generation := make([]byte, 16)
	if _, err = rand.Read(generation); err != nil {
		return err
	}
	if err = os.WriteFile(generationPath, []byte(hex.EncodeToString(generation)), 0600); err != nil {
		return err
	}
	if err = Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if Healthy(dir) == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("installed_but_runtime_not_healthy")
}
func Uninstall() error {
	release, lockErr := deploymentLock()
	if lockErr != nil {
		return lockErr
	}
	defer release()
	target, dir, err := Paths()
	if err != nil {
		return err
	}
	if err = Stop(); err != nil {
		return err
	}
	if err = command("/Delete", "/TN", TaskName(), "/F"); err != nil {
		return err
	}

	// The CLI may be executing the file being removed. A hidden OS helper waits
	// for this exact process to exit, then removes only the known executable.
	current, _ := os.Executable()
	exe := filepath.Join(target, "agent-statusd.exe")
	if strings.EqualFold(filepath.Clean(current), filepath.Clean(exe)) {
		quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

		generationPath := filepath.Join(target, "generation")
		generation, err := os.ReadFile(generationPath)
		if err != nil {
			return err
		}
		lockName := "Local" + string(rune(92)) + TaskName() + "-install"
		code := fmt.Sprintf("$ErrorActionPreference='Stop'; $p=Get-Process -Id %d -ErrorAction SilentlyContinue; if($p){$p.WaitForExit()}; $m=[Threading.Mutex]::new($false,%s); try { try {$null=$m.WaitOne()} catch [Threading.AbandonedMutexException] {}; if((Get-Content -Raw -LiteralPath %s) -eq %s){Remove-Item -LiteralPath %s -Force; Remove-Item -LiteralPath %s -Force; if((Get-ChildItem -LiteralPath %s -Force | Measure-Object).Count -eq 0){Remove-Item -LiteralPath %s}} } finally {$m.ReleaseMutex();$m.Dispose()}", os.Getpid(), quote(lockName), quote(generationPath), quote(string(generation)), quote(exe), quote(generationPath), quote(target), quote(target))

		units := utf16.Encode([]rune(code))
		raw := make([]byte, len(units)*2)
		for i, v := range units {
			raw[2*i] = byte(v)
			raw[2*i+1] = byte(v >> 8)
		}
		cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err = cmd.Start(); err != nil {
			return err
		}
		_ = cmd.Process.Release()
	} else {
		if err = os.Remove(exe); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(filepath.Join(target, "generation"))
		_ = os.Remove(target)
	}
	for _, name := range []string{"connection.json", "ingest.json"} {
		_ = os.Remove(filepath.Join(dir, "private", name))
	}
	return nil
}

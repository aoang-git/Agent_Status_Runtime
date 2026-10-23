//go:build windows

package install

import (
	"encoding/xml"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskUsesUserLoginAndBoundedRecovery(t *testing.T) {
	raw := TaskXML("C:\\Apps & Tools\\core.exe", "C:\\Users\\test data", "S-1-test")
	var task struct {
		Triggers struct {
			LogonTrigger struct {
				UserID string `xml:"UserId"`
			}
			TimeTrigger struct {
				Repetition struct {
					Interval string
					Duration string
				}
			}
		}
		Principals struct {
			Principal struct {
				LogonType string
				RunLevel  string
			}
		}
		Settings struct {
			MultipleInstancesPolicy    string
			ExecutionTimeLimit         string
			Enabled                    bool
			DisallowStartIfOnBatteries bool
		}
		Actions struct {
			Exec struct {
				Command   string
				Arguments string
			}
		}
	}
	if err := xml.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatal(err)
	}
	if task.Triggers.LogonTrigger.UserID != "S-1-test" || task.Triggers.TimeTrigger.Repetition.Interval != "PT1M" || task.Triggers.TimeTrigger.Repetition.Duration != "" {
		t.Fatal("triggers")
	}
	if task.Principals.Principal.LogonType != "InteractiveToken" || task.Principals.Principal.RunLevel != "LeastPrivilege" || task.Settings.ExecutionTimeLimit != "PT0S" || task.Settings.MultipleInstancesPolicy != "IgnoreNew" || task.Settings.Enabled {
		t.Fatal("task settings")
	}
	if !strings.Contains(task.Actions.Exec.Command, "Apps & Tools") || !strings.Contains(task.Actions.Exec.Arguments, "--background") {
		t.Fatal("command escaping")
	}
}
func TestInstallIdentityAndStopRefuseUnrelatedProcess(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	first := TaskName()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if first == TaskName() {
		t.Fatal("install identities overlap")
	}
	if _, err := process(os.Getpid(), filepath.Join(t.TempDir(), "unrelated.exe")); err == nil {
		t.Fatal("unrelated process accepted")
	}
	if EventName("C:\\Example") != EventName("c:\\example") {
		t.Fatal("event identity not normalized")
	}
}

func TestDeploymentLockAllowsNestedMaintenance(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	first, err := deploymentLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := deploymentLock()
	if err != nil {
		t.Fatal(err)
	}
	second()
}

func TestCommandErrorsAreReadable(t *testing.T) {
	if commandOutput([]byte(" access denied ")) != "access denied" {
		t.Fatal("ASCII error")
	}
	if windows.GetACP() == 936 && commandOutput([]byte{0xb4, 0xed, 0xce, 0xf3}) != "错误" {
		t.Fatal("system codepage error")
	}
}

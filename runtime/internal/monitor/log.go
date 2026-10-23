package monitor

import (
	"os"
	"sync"
)

type DiagnosticLog struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func NewDiagnosticLog(path string) (*DiagnosticLog, error) {
	l := &DiagnosticLog{path: path}
	if info, err := os.Stat(path); err == nil && info.Size() >= 1<<20 {
		_ = os.Remove(path + ".1")
		if err = os.Rename(path, path+".1"); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	l.file = f
	return l, nil
}
func (l *DiagnosticLog) Close() error { l.mu.Lock(); defer l.mu.Unlock(); return l.file.Close() }
func (l *DiagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	original := len(p)
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	if info, err := l.file.Stat(); err == nil && info.Size()+int64(len(p)) > 1<<20 {
		if err = l.file.Close(); err != nil {
			return 0, err
		}
		_ = os.Remove(l.path + ".1")
		if err = os.Rename(l.path, l.path+".1"); err != nil {
			return 0, err
		}
		l.file, err = os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return 0, err
		}
	}
	_, err := l.file.Write(p)
	if err != nil {
		return 0, err
	}
	return original, nil
}

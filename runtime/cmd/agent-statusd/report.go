package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"agent-status-runtime/internal/monitor"
)

func loadConnection(dir, name string) (connection, error) {
	var conn connection
	f, err := os.Open(filepath.Join(dir, "private", name))
	if err != nil {
		return conn, errors.New("connection_unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err != nil {
		return conn, errors.New("connection_unavailable")
	}
	if len(b) > 16<<10 || json.Unmarshal(b, &conn) != nil {
		return conn, errors.New("invalid_connection")
	}
	u, err := url.Parse(conn.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return conn, errors.New("invalid_connection")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || conn.Token == "" || len(conn.Token) > 4096 || strings.ContainsFunc(conn.Token, unicode.IsControl) {
		return conn, errors.New("invalid_connection")
	}
	conn.URL = strings.TrimSuffix(conn.URL, "/")
	return conn, nil
}

func report(dir string, bestEffort, validateOnly bool) error {
	err := sendReport(dir, validateOnly, bestEffort)
	if bestEffort {
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		return nil
	}
	return err
}

func sendReport(dir string, validateOnly, silent bool) error {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, monitor.MaxInput+1))
	if err != nil {
		return errors.New("stdin_read_failed")
	}
	input, err := monitor.DecodeInput(b)
	if err != nil {
		return err
	}
	if validateOnly {
		if !silent {
			fmt.Fprintln(os.Stdout, `{"schema":2,"valid":true}`)
		}
		return nil
	}
	b, _ = json.Marshal(input)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := loadConnection(dir, "ingest.json")
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", conn.URL+"/v2/ingest", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+conn.Token)
		resp, err := client.Do(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
			return errors.New("report_unavailable")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		resp.Body.Close()
		if resp.StatusCode == 401 && attempt == 0 {
			continue
		}
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			return fmt.Errorf("report_http_%d", resp.StatusCode)
		}
		if readErr != nil || len(body) > 4096 {
			return errors.New("invalid_ack")
		}
		var ack struct {
			Schema int    `json:"schema"`
			ID     int64  `json:"id"`
			Epoch  string `json:"epoch"`
		}
		if json.Unmarshal(body, &ack) != nil || ack.Schema != 2 || ack.ID < 1 || ack.Epoch == "" {
			return errors.New("invalid_ack")
		}
		// Hook-friendly mode is handled without emitting acknowledgement content.
		if !silent {
			_, err = os.Stdout.Write(body)
			return err
		}
		return nil
	}
	return errors.New("report_unavailable")
}

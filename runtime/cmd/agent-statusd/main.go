package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"agent-status-runtime/internal/monitor"
)

type rootsFlag []monitor.Root

func (r *rootsFlag) String() string { return fmt.Sprint([]monitor.Root(*r)) }
func (r *rootsFlag) Set(s string) error {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 || parts[1] == "" {
		return errors.New("root must be agent=path")
	}
	*r = append(*r, monitor.Root{Agent: parts[0], Path: parts[1]})
	return nil
}

type connection struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: agent-statusd serve|status|doctor|watch|version [flags]")
	}
	if args[0] == "version" {
		fmt.Println(monitor.Version)
		return nil
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dataDir := flags.String("data-dir", filepath.Join(base, "AgentStatusRuntime"), "runtime-owned data directory")
	listen := flags.String("listen", "127.0.0.1:9120", "loopback address (serve)")
	interval := flags.Duration("interval", 5*time.Second, "fallback discovery scan interval")
	stale := flags.Duration("stale-after", 2*time.Minute, "active evidence freshness limit; never implies completion")
	var roots rootsFlag
	flags.Var(&roots, "root", "session root agent=path (repeatable; overrides automatic discovery)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if args[0] != "serve" {
		return client(args[0], *dataDir)
	}
	if *interval < 250*time.Millisecond {
		return errors.New("interval must be at least 250ms")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("listen address must be a loopback IP")
	}
	// A dedicated child prevents changing ACLs on a caller's existing directory.
	private := filepath.Join(*dataDir, "private")
	if err := os.MkdirAll(private, 0700); err != nil {
		return err
	}
	if err := monitor.ProtectDirectory(private); err != nil {
		return fmt.Errorf("protect runtime directory: %w", err)
	}
	dbPath := filepath.Join(private, "state.sqlite")
	release, err := monitor.AcquireLease(dbPath)
	if err != nil {
		return err
	}
	defer release()
	store, err := monitor.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if len(roots) == 0 {
		roots = monitor.DefaultRoots()
	}
	engine, err := monitor.NewEngine(store, roots, *stale)
	if err != nil {
		return err
	}
	if err := engine.Scan(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	token := monitor.NewToken()
	url := "http://" + listener.Addr().String()
	connFile := filepath.Join(private, "connection.json")
	b, _ := json.Marshal(connection{url, token})
	if err := os.WriteFile(connFile, b, 0600); err != nil {
		return err
	}
	defer os.Remove(connFile)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	server := &http.Server{Handler: monitor.Handler(engine, token), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- engine.Run(ctx, *interval) }()
	go func() { errorsCh <- server.Serve(listener) }()
	fmt.Fprintln(os.Stderr, "Agent Status Runtime", monitor.Version, "passive", url)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = server.Shutdown(shutdownCtx)
	// SSE handlers inherit request contexts, which Shutdown alone does not cancel.
	_ = server.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func client(command, dataDir string) error {
	path := "/v1/status"
	switch command {
	case "status":
	case "doctor":
		path = "/v1/health"
	case "watch":
		path = "/v1/events"
	default:
		return errors.New("unknown command")
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "private", "connection.json"))
	if err != nil {
		return errors.New("runtime connection unavailable; start 'agent-statusd serve' first")
	}
	var conn connection
	if json.Unmarshal(b, &conn) != nil {
		return errors.New("invalid connection file")
	}
	req, err := http.NewRequest("GET", conn.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+conn.Token)
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	if command == "watch" {
		httpClient.Timeout = 0
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("runtime returned HTTP %d", response.StatusCode)
	}
	_, err = io.Copy(os.Stdout, response.Body)
	return err
}

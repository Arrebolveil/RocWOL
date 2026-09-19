package main

import (
	"bytes"
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
	"syscall"
	"time"

	"hubu.wiki/roc-wol/internal/gateway"
)

const usage = `roc-wol — authenticated remote wake gateway

Usage:
  roc-wol serve --config PATH
  roc-wol pair-code --socket /run/roc-wol/admin.sock --ttl 10m
  roc-wol devices list --socket /run/roc-wol/admin.sock
  roc-wol devices revoke --socket /run/roc-wol/admin.sock --id DEVICE_ID
  roc-wol devices admin --socket /run/roc-wol/admin.sock --id DEVICE_ID --enabled=true|false
  roc-wol --help

Administration uses a local, filesystem-protected Unix socket only.
Pair codes are shown once. Device lists are JSON; no public keys are printed.
`

func main() {
	if e := run(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "roc-wol:", e)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "serve":
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		config := f.String("config", "", "required JSON configuration path")
		if e := f.Parse(args[1:]); e != nil {
			if errors.Is(e, flag.ErrHelp) {
				return nil
			}
			return e
		}
		if *config == "" || f.NArg() != 0 {
			return errors.New("serve requires --config PATH")
		}
		return serve(*config)
	case "pair-code":
		f := flag.NewFlagSet("pair-code", flag.ContinueOnError)
		socket := f.String("socket", "/run/roc-wol/admin.sock", "local administration socket")
		ttl := f.String("ttl", "10m", "single-use pairing code lifetime")
		if e := f.Parse(args[1:]); e != nil {
			if errors.Is(e, flag.ErrHelp) {
				return nil
			}
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		data, e := admin(*socket, "POST", "/pair-code", map[string]string{"ttl": *ttl})
		if e != nil {
			return e
		}
		var v struct {
			Code string `json:"code"`
		}
		if e = json.Unmarshal(data, &v); e != nil {
			return e
		}
		fmt.Println(v.Code)
		return nil
	case "devices":
		if len(args) < 2 {
			return errors.New("devices requires list, revoke or admin")
		}
		f := flag.NewFlagSet("devices "+args[1], flag.ContinueOnError)
		socket := f.String("socket", "/run/roc-wol/admin.sock", "local administration socket")
		id := f.String("id", "", "device ID to administer")
		enabled := f.String("enabled", "", "explicit true or false device-administration capability")
		if e := f.Parse(args[2:]); e != nil {
			if errors.Is(e, flag.ErrHelp) {
				return nil
			}
			return e
		}
		if f.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		var data []byte
		var e error
		switch args[1] {
		case "list":
			data, e = admin(*socket, "GET", "/devices", nil)
		case "revoke":
			if *id == "" {
				return errors.New("revoke requires --id")
			}
			data, e = admin(*socket, "POST", "/devices/revoke", map[string]string{"deviceId": *id})
		case "admin":
			if *id == "" || (*enabled != "true" && *enabled != "false") {
				return errors.New("admin requires --id and --enabled=true|false")
			}
			data, e = admin(*socket, "POST", "/devices/admin", map[string]any{"deviceId": *id, "enabled": *enabled == "true"})
		default:
			return errors.New("devices requires list, revoke or admin")
		}
		if e != nil {
			return e
		}
		fmt.Print(string(data))
		return nil
	default:
		return errors.New("unknown command; use --help")
	}
}
func admin(socket, method, path string, payload any) ([]byte, error) {
	var b bytes.Buffer
	if payload != nil {
		if e := json.NewEncoder(&b).Encode(payload); e != nil {
			return nil, e
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, e := http.NewRequest(method, "http://localhost"+path, &b)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return nil, e
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("administration request failed (HTTP %d)", resp.StatusCode)
	}
	return data, nil
}
func serve(path string) error {
	cfg, e := gateway.LoadConfig(path)
	if e != nil {
		return e
	}
	g, e := gateway.New(cfg)
	if e != nil {
		return e
	}
	defer g.Close()
	adminServer, adminListener, e := g.AdminServer()
	if e != nil {
		return e
	}
	defer adminListener.Close()
	publicListener, e := net.Listen("tcp", cfg.Listen)
	if e != nil {
		return e
	}
	defer publicListener.Close()
	server := g.HTTPServer()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go g.Auth.Sweep(ctx)
	errs := make(chan error, 2)
	go func() { errs <- adminServer.Serve(adminListener) }()
	go func() { errs <- server.Serve(publicListener) }()
	select {
	case <-ctx.Done():
	case e = <-errs:
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	_ = adminServer.Shutdown(shutdown)
	_ = server.Close()
	_ = adminServer.Close()
	return e
}

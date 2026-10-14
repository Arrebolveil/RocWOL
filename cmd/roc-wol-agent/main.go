package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"hubu.wiki/roc-wol/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "roc-wol-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("roc-wol-agent", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "Required absolute or relative path to the agent JSON configuration")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: roc-wol-agent --config PATH\n\nLoopback-only mTLS router agent with configured-target wake and optional LuCI proxy.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		flags.Usage()
		return errors.New("--config PATH is required; positional arguments are not supported")
	}
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	a, err := agent.New(cfg, nil)
	if err != nil {
		return err
	}
	defer a.Close()
	server := a.HTTPServer()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS("", "") }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// Close HTTP readers first so an incomplete upload cannot hold shutdown
		// open. Agent cancellation records interrupted commands as unknown and
		// closes upgraded streams before closing its database.
		serverErr := server.Close()
		if err := a.Close(); err != nil {
			return err
		}
		return serverErr
	}
}

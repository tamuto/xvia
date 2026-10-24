package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/tamuto/xvia/internal/agent"
	"github.com/tamuto/xvia/internal/config"
	"github.com/tamuto/xvia/internal/logx"
)

func runAgent(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("xvia agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socket := fs.String("socket", config.Env(config.EnvSocket, config.HostSocketPath()), "")
	bind := fs.String("bind", config.Env(config.EnvBind, "127.0.0.1"), "")
	allow := fs.String("allow-ports", config.Env(config.EnvAllowPorts, config.DefaultAllowPorts), "")
	maxConns := fs.Int("max-conns", 1024, "")
	level := fs.String("log-level", config.Env(config.EnvLogLevel, "info"), "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, usageText)
			return 0
		}
		return usageError(stderr, err)
	}
	if fs.NArg() > 0 {
		return usageError(stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	lv, err := logx.ParseLevel(*level)
	if err != nil {
		return usageError(stderr, err)
	}
	ip, loopback, err := config.ParseBind(*bind)
	if err != nil {
		return usageError(stderr, err)
	}
	ports, err := config.ParsePortRange(*allow)
	if err != nil {
		return usageError(stderr, err)
	}
	if *maxConns < 1 || *maxConns > 65536 {
		return usageError(stderr, fmt.Errorf("invalid --max-conns %d (want 1-65536)", *maxConns))
	}
	log := logx.New(stderr, lv, true)
	if !loopback {
		log.Warnf("binding forwarded ports to %s: they are reachable from other machines on the network", ip)
	}

	a, err := agent.Listen(agent.Config{
		SocketPath: *socket,
		Bind:       ip,
		AllowPorts: ports,
		MaxConns:   *maxConns,
		Software:   softwareVersion(),
		Log:        log,
	})
	if err != nil {
		log.Errorf("%v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Serve(ctx); err != nil {
		log.Errorf("%v", err)
		return 1
	}
	log.Infof("agent stopped")
	return 0
}

// Command xvia forwards TCP ports of development servers running inside a
// container to the host, using only a shared Unix domain socket.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tamuto/xvia/internal/config"
	"github.com/tamuto/xvia/internal/logx"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func softwareVersion() string {
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return version
}

const usageText = `xvia - Cross-Environment Port Forwarding

Usage:
  xvia agent [options]                 run the host agent
  xvia [options] [--] <command> [args] run a command and forward its ports
  xvia version                         print the version
  xvia help                            show this help

Run options (must come before the command):
  -p, --port PORT        forward PORT even if it is not detected (repeatable)
      --socket PATH      agent socket (env XVIA_SOCKET, default /run/xvia/agent.sock
                         or the host agent socket if that exists)
      --no-detect        disable automatic port detection
      --poll-interval D  port detection interval (env XVIA_POLL_INTERVAL, default 500ms)
      --log-level LEVEL  debug, info, warn or error (env XVIA_LOG_LEVEL, default info)
  -v, --version          print the version
  -h, --help             show this help

Agent options:
      --socket PATH      socket to listen on (env XVIA_SOCKET,
                         default $XDG_RUNTIME_DIR/xvia/agent.sock)
      --bind ADDR        host address for forwarded ports (env XVIA_BIND, default 127.0.0.1)
      --allow-ports R    ports clients may register (env XVIA_ALLOW_PORTS, default 1024-65535)
      --max-conns N      concurrent forwarded connections (default 1024)
      --log-level LEVEL  debug, info, warn or error (env XVIA_LOG_LEVEL, default info)

Everything after the first non-option argument (or after "--") belongs to the
command. Use "xvia -- agent" to run a command that is literally named "agent".

Examples:
  xvia agent
  xvia pnpm run dev
  xvia --port 5173 --port 8000 pnpm run dev
  xvia --socket /run/xvia/agent.sock -- python3 -m http.server 8000 --bind 127.0.0.1
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "agent":
			return runAgent(args[1:], stderr)
		case "version":
			fmt.Fprintf(stdout, "xvia %s\n", softwareVersion())
			return 0
		case "help":
			fmt.Fprint(stdout, usageText)
			return 0
		}
	}

	fs := flag.NewFlagSet("xvia", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ports portList
	fs.Var(&ports, "port", "")
	fs.Var(&ports, "p", "")
	socket := fs.String("socket", config.Env(config.EnvSocket, ""), "")
	noDetect := fs.Bool("no-detect", false, "")
	interval := fs.String("poll-interval", config.Env(config.EnvPollInterval, "500ms"), "")
	level := fs.String("log-level", config.Env(config.EnvLogLevel, "info"), "")
	showVersion := fs.Bool("version", false, "")
	fs.BoolVar(showVersion, "v", false, "")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageText)
			return 0
		}
		return usageError(stderr, err)
	}
	if *showVersion {
		fmt.Fprintf(stdout, "xvia %s\n", softwareVersion())
		return 0
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	lv, err := logx.ParseLevel(*level)
	if err != nil {
		return usageError(stderr, err)
	}
	poll, err := time.ParseDuration(*interval)
	if err != nil || poll < 50*time.Millisecond || poll > time.Minute {
		return usageError(stderr, fmt.Errorf("invalid poll interval %q (want 50ms-1m)", *interval))
	}
	path := *socket
	if path == "" {
		path = config.ClientSocketPath()
	}
	if err := config.ValidateSocketPath(path); err != nil {
		return usageError(stderr, err)
	}
	return runCommand(runOptions{
		socket:   path,
		ports:    ports,
		detect:   !*noDetect,
		interval: poll,
		log:      logx.New(stderr, lv, false),
	}, fs.Args())
}

func usageError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "xvia: %v\nRun 'xvia --help' for usage.\n", err)
	return 2
}

// portList is a repeatable --port flag. It also accepts comma separated values.
type portList []int

func (p *portList) String() string {
	s := make([]string, len(*p))
	for i, v := range *p {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ",")
}

func (p *portList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		port, err := config.ParsePort(part)
		if err != nil {
			return err
		}
		*p = append(*p, port)
	}
	return nil
}

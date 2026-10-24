package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/tamuto/xvia/internal/client"
	"github.com/tamuto/xvia/internal/detector"
	"github.com/tamuto/xvia/internal/logx"
	"github.com/tamuto/xvia/internal/process"
)

type runOptions struct {
	socket   string
	ports    []int
	detect   bool
	interval time.Duration
	log      *logx.Logger
}

// manualProbeTimeout is how long a --port may stay unreachable before we warn.
const manualProbeTimeout = 60 * time.Second

func runCommand(o runOptions, args []string) int {
	log := o.log
	fwd := client.New(client.Config{SocketPath: o.socket, Software: softwareVersion(), Log: log})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fwdDone := make(chan struct{})
	go func() {
		defer close(fwdDone)
		fwd.Run(ctx) // errors are logged; the command keeps running regardless
	}()

	manual := map[int]bool{}
	for _, p := range o.ports {
		if manual[p] {
			continue
		}
		manual[p] = true
		fwd.Set(p, client.LocalAddrs(p))
		go probeManual(ctx, p, o.interval, log)
	}

	child, err := process.Start(args, log)
	if err != nil {
		log.Errorf("%v", err)
		var se *process.StartError
		if errors.As(err, &se) {
			return se.Code
		}
		return 1
	}

	if o.detect {
		go detectLoop(ctx, detector.New(os.Getpid(), log), fwd, manual, o.interval, log)
	}

	<-child.Done()
	// Closing the control session makes the agent drop all our listeners.
	cancel()
	child.Cleanup(3 * time.Second)
	select {
	case <-fwdDone:
	case <-time.After(time.Second):
	}
	return child.ExitCode()
}

// detectLoop polls for listening ports and keeps the forwarder in sync.
func detectLoop(ctx context.Context, det *detector.Detector, fwd *client.Forwarder, manual map[int]bool, interval time.Duration, log *logx.Logger) {
	known := map[int][]string{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cur := det.Scan()
		for port, addrs := range cur {
			if manual[port] {
				continue
			}
			if prev, ok := known[port]; !ok || !slices.Equal(prev, addrs) {
				if !ok {
					log.Debugf("detected listening port %d (%v)", port, addrs)
				}
				fwd.Set(port, addrs)
			}
		}
		for port := range known {
			if _, ok := cur[port]; !ok && !manual[port] {
				log.Debugf("port %d is no longer listening", port)
				fwd.Remove(port)
			}
		}
		known = cur
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// probeManual warns when a manually forwarded port does not become reachable.
func probeManual(ctx context.Context, port int, interval time.Duration, log *logx.Logger) {
	deadline := time.Now().Add(manualProbeTimeout)
	addrs := client.LocalAddrs(port)
	for {
		if client.Probe(addrs, time.Second) {
			log.Debugf("port %d is reachable inside the container", port)
			return
		}
		if time.Now().After(deadline) {
			log.Warnf("nothing accepts connections on port %d inside the container after %s; check the --port value and the server's bind address", port, manualProbeTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

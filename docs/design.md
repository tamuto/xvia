# xvia design notes

This document records the design decisions behind xvia, including those that deviate from the initial specification, and the reasons for them.

## Components

| Package | Role |
| --- | --- |
| `cmd/xvia` | CLI parsing, wiring of agent and client mode |
| `internal/agent` | Host agent: Unix socket server, TCP listeners, stream setup |
| `internal/client` | Container side: control session, reconnect, stream setup |
| `internal/protocol` | Framed JSON control messages, protocol version |
| `internal/tunnel` | Bidirectional copy with half-close |
| `internal/detector` | `/proc` based detection of listening ports (Linux) |
| `internal/process` | Command wrapper: subreaper, signals, reaping, cleanup (Linux) |
| `internal/config` | Defaults, environment variables, input validation |
| `internal/logx` | Minimal leveled logger |

There are no third-party dependencies. Linux-specific code (`/proc`, `prctl`, `wait4`, `ioctl`) lives in `*_linux.go` files under `internal/detector` and `internal/process`, with stubs for other platforms so that the rest still compiles.

## Protocol

Every connection to the Unix socket starts with one *frame*: a 4-byte big-endian length followed by a JSON object (at most 16 KiB). The first frame decides what the connection is.

**Control connection** (`hello`): one per client, alive as long as the client runs.

```
client                                  agent
  hello {version, software}       ->
                                  <-    welcome {version, software, error?}
  register {port}                 ->
                                  <-    registered {port, address | error}
  unregister {port}               ->
                                  <-    open {port, stream, token}      (a TCP connection arrived)
  open_failed {stream, error}     ->                                    (nothing listens in the container)
  ping                            ->
                                  <-    pong
```

**Data connection** (`attach`): one per forwarded TCP connection.

```
client  attach {version, stream, token}  ->  agent
        ... raw TCP bytes in both directions, no further framing ...
```

`protocol.Read` reads exactly one frame and never buffers beyond it, so the same `net.Conn` can be handed to the byte relay right after the `attach` frame.

### Versioning

`hello`, `welcome` and `attach` carry `version` (currently 1). The agent answers a different version with a `welcome` that contains an error and its own version. The client reports `protocol version mismatch (local 1, peer 2)` with both release strings and stops retrying, but leaves the wrapped command running. Unknown message types on an established control connection are ignored, so that optional additions do not require a version bump.

### Deviation: one Unix connection per TCP stream instead of a multiplexer

The specification suggests multiplexing control messages and TCP data over the Unix socket. We considered both approaches:

1. A single connection per client carrying framed `data` messages for all streams. This would need per-stream flow-control windows to avoid head-of-line blocking (one slow browser tab would stall every other stream). It would also need explicit half-close and reset messages, buffering limits, and a scheduler. That is effectively a re-implementation of yamux/HTTP/2 flow control.
2. A control connection for messages, plus a fresh Unix connection for each forwarded TCP connection.

xvia uses option 2. Opening a Unix socket connection is cheap: there is no handshake beyond the `attach` frame, and dev workloads open at most a few dozen connections per second. Option 2 is still multiplexing in the sense of the requirements (many ports per client, many concurrent connections per port, all through one socket path), but each stream gets from the kernel:

- **Backpressure:** blocking writes on a full socket buffer slow down exactly the stream that is behind, and no other.
- **Half-close:** `CloseWrite` on both the TCP and the Unix side maps one-to-one.
- **Isolation and cleanup:** closing one stream cannot corrupt another, and a crashed client closes all its sockets.
- **Simplicity:** the relay is `io.CopyBuffer` in two goroutines (`internal/tunnel`).

A data connection is bound to its pending TCP connection by the stream ID plus a 128-bit random token that the agent sent only on the owning client's control connection. Tokens are compared in constant time and are single-use. Unclaimed streams are closed after 10 seconds.

## Agent

- **Socket setup.** The agent creates the socket directory with `0700` and refuses a directory that is a symlink or belongs to another UID, since whoever owns the directory can swap the socket. It chmods the socket to `0600`.
- **Stale sockets.** An existing socket file is probed with a connect. If an agent answers, startup fails. Otherwise the file is a leftover from a crash and is replaced. The socket is removed on shutdown.
- **Registration.** Registration validates the port (1–65535 and `--allow-ports`), enforces the per-client port limit and checks the conflict table. The listener is then created outside the lock, with the port reserved in the table. A failed `listen` with `EADDRINUSE` is reported with the same `port N is already in use` message as a conflict with another client.
- **Lifetime.** Each session has a read deadline of 45 seconds (clients ping every 15 seconds). When the deadline passes, or the connection closes or misbehaves, the agent closes all of the session's listeners, pending streams and active streams. A crashed or `SIGKILL`ed client thus never leaves host listeners behind: the kernel closes its socket, and the agent notices immediately.
- **Limits.** Concurrent forwarded connections (pending plus active) are limited by a semaphore (`--max-conns`); excess connections are closed right away. Other limits: 64 sessions, 64 ports per session, a 5-second handshake timeout and a 10-second write timeout on control messages.
- **Shutdown.** SIGINT or SIGTERM closes the Unix listener, every session (and with it every listener and stream), and removes the socket file.

## Client

- **Port set.** The forwarder keeps the desired port set (`port -> dial addresses`) independently of the connection. `Set` and `Remove` update the set and send `register`/`unregister` if connected. One mutex covers both the set and control writes, so messages leave in the same order as the calls.
- **Reconnect.** On connect, all ports in the set are registered. This is how ports are restored after an agent restart. Reconnects use exponential backoff from 250 ms to 10 s with jitter, so several clients do not hit a restarting agent in lockstep. The first failure of an outage is logged as a warning; repeats are logged at debug level only.
- **Streams.** For `open`, the client first connects to the server inside the container, trying the dial addresses in order. Only if that works does it open the data connection, so a dead server results in an immediate `open_failed` and a closed host connection rather than a hang.

## Port detection

See the README for the algorithm. Further notes:

- **No cache across scans.** The process tree is rebuilt from our own PID on every scan, so PID reuse cannot attribute a foreign process to us: a reused PID is only scanned if it really is our descendant at that moment. When terminating leftovers, the start time from `/proc/<pid>/stat` is re-checked right before `kill`.
- **One table per namespace.** `net/tcp` is read once per network namespace (`/proc/<pid>/ns/net`), not once per process.
- **Socket ownership.** A socket inherited by several processes (e.g. a pre-forked server) is attributed once per port.
- **Manual ports.** Ports from `--port` take precedence over detected addresses and are not removed when detection does not see them.
- **Polling interval.** The interval is a parameter of the detection loop (`--poll-interval`, `XVIA_POLL_INTERVAL`). An event-based source could replace the loop without touching the forwarder; netlink `sock_diag` would still need the inode match.

## Process management

- **Subreaper.** `prctl(PR_SET_CHILD_SUBREAPER)` keeps orphaned descendants in our tree, both for detection and for cleanup.
- **Reaping.** Because orphans are reparented to us, a single process-wide reaper (`wait4(-1, WNOHANG)` on SIGCHLD, plus a 1-second safety tick) collects all exit statuses. `exec.Cmd.Wait` is never used, since it would race with the reaper. The reaper lock is held across `fork`/`exec` and the registration of the child's PID, so an immediate exit cannot be reaped before it is attributed.
- **Parent death.** `Pdeathsig` is set to SIGTERM. The kernel ties it to the forking *thread*, so the thread that starts the child is locked (`runtime.LockOSThread`) and kept alive until the child exits.
- **Terminal handling.** If stdin is a terminal and we are in its foreground process group, the child stays in our process group. It can then read the terminal (no SIGTTIN) and receives terminal signals directly. To avoid double delivery, SIGINT and SIGQUIT received by xvia are not forwarded in this mode. Without a terminal the child gets its own process group, and signals are forwarded to the group.
- **Exit.** When the main child exits, its status becomes xvia's exit code (`128+signal` when killed). Remaining descendants get SIGTERM, and SIGKILL after 3 seconds. The control connection is closed first, so the host listeners disappear immediately.

# xvia

**Cross-Environment Port Forwarding** — automatically expose development servers running inside containers to the host, without Docker port publishing.

```sh
# on the host (e.g. WSL2 Ubuntu)
xvia agent

# inside the dev container
xvia pnpm run dev
# Vite listens on 127.0.0.1:5173 in the container
# -> http://localhost:5173 works on the host
```

xvia needs **no** `ports:` section, **no** access to `/var/run/docker.sock`, **no** `privileged: true`, **no** `network_mode: host` and **no** SSH server in the container. The only thing shared between host and container is one directory containing a Unix domain socket.

## Why

Dev servers inside containers usually bind to `127.0.0.1` and choose ports at runtime. Publishing them with Docker means editing `ports:`, recreating containers, configuring the server to bind `0.0.0.0`, and fighting port clashes between projects. Tools that discover ports automatically usually need the Docker API, which is root-equivalent access to the host.

xvia instead wraps the command you already run. It watches which TCP ports that command (and everything it spawns) starts listening on, and asks a small agent on the host to listen on the same port numbers and relay the connections through the shared socket.

## How it works

```
Browser / curl on the host
    |  127.0.0.1:5173
    v
xvia agent (host)  ──  Unix socket  ──  xvia (container)  ──  TCP  ──  Vite on 127.0.0.1:5173
   $XDG_RUNTIME_DIR/xvia/agent.sock      /run/xvia/agent.sock
                     └──── same directory, bind-mounted ────┘
```

1. `xvia <command>` starts the command and connects to the agent over the Unix socket.
2. Every 500 ms it inspects `/proc` to find TCP sockets in LISTEN state owned by the command or any of its descendants.
3. For every new port it sends a `register` message. The agent opens a TCP listener on `127.0.0.1:<port>` on the host.
4. When a connection arrives on the host, the agent asks the client to open the stream. The client connects to the server inside the container, opens a second connection to the Unix socket, and the agent pipes the bytes in both directions.
5. When the port stops listening, the command exits or the client disappears, the host listener is closed.

The relay copies raw TCP bytes, so HTTP/1.1, HTTP/2, WebSocket, Vite HMR, database protocols and anything else built on TCP work unchanged. Nothing is parsed or rewritten.

Windows browsers reach the ports through WSL2's built-in localhost forwarding; xvia does nothing Windows-specific.

The design is described in more detail in [docs/design.md](docs/design.md).

## Installation

xvia is a single static binary for Linux (amd64 and arm64), and works on Debian, Ubuntu and Alpine. You need it **on the host and in the container**, and both should run the same release.

From GitHub Releases:

```sh
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLo xvia "https://github.com/tamuto/xvia/releases/latest/download/xvia-linux-$arch"
curl -fsSLo SHA256SUMS https://github.com/tamuto/xvia/releases/latest/download/SHA256SUMS
grep "xvia-linux-$arch\$" SHA256SUMS | sed "s/xvia-linux-$arch/xvia/" | sha256sum -c -
sudo install -m 0755 xvia /usr/local/bin/xvia
```

With Go:

```sh
go install github.com/tamuto/xvia/cmd/xvia@latest
```

From source:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o xvia ./cmd/xvia
```

In a Dockerfile:

```dockerfile
ARG XVIA_VERSION=v0.1.0
ARG TARGETARCH
RUN curl -fsSLo /usr/local/bin/xvia \
      "https://github.com/tamuto/xvia/releases/download/${XVIA_VERSION}/xvia-linux-${TARGETARCH}" \
 && chmod +x /usr/local/bin/xvia
```

## Starting the host agent

```sh
xvia agent
```

By default the agent:

- listens on `$XDG_RUNTIME_DIR/xvia/agent.sock` (usually `/run/user/1000/xvia/agent.sock`). If `XDG_RUNTIME_DIR` is unset it uses `/run/user/<uid>/xvia/` if that exists, otherwise `/tmp/xvia-<uid>/`.
- creates the socket directory with mode `0700` and the socket with mode `0600`.
- binds forwarded ports to `127.0.0.1` only.
- accepts registrations for ports 1024–65535.
- does not need root.

Options:

```
xvia agent \
  --socket /run/user/1000/xvia/agent.sock \
  --bind 127.0.0.1 \
  --allow-ports 1024-65535 \
  --max-conns 1024 \
  --log-level info
```

`--bind 0.0.0.0` exposes forwarded ports to your network. This is never the default and the agent prints a warning when you use it.

To keep the agent running, use a systemd user service (WSL2 with systemd enabled):

```ini
# ~/.config/systemd/user/xvia-agent.service
[Unit]
Description=xvia host agent

[Service]
ExecStart=/usr/local/bin/xvia agent
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now xvia-agent
```

## Docker Compose integration

Bind-mount the **directory** that holds the socket to `/run/xvia` in every container that should be able to forward ports:

```yaml
services:
  app:
    image: my-dev-image   # contains xvia
    volumes:
      - ${XDG_RUNTIME_DIR}/xvia:/run/xvia
    command: ["xvia", "pnpm", "run", "dev"]
```

That is all. No `ports:`, no Docker socket, no extra privileges.

- Mount the directory, not the socket file. A restarted agent creates a new socket file, which a file bind mount would not see.
- Start the agent **before** `docker compose up`. If the source directory does not exist, Docker creates it as root, and the agent then refuses to use it (`owned by uid 0`). Remove it and start the agent first. Alternatively, use the long volume syntax with `bind: {create_host_path: false}` so compose fails instead.
- The container user must be able to open the `0600` socket. That is the case when it is root, or has the same UID as the user running the agent (UID 1000 for both the default WSL user and the `node` user of the official Node images).

A complete, runnable example with two containers is in [examples/docker-compose](examples/docker-compose). `examples/docker-compose/test.sh` builds xvia, starts the agent and the containers, and fetches both servers from the host:

```sh
./examples/docker-compose/test.sh
```

## CLI usage

```
xvia agent [options]                 run the host agent
xvia [options] [--] <command> [args] run a command and forward its ports
xvia version | --version             print the version
xvia help | --help                   show help
```

xvia options must come before the command. Everything from the first non-option argument (or after `--`) is passed to the command untouched, so the command's own flags never get mixed up with xvia's:

```sh
xvia pnpm run dev                          # detect ports automatically
xvia --port 5173 pnpm run dev              # also forward 5173, detected or not
xvia --port 5173 --port 8000 pnpm run dev  # several ports (or --port 5173,8000)
xvia --no-detect --port 5173 pnpm run dev  # only the given ports
xvia --socket /run/xvia/agent.sock pnpm run dev
xvia -- python3 -m http.server 8000 --bind 127.0.0.1
xvia -- agent                              # run a program that is literally called "agent"
```

| Option | Environment | Default |
| --- | --- | --- |
| `--socket PATH` | `XVIA_SOCKET` | client: `/run/xvia/agent.sock`, or the host default if only that exists. Agent: `$XDG_RUNTIME_DIR/xvia/agent.sock` |
| `--bind ADDR` (agent) | `XVIA_BIND` | `127.0.0.1` |
| `--allow-ports MIN-MAX` (agent) | `XVIA_ALLOW_PORTS` | `1024-65535` |
| `--log-level LEVEL` | `XVIA_LOG_LEVEL` | `info` (`debug`, `info`, `warn`, `error`) |
| `--poll-interval D` (client) | `XVIA_POLL_INTERVAL` | `500ms` |
| `-p, --port PORT` (client) | | |
| `--no-detect` (client) | | detection on |

xvia behaves like the command it wraps:

- It keeps the working directory, environment, stdin, stdout and stderr.
- Its exit code is the command's exit code. It is `128+N` if the command was killed by signal N, `127` if the command was not found, and `126` if it is not executable.
- The command keeps running even when the agent is missing, unreachable or refuses a port. xvia prints a warning and keeps retrying in the background with exponential backoff (250 ms up to 10 s). After the agent restarts, all current ports are registered again.

Example output:

```
$ xvia pnpm run dev
xvia: connected to agent at /run/xvia/agent.sock
  VITE v6.0.0  ready in 300 ms
  ➜  Local:   http://localhost:5173/
xvia: forwarding 127.0.0.1:5173 (host) -> port 5173 (container)
```

## Automatic port detection

xvia uses only `/proc` and runs no external commands:

1. **Process tree.** xvia makes itself a *child subreaper* (`PR_SET_CHILD_SUBREAPER`), so processes whose parent exits are reparented to xvia instead of PID 1 and stay in the tree. Descendants are found through `/proc/<pid>/task/<tid>/children`. If the kernel lacks that file, xvia falls back to the parent PIDs in `/proc/<pid>/stat`.
2. **Socket ownership.** For every descendant it reads `/proc/<pid>/fd/*` and collects the inodes of `socket:[inode]` links.
3. **Listening sockets.** It reads `/proc/<pid>/net/tcp` and `/proc/<pid>/net/tcp6` once per network namespace and keeps the entries in LISTEN state (`0A`) whose inode belongs to a descendant. The `net/tcp` tables list *every* socket of the namespace, so this inode match is what keeps unrelated services from being forwarded, including services started before xvia.
4. **Target address.** A server on `0.0.0.0` or `127.0.0.1` is reached through `127.0.0.1`, one on `::1` through `[::1]`, and one on `::` through `127.0.0.1` then `[::1]`. A server on a specific address is reached through that address.

Detection runs every 500 ms (`--poll-interval`). The process tree is rebuilt on every scan, so short-lived children and PID reuse do not leave stale entries behind. Ports appear on the host within about half a second after the server starts listening, and disappear within the same interval after it stops.

If detection does not work in your environment, use `--port`. Manually given ports are registered right away. xvia checks that something inside the container accepts connections on them and warns if nothing does within 60 seconds.

## Port conflicts

Ports keep their number. If a port is already forwarded for another container, or already used by a program on the host, the agent refuses the registration:

```
xvia: port 5173 is already in use
```

The dev server keeps running, it is just not forwarded. Configure a different port for one of the projects, or stop the other one. A refused port is requested again the next time xvia reconnects to the agent.

## Security

xvia is meant for a single developer's machine. Its defaults are restrictive:

- Forwarded ports listen on `127.0.0.1` unless you pass `--bind`.
- The socket directory is `0700` and the socket `0600`. The agent refuses socket directories owned by another user.
- The agent never runs commands. The only thing a client can request is "listen on TCP port N on the host and relay connections to me".
- Ports are validated against `--allow-ports` (1024–65535 by default). The number of clients (64), ports per client (64) and concurrent connections (`--max-conns`, 1024) is limited.
- Control messages are size-limited (16 KiB), malformed input closes only the offending connection, and handshakes and stream setup time out.
- Each relayed stream is authorized with a random one-time token that is only sent to the client that owns the port.
- Logs never contain relayed data.
- Neither side needs root, the Docker API, privileged containers or host networking.

> **Only mount the socket into containers you trust.** Any process that can open the socket can make the agent listen on host ports (within `--allow-ports`, on the `--bind` address) and serve whatever it wants there. For example, a malicious container could register a port that a host application expects to belong to something else. Treat the socket like any other way into your user account.

## Limitations

- **Linux only.** Host and container must run Linux, for example WSL2, a Linux desktop or a Linux VM. macOS and Windows hosts (Docker Desktop's file sharing does not pass Unix sockets) are future work.
- **Same kernel.** The Unix socket only works across a bind mount when host and container share a kernel: Docker Engine on Linux or inside WSL2. It does not work with remote Docker hosts.
- **Polling.** Ports are detected with a delay of up to one poll interval. A server that listens only briefly may be missed.
- **Permissions.** xvia must be able to read `/proc/<pid>/fd` of the processes it watches. Processes running as another user (e.g. setuid helpers) cannot be inspected; their ports are only forwarded with `--port`. xvia warns once per process.
- **Process management is best effort.**
  - When the command exits, remaining descendants get SIGTERM and, after 3 seconds, SIGKILL.
  - If xvia itself is killed with SIGKILL, the direct child receives SIGTERM (parent-death signal), but its descendants depend on that child to clean up.
  - Processes that escape the tree are neither detected nor cleaned up. This happens when they move to another PID namespace, or when they are reparented to a subreaper other than xvia, such as a nested `tini -s`.
- **Signals.**
  - **Interactive terminal** (stdin is the controlling terminal): the command shares xvia's process group, so it can read from the terminal and receives Ctrl+C and Ctrl+\ from the kernel directly. Signals sent to xvia itself (SIGTERM, SIGHUP) are forwarded to the command's main process only.
  - **No terminal:** the command gets its own process group, and SIGINT, SIGTERM, SIGHUP and SIGQUIT are forwarded to the whole group. Descendants that create their own process group or session do not receive forwarded signals, but they are still terminated when the command exits.
- **No automatic port remapping.** A conflicting port is refused, not moved to another number.
- **Same version.** Host and container should run the same xvia release. Incompatible protocol versions are refused with an error.

## Troubleshooting

**`agent socket /run/xvia/agent.sock not found`** — Check that the agent is running on the host (`ls -l $XDG_RUNTIME_DIR/xvia/`) and that the directory is mounted into the container (`ls -l /run/xvia/` inside it). Remember to mount the directory, not the file.

**`permission denied on agent socket`** — The container user differs from the agent's user and is not root. Run the container with the same UID (`user: "1000:1000"`), or run xvia as root in the container.

**`socket directory ... is owned by uid 0`** — Docker created the bind-mount source before the agent started. Run `sudo rm -r $XDG_RUNTIME_DIR/xvia`, start the agent, then recreate the containers.

**`another xvia agent is already listening`** — An agent is already running for this socket. Only one agent per socket is possible.

**`port N is already in use`** — Another container forwards N, or a host program uses it. Check with `ss -ltnp 'sport = :N'` on the host.

**`protocol version mismatch`** — Update xvia on the host or in the container so that both run the same release.

**The port is not detected** — Run with `--log-level debug` to see what is detected. Make sure the server is started *by* xvia (`xvia npm run dev`, not `npm run dev &` followed by `xvia sleep infinity`). Use `--port N` as a fallback.

**Connections are closed immediately** — The server inside the container may listen on an address other than loopback or the wildcard. Check `--log-level debug` output and the server's bind address.

**`XDG_RUNTIME_DIR` is empty** — Typical in `sudo` or minimal sessions. Set it (`export XDG_RUNTIME_DIR=/run/user/$(id -u)`), or pass `--socket` to the agent and use the same directory in the compose file.

## Development

```sh
go fmt ./... && go vet ./... && go test -race ./...
./examples/docker-compose/test.sh    # end-to-end with Docker
```

The tests start real agents, clients and processes on loopback (the agent binds `127.0.0.2` so it can share port numbers with test servers on `127.0.0.1`). They need neither network access nor Docker.

Tagging `vX.Y.Z` builds `linux/amd64` and `linux/arm64` binaries with GitHub Actions and publishes them with a `SHA256SUMS` file to GitHub Releases.

## License

[MIT](LICENSE)

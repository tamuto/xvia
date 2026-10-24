# Release procedure

xvia does not use CI/CD. Checks, builds and releases are done by hand from a development machine (Linux, with Go, git, Docker and curl).

Release artifacts and their names (the README's install instructions depend on them):

| File | Content |
| --- | --- |
| `xvia-linux-amd64` | static binary for linux/amd64 |
| `xvia-linux-arm64` | static binary for linux/arm64 |
| `SHA256SUMS` | SHA-256 checksums of both binaries |

Versions follow [Semantic Versioning](https://semver.org/) and tags look like `v0.1.0`. While the version is 0.x, a change to the control protocol (`protocol.Version`) requires at least a minor version bump and a note in the release notes, because host and container must then be updated together.

## 1. Prepare

```sh
git checkout main
git pull
git status            # must be clean
```

If the control protocol changed, make sure `protocol.Version` in `internal/protocol/protocol.go` was incremented.

## 2. Check

All of these must pass:

```sh
test -z "$(gofmt -l .)"                 # formatting
go vet ./...                            # static checks
go test -race -count=1 ./...            # unit, integration and end-to-end tests
./examples/docker-compose/test.sh       # real Docker Compose example (ends with PASS)
```

`test.sh` needs Docker. When running it inside a dev container whose paths differ from the Docker host, pass both views of the socket directory, for example:

```sh
XVIA_SOCKET_DIR=$PWD/.tmp/xvia \
XVIA_DOCKER_SOCKET_DIR=$DENV_HOST_WORKSPACES/xvia/.tmp/xvia \
./examples/docker-compose/test.sh
```

## 3. Tag

```sh
VERSION=v0.1.0
git tag -a "$VERSION" -m "xvia $VERSION"
git push origin "$VERSION"
```

## 4. Build

```sh
scripts/build-release.sh "$VERSION"
```

The script refuses to build from a dirty tree or when the tag does not point at `HEAD`. It writes the three files to `dist/` (ignored by git) and embeds the version, so check:

```sh
./dist/xvia-linux-amd64 version        # -> xvia v0.1.0
```

## 5. Publish on GitHub

1. Open <https://github.com/tamuto/xvia/releases/new>.
2. **Choose a tag**: select the tag pushed in step 3.
3. **Release title**: `xvia v0.1.0`.
4. **Description**: click *Generate release notes*, then add anything users must know: protocol changes, changed defaults, known issues.
5. Attach `dist/xvia-linux-amd64`, `dist/xvia-linux-arm64` and `dist/SHA256SUMS`.
6. Click **Publish release**.

With the [GitHub CLI](https://cli.github.com/) installed and logged in, steps 1–6 are a single command:

```sh
gh release create "$VERSION" dist/* --verify-tag --generate-notes --title "xvia $VERSION"
```

## 6. Verify the release

On a clean machine or container, follow the README's installation section:

```sh
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLo xvia "https://github.com/tamuto/xvia/releases/latest/download/xvia-linux-$arch"
curl -fsSLo SHA256SUMS https://github.com/tamuto/xvia/releases/latest/download/SHA256SUMS
grep "xvia-linux-$arch\$" SHA256SUMS | sed "s/xvia-linux-$arch/xvia/" | sha256sum -c -
chmod +x xvia && ./xvia version
```

## Fixing a broken release

- **Wrong file attached:** edit the release on GitHub, replace the file and regenerate `SHA256SUMS` if a binary changed.
- **Bug in the code:** do not move or reuse the tag; users may already have downloaded it. Fix on `main` and release the next patch version (e.g. `v0.1.1`).
- **Tag pushed by mistake, nothing published yet:** `git push --delete origin "$VERSION" && git tag -d "$VERSION"`.

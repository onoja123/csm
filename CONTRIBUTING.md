# Contributing to csm

Thanks for your interest in improving csm. Bug reports, fixes and well-scoped features are all welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach before you spend time on it.
- Problems with the desktop app belong in the [csm-desktop repository](https://github.com/onoja123/csm-desktop).
- Report security problems privately, as described in [SECURITY.md](SECURITY.md).

## Development setup

You need macOS or Linux (csm doesn't run on Windows yet) and Go 1.25 or newer. You don't need any of the agents installed or signed in: each agent has a fake that the tests run through the `csm` binary.

```bash
git clone https://github.com/onoja123/csm.git
cd csm
go build -o csm ./cmd/csm
```

The [Development](README.md#development) section of the README describes the code layout and how to add an agent, task handoff or MCP support.

## Checks

CI runs these on macOS and Ubuntu for every push and pull request, and cross-builds for macOS, Linux and Windows on amd64 and arm64. Run them before you push:

```bash
gofmt -l cmd internal   # prints nothing when formatting is right; gofmt -w cmd internal fixes it
go vet ./...
go test -race ./...
```

## Pull requests

- Keep each pull request to one change, and describe what it does and how you tested it.
- Add or update tests when behavior changes, using the fake agents rather than real accounts.
- Never put real tokens, account emails or agent output in tests, fixtures or issues.
- Write commit messages as a short imperative summary, for example "Show a run's receipt with csm build receipt".

`main` is protected: every change goes through a pull request, and CI must pass before it merges.

## License

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).

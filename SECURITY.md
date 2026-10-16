# Security Policy

## Supported versions

csm is pre-1.0. Security fixes land on the `main` branch and in the latest release only.

## Reporting a vulnerability

Please don't open a public issue for a security problem. Report it privately instead:

- on GitHub, through **Security → Report a vulnerability** on this repository, or
- by email to [okpeonoja18@gmail.com](mailto:okpeonoja18@gmail.com).

Include what you found, how to reproduce it and the impact you expect. I aim to acknowledge reports within a week and to share a fix or a plan once the problem is confirmed. Please allow time for a fix to ship before disclosing the issue publicly.

## Scope

csm runs coding agents in separate profile directories on your own machine. It is built so that:

- it never reads, copies or stores tokens, cookies or Keychain secrets; signing in is always the agent's own login flow;
- everything it writes under `~/.csm` is private to your user (directories `0700`, files `0600`);
- task handoffs never include tool output, and are redacted for tokens, bearer headers, passwords in URLs and secret-looking assignments before they are written;
- the service for the desktop app, `csm serve`, listens only on the unix socket `~/.csm/csm.sock` and never on a network port.

A way around any of these is a vulnerability. The [Security](README.md#security) section of the README describes the full model. Problems in the desktop app belong in the [csm-desktop repository](https://github.com/onoja123/csm-desktop/security).

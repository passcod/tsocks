A Tailscale userspace proxy that exposes a SOCKS5 and an HTTP CONNECT proxy.

This is designed to be super lightweight and convenient to use, instead of the `tailscale set --exit-node` dance.

## Install

```
go get github.com/passcod/tsocks
```

## Use

```
tsocks
```

On first run, this will print the Auth URL to authenticate to Tailscale, then a TUI picker to select your exit node.
On subsequent runs, it will automatically pick the last exit node you were using.
You can override that using `--exit-node hostname-or-ip`.

When running, it shows the exit node you're connected to, the effective external IPs, and some metrics.
You can pause/unpause the proxy (which will stop traffic using it) and switch the exit node midway.

```
q quit │ r reset │ p pause │ s switch exit node
nz-akl-wg-303: 103.75.11.109, 2404:f780:5:def::e213
⏱ 06:17 │ ↑ 218.5 KB (5.3 KB/s) │ ↓ 11.1 MB (2.8 MB/s) │ 19 conns
```

SOCKS5 proxy listens on localhost:1080 by default, and HTTP CONNECT proxy listens on localhost:4444.

## License

CC-0.

# Configurable Server Listen Address Design

## Goal

Allow other networked machines to access the web server while retaining simple Makefile overrides for the bind address and port.

## Design

The Makefile will define these overridable variables:

- `BIND ?= 0.0.0.0`
- `PORT ?= 8080`
- `LISTEN := $(BIND):$(PORT)`

The `run` and `server` targets will pass `--listen $(LISTEN)` to the server command. Their help descriptions will report the configurable default rather than claiming the server always listens on loopback.

The default `make run` and `make server` behavior will therefore expose the web server on all IPv4 interfaces at port 8080. Users can restrict or relocate it without editing the Makefile, for example:

```sh
make run BIND=127.0.0.1 PORT=9000
```

## Scope

Only the web server's Makefile targets are affected. The egress proxy address and behavior remain unchanged.

## Validation

Use Make dry runs to confirm that both targets generate the expected default and overridden `--listen` arguments. Run the existing test suite to detect regressions.

## Security Note

Binding to `0.0.0.0` makes the server reachable through every permitted network interface. Host firewall and network policy remain responsible for limiting access.

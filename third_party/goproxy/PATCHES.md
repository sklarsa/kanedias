# Local goproxy patches

This directory contains the non-test Go sources, required subpackages, module
metadata, and license from `github.com/elazarl/goproxy` v1.8.6.

Two calls in `https.go` preserve typed errors when forwarding warnings to the
configured logger:

- CONNECT dial failures pass `err` instead of `err.Error()`.
- Tunnel copy failures pass `err` instead of `err.Error()`.

These changes let the application use `errors.Is` to suppress expected
connection teardown without inspecting or logging arbitrary error text.

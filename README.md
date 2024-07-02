# ed

[![Go Reference](https://pkg.go.dev/badge/code.nkcmr.net/ed.svg)](https://pkg.go.dev/code.nkcmr.net/ed)
[![Go Test](https://github.com/nkcmr/ed/actions/workflows/go_test.yaml/badge.svg?branch=main)](https://github.com/nkcmr/ed/actions/workflows/go_test.yaml)

ed is an event dispatcher in Go. Documentation is available in [pkg.go.dev](https://pkg.go.dev/code.nkcmr.net/ed).

## Stablility

This module is tagged as v0, thus complies with Go's definition and rules about v0 modules (https://go.dev/doc/modules/version-numbers#v0-number). In short, it means that the API of this module may change without incrementing the major version number. Each releasable version will simply increment the patch number. Given the surface area of this module is quite small, this should not be a huge issue if used in production code.

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.0 -config sleepergen/genconfig.yaml openapi.yaml

// Package sleeper vendors a generated client (see sleepergen) for the
// Sleeper API; regenerate with `go generate ./sleeper/...` after editing
// openapi.yaml.
package sleeper

// Package policies embeds the Rego policy and its data into the binary, so that
// the gateway needs no files at run time. The tests of the policy (*_test.rego)
// are run by `make policy-test` and are not embedded.
package policies

import "embed"

//go:embed edigate.rego data.json
var FS embed.FS

// Package forkbox holds module-level assets shared by the Forkbox binaries.
package forkbox

import _ "embed"

// VersionsYAML is the versions.yaml this build was made with. Binaries use it
// unless the operator passes --versions.
//
//go:embed versions.yaml
var VersionsYAML []byte

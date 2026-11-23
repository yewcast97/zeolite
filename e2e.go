//go:build e2e

package main

// The automation plugin lets a test drive the built app over a socket
// named by MYGO_AUTOMATION. It is linked into test builds only.
import _ "github.com/egoist/mygo/plugins/automation"

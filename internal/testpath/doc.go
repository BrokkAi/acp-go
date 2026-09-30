// Package testpath roots host-native test fixture paths on the test host.
//
// Tests that exercise host-local paths (clienthost workspaces, runner
// directories, local agent processes) need paths that are absolute for the
// host running them, so a fixture such as "/repo" needs a drive on Windows.
// Agent-side ACP paths accept either platform's absolute form and do not need
// this treatment. Root is an untyped constant so Root+"/repo" still converts
// to the schema AbsolutePath types.
package testpath

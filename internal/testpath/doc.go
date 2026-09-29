// Package testpath roots Unix-shaped test fixture paths on the test host.
//
// ACP paths must be absolute for the host running the client, so a fixture
// such as "/repo" needs a drive on Windows. Root is an untyped constant so
// Root+"/repo" still converts to the schema AbsolutePath types.
package testpath

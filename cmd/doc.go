// Package cmd is the shinyctl command line and shinyctl-api in one binary.
//
// The commands (apply.go, status.go, logs.go and the others) are thin: they
// read a manifest and talk to shinyctl-api over api.sock. shinyctl-api is
// `shinyctl serve`: its handlers are in serve_*.go, and the reconcile cycle is
// in reconcile*.go and serve_reconcile.go. A cycle observes an app, asks
// package reconcile what to do, records it in the state file, pushes the
// copies table to the load balancer, and starts and stops the copies. The
// load balancer is the hidden `shinyctl proxy` command, a process of its own
// (see package proxy).
package cmd

// Package process starts, finds and stops the processes shinyctl runs: the R
// process of each copy and the load balancer. It tells a recorded process from
// a stranger that reused its number (by command line, started-at and port),
// checks that a port is free and who holds it, waits for a copy to accept
// connections, watches for an exit, and measures memory. It decides nothing
// about what should run; that is package reconcile.
package process

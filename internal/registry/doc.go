// Package registry remembers which apps shinyctl-api manages: the last
// applied spec of each and whether it was stopped on purpose. It is kept in a
// file, so a restarted shinyctl-api goes on watching what the previous one
// watched and does not bring back what the user took down.
package registry

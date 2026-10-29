// Package state is the record of one app on disk: state.json holds a record
// per copy (its process, port, start time, crash history and the time it began
// to drain), the app's revision, and the settings the copies were started
// from. Files are replaced whole and never edited in place, and Lock serializes
// the cycle, the exit-watchers and the handlers that read and write one.
package state

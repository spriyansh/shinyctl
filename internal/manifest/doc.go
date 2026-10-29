// Package manifest reads and validates shinyctl.yaml and holds AppSpec, the
// settings of one app with their defaults and limits: the fields users write,
// the paths made absolute, and the helpers that answer "how many copies" or
// "what restart policy" with the default applied.
package manifest

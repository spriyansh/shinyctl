// Package proxy is shinyctl's built-in load balancer: the settings
// shinyctl-api renders for it, the hidden process that runs it, and the
// supervision that keeps that process up.
//
// It embeds an HTTP server library as the engine and configures it only
// through JSON documents (see Render), so nothing outside this package
// needs to know which engine it is.
package proxy

import (
	"log"
	"os"

	// The engine's modules. Importing the config package registers its
	// /load endpoint; the TLS and events apps come in through the HTTP app.
	_ "github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"
)

// The engine's package init redirects Go's standard log into its own
// structured logger, which would turn every line this process writes
// through log (api.log, in shinyctl-api) into JSON. Package inits run
// before this one, so this puts the standard logger back.
func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")
}

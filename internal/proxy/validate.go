package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/caddyserver/caddy/v2"
)

// Validate checks cfg the way the load balancer would when loading it —
// every module found and provisioned — without starting anything or
// touching the running one. It catches a config shinyctl rendered wrongly
// before it is sent.
func Validate(cfg []byte) error {
	var c caddy.Config
	if err := json.Unmarshal(cfg, &c); err != nil {
		return fmt.Errorf("parsing settings: %w", err)
	}
	return caddy.Validate(&c)
}

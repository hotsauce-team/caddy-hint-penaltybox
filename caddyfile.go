package penaltybox

import (
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("hint_penaltybox", parseCaddyfile)
	// Enforcement must run before the proxy hands the request upstream.
	// Inside an explicit route{} block ordering is positional anyway.
	httpcaddyfile.RegisterDirectiveOrder("hint_penaltybox", httpcaddyfile.Before, "reverse_proxy")
}

func parseCaddyfile(helper httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var h Handler
	err := h.UnmarshalCaddyfile(helper.Dispenser)
	return &h, err
}

// UnmarshalCaddyfile parses:
//
//	hint_penaltybox {
//	    header      <name>
//	    key         <placeholder>
//	    min_level   <1|2|3>
//	    window      <duration>
//	    limit       <n>
//	    penalty_ttl <duration>
//	    strip       [true|false]
//	    status      <code>
//	    max_keys    <n>
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name
	if d.NextArg() {
		return d.ArgErr() // no positional args; everything is in the block
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "header":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.Header = d.Val()
		case "key":
			if !d.NextArg() {
				return d.ArgErr()
			}
			h.Key = d.Val()
		case "min_level":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid min_level %q: %v", d.Val(), err)
			}
			h.MinLevel = n
		case "window":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid window %q: %v", d.Val(), err)
			}
			h.Window = caddy.Duration(dur)
		case "limit":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid limit %q: %v", d.Val(), err)
			}
			h.Limit = n
		case "penalty_ttl":
			if !d.NextArg() {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("invalid penalty_ttl %q: %v", d.Val(), err)
			}
			h.PenaltyTTL = caddy.Duration(dur)
		case "strip":
			val := true
			if d.NextArg() {
				switch d.Val() {
				case "true":
					val = true
				case "false":
					val = false
				default:
					return d.Errf("strip must be true or false, got %q", d.Val())
				}
			}
			h.Strip = &val
		case "status":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid status %q: %v", d.Val(), err)
			}
			h.Status = n
		case "max_keys":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n, err := strconv.Atoi(d.Val())
			if err != nil {
				return d.Errf("invalid max_keys %q: %v", d.Val(), err)
			}
			h.MaxKeys = n
		default:
			return d.Errf("unknown subdirective %q", d.Val())
		}
		if d.NextArg() {
			return d.ArgErr() // no subdirective takes more than one arg
		}
	}
	return nil
}

// Interface guard.
var _ caddyfile.Unmarshaler = (*Handler)(nil)

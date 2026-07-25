package penaltybox

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestCaddyfileUnmarshalFullBlock(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		header      X-Custom-Level
		key         {http.request.header.X-Client}
		min_level   3
		window      10s
		limit       12
		penalty_ttl 1m
		strip       false
		status      503
		max_keys    5000
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.Header != "X-Custom-Level" {
		t.Errorf("header = %q", h.Header)
	}
	if h.Key != "{http.request.header.X-Client}" {
		t.Errorf("key = %q", h.Key)
	}
	if h.MinLevel != 3 {
		t.Errorf("min_level = %d", h.MinLevel)
	}
	if time.Duration(h.Window) != 10*time.Second {
		t.Errorf("window = %v", time.Duration(h.Window))
	}
	if h.Limit != 12 {
		t.Errorf("limit = %d", h.Limit)
	}
	if time.Duration(h.PenaltyTTL) != time.Minute {
		t.Errorf("penalty_ttl = %v", time.Duration(h.PenaltyTTL))
	}
	if h.Strip == nil || *h.Strip {
		t.Error("strip should be explicitly false")
	}
	if h.Status != 503 {
		t.Errorf("status = %d", h.Status)
	}
	if h.MaxKeys != 5000 {
		t.Errorf("max_keys = %d", h.MaxKeys)
	}
}

func TestCaddyfileUnmarshalEmptyBlockLeavesDefaultsToProvision(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	// All zero — Provision applies the documented defaults.
	if h.Header != "" || h.Key != "" || h.MinLevel != 0 || h.Window != 0 ||
		h.Limit != 0 || h.PenaltyTTL != 0 || h.Strip != nil || h.Status != 0 || h.MaxKeys != 0 {
		t.Errorf("empty directive must leave zero values, got %+v", h)
	}
}

func TestCaddyfileBareStripMeansTrue(t *testing.T) {
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		strip
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if h.Strip == nil || !*h.Strip {
		t.Error("bare strip should mean true")
	}
}

func TestCaddyfileWindowSupportsDayUnits(t *testing.T) {
	// caddy.ParseDuration supports "d" — a reason we don't use
	// time.ParseDuration directly.
	d := caddyfile.NewTestDispenser(`hint_penaltybox {
		penalty_ttl 1d
	}`)
	var h Handler
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(h.PenaltyTTL) != 24*time.Hour {
		t.Errorf("penalty_ttl = %v, want 24h", time.Duration(h.PenaltyTTL))
	}
}

func TestCaddyfileErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"positional arg", `hint_penaltybox extra`},
		{"unknown subdirective", `hint_penaltybox {
			bogus 1
		}`},
		{"bad duration", `hint_penaltybox {
			window banana
		}`},
		{"bad min_level", `hint_penaltybox {
			min_level two
		}`},
		{"bad limit", `hint_penaltybox {
			limit many
		}`},
		{"bad strip", `hint_penaltybox {
			strip maybe
		}`},
		{"missing arg", `hint_penaltybox {
			header
		}`},
		{"extra arg", `hint_penaltybox {
			limit 1 2
		}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h Handler
			if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(c.input)); err == nil {
				t.Error("expected parse error")
			}
		})
	}
}

func TestProvisionAppliesDefaults(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()

	var h Handler
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Cleanup(); err != nil {
			t.Errorf("Cleanup: %v", err)
		}
	}()

	if h.Header != "X-Rate-Limit-Level" || h.Key != "{http.vars.client_ip}" || h.MinLevel != 2 ||
		time.Duration(h.Window) != time.Minute || h.Limit != 30 ||
		time.Duration(h.PenaltyTTL) != 5*time.Minute || h.Status != 429 || h.MaxKeys != 100_000 {
		t.Errorf("unexpected defaults: %+v", h)
	}
	if !h.stripOn {
		t.Error("strip should default to on")
	}
	if err := h.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

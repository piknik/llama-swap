package config

import (
	"fmt"
	"strings"
)

// Park-aware routing defaults. A model is "parked" when its engine keeps its
// container and its host-side state (weights, KV, caches) while giving back the
// device, so the router can make room for another model without a cold load.
//
// PCNI fork feature: see docs/kb/guides/model-runtime/parking.md. Nothing here
// changes behavior for a model that does not declare `park:`.
const (
	// DefaultParkTimeout bounds, in seconds, how long a park waits for the
	// model's in-flight requests to finish. On expiry the park is refused and
	// the model stays awake: a park must never cut a running generation.
	DefaultParkTimeout = 30

	// DefaultParkConfirmTimeout bounds, in seconds, how long llama-swap polls the
	// status endpoint for confirmation after the park command ran. On expiry the
	// model is recorded stopped rather than parked.
	DefaultParkConfirmTimeout = 10

	// DefaultParkedField is the JSON key polled from ParkConfig.StatusEndpoint
	// that must report true before llama-swap records the model as parked.
	// basalt reports this as "asleep", NInfer as "parked".
	DefaultParkedField = "parked"
)

// ParkConfig arms parking for one model. Its presence is the declaration: the
// router parks this model only because it is declared here.
//
// With `park:` armed the verbs split:
//
//	park    -> ParkConfig.Cmd (or cmdStop when Cmd is empty)
//	destroy -> cmdDestroy
//
// and `ttl` parks the model instead of unloading it. Without `park:` the model
// keeps upstream's single stop verb (cmdStop, or SIGTERM) unchanged.
type ParkConfig struct {
	// Cmd parks the engine. Defaults to cmdStop, which is the command most
	// park-aware entries already carry (POST to the engine's sleep route).
	// ${PID} is substituted with the tracked upstream process id and ${PORT}
	// with the model's port, exactly as in cmdStop.
	Cmd string `yaml:"cmd"`

	// ParkTimeout, in seconds, is how long a park waits for in-flight requests
	// to drain before refusing. 0 selects DefaultParkTimeout; a negative value
	// is rejected.
	ParkTimeout int `yaml:"parkTimeout"`

	// WakeTimeout, in seconds, is the readiness budget for the wake path
	// (re-attaching to a parked container and waiting for the engine to serve).
	// 0 falls back to the model's startTimeout and the global healthCheckTimeout.
	WakeTimeout int `yaml:"wakeTimeout"`

	// StatusEndpoint is polled after the park command, e.g. "/v1/sleep". When it
	// is set the model is only recorded as parked after the endpoint answers
	// with ParkedField true; without it llama-swap trusts the park command's
	// exit status. An engine that reports its health endpoint as loaded while
	// asleep needs this endpoint, otherwise a parked model would be served.
	StatusEndpoint string `yaml:"statusEndpoint"`

	// ParkedField is the JSON key StatusEndpoint must report true. Defaults to
	// DefaultParkedField ("parked"); basalt uses "asleep".
	ParkedField string `yaml:"parkedField"`

	// ConfirmTimeout, in seconds, bounds the confirmation poll after the park
	// command. 0 selects DefaultParkConfirmTimeout (10 s); a negative value is
	// rejected. A slow engine needs a larger value; a test wants a smaller one.
	ConfirmTimeout int `yaml:"confirmTimeout"`

	// WakeEndpoint is POSTed before the readiness gate on the wake path, e.g.
	// "/v1/wake". Leave it empty for engines that wake on the first request.
	WakeEndpoint string `yaml:"wakeEndpoint"`
}

// Validate rejects impossible park settings and reports the command the model
// would actually run, so a mis-armed entry fails at config load instead of at
// the first eviction.
func (p *ParkConfig) Validate() error {
	if p == nil {
		return nil
	}

	if p.Cmd != "" {
		if _, err := SanitizeCommand(p.Cmd); err != nil {
			return fmt.Errorf("cmd: invalid park command: %w", err)
		}
	}

	if p.ParkTimeout < 0 {
		return fmt.Errorf("parkTimeout must be >= 0 (0 uses %d s)", DefaultParkTimeout)
	}

	if p.WakeTimeout < 0 {
		return fmt.Errorf("wakeTimeout must be >= 0")
	}

	if p.ConfirmTimeout < 0 {
		return fmt.Errorf("confirmTimeout must be >= 0 (0 uses %d s)", DefaultParkConfirmTimeout)
	}

	if p.StatusEndpoint != "" && !strings.HasPrefix(p.StatusEndpoint, "/") {
		return fmt.Errorf("statusEndpoint must start with '/'")
	}

	if p.WakeEndpoint != "" && !strings.HasPrefix(p.WakeEndpoint, "/") {
		return fmt.Errorf("wakeEndpoint must start with '/'")
	}

	if p.ParkedField != "" {
		for _, r := range p.ParkedField {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
				continue
			}
			return fmt.Errorf("parkedField must match ^[a-zA-Z0-9_]+$")
		}
	}

	return nil
}

// ParkCommand returns the command that parks this model: park.cmd when set,
// otherwise the model's cmdStop. Returns "" when neither exists, which
// ValidateModelPark rejects.
func (m ModelConfig) ParkCommand() string {
	if m.Park != nil && m.Park.Cmd != "" {
		return m.Park.Cmd
	}
	return m.CmdStop
}

// ParkTimeout returns the drain-wait budget in seconds, applying the default.
func (m ModelConfig) ParkTimeout() int {
	if m.Park == nil {
		return DefaultParkTimeout
	}
	if m.Park.ParkTimeout == 0 {
		return DefaultParkTimeout
	}
	return m.Park.ParkTimeout
}

// WakeTimeout returns the readiness budget used when waking a parked model,
// 0 when the caller's timeout should stand. Waking is cheaper than a cold load
// but not free (the engine has to re-register its device tier), so a deployment
// can give it a budget of its own instead of inheriting healthCheckTimeout.
func (m ModelConfig) WakeTimeout() int {
	if m.Park == nil {
		return 0
	}
	return m.Park.WakeTimeout
}

// DestroyCommand returns the command that destroys this model outright. It is
// "" when the model declares none, which is today's behaviour: cmdStop, or no
// command at all.
func (m ModelConfig) DestroyCommand() string {
	return m.CmdDestroy
}

// ParkConfirmTimeout returns the confirmation budget in seconds, applying the
// default. It only matters when StatusEndpoint is armed.
func (m ModelConfig) ParkConfirmTimeout() int {
	if m.Park == nil || m.Park.ConfirmTimeout == 0 {
		return DefaultParkConfirmTimeout
	}
	return m.Park.ConfirmTimeout
}

// ParkedField returns the status JSON key that reports a parked engine.
func (m ModelConfig) ParkedField() string {
	if m.Park == nil || m.Park.ParkedField == "" {
		return DefaultParkedField
	}
	return m.Park.ParkedField
}

// CanPark reports that this model declared parking and has both verbs it needs:
// a park command and an explicit destroy command.
func (m ModelConfig) CanPark() bool {
	return m.Park != nil && m.ParkCommand() != "" && m.CmdDestroy != ""
}

// ValidateModelPark checks one model's park declaration. Called for every model
// at load time regardless of the selected router, so an entry that is parked
// later still fails early.
func ValidateModelPark(modelID string, m ModelConfig) error {
	if m.Park == nil {
		if m.CmdDestroy != "" {
			// cmdDestroy alone is allowed: it gives an entry an explicit destroy
			// command without arming parking.
			if _, err := SanitizeCommand(m.CmdDestroy); err != nil {
				return fmt.Errorf("cmdDestroy: invalid command: %w", err)
			}
		}
		if m.ParkTTL != 0 {
			return fmt.Errorf("parkTTL requires park to be armed")
		}
		return nil
	}

	if err := m.Park.Validate(); err != nil {
		return fmt.Errorf("park: %w", err)
	}

	if m.ParkCommand() == "" {
		return fmt.Errorf("park: no park command - set park.cmd or cmdStop")
	}

	if m.CmdDestroy == "" {
		return fmt.Errorf("park: cmdDestroy is required, unload and parkTTL must have an explicit destroy command")
	}

	if _, err := SanitizeCommand(m.CmdDestroy); err != nil {
		return fmt.Errorf("cmdDestroy: invalid command: %w", err)
	}

	if m.ParkTTL < 0 {
		return fmt.Errorf("parkTTL must be >= 0 (0 keeps a parked model parked)")
	}

	return nil
}

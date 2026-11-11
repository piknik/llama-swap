package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const parkModels = `models:
  awake:
    cmd: sleep 60 --port ${PORT}
    cmdStop: 'sleepctl park ${MODEL_ID}'
    cmdDestroy: 'sleepctl destroy ${MODEL_ID}'
    port: 5901
    parkTTL: 900
    park:
      cmd: 'sleepctl park ${MODEL_ID} --now'
      statusEndpoint: /v1/sleep
      parkedField: asleep
      wakeEndpoint: /v1/wake
      parkTimeout: 45
      wakeTimeout: 90
  plain:
    cmd: sleep 30 --port ${PORT}
  parkedonly:
    cmd: sleep 15 --port ${PORT}
    cmdStop: 'ctl park ${MODEL_ID}'
    cmdDestroy: 'ctl destroy ${MODEL_ID}'
    park: {}
`

func TestConfig_Park(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(parkModels))
	require.NoError(t, err)

	awake := cfg.Models["awake"]
	require.NotNil(t, awake.Park)
	require.Equal(t, "sleepctl park awake --now", awake.Park.Cmd)
	require.Equal(t, "/v1/sleep", awake.Park.StatusEndpoint)
	require.Equal(t, "asleep", awake.Park.ParkedField)
	require.Equal(t, "/v1/wake", awake.Park.WakeEndpoint)
	require.Equal(t, 45, awake.Park.ParkTimeout)
	require.Equal(t, 90, awake.Park.WakeTimeout)
	require.Equal(t, 900, awake.ParkTTL)
	require.Equal(t, 5901, awake.Port)
	require.True(t, awake.CanPark())
	// park.cmd wins over cmdStop
	require.Equal(t, "sleepctl park awake --now", awake.ParkCommand())

	// park.cmd absent: cmdStop is the park verb
	parked := cfg.Models["parkedonly"]
	require.NotNil(t, parked.Park)
	require.True(t, parked.CanPark())
	require.Equal(t, "ctl park parkedonly", parked.ParkCommand())
	require.Equal(t, DefaultParkTimeout, parked.ParkTimeout())
	require.Equal(t, DefaultParkedField, parked.ParkedField())

	// undeclared models are never parkable and keep the default timeouts
	plain := cfg.Models["plain"]
	require.Nil(t, plain.Park)
	require.False(t, plain.CanPark())
	require.Empty(t, plain.ParkCommand())
	require.Equal(t, DefaultParkTimeout, plain.ParkTimeout())
}

// TestConfig_ParkValidation covers the declarations that would otherwise fail at
// the first eviction instead of at load.
func TestConfig_ParkValidation(t *testing.T) {
	cases := []struct {
		name   string
		models string
		want   string
	}{
		{
			name:   "park without cmdDestroy",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    park: {}\n",
			want:   "cmdDestroy is required",
		},
		{
			name:   "park without any park command",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdDestroy: ctl destroy\n    park: {}\n",
			want:   "no park command",
		},
		{
			name:   "parkTTL without park",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    parkTTL: 60\n",
			want:   "parkTTL requires park",
		},
		{
			name:   "negative parkTimeout",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      parkTimeout: -5\n",
			want:   "parkTimeout must be >= 0",
		},
		{
			name:   "statusEndpoint without leading slash",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      statusEndpoint: v1/sleep\n",
			want:   "statusEndpoint must start with '/'",
		},
		{
			name:   "wakeEndpoint without leading slash",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      wakeEndpoint: wake\n",
			want:   "wakeEndpoint must start with '/'",
		},
		{
			name:   "parkedField with invalid characters",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      parkedField: \"is parked\"\n",
			want:   "parkedField must match",
		},
		{
			name:   "negative parkTTL",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    parkTTL: -1\n    park: {}\n",
			want:   "parkTTL must be >= 0",
		},
		{
			name:   "parkedField rejects a dotted path",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      parkedField: status.parked\n",
			want:   "parkedField must match",
		},
		{
			name:   "park.detect that is only a comment",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      detect: \"# nothing but a note\"\n",
			want:   "detect: invalid command",
		},
		{
			name:   "park.detect cannot reference ${PID}",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    cmdStop: ctl park\n    cmdDestroy: ctl destroy\n    park:\n      detect: ctl is-parked ${PID}\n",
			want:   "${PID} is not available at detection time",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader("models:\n" + tc.models))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestConfig_PinnedPort is the reason a parkable model declares its own port:
// ${PORT} is allocated in sorted model-ID order, so every entry added later
// renumbers the models after it, and a parked engine keeps listening on the port
// it was started with.
func TestConfig_PinnedPort(t *testing.T) {
	base := `models:
  mma:
    cmd: sleep 1 --port ${PORT}
  pin:
    cmd: sleep 1 --port ${PORT}
    port: 5901
  zzz:
    cmd: sleep 1 --port ${PORT}
`

	cfg, err := LoadConfigFromReader(strings.NewReader(base))
	require.NoError(t, err)
	require.Equal(t, 5800, cfg.Models["mma"].Port)
	require.Equal(t, 5901, cfg.Models["pin"].Port)
	require.Equal(t, 5801, cfg.Models["zzz"].Port)
	require.Contains(t, cfg.Models["pin"].Cmd, "--port 5901")
	require.Contains(t, cfg.Models["pin"].Proxy, ":5901")

	// An entry added before the pin in sort order must not move it: the pin is
	// reserved before allocation, and allocation steps over it.
	withExtra := strings.Replace(base, "  pin:\n    cmd: sleep 1 --port ${PORT}\n    port: 5901\n",
		"  aaa:\n    cmd: sleep 1 --port ${PORT}\n  pin:\n    cmd: sleep 1 --port ${PORT}\n    port: 5801\n", 1)
	cfg, err = LoadConfigFromReader(strings.NewReader(withExtra))
	require.NoError(t, err)
	require.Equal(t, 5800, cfg.Models["aaa"].Port)
	require.Equal(t, 5801, cfg.Models["pin"].Port)
	// mma and zzz were 5800/5801 when the pin sorted elsewhere; now the pin owns
	// 5801 and allocation steps over it instead of onto it.
	require.Equal(t, 5802, cfg.Models["mma"].Port)
	require.Equal(t, 5803, cfg.Models["zzz"].Port)
	ports := map[int]bool{}
	for _, m := range cfg.Models {
		require.False(t, ports[m.Port], "two models allocated port %d", m.Port)
		ports[m.Port] = true
	}

	// startPort still applies to the unpinned models.
	stPort := "startPort: 6100\n" + strings.Replace(base, "port: 5901", "port: 6101", 1)
	cfg, err = LoadConfigFromReader(strings.NewReader(stPort))
	require.NoError(t, err)
	require.Equal(t, 6100, cfg.Models["mma"].Port)
	require.Equal(t, 6101, cfg.Models["pin"].Port)
	require.Equal(t, 6102, cfg.Models["zzz"].Port)
}

func TestConfig_PinnedPortValidation(t *testing.T) {
	cases := []struct {
		name   string
		models string
		want   string
	}{
		{
			name:   "duplicate pinned port",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    port: 5901\n  b:\n    cmd: sleep 1 --port ${PORT}\n    port: 5901\n",
			want:   "already pinned by model a",
		},
		{
			name:   "pinned port without ${PORT} in cmd",
			models: "  a:\n    cmd: sleep 1\n    port: 5901\n",
			want:   "pinned but cmd does not use ${PORT}",
		},
		{
			name:   "port out of range",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    port: 65536\n",
			want:   "between 1 and 65535",
		},
		{
			name:   "port is not a number",
			models: "  a:\n    cmd: sleep 1 --port ${PORT}\n    port: \"5901\"\n",
			want:   "must be an integer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader("models:\n" + tc.models))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestConfig_ParkDetectPort: ${PORT} is available to park.detect too, because a
// detection script has to ask the engine on the port it actually bound — and detect
// may be the only field in the model that references a port at all.
func TestConfig_ParkDetectPort(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`models:
  detector:
    cmd: sleep 1
    cmdDestroy: ctl destroy detector
    park:
      cmd: ctl park detector --port ${PORT}
      detect: ctl is-parked detector --port ${PORT}
  other:
    cmd: sleep 1 --port ${PORT}
`))
	require.NoError(t, err)

	detector := cfg.Models["detector"]
	require.Equal(t, "ctl park detector --port 5800", detector.ParkCommand())
	require.Equal(t, "ctl is-parked detector --port 5800", detector.DetectCommand())
	require.Equal(t, 5800, detector.Port)
	// The auto allocation stepped over the model that only needed the port for its
	// park fields, and gave the next model the following one.
	require.Equal(t, 5801, cfg.Models["other"].Port)
}

// TestConfig_ParkDetectIsDeclaredAboveTheScalar: a `#` note inside a folded
// park.detect scalar would fold into that scalar's first line and swallow the
// command (an "empty command" load error, on purpose), so the note goes above the
// key. This is the shape an operator should write, and ${MODEL_ID} is substituted
// in park.detect like in every other command field.
func TestConfig_ParkDetectIsDeclaredAboveTheScalar(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`models:
  a:
    cmd: sleep 1 --port ${PORT}
    cmdStop: ctl park a
    cmdDestroy: ctl destroy a
    # Ask the engine container whether it is still parked before cold-loading.
    park:
      detect: /models/.pcni/park-detect.sh ${MODEL_ID}
`))
	require.NoError(t, err)
	require.Equal(t, "/models/.pcni/park-detect.sh a", cfg.Models["a"].DetectCommand())
}

package router

import (
	"errors"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// parkTestConfig is a two-model setup: "parkable" declares park and cmdDestroy,
// "plain" declares neither.
func parkTestConfig() config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			"parkable": {
				Cmd:        "run-parkable",
				CmdStop:    "docker stop llama-swap_parkable",
				CmdDestroy: "docker rm -f llama-swap_parkable",
				Park:       &config.ParkConfig{Cmd: "curl -fsS http://127.0.0.1:${PORT}/v1/sleep"},
			},
			"plain": {Cmd: "run-plain", CmdStop: "docker stop llama-swap_plain"},
		},
	}
}

// TestRouter_UnloadDestroys pins the verb unload means: the scheduler's unload
// effect goes through Destroy, so a park-capable model unloaded on purpose has its
// container removed too, not merely released.
func TestRouter_UnloadDestroys(t *testing.T) {
	fake := newFakeProcess("parkable")
	fake.markReady()
	r := newTestGroup(t, parkTestConfig(), map[string]process.Process{"parkable": fake})

	r.Unload(0, "parkable")

	if got := fake.destroyCalls.Load(); got != 1 {
		t.Errorf("unload called Destroy %d times, want 1", got)
	}
	if got := fake.parkCalls.Load(); got != 0 {
		t.Errorf("unload parked the model %d times, want 0: unload discards", got)
	}
}

// TestRouter_ParkModelNeedsParkArmed shows park is opt-in per model: the router
// refuses to park a model that never declared park:, and never reaches the
// process.
func TestRouter_ParkModelNeedsParkArmed(t *testing.T) {
	plain := newFakeProcess("plain")
	plain.markReady()
	r := newTestGroup(t, parkTestConfig(), map[string]process.Process{"plain": plain})

	if err := r.ParkModel("plain"); err == nil {
		t.Fatal("ParkModel succeeded on a model with no park: block")
	}
	if got := plain.parkCalls.Load(); got != 0 {
		t.Errorf("process Park called %d times on an unarmed model, want 0", got)
	}
	if err := r.ParkModel("no-such-model"); err == nil {
		t.Error("ParkModel succeeded on an unknown model")
	}
}

// TestRouter_ParkModelAndUnparkModel walks the two explicit verbs: park takes the
// model out of service while keeping it, and unpark runs the normal start path to
// bring it back.
func TestRouter_ParkModelAndUnparkModel(t *testing.T) {
	fake := newFakeProcess("parkable")
	fake.autoReady = true // the wake's readiness gate passes without a real health check
	fake.markReady()
	r := newTestGroup(t, parkTestConfig(), map[string]process.Process{"parkable": fake})

	if err := r.ParkModel("parkable"); err != nil {
		t.Fatalf("ParkModel: %v", err)
	}
	if got := fake.State(); got != process.StateParked {
		t.Fatalf("after ParkModel state is %s, want %s", got, process.StateParked)
	}
	if err := r.ParkModel("parkable"); err == nil {
		t.Error("second ParkModel succeeded on an already parked model")
	}

	// Parked is not ready: the scheduler sees a model that needs a swap, which is
	// how the next request wakes it. It is not stopped either: an operator listing
	// models has to see that it is holding something.
	state, known := r.ModelState("parkable")
	if !known || state != process.StateParked {
		t.Errorf("ModelState after park: %s (known=%v), want %s", state, known, process.StateParked)
	}
	if got := r.RunningModels()["parkable"]; got == process.StateReady {
		t.Errorf("RunningModels reports %s for a parked model, want a non-ready state", got)
	}
	if status := r.RunningStatus()["parkable"]; status.State != process.StateParked {
		t.Errorf("RunningStatus reports %s for a parked model, want %s", status.State, process.StateParked)
	}

	if err := r.UnparkModel("parkable"); err != nil {
		t.Fatalf("UnparkModel: %v", err)
	}
	if got := fake.State(); got != process.StateReady {
		t.Fatalf("after UnparkModel state is %s, want %s", got, process.StateReady)
	}
	if got := fake.runCalls.Load(); got == 0 {
		t.Error("UnparkModel did not run the model's cmd: waking re-attaches through cmd")
	}
	if err := r.UnparkModel("parkable"); err == nil {
		t.Error("UnparkModel succeeded on a model that is not parked")
	}
}

// TestRouter_ParkModelReportsDrainRefusal keeps the busy answer visible: a model
// that would have been cut off returns an error and stays serving.
func TestRouter_ParkModelReportsDrainRefusal(t *testing.T) {
	fake := newFakeProcess("parkable")
	fake.markReady()
	fake.parkErr = errors.New("still busy")
	r := newTestGroup(t, parkTestConfig(), map[string]process.Process{"parkable": fake})

	if err := r.ParkModel("parkable"); err == nil {
		t.Fatal("ParkModel reported success for a model that refused to park")
	}
	if got := fake.State(); got != process.StateReady {
		t.Errorf("a refused park left the model in %s, want %s: the refusal leaves it serving",
			got, process.StateReady)
	}
}

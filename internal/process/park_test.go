package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// parkTestEngine stands in for a park-capable engine container. The important
// shape is that the engine is NOT the process llama-swap tracks: the tracked
// client is a shell llama-swap can release while the engine keeps serving, which
// is exactly the relationship between `docker run` and the container it runs.
//
// Its state lives in a file the park command writes, so "am I parked" is answered
// by the engine's own status endpoint (GET /v1/sleep) rather than by llama-swap's
// assumption, and POST /v1/wake clears it.
type parkTestEngine struct {
	t    *testing.T
	dir  string
	srv  *httptest.Server
	mu   sync.Mutex
	wake int
	// release, when non-nil, makes /slow block until the test closes it.
	release chan struct{}
}

func newParkTestEngine(t *testing.T) *parkTestEngine {
	t.Helper()

	e := &parkTestEngine{t: t, dir: t.TempDir()}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		release := e.release
		e.mu.Unlock()
		if release != nil {
			<-release
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "slow ok")
	})
	mux.HandleFunc("/v1/sleep", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parked := e.isParked()
		if r.Method == http.MethodPost {
			e.setParked(true)
			parked = true
		}
		json.NewEncoder(w).Encode(map[string]any{
			"parked":      parked,
			"freed_bytes": 1,
		})
	})
	mux.HandleFunc("/v1/wake", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.wake++
		e.mu.Unlock()
		e.setParked(false)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"parked": false})
	})

	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *parkTestEngine) parkedFile() string { return filepath.Join(e.dir, "parked") }
func (e *parkTestEngine) destroyedFile() string {
	return filepath.Join(e.dir, "destroyed")
}

func (e *parkTestEngine) isParked() bool {
	_, err := os.Stat(e.parkedFile())
	return err == nil
}

func (e *parkTestEngine) setParked(parked bool) {
	if parked {
		os.WriteFile(e.parkedFile(), []byte("1"), 0o644)
		return
	}
	os.Remove(e.parkedFile())
}

// parkCmd is the command the model runs to park: it marks the engine parked and
// releases the tracked client, leaving the engine up. It is the shape of ca4's
// live `POST /v1/sleep … kill -9 ${PID}` cmdStop, minus docker.
func (e *parkTestEngine) parkCmd() string {
	return fmt.Sprintf(`sh -c "touch %s; kill ${PID}"`, e.parkedFile())
}

// destroyCmd is the destroy verb. It deliberately has no ${PID}: a destroy
// command has to work on a parked model too, where there is no process to
// signal, so it names the engine (here: writes the marker the test checks; on a
// real deployment: `docker rm -f llama-swap_<id>`). The tracked client is then
// taken down by the normal graceful-then-kill teardown.
func (e *parkTestEngine) destroyCmd() string {
	return fmt.Sprintf(`sh -c "touch %s"`, e.destroyedFile())
}

func (e *parkTestEngine) wakeCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wake
}

func (e *parkTestEngine) blockSlow() {
	e.mu.Lock()
	e.release = make(chan struct{})
	e.mu.Unlock()
}

func (e *parkTestEngine) unblockSlow() {
	e.mu.Lock()
	release := e.release
	e.release = nil
	e.mu.Unlock()
	if release != nil {
		close(release)
	}
}

// parkTestConfig builds a model whose tracked client is a shell (so parking has
// something to release) and whose engine is the fake server.
func parkTestConfig(t *testing.T, e *parkTestEngine) config.ModelConfig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("park tests drive POSIX shell commands")
	}

	return config.ModelConfig{
		// sleep is the tracked process itself, not a shell's child: the engine
		// side of this test is the fake server, and killing the tracked client must
		// not leave a grandchild holding the log pipes open (which cmd.WaitDelay
		// would only close 10 s later).
		Cmd:                "sleep 120",
		CmdStop:            e.parkCmd(),
		CmdDestroy:         e.destroyCmd(),
		Proxy:              e.srv.URL,
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
		UnloadTimeout:      5,
		Park: &config.ParkConfig{
			StatusEndpoint: "/v1/sleep",
			WakeEndpoint:   "/v1/wake",
			ParkTimeout:    2,
			ConfirmTimeout: 1,
		},
	}
}

func engineIsUp(t *testing.T, e *parkTestEngine) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/health", nil)
	if err != nil {
		t.Fatalf("engine request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func readMarker(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func waitForMarker(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readMarker(path) {
			return
		}
		time.Sleep(testPollInterval)
	}
	t.Fatalf("%s was never written", path)
}

// TestProcessCommand_ParkKeepsTheEngineUp is the core claim of parking: llama-swap
// releases the process it tracks and the engine is still there, so the next
// request re-attaches instead of cold-loading.
func TestProcessCommand_ParkKeepsTheEngineUp(t *testing.T) {
	e := newParkTestEngine(t)
	p := newProcessCommand(t, parkTestConfig(t, e))

	runErr := runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Destroy(testStopTimeout)
		}
	}()

	if err := p.Park(context.Background(), 0); err != nil {
		t.Fatalf("Park: expected nil, got %v", err)
	}

	waitForState(t, p, StateParked)

	if !e.isParked() {
		t.Error("engine was never parked: park command did not run")
	}
	if !engineIsUp(t, e) {
		t.Error("engine stopped serving after Park: parking must not destroy it")
	}

	if since := p.Status().ParkedSince; since.IsZero() {
		t.Error("Status().ParkedSince is zero: parkTTL and any operator readout need it")
	}

	// Run() is released by parking: from llama-swap's side the process it tracked
	// is gone, even though the engine is still up.
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run() after Park: expected nil, got %v", err)
		}
	case <-time.After(testReturnTimeout):
		t.Fatal("Run() did not return after Park")
	}

	// The wake path: EnsureReady on a parked model goes through cmd + readiness
	// gate again and the engine answers on the same server instance.
	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("EnsureReady on a parked model: %v", err)
	}

	if got := p.State(); got != StateReady {
		t.Fatalf("after wake: state is %s, want %s", got, StateReady)
	}
	if e.wakeCalls() == 0 {
		t.Error("wake endpoint was never POSTed: park.wakeEndpoint is armed and must be used")
	}
	if e.isParked() {
		t.Error("engine is still marked parked after the wake")
	}

	rr := serveThrough(p, "/health")
	if rr.Code != http.StatusOK {
		t.Errorf("after wake: expected 200, got %d", rr.Code)
	}
}

// TestProcessCommand_ParkWaitsOutAnInFlightRequest is decision 3 of the park
// design: a model with a generation running is waited out, and a park that runs
// out of patience refuses instead of cutting it.
func TestProcessCommand_ParkWaitsOutAnInFlightRequest(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.Park.ParkTimeout = 1
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Destroy(testStopTimeout)
		}
	}()

	e.blockSlow()
	served := make(chan int, 1)
	go func() {
		rr := serveThrough(p, "/slow")
		served <- rr.Code
	}()

	// Give the request time to land in the process's in-flight counter.
	deadline := time.Now().Add(2 * time.Second)
	for p.inflight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(testPollInterval)
	}
	if p.inflight.Load() == 0 {
		t.Fatal("could not get a request in flight to test the drain")
	}

	err := p.Park(context.Background(), 200*time.Millisecond)
	if !errors.Is(err, ErrParkBusy) {
		t.Fatalf("Park while busy: expected ErrParkBusy, got %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("Park refused: state is %s, want %s (a refused park must leave the model serving)", got, StateReady)
	}
	if e.isParked() {
		t.Error("park command ran despite the park being refused")
	}

	e.unblockSlow()
	select {
	case code := <-served:
		if code != http.StatusOK {
			t.Errorf("in-flight request: expected 200, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed: the refused park cut it")
	}

	if err := p.Park(context.Background(), 0); err != nil {
		t.Fatalf("Park after the request finished: expected nil, got %v", err)
	}
	waitForState(t, p, StateParked)
}

// TestProcessCommand_ParkUnconfirmedIsStopped: an engine that never reports itself
// parked is recorded stopped. llama-swap must not claim a model holds a container
// it cannot verify, because a phantom parked model is a model nobody can wake.
func TestProcessCommand_ParkUnconfirmedIsStopped(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.Park.StatusEndpoint = "/v1/never-parked"
	cfg.CmdStop = fmt.Sprintf(`sh -c "kill ${PID}"`)
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() != StateStopped {
			p.Destroy(testStopTimeout)
		}
	}()

	err := p.Park(context.Background(), 0)
	if !errors.Is(err, ErrParkUnconfirmed) {
		t.Fatalf("Park with no confirmation: expected ErrParkUnconfirmed, got %v", err)
	}
	waitForState(t, p, StateStopped)

	if readMarker(e.destroyedFile()) {
		t.Log("destroy command ran during the unconfirmed park teardown")
	}
}

// TestProcessCommand_TTL_ParksParkCapableModel: ttl parks a model that declared
// park, and destroys one that did not.
func TestProcessCommand_TTL_ParksParkCapableModel(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.UnloadAfter = 1
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() != StateStopped {
			p.Destroy(testStopTimeout)
		}
	}()

	waitForState(t, p, StateParked)

	if !e.isParked() {
		t.Error("ttl did not run the park command")
	}
	if readMarker(e.destroyedFile()) {
		t.Error("ttl destroyed a model that declared park")
	}
	if !engineIsUp(t, e) {
		t.Error("ttl park destroyed the engine")
	}
}

// TestProcessCommand_TTL_StillDestroysUnarmedModel keeps upstream's behaviour on
// the table: without park: armed, ttl unloads exactly as it did before.
func TestProcessCommand_TTL_StillDestroysUnarmedModel(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.UnloadAfter = 1
	cfg.Park = nil
	// An unload with no park: armed uses cmdStop, and a stop verb may use ${PID}.
	cfg.CmdStop = fmt.Sprintf(`sh -c "touch %s; kill ${PID}"`, e.destroyedFile())
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	waitForState(t, p, StateStopped)

	if !readMarker(e.destroyedFile()) {
		t.Error("ttl did not run the stop command on an unarmed model")
	}
}

// TestProcessCommand_ParkTTLDestroys bounds how long a parked model may keep its
// host RAM: parkTTL destroys it, and destroy is the destroy verb, not the park
// verb.
func TestProcessCommand_ParkTTLDestroys(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.ParkTTL = 1
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() != StateStopped {
			p.Destroy(testStopTimeout)
		}
	}()

	if err := p.Park(context.Background(), 0); err != nil {
		t.Fatalf("Park: %v", err)
	}
	waitForState(t, p, StateParked)
	waitForState(t, p, StateStopped)

	if !readMarker(e.destroyedFile()) {
		t.Error("parkTTL did not run cmdDestroy")
	}
}

// TestProcessCommand_ParkTTLIsCancelledByWake: traffic cancels a pending
// parkTTL destroy the same way it cancels a ttl unload today.
func TestProcessCommand_ParkTTLIsCancelledByWake(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.ParkTTL = 3
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Destroy(testStopTimeout)
		}
	}()

	if err := p.Park(context.Background(), 0); err != nil {
		t.Fatalf("Park: %v", err)
	}
	waitForState(t, p, StateParked)

	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("wake: %v", err)
	}

	time.Sleep(4 * time.Second)
	if got := p.State(); got != StateReady {
		t.Fatalf("wake then parkTTL: state is %s, want %s", got, StateReady)
	}
	if readMarker(e.destroyedFile()) {
		t.Error("parkTTL destroyed a model that was woken first")
	}
}

// TestProcessCommand_DestroyRunsCmdDestroy is the unload decision: Stop, which is
// what /api/models/unload and a router eviction use, takes the destroy verb on a
// park-capable model rather than the park verb.
func TestProcessCommand_DestroyRunsCmdDestroy(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	p := newProcessCommand(t, cfg)

	runAsync(t, p)

	if err := p.Destroy(testStopTimeout); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	waitForState(t, p, StateStopped)

	if !readMarker(e.destroyedFile()) {
		t.Error("Destroy did not run cmdDestroy")
	}
	if e.isParked() {
		t.Error("Destroy parked the model instead of destroying it")
	}
}

// TestProcessCommand_StopUsesCmdStopNotCmdDestroy keeps the two verbs apart: Stop
// (what a router eviction uses today) runs cmdStop, and only Destroy runs
// cmdDestroy. What the engine does with cmdStop — park or die — is the command's
// business; the state machine must not mix the verbs.
func TestProcessCommand_StopUsesCmdStopNotCmdDestroy(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	p := newProcessCommand(t, cfg)

	runAsync(t, p)

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForState(t, p, StateStopped)

	if !e.isParked() {
		t.Error("Stop did not run cmdStop")
	}
	if readMarker(e.destroyedFile()) {
		t.Error("Stop ran cmdDestroy: only Destroy may use the destroy verb")
	}
}

// TestProcessCommand_ParkRefusedWhenNotArmed keeps the fork idle for every model
// that did not declare park:.
func TestProcessCommand_ParkRefusedWhenNotArmed(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	cfg.Park = nil
	cfg.CmdStop = e.destroyCmd()
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		if p.State() == StateReady {
			p.Destroy(testStopTimeout)
		}
	}()

	err := p.Park(context.Background(), 0)
	if err == nil {
		t.Fatal("Park on a model without park: expected an error")
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("state after a refused park: %s, want %s", got, StateReady)
	}
}

// TestProcessCommand_StopOnParkedIsANoOp guards the orphan hazard. Evictions and
// other teardown paths call Stop; if Stop marked a parked model stopped, the
// container it still holds would become invisible and the next start would lose
// the container-name race. Discarding a parked engine is Destroy's job.
func TestProcessCommand_StopOnParkedIsANoOp(t *testing.T) {
	e := newParkTestEngine(t)

	cfg := parkTestConfig(t, e)
	p := newProcessCommand(t, cfg)

	runAsync(t, p)
	defer func() {
		p.Destroy(testStopTimeout)
	}()

	if err := p.Park(context.Background(), 0); err != nil {
		t.Fatalf("Park: %v", err)
	}
	waitForState(t, p, StateParked)

	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop on a parked model: %v", err)
	}
	if got := p.State(); got != StateParked {
		t.Fatalf("Stop changed a parked model to %s: parked must survive Stop", got)
	}
	if readMarker(e.destroyedFile()) {
		t.Error("Stop destroyed a parked model: only Destroy may do that")
	}

	// Still wakeable: the whole point of keeping it parked.
	ctx, cancel := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancel()
	if err := p.EnsureReady(ctx, testStartTimeout); err != nil {
		t.Fatalf("wake after Stop: %v", err)
	}
	if got := p.State(); got != StateReady {
		t.Fatalf("wake after Stop: state is %s, want %s", got, StateReady)
	}
}

func serveThrough(p *ProcessCommand, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	p.ServeHTTP(rr, req)
	return rr
}

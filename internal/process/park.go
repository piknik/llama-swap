package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Parking bounds. The release bound covers a park command that parks the engine
// but leaves the tracked client attached (the `docker run`/`docker attach` client
// llama-swap is waiting on); the confirm bound is how long llama-swap polls the
// engine's status endpoint before concluding the model was destroyed rather than
// parked.
const (
	parkReleaseTimeout = 15 * time.Second
	parkPollInterval   = 200 * time.Millisecond
)

// ErrParkBusy reports that a park was refused because the model still had
// requests in flight when its parkTimeout expired. The model is left awake:
// parking must never cut a running generation.
var ErrParkBusy = errors.New("park refused: requests still in flight")

// ErrParkUnconfirmed reports that the park command ran but the engine never
// reported the model parked. The model is recorded stopped, because an
// unconfirmed park is indistinguishable from a destroy and must not be trusted
// to still be holding a container.
var ErrParkUnconfirmed = errors.New("park unconfirmed: engine did not report the model parked")

type parkReq struct {
	drainTimeout time.Duration
	respond      chan error
}

// Park parks a ready process. The engine keeps its container and its host-side
// state (weights, KV, caches) and releases the device; llama-swap stops tracking
// a process and records the model as StateParked. drainTimeout bounds the wait
// for in-flight requests, 0 selects the model's parkTimeout.
//
// Returns ErrParkBusy when the model stayed busy (it is still awake and serving),
// ErrParkUnconfirmed when the park could not be confirmed (the model is recorded
// stopped), and nil when the model is parked.
func (p *ProcessCommand) Park(ctx context.Context, drainTimeout time.Duration) error {
	req := parkReq{drainTimeout: drainTimeout, respond: make(chan error, 1)}
	select {
	case p.parkCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	select {
	case err := <-req.respond:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
}

// Destroy stops the process outright, running cmdDestroy when the model declares
// one and otherwise falling back to cmdStop / SIGTERM. After Destroy the next
// request cold-loads.
func (p *ProcessCommand) Destroy(timeout time.Duration) error {
	req := stopReq{timeout: timeout, respond: make(chan error, 1), destroy: true}
	select {
	case p.stopCh <- req:
	case <-p.parentCtx.Done():
		return fmt.Errorf("[%s] shutdown", p.id)
	}
	return <-req.respond
}

// parkCommand is the command that parks this model: park.cmd when set, otherwise
// cmdStop. config.ModelConfig.ParkCommand already encodes that precedence.
func (p *ProcessCommand) parkCommand() string {
	return p.config.ParkCommand()
}

// waitForIdle blocks until the process has no in-flight proxied requests, or the
// timeout expires. A parked engine must not be interrupted mid-generation.
func (p *ProcessCommand) waitForIdle(timeout time.Duration) bool {
	if p.inflight.Load() == 0 {
		return true
	}
	if timeout <= 0 {
		return false
	}

	deadline := time.Now().Add(timeout)
	for {
		if p.inflight.Load() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(parkPollInterval)
	}
}

// awaitClientExit waits for the tracked upstream client to exit on its own.
func (p *ProcessCommand) awaitClientExit(cmdDone <-chan struct{}, timeout time.Duration) bool {
	if cmdDone == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-cmdDone:
		return true
	case <-timer.C:
		return false
	}
}

// engineURL builds a URL against the same host the model is proxied to, with
// endpoint as the path. The proxy target is what reaches the engine (a container
// name on ca4, localhost for a process on the host), so parking and waking use
// exactly the address the gateway already knows works.
func (p *ProcessCommand) engineURL(endpoint string) (*url.URL, error) {
	base, err := url.Parse(p.config.Proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy %q: %w", p.config.Proxy, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("proxy %q has no host", p.config.Proxy)
	}

	target := *base
	target.Path = endpoint
	target.RawQuery = ""
	target.Fragment = ""
	return &target, nil
}

// confirmParked polls the model's park.statusEndpoint until it reports the parked
// field truthy, or the confirm bound expires. A model with no statusEndpoint is
// confirmed by the park command having run: there is nothing to ask.
func (p *ProcessCommand) confirmParked() (bool, error) {
	if p.config.Park == nil || p.config.Park.StatusEndpoint == "" {
		return true, nil
	}

	target, err := p.engineURL(p.config.Park.StatusEndpoint)
	if err != nil {
		return false, err
	}

	deadline := time.Now().Add(time.Duration(p.config.ParkConfirmTimeout()) * time.Second)
	lastErr := error(nil)
	for {
		parked, err := p.engineReportsParked(target)
		if parked {
			return true, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			if lastErr != nil {
				return false, lastErr
			}
			return false, nil
		}
		time.Sleep(parkPollInterval)
	}
}

// engineReportsParked issues one GET and looks for the parked field in the JSON
// answer. NInfer answers {"parked":true,...} on GET /v1/sleep; basalt reports
// "asleep" per model in /v1/models, so the search is recursive and accepts a
// boolean or the strings "true"/"1".
func (p *ProcessCommand) engineReportsParked(target *url.URL) (bool, error) {
	ctx, cancel := context.WithTimeout(p.parentCtx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s answered %d", target.String(), resp.StatusCode)
	}

	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("%s answered unreadable JSON: %w", target.String(), err)
	}

	return jsonReportsFlag(body, p.config.ParkedField()), nil
}

// jsonReportsFlag reports whether key appears anywhere in the decoded JSON with a
// truthy value.
func jsonReportsFlag(value any, key string) bool {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			if k == key && truthyFlag(child) {
				return true
			}
			if jsonReportsFlag(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if jsonReportsFlag(child, key) {
				return true
			}
		}
	}
	return false
}

func truthyFlag(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case float64:
		return v == 1
	}
	return false
}

// wakeEngine POSTs the model's wake endpoint, for engines that wake on an
// explicit route rather than on the first request. A failure here is not fatal:
// the readiness gate that follows is what decides whether the model serves, and
// engines that wake on request answer 404 or 501 to this POST.
func (p *ProcessCommand) wakeEngine(log *logmon.Monitor) {
	if p.config.Park == nil || p.config.Park.WakeEndpoint == "" {
		return
	}

	target, err := p.engineURL(p.config.Park.WakeEndpoint)
	if err != nil {
		log.Warnf("<%s> wake: %v", p.id, err)
		return
	}

	ctx, cancel := context.WithTimeout(p.parentCtx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		log.Warnf("<%s> wake: %v", p.id, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("<%s> wake request failed: %v (the engine may wake on the first request)", p.id, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Debugf("<%s> wake answered %d (the engine may wake on the first request)", p.id, resp.StatusCode)
		return
	}

	log.Infof("<%s> wake: POST %s -> %d", p.id, target.Path, resp.StatusCode)
}

// parkUpstream runs the model's park command against the tracked upstream
// process. When park.cmd is unset the park command *is* cmdStop, so the existing
// stop path is used and no new mechanics enter the hot path. When it is set, the
// command runs with the same environment, process-group and pipe-backstop rules
// cmdStop runs with.
func (p *ProcessCommand) parkUpstream(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("no tracked upstream process to park")
	}

	cmdStr := p.parkCommand()
	if cmdStr == "" {
		return fmt.Errorf("no park command configured")
	}
	if cmdStr == p.config.CmdStop {
		return p.sendStopSignal(cmd, false)
	}

	pid := cmd.Process.Pid
	args, err := config.SanitizeCommand(strings.ReplaceAll(cmdStr, "${PID}", fmt.Sprintf("%d", pid)))
	if err != nil {
		return fmt.Errorf("invalid park command %q: %w", cmdStr, err)
	}

	parkCmd := exec.Command(args[0], args[1:]...)
	parkCmd.Stderr = p.processLogger
	parkCmd.Stdout = p.processLogger
	// Same WaitDelay as cmdStop: a park command that backgrounds a child holding
	// the output pipes must not block the run loop. See cmdWaitDelay.
	parkCmd.WaitDelay = p.waitDelay
	parkCmd.Env = cmd.Env
	setProcAttributes(parkCmd)

	p.processLogger.Debugf("<%s> running park command: %s", p.id, strings.Join(args, " "))

	runErr := parkCmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		p.processLogger.Warnf("<%s> park command exited but a child held its output open; output may be truncated", p.id)
		runErr = nil
	}
	return runErr
}

// destroyParked runs cmdDestroy for a model that is parked: there is no tracked
// process to signal, so the command has to name the engine itself. A destroy
// command that needs ${PID} is refused rather than run against a pid that no
// longer exists — substituting a stand-in there risks signalling the wrong
// process (kill 0 means "every process on the system").
//
// The shape that works is the container: `docker rm -f llama-swap_${MODEL_ID}`.
func (p *ProcessCommand) destroyParked() error {
	cmdStr := p.config.DestroyCommand()
	if cmdStr == "" {
		return fmt.Errorf("no cmdDestroy configured, cannot destroy parked model from llama-swap")
	}
	if strings.Contains(cmdStr, "${PID}") {
		return fmt.Errorf("cmdDestroy references ${PID} but the model is parked: there is no process to signal, address the engine itself (docker rm -f llama-swap_${MODEL_ID})")
	}

	args, err := config.SanitizeCommand(cmdStr)
	if err != nil {
		return fmt.Errorf("invalid cmdDestroy %q: %w", cmdStr, err)
	}

	destroyCmd := exec.Command(args[0], args[1:]...)
	destroyCmd.Stderr = p.processLogger
	destroyCmd.Stdout = p.processLogger
	destroyCmd.WaitDelay = p.waitDelay
	setProcAttributes(destroyCmd)

	p.processLogger.Infof("<%s> destroying parked model: %s", p.id, strings.Join(args, " "))

	runErr := destroyCmd.Run()
	if errors.Is(runErr, exec.ErrWaitDelay) {
		p.processLogger.Warnf("<%s> destroy command exited but a child held its output open; output may be truncated", p.id)
		runErr = nil
	}
	return runErr
}

package process

import (
	"bytes"
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

// Parking bounds. `parkReleaseGrace` is how long llama-swap waits for the park
// command to release the tracked `docker run`/`docker attach` client itself (the
// fleet's entries do this with `kill -9 ${PID}`) before llama-swap releases it
// with SIGKILL. SIGTERM is deliberately not used: docker's attach signal proxy
// forwards it into the container, so a graceful park would shut down the engine
// container parking is meant to preserve (measured 2026-11-11 via docker events).
// `parkKillTimeout` bounds the wait for that release to take effect; the confirm
// bound is how long llama-swap polls the engine's status endpoint before
// concluding the model was destroyed rather than parked.
const (
	parkReleaseGrace = 2 * time.Second
	parkKillTimeout  = 5 * time.Second
	parkPollInterval = 200 * time.Millisecond
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

// adoptParked asks `park.detect` whether an engine this llama-swap instance does
// not know about is already up and parked, and adopts it when it says so. It is
// what makes a gateway restart safe for a parked entry: the container outlives
// llama-swap, but llama-swap's knowledge of it does not, so without adoption the
// first request cold-replaces an engine that was ready to wake in seconds — and
// the replacement loses the container-name race.
//
// Detection is a command, not a Docker call. llama-swap calls no Docker API
// anywhere (the lifecycle is all `docker` CLI through cmd/cmdStop), and only the
// deployment knows how its containers are named, so the script does the looking
// and answers one JSON object on stdout:
//
//	{"parked":true,"proxy":"http://llama-swap_<id>:<port>"}
//
// The reported proxy overrides p.config.Proxy for this process, which is what the
// readiness gate, the wake endpoint and the reverse proxy are all built from — that
// is how an engine bound to a port this instance would not have allocated can
// still be woken rather than replaced.
//
// Anything else means "nothing to adopt", including an awake engine: adopting a
// live engine as parked would leave llama-swap tracking nothing while a container
// it cannot stop holds the name.
func (p *ProcessCommand) adoptParked() bool {
	cmdStr := p.config.DetectCommand()
	if cmdStr == "" {
		return false
	}

	args, err := config.SanitizeCommand(cmdStr)
	if err != nil {
		p.proxyLogger.Warnf("<%s> adopt: invalid park.detect command: %v", p.id, err)
		return false
	}

	ctx, cancel := context.WithTimeout(p.parentCtx, time.Duration(config.ParkDetectTimeout)*time.Second)
	defer cancel()

	detectCmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stdout bytes.Buffer
	detectCmd.Stdout = &stdout
	detectCmd.Stderr = p.processLogger
	setProcAttributes(detectCmd)

	runErr := detectCmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		p.proxyLogger.Warnf("<%s> adopt: park.detect timed out after %ds, cold-loading instead",
			p.id, config.ParkDetectTimeout)
		return false
	}
	if runErr != nil {
		// A detect command that cannot even be started is not "nothing to adopt", it
		// is a broken declaration: the operator armed adoption and pointed it at a
		// path that does not exist, so this entry silently keeps paying a cold load
		// after every gateway restart. Say so at a level the operator will see.
		if errors.Is(runErr, exec.ErrNotFound) {
			p.proxyLogger.Warnf("<%s> adopt: park.detect could not be started (%v), adoption is doing nothing and this entry cold-loads after every restart", p.id, runErr)
			return false
		}
		// A failing detect command is the "nothing to adopt" answer. The only cost of
		// treating it that way is the cold load adoption would have avoided.
		p.proxyLogger.Debugf("<%s> adopt: park.detect adopted nothing (%v)", p.id, runErr)
		return false
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return false
	}

	var adopted struct {
		Parked bool   `json:"parked"`
		Proxy  string `json:"proxy"`
	}
	if err := json.Unmarshal(out, &adopted); err != nil {
		brief := string(out)
		if len(brief) > 200 {
			brief = brief[:200] + "..."
		}
		p.proxyLogger.Warnf("<%s> adopt: park.detect printed neither JSON nor nothing: %s", p.id, brief)
		return false
	}

	if !adopted.Parked {
		p.proxyLogger.Debugf("<%s> adopt: the engine answers awake, leaving it to the normal start path", p.id)
		return false
	}

	if adopted.Proxy == "" {
		p.proxyLogger.Warnf("<%s> adopt: parked engine reported no proxy, refusing to adopt", p.id)
		return false
	}
	target, err := url.Parse(adopted.Proxy)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		p.proxyLogger.Warnf("<%s> adopt: %q is not a usable engine URL, refusing to adopt", p.id, adopted.Proxy)
		return false
	}
	if adopted.Proxy != p.config.Proxy {
		p.proxyLogger.Infof("<%s> adopting the parked engine at %s (this instance would have proxied %s)",
			p.id, adopted.Proxy, p.config.Proxy)
	} else {
		p.proxyLogger.Infof("<%s> adopting the parked engine at %s", p.id, adopted.Proxy)
	}
	// p.config is this process's own copy of the model config (a value, not a
	// pointer into the shared config), so the override is local to the process and
	// is what every later Proxy read — readiness gate, engineURL, detectUpstream —
	// sees. The shared config object keeps the configured value.
	// Remember what to restore. A wake that fails, or a destroy, has to put this
	// back: every start path builds its proxy target and its readiness gate from
	// p.config.Proxy, and an adopted address that stops existing would make every
	// later start fail.
	p.adoptedProxy = p.config.Proxy
	p.config.Proxy = adopted.Proxy
	return true
}

// restoreAdoptedProxy puts back the proxy startup adoption replaced. No-op for a
// model that was never adopted. Called when an adopted engine turns out unusable
// (failed wake) or is deliberately discarded (destroy), i.e. whenever this
// process is about to cold-start the engine itself through cmd.
func (p *ProcessCommand) restoreAdoptedProxy() {
	if p.adoptedProxy == "" {
		return
	}
	p.proxyLogger.Infof("<%s> proxy back to %s, the adopted engine at %s is not being used", p.id, p.adoptedProxy, p.config.Proxy)
	p.config.Proxy = p.adoptedProxy
	p.adoptedProxy = ""
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
	// Whatever the outcome, this process no longer intends to talk to the adopted
	// engine: after a destroy the next start comes through cmd on the configured
	// address, and a destroy that failed still means cmd will replace the container
	// it cannot attach to.
	p.restoreAdoptedProxy()
	return runErr
}

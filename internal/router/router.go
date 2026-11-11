package router

import (
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

var (
	ErrNoRouterFound     = swaputil.ErrNoRouterFound
	ErrNoPeerModelFound  = swaputil.ErrNoPeerModelFound
	ErrNoLocalModelFound = swaputil.ErrNoLocalModelFound
)

type Router interface {
	// Shutdown blocks until the router has shutdown returning nil
	// when the router has shutdown successfully.
	//
	// timeout controls how long to wait for inflight requests to finish. After
	// the timeout all inflight requests will be cancelled.
	Shutdown(timeout time.Duration) error

	// ServeHTTP implements the http.Handler and requests coming in will
	// trigger any model swapping and routing logic.
	ServeHTTP(http.ResponseWriter, *http.Request)

	// Handles reports whether this router can serve requests for the given model.
	Handles(model string) bool
}

// LocalRouter is a Router backed by local processes whose state can be
// inspected and which can be individually stopped. Peer routers, which only
// forward to remote hosts, do not implement it.
type LocalRouter interface {
	Router

	// RunningModels returns the current state of every process that is not
	// stopped or shut down, keyed by model ID.
	RunningModels() map[string]process.ProcessState

	// RunningStatus is RunningModels with each model's ready time, read
	// together with its state so the two never disagree.
	RunningStatus() map[string]process.Status

	// Unload stops the named models, or every running model when none are
	// named. It blocks until each targeted process has stopped. A timeout <= 0
	// gives each process its configured unloadTimeout to stop gracefully:
	// models sharing a timeout stop in parallel, smaller timeouts before
	// larger ones. A positive timeout overrides the configured values for
	// every target.
	Unload(timeout time.Duration, models ...string)

	// ProcessLogger returns the log monitor for the named model's process.
	// modelID must be a real (non-alias) config key. Returns false when the
	// model is not known to this router.
	ProcessLogger(modelID string) (*logmon.Monitor, bool)

	// SetStartGate installs a check that runs before the router starts any
	// process. While it returns an error no model starts, and the requests
	// that needed one get that error.
	SetStartGate(gate func() error)
}

// Parkable is implemented by a router whose models can be parked and woken.
//
// PCNI fork. It is deliberately separate from LocalRouter: a Peer cannot park a
// model that lives on another machine, and no router parks anything by itself
// yet — parking comes from a model's ttl or from an operator calling these
// verbs. Assert for it, do not assume it.
type Parkable interface {
	// ParkModel drains the model's in-flight requests and parks it: the engine
	// keeps its container and host-side state (weights, KV, caches) and releases
	// the device, so the next request wakes it instead of cold-loading it.
	//
	// It returns process.ErrParkBusy when the model stayed busy past its
	// parkTimeout — the model is left awake and serving, which is the point of the
	// drain — process.ErrParkUnconfirmed when the engine never confirmed the park
	// (the model is recorded stopped, not parked), and nil when the model is
	// parked.
	ParkModel(modelID string) error

	// UnparkModel wakes a parked model now, through the model's own cmd (which
	// attaches to the container it still holds) and the normal readiness gate. A
	// request for the model would wake it anyway; this is for warming a model
	// before traffic arrives.
	UnparkModel(modelID string) error
}

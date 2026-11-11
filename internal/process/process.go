package process

import (
	"context"
	"github.com/mostlygeek/llama-swap/internal/hint"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type ProcessState string

const (
	StateStopped  ProcessState = ProcessState("stopped")
	StateStarting ProcessState = ProcessState("starting")
	StateReady    ProcessState = ProcessState("ready")
	StateStopping ProcessState = ProcessState("stopping")

	// process is shutdown and will not be restarted
	StateShutdown ProcessState = ProcessState("shutdown")

	// StateParked is a model whose engine released its device memory and which
	// llama-swap no longer tracks as a process, but that keeps its container and
	// host-side state. It is neither Ready nor Stopped: the next request wakes it
	// instead of cold-loading it.
	//
	// Parked is deliberately not "running": a parked model holds no upstream
	// handle, so anything that counts it as running would proxy to a process that
	// is not there.
	StateParked ProcessState = ProcessState("parked")

	// StateParking is the transient state while a model is drained and its park
	// command runs. Parking is a multi-second operation (drain, sleep call,
	// confirmation) and the run loop is single-threaded, so an observer must be
	// able to tell "being parked" from "stopped".
	StateParking ProcessState = ProcessState("parking")
)

// Status is a snapshot of a process's state together with when it became
// ready. All fields are read at once, so they never disagree.
type Status struct {
	State ProcessState
	// ReadySince is when the process last entered StateReady. It is the zero
	// time unless State is StateReady.
	ReadySince time.Time
	// Loading is the latest loading progress the upstream reported through
	// its health check. Only set while State is StateStarting.
	Loading *LoadingProgress
	// Upstream is the server family detected when the process became ready,
	// or hint.Unknown before detection finishes or when it failed. Only set
	// while State is StateReady.
	Upstream hint.Upstream
	// ParkedSince is when the process last entered StateParked. It is the zero
	// time unless State is StateParked, and it is what parkTTL is measured from.
	ParkedSince time.Time
}

type Process interface {
	// Run starts the process blocks until the process is terminated.
	// The timeout parameter controls how long to wait for the process to get
	// to a ready state to process traffic
	Run(timeout time.Duration) error

	// WaitReady blocks until the process is ready to serve requests
	// or the context is cancelled. It returns nil when the process is ready
	//
	// WaitReady only subscribes, it never starts anything, and it cannot tell
	// "stopped, but a start is coming" apart from "stopped, and nothing is
	// coming" — a subscription that arrives after a start has already failed or
	// been aborted waits for a process nobody is going to start. Pass a context
	// with a deadline if that matters. Callers that want the process serving
	// should use EnsureReady, which has neither problem.
	WaitReady(context.Context) error

	// EnsureReady brings the process to a ready state and blocks until it is
	// serving, the start fails, or ctx is cancelled. The timeout parameter
	// controls how long to wait for the process to become ready.
	//
	// Unlike Run, the decision of whether a start is needed is made inside the
	// process's own state machine, so callers never inspect State() first and
	// cannot race a concurrent transition:
	//
	//	ready    -> returns nil immediately
	//	stopped  -> starts the process and waits for it to become ready
	//	stopping -> waits for the stop to finish, then starts
	//	shutdown -> returns an error
	EnsureReady(ctx context.Context, timeout time.Duration) error

	// Stop blocks until the process has terminated. It returns nil when
	// the process terminated as expected (exit 0)
	Stop(timeout time.Duration) error

	// Park drains the model's in-flight requests, runs its park command, and
	// records the model as StateParked: the engine keeps its container and its
	// host-side state (weights, KV, caches) and releases the device.
	//
	//	ready  -> drain, park command, confirm, parked
	//	else   -> returns an error (nothing to park)
	//
	// It returns ErrParkBusy when the model stayed busy — it is left awake and
	// serving, which is the point of the drain — ErrParkUnconfirmed when the engine
	// never confirmed parking (the model is recorded stopped), and nil when the
	// model is parked. drainTimeout of 0 selects the model's parkTimeout.
	Park(ctx context.Context, drainTimeout time.Duration) error

	// Destroy stops the model outright, running cmdDestroy when the model declares
	// one and falling back to cmdStop / the process-group kill otherwise. Unlike
	// Park it leaves nothing resident: the next request cold-loads.
	Destroy(timeout time.Duration) error

	// State returns the current state of the process
	// Note: this is a snapshot of the state at the time of the call
	// and may change at any time after the call returns.
	State() ProcessState

	// Status returns the current state and, while ready, when the process
	// became ready, read together as one snapshot.
	Status() Status

	// ServeHTTP forwards requests to the underlying process
	// Calling it when the process is not ready will result in a
	// 503 response with an error body identifying llama-swap as the source
	ServeHTTP(http.ResponseWriter, *http.Request)

	// Logger returns the monitor that captures this process's stdout/stderr.
	Logger() *logmon.Monitor
}

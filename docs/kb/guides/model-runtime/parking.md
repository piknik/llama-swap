---
title: Parking a model instead of unloading it
summary: park, parkTTL and cmdDestroy split "release the device" from "throw the model away", so an idle model keeps its container and host-side state and wakes in seconds.
category: guides
tags: [park, sleep, wake, ttl, unload, cmd-stop, destroy, docker, vram, memory, container]
config_keys: [models.*.park, models.*.park.cmd, models.*.park.parkTimeout, models.*.park.wakeTimeout, models.*.park.statusEndpoint, models.*.park.parkedField, models.*.park.wakeEndpoint, models.*.park.confirmTimeout, models.*.park.detect, models.*.cmdDestroy, models.*.parkTTL, models.*.port]
updated: 2026-11-11
---

# Parking a model instead of unloading it

> **PCNI fork feature.** Nothing on this page changes behaviour for a model that
> does not declare `park:`. An unarmed model keeps upstream's single stop verb
> (`cmdStop`, or SIGTERM) exactly as before.

An unload throws a model away: the process dies, and for a Docker model the
container dies with it, so the next request pays a full cold load. For a large
mixture-of-experts engine that cold load is minutes, and it also throws away the
host-side state the engine built — the resident expert tier, the KV cache, the
caches it extracted from its pack.

**Parking** separates the two things an unload currently does at once:

- **release the device** — the engine gives back its VRAM and keeps everything else
- **discard the model** — the container and its host RAM are gone

A parked model is neither running nor stopped. llama-swap stops tracking a
process, the engine keeps its container and its memory, and the next request
**wakes** it instead of cold-loading it.

## The two verbs

Without `park:` a model has one teardown command, `cmdStop`, and every caller —
TTL, an eviction, `POST /api/models/unload` — runs it. With `park:` armed the
verbs split:

| what happens | which command runs | result |
| --- | --- | --- |
| a park (`ttl`, `POST /api/models/park`) | `park.cmd`, or `cmdStop` when `park.cmd` is empty | model recorded **parked**, container kept |
| a destroy (`parkTTL`, `POST /api/models/unload`) | `cmdDestroy` | container gone, next request cold-loads |

So a park-capable entry declares **both**:

```yaml
models:
  big-moe:
    cmd: >-
      sh -c '
        if [ "$(docker inspect -f {{.State.Running}} llama-swap_${MODEL_ID} 2>/dev/null)" = "true" ]; then
          docker attach llama-swap_${MODEL_ID}
        else
          docker run --rm --init --gpus all --name llama-swap_${MODEL_ID} \
            --network llm --ip 192.168.128.50 --port ${PORT} engine:latest
        fi
      '

    # park: ask the engine to release its device memory, then let go of the
    # `docker run` client we are tracking. The container survives, which is the
    # point: the engine's own state survives with it.
    cmdStop: >-
      sh -c '
        curl -sf -X POST http://llama-swap_big-moe:${PORT}/v1/sleep || docker stop -t 10 llama-swap_${MODEL_ID};
        kill -9 ${PID}
      '

    # destroy: the verb that actually throws the container away. It must work
    # when the model is parked, where there is no process to signal, so it names
    # the container and must not use ${PID}.
    cmdDestroy: docker rm -f llama-swap_${MODEL_ID}

    park:
      statusEndpoint: /v1/sleep     # polled for parkedField after the park command
      parkedField: parked           # basalt engines report "asleep" instead
      wakeEndpoint: /v1/wake        # POSTed before the readiness gate on the wake path
      parkTimeout: 30               # how long a park waits for requests in flight
      wakeTimeout: 120              # readiness budget for a wake
      confirmTimeout: 10            # how long to wait for confirmation

    ttl: 900        # idle 15 minutes -> park (not unload), because park: is armed
    parkTTL: 3600   # parked for an hour with nobody asking for it -> destroy
```

## A park never cuts a generation

Before anything is sent to the engine, the model's in-flight proxied requests are
waited out, up to `parkTimeout`. If they are still running when that expires the
park is **refused**: the model stays awake and serving, and the caller gets an
error. Parking is a courtesy to the next model, not a licence to interrupt the
current one.

This is the difference from today's eviction, which SIGTERMs the engine (or runs
`cmdStop`), and what makes a park safe to arm next to live traffic.

## Confirmation, not assumption

After the park command runs, llama-swap polls `statusEndpoint` for `parkedField`
being true, up to `confirmTimeout` seconds.

- confirmed → the model is recorded **parked** and can be woken
- not confirmed → the model is recorded **stopped**

The second case is deliberate. An engine that never said "I am parked" is
indistinguishable from one that was destroyed, and a *phantom* parked model is
worse than a stopped one: llama-swap would wait forever on a container that is
not there. A model whose park cannot be confirmed is treated as cold.

If your engine has no status route that reports this state, leave
`statusEndpoint` empty: llama-swap then trusts the park command's exit status.

## How a parked model wakes

Wake is not a special start. The model's `cmd` runs again and goes through the
normal readiness gate — which is exactly why the `cmd` above **attaches** to a
running container instead of always running a new one. If `wakeEndpoint` is set it
is POSTed first, for engines that wake on a route rather than on the first
request. `wakeTimeout` gives the wake its own readiness budget; without it the
model's usual start timeout applies.

A wake that fails is recorded stopped, and the next request cold-loads. A parked
model whose container was removed from under it behaves the same way.

## `parkTTL`: parking is not free

A parked engine releases its **device** memory. It keeps its host RAM, which on a
machine with one card is the scarce resource, not the VRAM. `parkTTL` bounds that:
a model parked for longer than `parkTTL` with nobody asking for it is **destroyed**
(`cmdDestroy`), which is what hands its host RAM back. Waking the model cancels
the pending destroy, the same way traffic cancels a `ttl` unload today.

## `port:` — pin the port a parked engine listens on

`${PORT}` is normally allocated in sorted model-ID order, so **adding one entry
renumbers every model that sorts after it**. For a model that is restarted by that
change it is invisible. For a parked model it is a bug: a parked engine is still
listening on the port it was started with, and a wake — or a park, or a destroy —
that addresses a different port reaches nothing.

```yaml
models:
  big-moe:
    port: 5801          # never allocated to another model
```

A pinned port is reserved before anything is allocated, auto-allocated ports step
over it, and duplicate pins fail config load:

```
model other-moe port 5801 is already pinned by model big-moe
model big-moe port: pinned but cmd does not use ${PORT}
```

Pin every model you intend to park.

## `park.detect` — adopting a parked engine that outlived llama-swap

Parking keeps a container that llama-swap no longer tracks, and llama-swap's
knowledge of it lives only in the process it started. So anything that recycles
llama-swap itself — an orchestrator restart, a config reload — leaves the container
parked and the model **stopped**. The next request then cold-loads it, throwing away
the exact thing parking was for, and on a container-backed entry the fresh
`docker run` collides with the stale container's name.

`park.detect` is how one model opts back in. Before this instance's first start of
the model, the command runs **once** and must print one JSON object on stdout within
5 s:

```yaml
models:
  big-moe:
    port: 5801
    # A note goes above the key: YAML folds a `#` line placed inside the scalar into
    # that scalar's first line, and the result loads as "empty command".
    park:
      # the script inspects the container, confirms it belongs to this entry,
      # asks the engine on the port the container recorded, and prints the
      # object above when all of that holds
      detect: /models/.pcni/park-detect.sh ${MODEL_ID}
```

- `{"parked":true,"proxy":"http://host:port"}` ⇒ **adopted**. The reported proxy
  replaces this process's upstream for the readiness gate, `wakeEndpoint` and
  proxied traffic — which is how an engine bound to a port this instance would not
  have allocated still gets woken rather than replaced. The model is recorded
  `parked` at once, so the wake path (`cmd`, `wakeEndpoint`, `wakeTimeout`,
  `parkTTL`) is the ordinary one.
- Anything else ⇒ **nothing to adopt**: empty stdout, `{"parked":false}`, a non-zero
  exit, output that is not that JSON, or a proxy that is not a usable http(s) URL.
  The model cold-loads through the configured proxy.

An engine that answers **awake** is deliberately not adopted. Adopting a live engine
would leave llama-swap tracking nothing while a container it cannot stop holds the
name, and the next cold start would lose that race — the failure adoption exists to
prevent.

The script is a deployment artifact: llama-swap calls no Docker API anywhere, so
only the deployment knows how its containers are named, and detection stays a
command like `cmdStop`. `${MODEL_ID}` and `${PORT}` are substituted at load, and
`${PID}` is **not** available — there is no process yet, that is the whole point.

What the script must prove is that this is *this entry's* engine, and which address
it answers on. A container name alone is not evidence: adopt on a name and you can
hand an entry another entry's engine. And a probe of an engine's park-state route is
a decision the deployment has to make knowingly — on some engines `GET` on a sleep
route is not a read, and read-only state routes can wait on an in-progress load.
`park-detect.sh` in the ca4 deployment's `scripts/` is that decision recorded:
container up, this entry's own fingerprint pair present on the container, engine
answering `GET /health` on the recorded port within 2 s, and an optional
`"<field>": true` gate only for engines whose GET is verified read-only.

Detection runs at most once per process instance, lazily, and only from `stopped`. A
parked engine that fails to wake is therefore not re-detected: `cmd` is what
re-attaches it, and adopting the same engine twice into the same failure would be a
loop. A config reload makes a new process instance, which asks again.
Two consequences of the override, both worth knowing:

- **`cmd` and the adopted address have to agree, so pin `port:`.** The adopted proxy
  is what the readiness gate and the reverse proxy use, but `cmd` is still the
  command the wake runs, and a container-backed `cmd` decides between *attach* and
  *replace* from the address it stamps into the container. If this instance allocated
  a different port, `cmd` takes the replace branch, destroys the engine adoption just
  discovered, and cold-loads — the correct-but-useless fallback. A pinned port makes
  them agree (`port:` above).
- **`p.config.Proxy` is restored when an adopted engine does not work out.** A wake
  that never passes the gate, or a `Destroy` of the parked one, puts the configured
  address back, so the following cold start health-checks the engine `cmd` starts
  rather than an address that no longer exists.

## What llama-swap can see

`parked` and `parking` are process states now, and they mean different things to
the two views:

- **`ModelState` reports `parked`, and parked is not ready.** A request for a
  parked model therefore goes through the normal start path, which is what wakes
  it. Nothing can proxy to it while it is parked.
- **`RunningStatus` includes it.** A parked model is resident — it holds a
  container and the host-side state that makes the wake fast — so an operator
  listing models has to see it, with its state. Reporting it as stopped would hide
  real, charged memory.

## Verbs

| | Command | Park-capable model | Unarmed model |
|---|---|---|---|
| idle for `ttl` seconds | `ttl` | **parks** | destroyed, as before |
| `POST /api/models/unload/{model}` | unload | **destroyed** (`cmdDestroy`) | destroyed, as before |
| `POST /api/models/park/{model}` | park | **parks** | refused, 400 |
| `POST /api/models/unpark/{model}` | wake | **woken** through `cmd` | refused, 409 (it is not parked) |

The unload change is the one to notice. Today llama-swap has one stop verb, so an
engine whose `cmdStop` parks will park on an unload you meant as a cleanup — and
there is no way to ask for the other thing. `unload` is now discard. A model
without `cmdDestroy` is unaffected: `Destroy` falls back to `cmdStop` and then to
the process kill.

`park` answers **409** when the model was still busy past `parkTimeout`. That is
not a failure to investigate: llama-swap refused to cut a generation short and left
the model serving. **502** means the park ran but the engine never confirmed it, and
the model is recorded stopped, not parked.

## Status of this feature

Built and tested here:

- the `park:` / `parkTTL` / `cmdDestroy` / `port:` config surface, validated at load
- `Park`, wake-by-request and `Destroy` inside the process state machine
- the drain-before-park rule and the refusal when it runs out
- confirmation via `statusEndpoint`, and stopped-on-unconfirmed
- `ttl` parks a park-capable model and destroys the rest; `parkTTL` destroys a
  parked one; a wake cancels that destroy
- `Stop` on a parked model is a no-op — only `Destroy` discards one, so an eviction
  cannot orphan a parked container and make the next start lose the name race
- pinned ports, including a parked model being destroyed by `cmdDestroy`
- the `POST /api/models/park/{model}` and `POST /api/models/unpark/{model}` verbs
- **`park.detect`**, which adopts a parked container llama-swap lost track of: an
  engine outliving an orchestrator restart or a config reload is woken through the
  normal path instead of cold-replaced

Not built yet:

- **router decisions that choose to park.** Park groups — two models allowed to be
  resident with at most one awake — are Phase 2. Today the router treats a parked
  model as absent when it picks evictions and never wakes one instead of cold
  starting one.
- **adopting a parked container across a llama-swap restart.** Built, but **not
  armed anywhere yet**: `park.detect` has to be declared per model and its detection
  script written by the deployment. Until it is armed, the failure mode to expect
  after a gateway restart is a cold replace of the parked container. `port:` removes
  the commonest cause (the port no longer renumbers on each wake), but on its own it
  does not discover the container.
- **a UI control.** The two endpoints are the only surface.

## See also

- `guides/model-runtime/ttl-and-unloading` — the unload path this splits in two
- `guides/model-runtime/writing-cmd` — `cmd`, `${PID}` and multi-line commands
- `guides/routing/groups-and-matrix` — which models may be resident together

---
title: Routing capacity and request queues
summary: Queue behavior, concurrency limits, and serializing service between co-resident models with serveMutexGroups.
category: guides
tags: [routing, queue, capacity, concurrency, concurrency-limit, max-concurrent-requests, global-concurrency-limit, rate-limit, serve-mutex, batching]
config_keys: [routing, models.*.concurrencyLimit, globalConcurrencyLimit, routing.scheduler.settings.fifo.serveMutexGroups]
updated: 2026-09-13
---

# Routing capacity and request queues

There is no `models.*.maxConcurrentRequests` setting. The real per-model key
is `models.*.concurrencyLimit`, which limits active parallel requests.

The router serializes model swaps and queues requests until the selected model
is ready. Avoid using many simultaneous client retries as a capacity control:
they create more queued work. Choose a routing policy that fits the models that
may coexist, then set client timeouts high enough for the queue and load time.

Inspect the Activity view and logs when latency grows. A queue that never drains
usually means the command, proxy, or health check is wrong; see
`guides/model-runtime/troubleshooting-model-wont-load`.

## Global concurrency limit

`globalConcurrencyLimit` is a top-level setting, separate from
`models.*.concurrencyLimit`. It caps the number of inference requests served
at once across every model combined, using a single shared semaphore:

```yaml
globalConcurrencyLimit: 8
```

The default is `0`, meaning no limit — the semaphore is not even added to the
request chain, so a default config pays no cost for the feature. Once the
limit is reached, further requests are rejected immediately with an HTTP 429
response rather than queued; there is no wait for a slot to free up. Clients
should treat a 429 as a signal to back off and retry, the same way they handle
a per-model concurrency limit rejection.

Use this to protect shared hardware (CPU, disk, network) from being
overwhelmed by traffic spread across many different models, which a per-model
`concurrencyLimit` cannot do since it only counts requests to one model at a
time.

## One model at a time: serve-mutex groups

`routing.scheduler.settings.fifo.serveMutexGroups` serializes *serving*
between models that are allowed to stay loaded together. Each group lists
model IDs; while one member has requests in flight, requests for the other
members wait in the queue and are promoted when that member drains — traffic
flows as per-model batches, and nothing unloads.

```yaml
routing:
  router:
    use: matrix
    settings:
      matrix:
        sets:
          resident: "chat-model & embed-model"   # allowed to coexist
  scheduler:
    settings:
      fifo:
        serveMutexGroups:
          - [chat-model, embed-model]            # but never served together
```

This fits hardware where keeping several models resident is cheap (weights
offloaded to system RAM, one discrete GPU): serving them at the same time
splits VRAM across their KV caches and slows both, while swapping them would
wipe caches and pay a cold start. The mutex keeps each model's cache warm
between its turns and reserves swapping for models that truly cannot share
the card.

Notes:

- Models in no group, and models of different groups, are untouched: they
  serve as soon as the router grants them.
- Inside a group only one model serves at a time, and a long stream holds
  the whole group for its duration; that is the intended trade. A model in
  several groups merges them: every member waits behind every other.
- `models.*.concurrencyLimit` still caps per-model parallelism within a
  turn, and `fifo.priority` still orders parked requests — follow-up
  requests can cut in front of a lower-priority sibling's waiting batch
  (see `guides/routing/groups-and-matrix`).
- Groups take model IDs or aliases, need at least two distinct models, and
  an unknown ID fails the config load.

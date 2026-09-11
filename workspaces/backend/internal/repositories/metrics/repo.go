/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	kubefloworgv1beta1 "github.com/kubeflow/notebooks/workspaces/controller/api/v1beta1"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/cache"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeflow/notebooks/workspaces/backend/internal/config"
	modelsCommon "github.com/kubeflow/notebooks/workspaces/backend/internal/models/common"
	models "github.com/kubeflow/notebooks/workspaces/backend/internal/models/workspaces/podtemplate/resources"
	repoCommon "github.com/kubeflow/notebooks/workspaces/backend/internal/repositories/common"
)

const (
	// apiAvailabilityTTL is how long a definitive metrics-API probe result
	// (served OR confirmed absent) is trusted before we probe again.
	apiAvailabilityTTL = 60 * time.Second

	// apiAvailabilityNegativeTTL is how long a *transient* probe failure is
	// cached before we retry. Deliberately short so a wedged API server does
	// not blank out live metrics for a full minute after it recovers.
	apiAvailabilityNegativeTTL = 3 * time.Second

	// resourceUsageCacheTTL is the freshness window for a successful
	// per-workspace usage snapshot served from usageCache.
	resourceUsageCacheTTL = 30 * time.Second

	// resourceUsageNegativeCacheTTL bounds warm-up polling. When PodMetrics
	// is not yet populated for a running pod (typical metrics-server scrape
	// lag), we return a degraded response and hold it briefly so that a
	// tight polling cadence does not hammer the Metrics Server. Short enough
	// that the UI reflects arriving metrics on the very next poll.
	resourceUsageNegativeCacheTTL = 3 * time.Second

	// resourceUsageCacheMaxCapacity caps the LRU. Sized to comfortably cover
	// large multi-team deployments; at ~1 KB per entry the steady-state
	// heap footprint is ~1 MB. See design discussion on this constant in the
	// review — deliberately not surfaced through EnvConfig.
	resourceUsageCacheMaxCapacity = 1000

	// metricsServerCallTimeout is the upper bound on any single Metrics
	// Server round-trip. Strictly less than resourceUsageCacheTTL so a slow
	// upstream cannot cause overlapping in-flight fetches, and comfortably
	// above p99 healthy-cluster latency.
	metricsServerCallTimeout = 5 * time.Second
)

// MetricsRepository exposes point-in-time workspace resource utilization, read
// from the Kubernetes Metrics Server, to the API layer.
//
// It maintains two levels of in-memory caching to minimize cluster overhead:
//
//  1. API Availability (apiAvailable): a memoized probe that caches whether the
//     Kubernetes Metrics API (metrics.k8s.io) is served in the cluster, avoiding
//     repetitive RESTMapper discovery calls. Uses stale-while-revalidate
//     semantics and distinguishes confirmed-absent (60 s TTL) from transient
//     probe failure (3 s TTL) so a metrics-server hiccup does not black out
//     live metrics for a full minute.
//
//  2. Resource Usage Cache (usageCache): a TTL LRU cache keyed by
//     "<namespace>/<workspace>" that stores computed resource usage
//     snapshots. Concurrent misses for the same key are coalesced via
//     singleflight so a burst of pollers produces at most one Metrics
//     Server round-trip per key per in-flight window. Degraded responses
//     (metrics-server unavailable, pod-metrics not yet populated) are cached
//     with a short negative TTL to bound warm-up polling load.
//
// All Kubernetes API reads (Workspace.Get, Pod.List, PodMetrics.Get) execute
// only on the miss path. Cache hits never touch the informer cache — the fast
// path is a single cache.Get and a type assertion.
type MetricsRepository struct {
	cfg          *config.EnvConfig
	client       client.Client
	apiAvailable func() bool
	usageCache   *cache.LRUExpireCache
	usageGroup   singleflight.Group
}

// cachedUsage is the value stored in usageCache.
//
// podUID is intentionally NOT part of the cache key and is currently unread.
// It is retained as an anchor for future observability (e.g., a log/metric on
// pod-recreation churn) — kept explicitly so that a future contributor sees
// the deliberate decision rather than re-introducing podUID into the key
// without realising the trade-off. Dropping podUID from the key trades
// automatic invalidation on pod recreation for eliminating the informer-copy
// cost from every cache-hit request; the resulting staleness is bounded by
// resourceUsageCacheTTL.
//
// The pointed-to WorkspaceResourceUsage is shared across all concurrent
// readers of a given cache entry for up to resourceUsageCacheTTL. Callers
// MUST treat it as immutable — see the GetWorkspaceResourceUsage godoc.
type cachedUsage struct {
	usage *models.WorkspaceResourceUsage
	// podUID is intentionally unused today; see the type doc above.
	podUID types.UID //nolint:unused
}

// NewMetricsRepository creates a MetricsRepository for accessing workspace metrics.
func NewMetricsRepository(cfg *config.EnvConfig, c client.Client) *MetricsRepository {
	return &MetricsRepository{
		cfg:    cfg,
		client: c,
		apiAvailable: memoize(
			apiAvailabilityTTL,
			apiAvailabilityNegativeTTL,
			func() (bool, error) { return metricsAPIServed(c) },
		),
		usageCache: cache.NewLRUExpireCache(resourceUsageCacheMaxCapacity),
	}
}

// GetWorkspaceResourceUsage returns the resource usage for the workspace's
// pod in the given namespace.
//
// Fast path (cache hit): one usageCache.Get and one type assertion. Zero
// Kubernetes API reads, zero informer deep-copies.
//
// Miss path: coalesced via singleflight. Exactly one goroutine per key runs
// Workspace.Get + Pod.List + (optionally) PodMetrics.Get; concurrent callers
// for the same key block on the shared result. The fetch runs on a context
// detached from the leader's request context so a cancelled leader does not
// abort the shared work; only the Metrics Server call itself is bounded (by
// metricsServerCallTimeout) — informer-served reads (Workspace.Get,
// Pod.List) are cache-served in-process and are not artificially capped.
//
// The returned *models.WorkspaceResourceUsage may be shared with other
// concurrent callers for up to resourceUsageCacheTTL. Callers MUST treat it
// as immutable — mutating a returned field would corrupt the cached entry
// for every subsequent reader until TTL expiry.
//
// Panic safety: a panic on the miss path (e.g., in a codec, mapper, or
// interceptor) is recovered inside the singleflight closure and surfaces as
// an error to every waiter, rather than crashing the process or aborting
// every coalesced caller with a re-panic.
func (r *MetricsRepository) GetWorkspaceResourceUsage(ctx context.Context, ns, workspace string) (*models.WorkspaceResourceUsage, error) {
	// Cache key format: "<ns>/<workspace>". Collision-safe because Kubernetes
	// namespace and object names both forbid "/" (DNS-subdomain grammar).
	cacheKey := ns + "/" + workspace

	// Fast path.
	if val, ok := r.usageCache.Get(cacheKey); ok {
		if entry, valid := val.(*cachedUsage); valid {
			return entry.usage, nil
		}
	}

	// Miss path: singleflight-coalesce. Using DoChan (not Do) lets the caller
	// select on its own ctx.Done() independently of the leader's context —
	// otherwise a leader cancellation would propagate to every waiter as
	// context.Canceled even if their own contexts are still alive.
	ch := r.usageGroup.DoChan(cacheKey, func() (_ interface{}, retErr error) {
		// Recover panics before they cross the singleflight boundary.
		// singleflight.DoChan re-panics into every receiver, so an
		// un-recovered panic here would kill every coalesced caller.
		// Convert to a plain error and let the next request retry — do
		// NOT cache a negative snapshot, panics are anomalous and we want
		// the next call to exercise the code path fresh.
		defer func() {
			if p := recover(); p != nil {
				retErr = fmt.Errorf("panic during metrics fetch for %q: %v", cacheKey, p)
			}
		}()

		// Detach from the leader's ctx so a cancelled leader does not abort
		// the shared fetch for other waiters. No outer WithTimeout is
		// applied: informer-served reads (Workspace.Get, Pod.List) are
		// cache reads in-process and should never block. The Metrics
		// Server call — the only real network hop — is bounded inside
		// fetchPodMetrics by metricsServerCallTimeout.
		fetchCtx := context.WithoutCancel(ctx)
		return r.fetchAndCacheUsage(fetchCtx, ns, workspace, cacheKey)
	})

	select {
	case <-ctx.Done():
		// Caller gave up. The in-flight fetch keeps running for the benefit
		// of any remaining waiters and to populate the cache.
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*models.WorkspaceResourceUsage), nil
	}
}

// fetchAndCacheUsage runs at most once per (ns, workspace) per in-flight
// coalescing window. All Kubernetes API reads live here.
func (r *MetricsRepository) fetchAndCacheUsage(
	ctx context.Context,
	ns, workspace, cacheKey string,
) (*models.WorkspaceResourceUsage, error) {
	// Confirm the workspace exists. Distinguishes "not found" from "exists
	// but pod not running"; without this, both would surface as
	// ErrWorkspacePodNotRunning. Informer-served, cheap.
	ws := &kubefloworgv1beta1.Workspace{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: workspace}, ws); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, repoCommon.ErrWorkspaceNotFound
		}
		return nil, err
	}

	selector := client.MatchingLabels{modelsCommon.LabelWorkspaceName: workspace}
	podList := &corev1.PodList{}
	if err := r.client.List(ctx, podList, client.InNamespace(ns), selector); err != nil {
		return nil, err
	}
	if len(podList.Items) == 0 {
		return nil, repoCommon.ErrWorkspacePodNotRunning
	}

	// Workspaces are backed by StatefulSets with replicas=1. Because
	// StatefulSets provide strict deployment guarantees, there will only
	// ever be a maximum of one pod running at any given time. Therefore,
	// we can safely just grab the first item in the list.
	pod := &podList.Items[0]

	// Metrics API not served (or transient probe failure). Return spec-only
	// usage without publishing to usageCache — availability is already
	// TTL-cached one layer up in apiAvailable, so double-caching here would
	// only delay recovery when the API becomes available again.
	if !r.apiAvailable() {
		return models.NewWorkspaceResourceUsage(pod, nil), nil
	}

	podMetrics, err := r.fetchPodMetrics(ctx, ns, pod.Name)
	switch {
	case err != nil:
		// Any error path caches a short-negative snapshot so a warming or
		// degraded workspace does not get polled continuously through the
		// negative TTL window. errors.Is(err, context.DeadlineExceeded)
		// specifically means our own metricsServerCallTimeout tripped —
		// anything else is a not-ready / not-found / transient transport
		// error. Both receive the same treatment: a spec-only response,
		// cached briefly.
		//
		// A *caller* cancellation cannot land here — ctx is derived from
		// context.WithoutCancel in GetWorkspaceResourceUsage, so
		// ctx.Err() reflects only our own deadlines. Do not introduce a
		// caller-cancellation branch: doing so would reintroduce the bug
		// where a leader's cancellation aborts the shared fetch for every
		// waiter.
		result := models.NewWorkspaceResourceUsage(pod, nil)
		r.usageCache.Add(cacheKey, &cachedUsage{usage: result, podUID: pod.UID}, resourceUsageNegativeCacheTTL)
		return result, nil

	case len(podMetrics.Containers) == 0:
		// Metrics object exists but container metrics are not populated yet
		// (typical Metrics Server warm-up). Cache briefly for the same
		// reason as the error path above.
		result := models.NewWorkspaceResourceUsage(pod, nil)
		r.usageCache.Add(cacheKey, &cachedUsage{usage: result, podUID: pod.UID}, resourceUsageNegativeCacheTTL)
		return result, nil
	}

	// Fully-populated result. Positive entry overwrites any prior negative
	// entry under the same cacheKey (LRUExpireCache.Add semantics), so a
	// workspace transitioning warm-up → warmed picks up new metrics on the
	// next poll rather than serving stale-negative for the negative TTL.
	usage := models.NewWorkspaceResourceUsage(pod, models.UsageForPod(podMetrics))
	r.usageCache.Add(cacheKey, &cachedUsage{usage: usage, podUID: pod.UID}, resourceUsageCacheTTL)
	return usage, nil
}

// fetchPodMetrics bounds the upstream Metrics Server call at
// metricsServerCallTimeout. This is the ONLY deadline applied on the miss
// path — callers should pass a context that does not itself impose a
// deadline (see GetWorkspaceResourceUsage). Bounding at this granularity
// (only the Metrics Server round-trip, not the whole fetch chain) prevents
// a slow informer cache from pre-starving the actual network call of its
// budget.
func (r *MetricsRepository) fetchPodMetrics(ctx context.Context, ns, podName string) (*metricsv1beta1.PodMetrics, error) {
	callCtx, cancel := context.WithTimeout(ctx, metricsServerCallTimeout)
	defer cancel()

	podMetrics := &metricsv1beta1.PodMetrics{}
	if err := r.client.Get(callCtx, client.ObjectKey{Namespace: ns, Name: podName}, podMetrics); err != nil {
		return nil, err
	}
	return podMetrics, nil
}

// memoizedProbe holds the last probe result and its per-snapshot validity
// window. Stored via atomic.Pointer so the fast path is a single lock-free
// atomic load. Immutable after Store — publishers always allocate a new
// snapshot rather than mutating the old one.
type memoizedProbe struct {
	result     bool
	capturedAt time.Time
	ttl        time.Duration
}

// memoize returns a function that caches probe's result with a validity window
// that depends on whether the probe returned an error:
//
//   - probe returns (result, nil): cached for posTTL. This includes the
//     "confirmed absent" case, e.g. RESTMapper meta.NoKindMatchError, which
//     is a legitimate steady state (bare clusters without metrics-server).
//   - probe returns (result, non-nil err): cached for negTTL. Signals a
//     transient failure — the underlying state might recover quickly.
//   - probe panics: caught, treated as a transient failure (negTTL). A
//     panic here does NOT propagate to callers and does NOT crash the
//     background refresh goroutine.
//
// Concurrency model:
//
//   - Fast path (warm + fresh): one atomic.Load + one time.Since. No mutex,
//     no allocation, no coordination.
//   - Cold start: singleflight coalesces the first-caller burst into a
//     single probe execution; all callers block on the leader's result.
//   - Stale-while-revalidate: after warm-up, callers observing a stale
//     snapshot receive the previous result immediately. An atomic gate
//     (refreshInFlight) ensures that a burst of N stale readers launches
//     exactly ONE refresh goroutine, not N — saving allocation and
//     scheduler churn while singleflight is still the belt-and-suspenders
//     guarantee that the underlying probe runs at most once concurrently.
//
// The probe never runs while any other goroutine is blocked waiting on a
// lock the probe is holding — the old memoize() implementation held a mutex
// across the probe call, which serialised every metrics request through
// whatever latency the probe was seeing.
func memoize(posTTL, negTTL time.Duration, probe func() (bool, error)) func() bool {
	var (
		state           atomic.Pointer[memoizedProbe]
		group           singleflight.Group
		refreshInFlight atomic.Bool
	)

	sfProbe := func() (interface{}, error) {
		// Recover a panicking probe so callers of memoize() and the
		// background refresh goroutine cannot be surprised by one. A
		// panic is treated as a transient failure and stored at negTTL,
		// so recovery is fast if the probe stops panicking.
		var (
			result   bool
			probeErr error
		)
		func() {
			defer func() {
				if p := recover(); p != nil {
					probeErr = fmt.Errorf("panic during metrics API probe: %v", p)
				}
			}()
			result, probeErr = probe()
		}()

		ttl := posTTL
		if probeErr != nil {
			ttl = negTTL
		}
		state.Store(&memoizedProbe{
			result:     result,
			capturedAt: time.Now(),
			ttl:        ttl,
		})
		// Publication of the probe result happens via state.Store above —
		// warm-path callers of the returned function read via state.Load
		// and never participate in this singleflight call. The
		// (interface{}, error) return signature is required by
		// singleflight.Group.Do; both cold-start and SWR call sites
		// discard these values, so returning nil, nil is correct.
		return nil, nil
	}

	return func() bool {
		cur := state.Load()

		// Fast path: warm and fresh.
		if cur != nil && time.Since(cur.capturedAt) < cur.ttl {
			return cur.result
		}

		// Cold start: no prior value to serve — must block on the first probe.
		// singleflight collapses concurrent first-callers into one probe.
		if cur == nil {
			_, _, _ = group.Do("probe", sfProbe)
			if snap := state.Load(); snap != nil {
				return snap.result
			}
			return false
		}

		// Stale-while-revalidate: return the last known value immediately,
		// kick off a background refresh (exactly one, via the atomic gate),
		// and let the next caller pick up the fresh snapshot when the
		// refresh completes.
		if refreshInFlight.CompareAndSwap(false, true) {
			go func() {
				// belt-and-suspenders: singleflight re-panics into every
				// caller of Do() when the fn panics, and this goroutine
				// has no parent to catch that. sfProbe already recovers,
				// but a defensive recover here means even a mistake in
				// sfProbe cannot crash the process.
				defer func() { _ = recover() }()
				defer refreshInFlight.Store(false)
				_, _, _ = group.Do("probe", sfProbe)
			}()
		}
		return cur.result
	}
}

// metricsAPIServed reports whether the Kubernetes Metrics API is served in
// the cluster.
//
// The second return value distinguishes a *definitive* answer (nil error —
// either served, or confirmed absent via meta.NoKindMatchError) from a
// *transient* failure (non-nil error — e.g., API-server 5xx during a rolling
// upgrade, network blip, CRD-install window). The caller (memoize) uses this
// to short-cache transient failures so recovery is fast, without churning
// RESTMapper on clusters that legitimately do not run metrics-server.
func metricsAPIServed(c client.Client) (bool, error) {
	_, err := c.RESTMapper().RESTMapping(
		schema.GroupKind{Group: metricsv1beta1.GroupName, Kind: "PodMetrics"},
		metricsv1beta1.SchemeGroupVersion.Version,
	)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		// Confirmed absent — legitimate steady state on bare clusters.
		return false, nil
	default:
		// Transient — retry sooner via the negative TTL.
		return false, err
	}
}

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
	"errors"
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

// cachedUsage is the value stored in usageCache. podUID is retained for
// future observability (e.g., a log/metric on pod-recreation churn) but is
// intentionally NOT part of the cache key. Dropping podUID from the key trades
// automatic invalidation on pod recreation for eliminating the informer-copy
// cost from every cache-hit request; the resulting staleness is bounded by
// resourceUsageCacheTTL.
type cachedUsage struct {
	usage  *models.WorkspaceResourceUsage
	podUID types.UID
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
// abort the shared work; the Metrics Server call is separately bounded by
// metricsServerCallTimeout.
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
	ch := r.usageGroup.DoChan(cacheKey, func() (interface{}, error) {
		// Detach from the leader's ctx so a cancelled leader does not abort
		// the shared fetch for other waiters, and give it its own bounded
		// deadline so a wedged Metrics Server cannot pin the singleflight
		// entry indefinitely.
		fetchCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			metricsServerCallTimeout,
		)
		defer cancel()
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
		// Distinguish "we ourselves bounded the call and it tripped" from
		// "the caller went away" — the former is a Metrics Server slowdown
		// and should be cached briefly so we do not re-attempt on every
		// poll; the latter is a caller-side event and should propagate.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			// Our bounded deadline expired. Cache a degraded response with
			// the short negative TTL so the next poll after
			// resourceUsageNegativeCacheTTL retries against a hopefully
			// recovered Metrics Server, rather than either (a) hammering
			// on every request or (b) staying wedged for a full 30 s.
			result := models.NewWorkspaceResourceUsage(pod, nil)
			r.usageCache.Add(cacheKey, &cachedUsage{usage: result, podUID: pod.UID}, resourceUsageNegativeCacheTTL)
			return result, nil
		}
		if ctx.Err() != nil {
			// Caller went away — surface the caller's error, do not cache.
			return nil, ctx.Err()
		}
		// Not-ready / not-found / transport error. Cache briefly so a
		// warming workspace whose PodMetrics is not yet populated does not
		// get polled continuously through the warm-up window.
		result := models.NewWorkspaceResourceUsage(pod, nil)
		r.usageCache.Add(cacheKey, &cachedUsage{usage: result, podUID: pod.UID}, resourceUsageNegativeCacheTTL)
		return result, nil

	case len(podMetrics.Containers) == 0:
		// Metrics object exists but container metrics are not populated yet
		// (same warm-up shape as above). Cache briefly for the same reason.
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

// fetchPodMetrics bounds the upstream Metrics Server call independently of the
// caller's deadline. The caller may legitimately have a longer or shorter
// deadline; neither should let a wedged Metrics Server pin request goroutines
// longer than resourceUsageCacheTTL.
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
//
// Concurrency model:
//
//   - Fast path (warm + fresh): one atomic.Load + one time.Since. No mutex,
//     no allocation, no coordination.
//   - Cold start: singleflight coalesces the first-caller burst into a
//     single probe execution; all callers block on the leader's result.
//   - Stale-while-revalidate: after warm-up, callers observing a stale
//     snapshot receive the previous result immediately and kick off a
//     background refresh. singleflight ensures at most one refresh probe
//     is ever in flight regardless of concurrency.
//
// The probe never runs while any other goroutine is blocked waiting on a
// lock the probe is holding — the old memoize() implementation held a mutex
// across the probe call, which serialised every metrics request through
// whatever latency the probe was seeing.
func memoize(posTTL, negTTL time.Duration, probe func() (bool, error)) func() bool {
	var (
		state atomic.Pointer[memoizedProbe]
		group singleflight.Group
	)

	sfProbe := func() (interface{}, error) {
		result, probeErr := probe()
		ttl := posTTL
		if probeErr != nil {
			ttl = negTTL
		}
		state.Store(&memoizedProbe{
			result:     result,
			capturedAt: time.Now(),
			ttl:        ttl,
		})
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
		// kick off a background refresh, and let the next caller pick up
		// the fresh snapshot when the refresh completes.
		go func() { _, _, _ = group.Do("probe", sfProbe) }()
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

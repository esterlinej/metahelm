package metahelm

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dollarshaveclub/metahelm/pkg/dag"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	appsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	batchv1 "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// K8sClient describes an object that functions as a Kubernetes client
type K8sClient interface {
	AppsV1() appsv1.AppsV1Interface
	// ExtensionsV1beta1() v1beta1.ExtensionsV1beta1Interface
	CoreV1() corev1.CoreV1Interface
	BatchV1() batchv1.BatchV1Interface
}

// LogFunc is a function that logs a formatted string somewhere
type LogFunc func(string, ...interface{})

// Manager is an object that manages installation of chart graphs
type Manager struct {
	K8c  kubernetes.Interface
	HCfg *action.Configuration
	LogF LogFunc
}

func (m *Manager) log(msg string, args ...interface{}) {
	if m.LogF != nil {
		m.LogF(msg, args...)
	}
}

type options struct {
	k8sNamespace, releaseNamePrefix string
	installCallback                 InstallCallback
	completedCallback               CompletedCallback
	timeout                         time.Duration
}

type InstallOption func(*options)

// WithK8sNamespace specifies the kubernetes namespace to install a chart graph into. DefaultK8sNamespace is used otherwise.
func WithK8sNamespace(ns string) InstallOption {
	return func(op *options) {
		op.k8sNamespace = strings.ToLower(ns)
	}
}

// WithReleaseNamePrefix specifies a prefix to use in Helm release names (useful for when multiple instances of a chart graph are installed into the same namespace)
func WithReleaseNamePrefix(pfx string) InstallOption {
	return func(op *options) {
		op.releaseNamePrefix = strings.ToLower(pfx)
	}
}

// WithTimeout sets a timeout for all chart installations/upgrades to complete. If the timeout is reached, chart operations are aborted and an error is returned.
func WithTimeout(timeout time.Duration) InstallOption {
	return func(op *options) {
		op.timeout = timeout
	}
}

// WithInstallCallback specifies a callback function that will be invoked immediately prior to each chart installation
func WithInstallCallback(cb InstallCallback) InstallOption {
	return func(op *options) {
		op.installCallback = cb
	}
}

// WithCompletedCallback specifies a callback function that will be invoked immediately after each chart installation completes
func WithCompletedCallback(cb CompletedCallback) InstallOption {
	return func(op *options) {
		op.completedCallback = cb
	}
}

// CallbackAction indicates the decision made by the callback
type InstallCallbackAction int

const (
	// Continue indicates the installation should proceed immediately
	Continue InstallCallbackAction = iota
	// Wait means the install should not happen right now but should be retried at some point in the future. The callback will be invoked again on the retry.
	Wait
	// Abort means the installation should not be attempted
	Abort
)

// InstallCallback is a function that decides whether to proceed with an individual chart installation
// This will be called concurrently from multiple goroutines, so make sure everything is threadsafe
type InstallCallback func(Chart) InstallCallbackAction

// CompletedCallback is a function that is called upon completion of each individual chart upgrade/install. The error returned by Helm (if any) will be included.
// This will be called concurrently from multiple goroutines, so make sure everything is threadsafe. Also make sure to return promptly, as execution will block waiting for the callback to complete.
type CompletedCallback func(Chart, error)

// ReleaseMap is a map of chart title to installed release name
type ReleaseMap map[string]string

// release names
type lockingReleases struct {
	sync.Mutex
	rmap ReleaseMap
}

// DefaultK8sNamespace is the k8s namespace to install a chart graph into if not specified
const DefaultK8sNamespace = "default"

var retryDelay = 10 * time.Second

// Install installs charts in order according to dependencies and returns the names of the releases, or error.
// In the event of an error, the client can check if the error returned is of type ChartError, which then provides information on the kubernetes objects
// that caused failure, if this can be determined. A helm error unrelated to pod failure may return either a non-ChartError error value or an empty ChartError.
func (m *Manager) Install(ctx context.Context, charts []Chart, opts ...InstallOption) (ReleaseMap, error) {
	return m.installOrUpgrade(ctx, nil, false, charts, opts...)
}

// Upgrade upgrades charts in order according to dependencies, using the release names in rmap. ValueOverrides will be used in the upgrade.
// In the event of an error, the client can check if the error returned is of type ChartError, which then provides information on the kubernetes objects
// that caused failure, if this can be determined. A helm error unrelated to pod failure may return either a non-ChartError error value or an empty ChartError.
func (m *Manager) Upgrade(ctx context.Context, rmap ReleaseMap, charts []Chart, opts ...InstallOption) error {
	for _, c := range charts {
		if _, ok := rmap[c.Title]; !ok {
			return fmt.Errorf("chart title missing from release map: %v", c.Title)
		}
	}
	_, err := m.installOrUpgrade(ctx, rmap, true, charts, opts...)
	return err
}

// releaseName returns a release name of not more than 53 characters. If the input is truncated, a random number is added to ensure uniqueness.
func ReleaseName(input string) string {
	input = strings.ToLower(input)
	rsl := []rune(input)
	if len(rsl) < 54 {
		return input
	}
	out := rsl[0 : 53-6]
	rand.NewSource(time.Now().UTC().UnixNano())
	return fmt.Sprintf("%v-%d", string(out), rand.Intn(99999))
}

// MaxPodLogLines is the maximum number of failed pod log lines to return in the event of chart install/upgrade failure
var MaxPodLogLines = uint(500)

// wrapper allows us to force cancellation around a long-running function that doesn't support contexts, or doesn't support them well enough
func wrapper(ctx context.Context, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		errc <- fn()
	}()
	select {
	case <-ctx.Done():
		return fmt.Errorf("function wrapper: context was cancelled")
	case err := <-errc:
		return err
	}
}

// installOrUpgrade does helm installs/upgrades in DAG order
func (m *Manager) installOrUpgrade(ctx context.Context, upgradeMap ReleaseMap, upgrade bool, charts []Chart, opts ...InstallOption) (ReleaseMap, error) {
	ops := &options{}
	for _, opt := range opts {
		opt(ops)
	}
	if len(charts) == 0 {
		return nil, errors.New("no charts were supplied")
	}
	if ops.k8sNamespace == "" {
		ops.k8sNamespace = DefaultK8sNamespace
	}
	cmap := map[string]*Chart{}
	objs := []dag.GraphObject{}
	for i := range charts {
		if charts[i].WaitTimeout == 0 {
			charts[i].WaitTimeout = DefaultDeploymentTimeout
		}
		if charts[i].Location == "" {
			return nil, fmt.Errorf("empty location for chart: %v (offset %v)", charts[i].Title, i)
		}
		switch charts[i].DeploymentHealthIndication {
		case IgnorePodHealth:
		case AllPodsHealthy:
		case AtLeastOnePodHealthy:
		default:
			return nil, fmt.Errorf("unknown value for DeploymentHealthIndication: %v", charts[i].DeploymentHealthIndication)
		}
		cmap[charts[i].Name()] = &charts[i]
		objs = append(objs, &charts[i])
	}
	lf := func(msg string, args ...interface{}) {
		if m.LogF != nil {
			m.LogF("objgraph: "+msg, args...)
		}
	}
	og := dag.ObjectGraph{LogF: dag.LogFunc(lf)}
	if err := og.Build(objs); err != nil {
		return nil, errors.Wrap(err, "error building graph")
	}
	rn := lockingReleases{rmap: make(map[string]string)}
	started := time.Now().UTC()
	var deadline time.Time
	if ops.timeout > 0 {
		deadline = started.Add(ops.timeout)
	}
	af := func(obj dag.GraphObject) error {
		m.log("%v: starting install", obj.Name())
	Loop:
		for {
			if ops.installCallback == nil {
				m.log("%v: install callback is not set; proceeding", obj.Name())
				break
			}
			v := ops.installCallback(*cmap[obj.Name()])
			switch v {
			case Continue:
				m.log("%v: install callback indicated Continue; proceeding", obj.Name())
				break Loop
			case Wait:
				m.log("%v: install callback indicated Wait; delaying", obj.Name())
				time.Sleep(retryDelay)
			case Abort:
				m.log("%v: install callback indicated Abort; aborting", obj.Name())
				return errors.New("callback requested abort")
			default:
				return fmt.Errorf("unknown callback result: %v", v)
			}
			if deadline.After(started) && time.Now().UTC().After(deadline) {
				return fmt.Errorf("timeout exceeded: %v", ops.timeout)
			}
		}
		c := cmap[obj.Name()]
		chart, err := loader.Load(c.Location)
		if err != nil {
			return fmt.Errorf("error loading chart from location %s: %w", c.Location, err)
		}
		vals, err := chartutil.ReadValues(c.ValueOverrides)
		if err != nil {
			return fmt.Errorf("error reading value overrides from raw YAML: %w", err)
		}
		var opstr string
		var exist bool
		if upgrade {
			var err error
			exist, err = releaseExists(ctx, m.HCfg, ops.k8sNamespace, ops.releaseNamePrefix+c.Title)
			if err != nil {
				return errors.Wrap(err, "error error getting release names")
			}
		}
		if upgrade && exist {
			relname, ok := upgradeMap[c.Title]
			if !ok {
				return fmt.Errorf("chart not found in release map: %v", c.Title)
			}
			opstr = "upgrade"
			m.log("%v: running helm upgrade", obj.Name())
			upgrade := action.NewUpgrade(m.HCfg)
			upgrade.Wait = true
			upgrade.Timeout = c.WaitTimeout
			fn := func() error {
				if _, err := upgrade.RunWithContext(ctx, relname, chart, vals); err != nil {
					return m.charterror(ctx, err, ops, c, relname, "upgrading")
				}
				return nil
			}
			if err := wrapper(ctx, fn); err != nil {
				return err
			}
			if ops.completedCallback != nil {
				m.log("%v: running completed callback", obj.Name())
				ops.completedCallback(*cmap[obj.Name()], err)
			}
			if err != nil {
				return m.charterror(ctx, err, ops, c, relname, "upgrading")
			}
		} else {
			opstr = "installation"
			m.log("%v: running helm install", obj.Name())
			install := action.NewInstall(m.HCfg)
			install.Wait = true
			install.ReleaseName = ReleaseName(ops.releaseNamePrefix + c.Title)
			install.Namespace = ops.k8sNamespace
			install.Timeout = c.WaitTimeout
			var release *release.Release
			fn := func() error {
				release, err = install.RunWithContext(ctx, chart, vals)
				if err != nil {
					return m.charterror(ctx, err, ops, c, install.ReleaseName, "installing")
				}
				return nil
			}
			if err := wrapper(ctx, fn); err != nil {
				return err
			}
			if ops.completedCallback != nil {
				m.log("%v: running completed callback", obj.Name())
				ops.completedCallback(*cmap[obj.Name()], err)
			}
			if err != nil {
				return m.charterror(ctx, err, ops, c, install.ReleaseName, "installing")
			}
			rn.Lock()
			rn.rmap[c.Title] = release.Name
			rn.Unlock()
		}
		m.log("%v: %v complete; waiting for health", opstr, obj.Name())
		return m.waitForChart(ctx, c, ops.k8sNamespace)
	}
	if err := og.Walk(ctx, af); err != nil {
		werr, ok := err.(dag.WalkError)
		if !ok {
			// shouldn't be possible
			return nil, errors.Wrap(err, "dag walk error (not a WalkError)")
		}
		err2 := errors.Cause(werr.Err)
		if ce, ok := err2.(ChartError); ok {
			ce.Level = werr.Level
			return nil, ce
		}
		return nil, err
	}
	return rn.rmap, nil
}

func (m *Manager) charterror(ctx context.Context, err error, ops *options, c *Chart, releaseName, operation string) error {
	ce := NewChartError(err)
	if c.WaitUntilHelmSaysItsReady {
		get := action.NewGet(m.HCfg)
		rel, err2 := get.Run(releaseName)
		if err2 != nil || rel == nil {
			m.log(fmt.Sprintf("error fetching helm release: %v", err2))
			return ce
		}
		if err2 := ce.PopulateFromRelease(ctx, rel, m.K8c, MaxPodLogLines); err2 != nil {
			m.log("error populating chart error from release: %v", err2)
			return errors.Wrap(err, "error "+operation+" chart")
		}
		return ce
	}
	if err2 := ce.PopulateFromDeployment(ctx, ops.k8sNamespace, c.WaitUntilDeployment, m.K8c, MaxPodLogLines); err2 != nil {
		m.log("error populating chart error from deployment: %v", err2)
		return errors.Wrap(err, "error "+operation+" chart")
	}
	return ce
}

// ChartWaitPollInterval is the amount of time spent between polling attempts when checking if a deployment is healthy
var ChartWaitPollInterval = 10 * time.Second

func (m *Manager) waitForChart(ctx context.Context, c *Chart, ns string) error {
	defer m.log("%v: done", c.Name())
	if c.WaitUntilHelmSaysItsReady {
		m.log("%v: helm waited until it thought the chart installation was healthy; done", c.Name())
		return nil
	}
	if c.DeploymentHealthIndication == IgnorePodHealth {
		m.log("%v: IgnorePodHealth, no health check needed", c.Name())
		return nil
	}
	return wait.Poll(ChartWaitPollInterval, c.WaitTimeout, func() (bool, error) {
		d, err := m.K8c.AppsV1().Deployments(ns).Get(ctx, c.WaitUntilDeployment, metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			m.log("%v: error getting deployment (retrying): %v", c.Name(), err)
			return false, nil // the deployment may not initially exist immediately after installing chart
		}
		if d.Spec.Replicas != nil {
			needed := 1
			if c.DeploymentHealthIndication == AllPodsHealthy {
				needed = int(*d.Spec.Replicas)
			}
			m.log("%v: %v ready replicas, %v needed", c.Name(), d.Status.ReadyReplicas, needed)
			return int(d.Status.ReadyReplicas) >= needed, nil
		}
		return false, nil
	})
}

func releaseExists(ctx context.Context, cfg *action.Configuration, namespace string, releaseName string) (bool, error) {
	list := action.NewList(cfg)
	list.AllNamespaces = true
	list.Filter = fmt.Sprintf("^%s$", regexp.QuoteMeta(strings.ToLower(releaseName)))
	releases, err := list.Run()
	if err != nil {
		return false, fmt.Errorf("error getting release name: %w", err)
	}
	for _, release := range releases {
		if release.Namespace == namespace {
			return true, nil
		}
	}
	return false, nil
}

// ValidateCharts verifies that a set of charts is constructed properly, particularly with respect
// to dependencies. It does not check to see if the referenced charts exist in the local filesystem.
func ValidateCharts(charts []Chart) error {
	objs := []dag.GraphObject{}
	for i := range charts {
		if charts[i].Title == "" {
			return fmt.Errorf("empty title at offset %v", i)
		}
		if charts[i].Location == "" {
			return fmt.Errorf("empty location at offset %v", i)
		}
		switch charts[i].DeploymentHealthIndication {
		case IgnorePodHealth:
		case AllPodsHealthy:
		case AtLeastOnePodHealthy:
		default:
			return fmt.Errorf("unknown value for DeploymentHealthIndication at offset %v: %v", i, charts[i].DeploymentHealthIndication)
		}
		objs = append(objs, &charts[i])
	}
	og := dag.ObjectGraph{}
	if err := og.Build(objs); err != nil {
		return errors.Wrap(err, "error building graph from charts")
	}
	return nil
}

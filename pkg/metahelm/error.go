package metahelm

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dollarshaveclub/metahelm/pkg/manifest"
	"github.com/pkg/errors"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FailedPod models a single failed pod with metadata and logs
type FailedPod struct {
	Name              string                   `json:"name"`
	Phase             string                   `json:"phase"`
	Message           string                   `json:"message"`
	Reason            string                   `json:"reason"`
	Conditions        []corev1.PodCondition    `json:"conditions"`
	ContainerStatuses []corev1.ContainerStatus `json:"container_statuses"`
	// Logs is a map of container name to raw log (stdout/stderr) output
	Logs map[string][]byte `json:"logs"`
}

// ChartError is a chart install/upgrade error due to failing Kubernetes resources. It contains all Deployment, Job or DaemonSet-related pods that appear to
// be in a failed state, including up to MaxPodLogLines of log data for each.
type ChartError struct {
	// HelmError is the original error returned by Helm
	// We always omit this value when json marshaling/unmarshaling since we would otherwise have to implement the UnmarshalJSON and MarshalJSON methods.
	HelmError error `json:"-"`
	// HelmErrorString is the error string of the original error returned by Helm
	HelmErrorString string `json:"helm_error_string"`
	// Level is the chart level (zero-indexed) at which the error occurred
	Level uint `json:"level"`
	// FailedDaemonSets is map of DaemonSet name to failed pods
	FailedDaemonSets map[string][]FailedPod `json:"failed_daemon_sets"`
	// FailedDeployments is map of Deployment name to failed pods
	FailedDeployments map[string][]FailedPod `json:"failed_deployments"`
	// FailedJobs is map of Job name to failed pods
	FailedJobs map[string][]FailedPod `json:"failed_jobs"`
}

// NewChartError returns an initialized empty ChartError
func NewChartError(err error) ChartError {
	errorString := ""
	if err != nil {
		errorString = err.Error()
	}
	return ChartError{
		HelmError:         err,
		HelmErrorString:   errorString,
		FailedDaemonSets:  make(map[string][]FailedPod),
		FailedDeployments: make(map[string][]FailedPod),
		FailedJobs:        make(map[string][]FailedPod),
	}
}

// Error satisfies the error interface
func (ce ChartError) Error() string {
	return errors.Wrap(fmt.Errorf("error executing level %v: failed resources (deployments: %v; jobs: %v; daemonsets: %v)", ce.Level, len(ce.FailedDeployments), len(ce.FailedJobs), len(ce.FailedDaemonSets)), ce.HelmErrorString).Error()
}

// PopulateFromRelease finds the failed Jobs and Pods for a given release and fills ChartError with names and logs of the failed resources
func (ce ChartError) PopulateFromRelease(ctx context.Context, rls *release.Release, kc K8sClient, maxloglines uint) error {
	if rls == nil {
		return errors.New("release is nil")
	}
	for _, m := range manifest.SplitManifests(releaseutil.SplitManifests(rls.Manifest)) {
		ml := map[string]string{}
		failedpods := []FailedPod{}
		switch m.Head.Kind {
		case "Deployment":
			d, err := kc.AppsV1().Deployments(rls.Namespace).Get(ctx, m.Head.Metadata.Name, metav1.GetOptions{})
			if err != nil || d.Spec.Replicas == nil || d == nil {
				return errors.Wrap(err, "error getting deployment")
			}
			ml = d.Spec.Selector.MatchLabels
		case "Job":
			j, err := kc.BatchV1().Jobs(rls.Namespace).Get(ctx, m.Head.Metadata.Name, metav1.GetOptions{})
			if err != nil {
				return errors.Wrap(err, "error getting job")
			}
			ml = j.Spec.Selector.MatchLabels
		case "DaemonSet":
			ds, err := kc.AppsV1().DaemonSets(rls.Namespace).Get(ctx, m.Head.Metadata.Name, metav1.GetOptions{})
			if err != nil {
				return errors.Wrap(err, "error getting daemonset")
			}
			ml = ds.Spec.Selector.MatchLabels
		default:
			// we don't care about any other resource types
			continue
		}
		if len(ml) == 0 {
			continue
		}
		ss := []string{}
		for k, v := range ml {
			ss = append(ss, fmt.Sprintf("%v = %v", k, v))
		}
		pl, err := kc.CoreV1().Pods(rls.Namespace).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(ss, ",")})
		if err != nil || pl == nil {
			return errors.Wrapf(err, "error listing pods for selector: %v", ss)
		}
		for _, pod := range pl.Items {
			if failed, fp := failedpod(ctx, pod, maxloglines, kc); failed {
				failedpods = append(failedpods, fp)
			}
		}
		if len(failedpods) > 0 {
			switch m.Head.Kind {
			case "Deployment":
				ce.FailedDeployments[m.Head.Metadata.Name] = failedpods
			case "Job":
				ce.FailedJobs[m.Head.Metadata.Name] = failedpods
			case "DaemonSet":
				ce.FailedDaemonSets[m.Head.Metadata.Name] = failedpods
			}
		}
	}
	return nil
}

// PopulateFromDeployment finds the failed pods for a deployment and fills ChartError with names and logs of the failed pods
func (ce ChartError) PopulateFromDeployment(ctx context.Context, namespace, deploymentName string, kc K8sClient, maxloglines uint) error {
	d, err := kc.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil || d.Spec.Replicas == nil || d == nil {
		return errors.Wrap(err, "error getting deployment")
	}
	ss := []string{}
	for k, v := range d.Spec.Selector.MatchLabels {
		ss = append(ss, fmt.Sprintf("%v = %v", k, v))
	}
	pl, err := kc.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(ss, ",")})
	if err != nil || pl == nil {
		return errors.Wrapf(err, "error listing pods for selector: %v", ss)
	}
	failedpods := []FailedPod{}
	for _, pod := range pl.Items {
		if failed, fp := failedpod(ctx, pod, maxloglines, kc); failed {
			failedpods = append(failedpods, fp)
		}
	}
	ce.FailedDeployments[deploymentName] = failedpods
	return nil
}

func failedpod(ctx context.Context, pod corev1.Pod, maxloglines uint, kc K8sClient) (bool, FailedPod) {
	var maxlines *int64
	if maxloglines > 0 {
		ml := int64(maxloglines)
		maxlines = &ml
	}
	plopts := corev1.PodLogOptions{
		TailLines: maxlines,
	}
	var scheduled, ready, running bool
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			scheduled = c.Status == corev1.ConditionTrue
		}
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return false, FailedPod{}
	}
	running = pod.Status.Phase == corev1.PodRunning
	if !running || (scheduled && !ready) {
		fp := FailedPod{Logs: make(map[string][]byte)}
		fp.Name = pod.ObjectMeta.Name
		fp.Phase = string(pod.Status.Phase)
		fp.Message = pod.Status.Message
		fp.Reason = pod.Status.Reason
		fp.Conditions = pod.Status.Conditions
		fp.ContainerStatuses = pod.Status.ContainerStatuses
		// get logs
		for _, cs := range fp.ContainerStatuses {
			if !cs.Ready && ((cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.ExitCode != 0) ||
				(cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0)) {
				plopts.Container = cs.Name
				logs, err := getlogs(ctx, pod.Namespace, pod.Name, &plopts, kc)
				if err != nil {
					logs = []byte("error gettings logs for pod: " + err.Error())
				}
				fp.Logs[cs.Name] = logs
			}
		}
		return true, fp
	}
	return false, FailedPod{}
}

func getlogs(ctx context.Context, namespace, podname string, plopts *corev1.PodLogOptions, kc K8sClient) ([]byte, error) {
	req := kc.CoreV1().Pods(namespace).GetLogs(podname, plopts)
	if req == nil {
		return []byte{}, nil
	}
	req.BackOff(nil) // fixes tests with fake client
	logrc, err := req.Stream(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "error getting logs")
	}
	defer logrc.Close()
	logs, err := io.ReadAll(logrc)
	if err != nil {
		return nil, errors.Wrap(err, "error reading logs")
	}
	return logs, nil
}

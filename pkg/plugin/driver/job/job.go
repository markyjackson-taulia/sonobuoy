/*
Copyright 2017 Heptio Inc.

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

package job

import (
	"bytes"
	"time"

	"github.com/heptio/sonobuoy/pkg/errlog"
	"github.com/heptio/sonobuoy/pkg/plugin"
	"github.com/heptio/sonobuoy/pkg/plugin/driver/utils"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
)

// Plugin is a plugin driver that dispatches a single pod to the given
// kubernetes cluster
type Plugin struct {
	Definition      plugin.Definition
	DfnTemplateData *plugin.DefinitionTemplateData
	cleanedUp       bool
}

// Ensure Plugin implements plugin.Interface
var _ plugin.Interface = &Plugin{}

// NewPlugin creates a new DaemonSet plugin from the given Plugin Definition
// and sonobuoy master address
func NewPlugin(namespace string, dfn plugin.Definition, cfg *plugin.WorkerConfig) *Plugin {
	return &Plugin{
		Definition: dfn,
		DfnTemplateData: &plugin.DefinitionTemplateData{
			SessionID:     utils.GetSessionID(),
			MasterAddress: cfg.MasterURL,
			Namespace:     namespace,
		},
		cleanedUp: false, // be explicit
	}
}

// ExpectedResults returns the list of results expected for this plugin. Since
// a Job only launches one pod, only one result type is expected.
func (p *Plugin) ExpectedResults(nodes []v1.Node) []plugin.ExpectedResult {
	return []plugin.ExpectedResult{
		plugin.ExpectedResult{ResultType: p.GetResultType()},
	}
}

// GetResultType returns the ResultType for this plugin (to adhere to plugin.Interface)
func (p *Plugin) GetResultType() string {
	return p.Definition.ResultType
}

// Run dispatches worker pods according to the Job's configuration.
func (p *Plugin) Run(kubeclient kubernetes.Interface) error {
	var (
		b   bytes.Buffer
		job v1.Pod
	)
	p.Definition.Template.Execute(&b, p.DfnTemplateData)
	if err := kuberuntime.DecodeInto(scheme.Codecs.UniversalDecoder(), b.Bytes(), &job); err != nil {
		return errors.Wrapf(err, "could not decode executed template into a Job for plugin %v", p.GetName())
	}

	if _, err := kubeclient.CoreV1().Pods(p.DfnTemplateData.Namespace).Create(&job); err != nil {
		return errors.Wrapf(err, "could not create Job resource for Job plugin %v", p.GetName())
	}

	return nil
}

// Monitor adheres to plugin.Interface by ensuring the pod created by the job
// doesn't have any urecoverable failures.
func (p *Plugin) Monitor(kubeclient kubernetes.Interface, _ []v1.Node, resultsCh chan<- *plugin.Result) {
	for {
		// Sleep between each poll, which should give the Job
		// enough time to create a Pod
		// TODO: maybe use a watcher instead of polling.
		time.Sleep(10 * time.Second)
		// If we've cleaned up after ourselves, stop monitoring
		if p.cleanedUp {
			break
		}

		// Make sure there's a pod
		pod, err := p.findPod(kubeclient)
		if err != nil {
			resultsCh <- utils.MakeErrorResult(p.GetResultType(), map[string]interface{}{"error": err.Error()}, "")
			break
		}

		// Make sure the pod isn't failing
		if isFailing, reason := utils.IsPodFailing(pod); isFailing {
			resultsCh <- utils.MakeErrorResult(p.GetResultType(), map[string]interface{}{
				"error": reason,
				"pod":   pod,
			}, "")
			break
		}
	}
}

// Cleanup cleans up the k8s Job and ConfigMap created by this plugin instance
func (p *Plugin) Cleanup(kubeclient kubernetes.Interface) {
	p.cleanedUp = true
	gracePeriod := int64(1)
	deletionPolicy := metav1.DeletePropagationBackground

	listOptions := metav1.ListOptions{
		LabelSelector: "sonobuoy-run=" + p.GetSessionID(),
	}
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &deletionPolicy,
	}

	// Delete the Pod created by the job manually (just deleting the Job
	// doesn't kill the pod, it still lets it finish.)
	// TODO: for now we're not actually creating a Job at all, just a
	// single Pod, to get the restart semantics we want. But later if we
	// want to make this a real Job, we still need to delete pods manually
	// after deleting the job.
	err := kubeclient.CoreV1().Pods(p.DfnTemplateData.Namespace).DeleteCollection(
		&deleteOptions,
		listOptions,
	)
	if err != nil {
		errlog.LogError(errors.Wrapf(err, "error deleting pods for Job-%v", p.GetSessionID()))
	}
}

func (p *Plugin) listOptions() metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: "sonobuoy-run=" + p.GetSessionID(),
	}
}

// findPod finds the pod created by this plugin, using a kubernetes label
// search.  If no pod is found, or if multiple pods are found, returns an
// error.
func (p *Plugin) findPod(kubeclient kubernetes.Interface) (*v1.Pod, error) {
	pods, err := kubeclient.CoreV1().Pods(p.DfnTemplateData.Namespace).List(p.listOptions())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if len(pods.Items) != 1 {
		return nil, errors.Errorf("no pods were created by plugin %v", p.Definition.Name)
	}

	return &pods.Items[0], nil
}

func (p *Plugin) GetSessionID() string {
	return p.DfnTemplateData.SessionID
}

// GetName returns the name of this Job plugin
func (p *Plugin) GetName() string {
	return p.Definition.Name
}

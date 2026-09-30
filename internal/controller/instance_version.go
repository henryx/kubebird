/*
Copyright 2026 Enrico Bianchi.

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

package controller

import (
	"context"
	"fmt"
	"maps"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// firebirdVersionLabelKey labels the objects Kubebird generates for an
// Instance with the version the running Firebird server reports (see
// reconcileFirebirdVersion), once it's known.
const firebirdVersionLabelKey = "kubebird.github.io/firebird-version"

// serverVersionPattern extracts the "<major>.<minor>.<patch>" engine
// version from fbsvcmgr's -info_server_version output, e.g. "3.0.14"
// from "Server version: LI-V3.0.14.33856 Firebird 3.0" — the same value
// rdb$get_context('SYSTEM', 'ENGINE_VERSION') returns, confirmed against
// both the 3.0.14 and 4.0.3 images. Digits and dots only, so it's always
// a valid label value.
var serverVersionPattern = regexp.MustCompile(`-V([0-9]+\.[0-9]+\.[0-9]+)`)

// parseServerVersion returns the engine version found in fbsvcmgr's
// -info_server_version output.
func parseServerVersion(output string) (string, error) {
	m := serverVersionPattern.FindStringSubmatch(output)
	if m == nil {
		return "", fmt.Errorf("no server version found in %q", output)
	}
	return m[1], nil
}

// withFirebirdVersionLabel returns a copy of labels with
// firebirdVersionLabelKey added when status.firebirdVersion is known. Only
// ever applied to object metadata — never to a selector, nor to the
// StatefulSet's pod template, where a change would restart the pod as
// soon as the version is first detected; the Firebird pod is labelled
// directly by reconcileFirebirdVersion instead.
func withFirebirdVersionLabel(instance *kubebirdv1.Instance, labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	if instance.Status.FirebirdVersion != "" {
		out[firebirdVersionLabelKey] = instance.Status.FirebirdVersion
	}
	return out
}

// reconcileFirebirdVersion asks the running Firebird server for its
// version via the Services API (fbsvcmgr -info_server_version, which,
// unlike a SQL query, needs no database to connect to — the security
// database can't be opened while the server holds it, and spec.databases
// may be empty), labels the Firebird pod with it, and records it in
// status.firebirdVersion, from which the next reconcile labels every
// other generated object (withFirebirdVersionLabel). The pod's own label
// doubles as the "already detected" marker: a pod recreated by the
// StatefulSet controller (e.g. after spec.version changed) comes back
// without it, so the version is re-detected exactly once per server pod.
func (r *InstanceReconciler) reconcileFirebirdVersion(ctx context.Context, instance *kubebirdv1.Instance) error {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: firebirdPodName(instance)}, pod); err != nil {
		return fmt.Errorf("failed to get Firebird pod: %w", err)
	}

	version := pod.Labels[firebirdVersionLabelKey]
	if version == "" {
		password, err := r.sysdbaPassword(ctx, instance)
		if err != nil {
			return err
		}
		output, err := r.execInPodOutput(ctx, instance.Namespace, pod.Name, []string{
			"fbsvcmgr", "localhost:service_mgr", flagUser, sysdbaUsername, flagPassword, password, "-info_server_version",
		})
		if err != nil {
			return fmt.Errorf("failed to query Firebird server version: %w", err)
		}
		if version, err = parseServerVersion(output); err != nil {
			return fmt.Errorf("failed to parse Firebird server version: %w", err)
		}

		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[firebirdVersionLabelKey] = version
		if err := r.Patch(ctx, pod, patch); err != nil {
			return fmt.Errorf("failed to label Firebird pod with its version: %w", err)
		}
		logf.FromContext(ctx).Info("Detected Firebird server version", "pod", pod.Name, "version", version)
	}

	if instance.Status.FirebirdVersion == version {
		return nil
	}
	instance.Status.FirebirdVersion = version
	return r.Status().Update(ctx, instance)
}

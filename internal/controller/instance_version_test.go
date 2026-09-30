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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

const upgradedTestVersion = "4.0.3"

func TestParseServerVersion(t *testing.T) {
	cases := map[string]string{
		// Actual -info_server_version output of the 3.0.14 and 4.0.3 images.
		"Server version: LI-V3.0.14.33856 Firebird 3.0\n": testVersion,
		"Server version: LI-V4.0.3.2975 Firebird 4.0\n":   upgradedTestVersion,
		"Server version: WI-V5.0.1.1469 Firebird 5.0":     "5.0.1",
	}
	for output, want := range cases {
		got, err := parseServerVersion(output)
		if err != nil {
			t.Errorf("parseServerVersion(%q) failed: %v", output, err)
			continue
		}
		if got != want {
			t.Errorf("parseServerVersion(%q) = %q, want %q", output, got, want)
		}
	}

	for _, output := range []string{"", "Your user name and password are not defined."} {
		if got, err := parseServerVersion(output); err == nil {
			t.Errorf("parseServerVersion(%q) = %q, want an error", output, got)
		}
	}
}

func versionTestInstance(version string) *kubebirdv1.Instance {
	return &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       kubebirdv1.InstanceSpec{Image: testImage, Version: testVersion},
		Status:     kubebirdv1.InstanceStatus{FirebirdVersion: version},
	}
}

func versionTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kubebirdv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&kubebirdv1.Instance{}).
		WithObjects(objs...).Build()
}

func TestWithFirebirdVersionLabel(t *testing.T) {
	base := labelsForInstance("test")

	if got := withFirebirdVersionLabel(versionTestInstance(""), base); len(got) != len(base) {
		t.Errorf("with no detected version, got labels %v, want %v", got, base)
	}

	got := withFirebirdVersionLabel(versionTestInstance(testVersion), base)
	if got[firebirdVersionLabelKey] != testVersion {
		t.Errorf("got labels %v, want %s=3.0.14", got, firebirdVersionLabelKey)
	}
	if _, ok := base[firebirdVersionLabelKey]; ok {
		t.Errorf("withFirebirdVersionLabel modified its input map: %v", base)
	}
}

func TestMutateStatefulSetFirebirdVersionLabel(t *testing.T) {
	instance := versionTestInstance(testVersion)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sysdbaSecretRefName(instance), Namespace: instance.Namespace},
		Data:       map[string][]byte{sysdbaSecretPasswordKey: []byte("secret")},
	}
	c := versionTestClient(t, secret)
	r := &InstanceReconciler{Client: c, Scheme: c.Scheme()}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.mutateStatefulSet(context.Background(), sts, instance); err != nil {
		t.Fatal(err)
	}
	if sts.Labels[firebirdVersionLabelKey] != testVersion {
		t.Errorf("StatefulSet labels = %v, want %s=3.0.14", sts.Labels, firebirdVersionLabelKey)
	}
	// Neither may carry the version: the selector is immutable, and a
	// changed pod template restarts the pod.
	if _, ok := sts.Spec.Selector.MatchLabels[firebirdVersionLabelKey]; ok {
		t.Errorf("StatefulSet selector carries the version label: %v", sts.Spec.Selector.MatchLabels)
	}
	if _, ok := sts.Spec.Template.Labels[firebirdVersionLabelKey]; ok {
		t.Errorf("StatefulSet pod template carries the version label: %v", sts.Spec.Template.Labels)
	}
}

func TestReconcileFirebirdVersionFromLabelledPod(t *testing.T) {
	// A pod already carrying the label must not be re-queried (the
	// reconciler has no ClientSet here, so an exec attempt would panic):
	// its label is just copied into status.firebirdVersion.
	instance := versionTestInstance("")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      firebirdPodName(instance),
		Namespace: instance.Namespace,
		Labels:    map[string]string{firebirdVersionLabelKey: upgradedTestVersion},
	}}
	c := versionTestClient(t, instance, pod)
	r := &InstanceReconciler{Client: c, Scheme: c.Scheme()}

	if err := r.reconcileFirebirdVersion(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	stored := &kubebirdv1.Instance{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(instance), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.FirebirdVersion != upgradedTestVersion {
		t.Errorf("status.firebirdVersion = %q, want %q", stored.Status.FirebirdVersion, upgradedTestVersion)
	}
}

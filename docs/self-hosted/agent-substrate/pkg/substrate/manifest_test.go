// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package substrate

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBuildAndWriteActorTemplate(t *testing.T) {
	cfg := ManifestConfig{
		Namespace:   "gemini-self-hosted",
		Template:    "gemini-env-actor",
		WorkerImage: "us-docker.pkg.dev/my-proj/gemini/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		BaseURL:     "https://generativelanguage.googleapis.com",
	}

	tmpl := BuildActorTemplate(cfg)
	var buf bytes.Buffer
	if err := WriteActorTemplate(&buf, tmpl); err != nil {
		t.Fatalf("WriteActorTemplate() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"atespace: gemini-self-hosted",
		"name: gemini-env-actor",
		"ate-gemini",
		"actor-daemon",
		"--port=80",
		"--runtime-bin=gemini-api",
		"--root-dir=/",
		"--workdir=/workspace",
		"--server-url=https://generativelanguage.googleapis.com",
		"path: /healthz",
		"port: 80",
		"preferredFidelity: SNAPSHOT_FIDELITY_MEMORY",
		"durableDir: {}",
		"cpuMillicores:",
		"memoryBytes:",
		"drop:",
		"- ALL",
		"egress-mitm.ate.dev",
		"system-roots.ate.dev",
		"path: egress-trust-bundle/trust-bundle.pem",
		"mountPath: /run/ate",
		"name: SSL_CERT_FILE",
		"value: /run/ate/egress-trust-bundle/trust-bundle.pem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteActorTemplate() output missing %q in:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{
		"GEMINI_API_KEY",
		"GEMINI_API_BASE_URL",
		"onPause",
		"onResume",
		"onCommit",
		"storageLocation",
		"NET_BIND_SERVICE",
		"SANDBOX_CLASS_GVISOR",
		"status:",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("WriteActorTemplate() must not include %q in actor template:\n%s", unwanted, out)
		}
	}

	// Verify optional per-template storageLocation when SnapshotsBucket is explicitly set.
	cfg.SnapshotsBucket = "gs://my-snapshots/actors/"
	var withBucket bytes.Buffer
	if err := WriteActorTemplate(&withBucket, BuildActorTemplate(cfg)); err != nil {
		t.Fatalf("WriteActorTemplate(with bucket) error = %v", err)
	}
	if !strings.Contains(withBucket.String(), "storageLocation: gs://my-snapshots/actors/") {
		t.Errorf("WriteActorTemplate(with bucket) missing storageLocation:\n%s", withBucket.String())
	}

	// Verify output parses cleanly as YAML.
	var parsed map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}
}

func TestBuildAndWriteManifests(t *testing.T) {
	cfg := ManifestConfig{
		Namespace:        "gemini-self-hosted",
		Template:         "gemini-env-actor",
		WorkerImage:      "us-docker.pkg.dev/my-proj/gemini/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		WorkerPoolImage:  "ghcr.io/agent-substrate/substrate/ateworker-gvisor@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		SubstrateVersion: "v1.0.0-rc.1",
		APIKey:           "test-api-key-123",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate() error = %v", err)
	}

	var buf bytes.Buffer
	if err := WriteManifests(&buf, BuildManifests(cfg)); err != nil {
		t.Fatalf("WriteManifests() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"kind: Namespace",
		"ate.dev/api-access: \"true\"",
		"kind: ServiceAccount",
		"name: gemini-substrate-controller",
		"kind: Secret",
		"GEMINI_API_KEY: test-api-key-123",
		"apiVersion: ate.dev/v1",
		"kind: WorkerPool",
		"sandboxClasses:",
		"- name: gvisor",
		"ate.dev/substrate-version: v1.0.0-rc.1",
		"kind: Deployment",
		"serviceAccountName: gemini-substrate-controller",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"ate-gemini",
		"controller",
		"--ateapi-addr=api.ate-system.svc.cluster.local:443",
		"--ateapi-ca-file=/var/run/secrets/servicedns-ca/trust-bundle.pem",
		"--atenet-router-addr=atenet-router.ate-system.svc.cluster.local:443",
		"--atenet-client-bundle=/var/run/secrets/podidentity/credential-bundle.pem",
		"--worker-idle-timeout=1m0s",
		"--control-plane-url=https://generativelanguage.googleapis.com",
		"audience: api.ate-system.svc",
		"signerName: servicedns.podcert.ate.dev/identity",
		"signerName: podidentity.podcert.ate.dev/identity",
		"credentialBundlePath: credential-bundle.pem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("WriteManifests() output missing %q in:\n%s", want, out)
		}
	}

	for _, unwanted := range []string{
		"kind: Role",
		"kind: RoleBinding",
		"ate-api-server",
		"gemini-credentials-reader",
		"ate.dev/v1alpha1",
		"sandboxClass:",
		"ATE_NAMESPACE:",
		"ATE_TEMPLATE:",
		"ATEAPI_ADDR:",
		"ATEAPI_CA_FILE:",
		"ATENET_ROUTER_ADDR:",
		"GEMINI_API_BASE_URL:",
		"ATE_WORKER_URL_TEMPLATE:",
		"--atespace=",
		"--template=",
		"--worker-url-template=",
		"prometheus.io/port",
		"http-metrics",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("WriteManifests() must not include %q in:\n%s", unwanted, out)
		}
	}
}

func TestRootCommandManifestSubcommands(t *testing.T) {
	cmd := NewRootCommand("1.2.3")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"manifest",
		"--worker-image=repo/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--worker-pool-image=repo/ateworker@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"--allowed-atespaces=gemini-self-hosted,team-b",
		"--allowed-templates=gemini-env-actor,gpu-actor",
		"--gemini-api-key=cli-key",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute(manifest) error = %v", err)
	}
	for _, want := range []string{
		"GEMINI_API_KEY: cli-key",
		"--allowed-atespaces=gemini-self-hosted,team-b",
		"--allowed-templates=gemini-env-actor,gpu-actor",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("manifest command output missing %q:\n%s", want, out.String())
		}
	}

	// Verify ATE_NAMESPACE, ATE_TEMPLATE, and --ateom-image alias fallbacks.
	t.Setenv("ATE_NAMESPACE", "env-atespace")
	t.Setenv("ATE_TEMPLATE", "env-actor-template")

	cmdEnv := NewRootCommand("1.2.3")
	var envOut bytes.Buffer
	cmdEnv.SetOut(&envOut)
	cmdEnv.SetArgs([]string{
		"manifest",
		"--worker-image=repo/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--ateom-image=repo/ateom@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"--gemini-api-key=cli-key",
	})
	if err := cmdEnv.Execute(); err != nil {
		t.Fatalf("cmdEnv.Execute(manifest with env) error = %v", err)
	}
	for _, want := range []string{
		"name: env-atespace",
		"--atespace=env-atespace",
		"--template=env-actor-template",
	} {
		if !strings.Contains(envOut.String(), want) {
			t.Errorf("manifest output missing %q:\n%s", want, envOut.String())
		}
	}

	// Verify `manifest template` succeeds without --snapshots-bucket in Substrate v1.
	cmdTmpl := NewRootCommand("1.2.3")
	var tmplOut bytes.Buffer
	cmdTmpl.SetOut(&tmplOut)
	cmdTmpl.SetArgs([]string{
		"manifest",
		"template",
		"--worker-image=repo/runtime@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err := cmdTmpl.Execute(); err != nil {
		t.Fatalf("cmdTmpl.Execute(manifest template with env) error = %v", err)
	}
	for _, want := range []string{"atespace: env-atespace", "name: env-actor-template", "SNAPSHOT_FIDELITY_MEMORY"} {
		if !strings.Contains(tmplOut.String(), want) {
			t.Errorf("manifest template output missing %q:\n%s", want, tmplOut.String())
		}
	}
}

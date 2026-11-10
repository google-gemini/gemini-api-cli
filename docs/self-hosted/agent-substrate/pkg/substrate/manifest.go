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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

const (
	// EgressTrustBundleDir is where BuildActorTemplate mounts Substrate's
	// SystemInfo volume inside the actor container.
	EgressTrustBundleDir = "/run/ate"

	// EgressTrustBundleFile is the PEM bundle (egress gateway MITM CA plus
	// public roots) that actor processes trust so Substrate `https`
	// interception rules work alongside `tlsPassthrough` and `http` rules.
	EgressTrustBundleFile = EgressTrustBundleDir + "/egress-trust-bundle/trust-bundle.pem"

	// DefaultReplicas is the default number of WorkerPool pods.
	DefaultReplicas = 3

	// DefaultSecretName is the Kubernetes Secret holding Gemini credentials.
	DefaultSecretName = "gemini-credentials"

	// DefaultWorkerPoolName is the default Substrate WorkerPool name.
	DefaultWorkerPoolName = "worker-pool"

	// DefaultControllerDeploymentName is the Kubernetes Deployment and
	// ServiceAccount name for the ate-gemini controller.
	DefaultControllerDeploymentName = "gemini-substrate-controller"
)

// ManifestConfig holds configuration for generating Kubernetes and Substrate manifests.
type ManifestConfig struct {
	Namespace                 string
	Template                  string
	AllowedAtespaces          []string
	AllowedTemplates          []string
	WorkerImage               string
	ControllerImage           string
	WorkerPoolImage           string
	AteomImage                string
	SubstrateVersion          string
	SnapshotsBucket           string
	Replicas                  int32
	ControllerReplicas        int32
	EnvironmentID             string
	EnvironmentKey            string
	APIKey                    string
	BaseURL                   string
	AteapiAddr                string
	AtenetRouterAddr          string
	RuntimeBin                string
	WorkerPort                int32
	DefaultWorkDir            string
	MaxConcurrentInteractions int
	WorkerIdleTimeout         time.Duration
}

// Defaults populates default values for unset fields on cfg.
func (cfg *ManifestConfig) Defaults() {
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = DefaultAtespace
	}
	if strings.TrimSpace(cfg.Template) == "" {
		cfg.Template = DefaultTemplate
	}
	if strings.TrimSpace(cfg.ControllerImage) == "" {
		cfg.ControllerImage = cfg.WorkerImage
	}
	cfg.WorkerPoolImage = firstNonEmpty(cfg.WorkerPoolImage, cfg.AteomImage)
	if cfg.Replicas <= 0 {
		cfg.Replicas = DefaultReplicas
	}
	if cfg.ControllerReplicas <= 0 {
		cfg.ControllerReplicas = 1
	}
	if strings.TrimSpace(cfg.AteapiAddr) == "" {
		cfg.AteapiAddr = DefaultAteapiAddr
	}
	if strings.TrimSpace(cfg.AtenetRouterAddr) == "" {
		cfg.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if strings.TrimSpace(cfg.RuntimeBin) == "" {
		cfg.RuntimeBin = DefaultRuntimeBin
	}
	if cfg.WorkerPort <= 0 {
		cfg.WorkerPort = DefaultWorkerPort
	}
	if strings.TrimSpace(cfg.DefaultWorkDir) == "" {
		cfg.DefaultWorkDir = DefaultWorkDir
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = DefaultControlPlaneURL
	}
	if cfg.MaxConcurrentInteractions <= 0 {
		cfg.MaxConcurrentInteractions = DefaultMaxConcurrentInteractions
	}
	if cfg.WorkerIdleTimeout <= 0 {
		cfg.WorkerIdleTimeout = DefaultWorkerIdleTimeout
	}
}

// Validate checks that required fields for BuildManifests are set.
func (cfg *ManifestConfig) Validate() error {
	cfg.Defaults()
	if err := validateDNSLabel("namespace", cfg.Namespace); err != nil {
		return err
	}
	if err := validateDNSLabel("template", cfg.Template); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.WorkerImage) == "" {
		return errors.New("--worker-image is required")
	}
	if strings.TrimSpace(cfg.WorkerPoolImage) == "" {
		return errors.New("--worker-pool-image (or --ateom-image) is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.EnvironmentKey) == "" {
		return errors.New("one of --gemini-api-key ($GEMINI_API_KEY or $GOOGLE_API_KEY) or --gemini-env-key ($GEMINI_ENVIRONMENT_KEY) is required")
	}
	return nil
}

// BuildActorTemplate constructs the Substrate v1 ActorTemplate running
// `ate-gemini actor-daemon`, which serves GET /healthz for Substrate's wakeup
// probe and spawns one-shot `gemini-api environments serve` processes for
// leased interactions routed via `atenet-router`.
func BuildActorTemplate(cfg ManifestConfig) *atepb.ActorTemplate {
	cfg.Defaults()

	args := []string{
		"actor-daemon",
		fmt.Sprintf("--port=%d", cfg.WorkerPort),
		"--runtime-bin=" + cfg.RuntimeBin,
		"--root-dir=/",
		"--workdir=" + cfg.DefaultWorkDir,
	}
	if u := strings.TrimSpace(cfg.BaseURL); u != "" {
		args = append(args, "--server-url="+u)
	}

	snapCfg := &atepb.SnapshotConfig{
		PreferredFidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
	}
	if bucket := strings.TrimSpace(cfg.SnapshotsBucket); bucket != "" {
		snapCfg.StorageLocation = bucket
	}

	return &atepb.ActorTemplate{
		Metadata: &atepb.ResourceMetadata{
			Atespace: cfg.Namespace,
			Name:     cfg.Template,
		},
		WorkerSelector: &atepb.Selector{
			MatchLabels: map[string]string{
				"workload": cfg.Template,
			},
		},
		Containers: []*atepb.Container{{
			Name:    "worker",
			Image:   cfg.WorkerImage,
			Command: []string{"ate-gemini"},
			Args:    args,
			WakeupProbe: &atepb.WakeupProbe{
				HttpGet: &atepb.HttpGetAction{
					Path: "/healthz",
					Port: cfg.WorkerPort,
				},
				TimeoutSeconds: 30,
			},
			// Trust the egress gateway CA so custom_config.egress_policy `https`
			// (interception) rules work; the bundle also carries public roots.
			Env: []*atepb.EnvVar{
				{Name: "SSL_CERT_FILE", Value: EgressTrustBundleFile},
				{Name: "REQUESTS_CA_BUNDLE", Value: EgressTrustBundleFile},
				{Name: "NODE_EXTRA_CA_CERTS", Value: EgressTrustBundleFile},
				{Name: "GIT_SSL_CAINFO", Value: EgressTrustBundleFile},
			},
			VolumeMounts: []*atepb.VolumeMount{
				{Name: "workspace", MountPath: cfg.DefaultWorkDir},
				{Name: "system-info", MountPath: EgressTrustBundleDir},
			},
			SecurityContext: &atepb.SecurityContext{
				Capabilities: &atepb.Capabilities{
					Drop: []string{"ALL"},
				},
			},
		}},
		Volumes: []*atepb.Volume{
			{
				Name:       "workspace",
				DurableDir: &atepb.DurableDirVolumeSource{},
			},
			{
				Name: "system-info",
				SystemInfo: &atepb.SystemInfoVolumeSource{
					DataSources: []*atepb.SystemInfoDataSource{{
						TrustBundle: &atepb.TrustBundleDataSource{
							Names: []string{"egress-mitm.ate.dev", "system-roots.ate.dev"},
							Path:  "egress-trust-bundle/trust-bundle.pem",
						},
					}},
				},
			},
		},
		SnapshotConfig: snapCfg,
		Resources: &atepb.ResourceRequirements{
			Limits: &atepb.Resources{
				CpuMillicores: 1000,
				MemoryBytes:   536870912, // 512Mi
			},
		},
	}
}

// BuildManifests constructs the Kubernetes resources needed to run the Gemini
// self-hosted Substrate integration on a cluster with Agent Substrate v1 installed.
func BuildManifests(cfg ManifestConfig) []map[string]any {
	cfg.Defaults()

	return []map[string]any{
		buildNamespaceManifest(cfg),
		buildControllerServiceAccountManifest(cfg),
		buildSecretManifest(cfg),
		buildWorkerPoolManifest(cfg),
		buildControllerDeploymentManifest(cfg),
	}
}

func buildNamespaceManifest(cfg ManifestConfig) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": cfg.Namespace,
			"labels": map[string]string{
				"ate.dev/api-access": "true",
			},
		},
	}
}

func buildControllerServiceAccountManifest(cfg ManifestConfig) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":      DefaultControllerDeploymentName,
			"namespace": cfg.Namespace,
		},
	}
}

func buildSecretManifest(cfg ManifestConfig) map[string]any {
	stringData := make(map[string]string)
	if cfg.APIKey != "" {
		stringData["GEMINI_API_KEY"] = cfg.APIKey
	}
	if cfg.EnvironmentKey != "" {
		stringData["GEMINI_ENVIRONMENT_KEY"] = cfg.EnvironmentKey
	}

	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      DefaultSecretName,
			"namespace": cfg.Namespace,
		},
		"type":       "Opaque",
		"stringData": stringData,
	}
}

func buildWorkerPoolManifest(cfg ManifestConfig) map[string]any {
	templateSpec := map[string]any{
		"resources": map[string]any{
			"limits": map[string]string{
				"cpu":    "1",
				"memory": "1Gi",
			},
			"requests": map[string]string{
				"cpu":    "250m",
				"memory": "1Gi",
			},
		},
	}
	if ver := strings.TrimSpace(cfg.SubstrateVersion); ver != "" {
		templateSpec["nodeSelector"] = map[string]string{
			"ate.dev/substrate-version": ver,
		}
	}

	return map[string]any{
		"apiVersion": "ate.dev/v1",
		"kind":       "WorkerPool",
		"metadata": map[string]any{
			"name":      DefaultWorkerPoolName,
			"namespace": cfg.Namespace,
			"labels": map[string]string{
				"workload": cfg.Template,
			},
		},
		"spec": map[string]any{
			"replicas":    cfg.Replicas,
			"workerImage": cfg.WorkerPoolImage,
			"sandboxClasses": []map[string]any{{
				"name": "gvisor",
			}},
			"template": templateSpec,
		},
	}
}

func buildControllerDeploymentManifest(cfg ManifestConfig) map[string]any {
	labels := map[string]string{"app": DefaultControllerDeploymentName}
	command := []string{
		"ate-gemini",
		"controller",
		"--ateapi-addr=" + cfg.AteapiAddr,
		"--ateapi-ca-file=" + DefaultAteapiCAFile,
		"--atenet-router-addr=" + cfg.AtenetRouterAddr,
		"--atenet-client-bundle=" + DefaultPodIdentityBundleFile,
		"--runtime-bin=" + cfg.RuntimeBin,
		fmt.Sprintf("--max-concurrent-interactions=%d", cfg.MaxConcurrentInteractions),
		"--worker-idle-timeout=" + cfg.WorkerIdleTimeout.String(),
	}
	if cfg.Namespace != DefaultAtespace {
		command = append(command, "--atespace="+cfg.Namespace)
	}
	if cfg.Template != DefaultTemplate {
		command = append(command, "--template="+cfg.Template)
	}
	if allowedAS := normalizeCSVFlagValues(cfg.AllowedAtespaces); allowedAS != "" {
		command = append(command, "--allowed-atespaces="+allowedAS)
	}
	if allowedTmpl := normalizeCSVFlagValues(cfg.AllowedTemplates); allowedTmpl != "" {
		command = append(command, "--allowed-templates="+allowedTmpl)
	}
	if u := strings.TrimSpace(cfg.BaseURL); u != "" {
		command = append(command, "--control-plane-url="+u)
	}
	if envID := strings.TrimSpace(cfg.EnvironmentID); envID != "" {
		command = append(command, "--environment-id="+envID)
	}

	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      DefaultControllerDeploymentName,
			"namespace": cfg.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"replicas": cfg.ControllerReplicas,
			"selector": map[string]any{
				"matchLabels": labels,
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": labels,
				},
				"spec": map[string]any{
					"serviceAccountName":           DefaultControllerDeploymentName,
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{
						"runAsNonRoot": true,
						"runAsUser":    1000,
						"runAsGroup":   1000,
						"fsGroup":      1000,
						"seccompProfile": map[string]any{
							"type": "RuntimeDefault",
						},
					},
					"containers": []map[string]any{{
						"name":    DefaultControllerDeploymentName,
						"image":   cfg.ControllerImage,
						"command": command,
						"env": []map[string]any{{
							"name":  "HOME",
							"value": "/tmp",
						}},
						"envFrom": []map[string]any{{
							"secretRef": map[string]any{
								"name": DefaultSecretName,
							},
						}},
						"resources": map[string]any{
							"requests": map[string]string{
								"cpu":    "250m",
								"memory": "256Mi",
							},
							"limits": map[string]string{
								"cpu":    "1",
								"memory": "512Mi",
							},
						},
						"securityContext": map[string]any{
							"runAsNonRoot":             true,
							"runAsUser":                1000,
							"runAsGroup":               1000,
							"allowPrivilegeEscalation": false,
							"readOnlyRootFilesystem":   true,
							"capabilities": map[string]any{
								"drop": []string{"ALL"},
							},
							"seccompProfile": map[string]any{
								"type": "RuntimeDefault",
							},
						},
						"volumeMounts": []map[string]any{
							{
								"name":      "ateapi-token",
								"mountPath": "/var/run/secrets/tokens",
								"readOnly":  true,
							},
							{
								"name":      "servicedns-ca",
								"mountPath": "/var/run/secrets/servicedns-ca",
								"readOnly":  true,
							},
							{
								"name":      "podidentity",
								"mountPath": "/var/run/secrets/podidentity",
								"readOnly":  true,
							},
							{
								"name":      "tmp",
								"mountPath": "/tmp",
							},
						},
					}},
					"volumes": []map[string]any{
						{
							"name": "ateapi-token",
							"projected": map[string]any{
								"sources": []map[string]any{{
									"serviceAccountToken": map[string]any{
										"audience":          "api.ate-system.svc",
										"expirationSeconds": 3600,
										"path":              "ateapi-token",
									},
								}},
							},
						},
						{
							"name": "servicedns-ca",
							"projected": map[string]any{
								"sources": []map[string]any{{
									"clusterTrustBundle": map[string]any{
										"signerName": "servicedns.podcert.ate.dev/identity",
										"labelSelector": map[string]any{
											"matchLabels": map[string]string{
												"podcert.ate.dev/canarying": "live",
											},
										},
										"path": "trust-bundle.pem",
									},
								}},
							},
						},
						{
							"name": "podidentity",
							"projected": map[string]any{
								"sources": []map[string]any{{
									"podCertificate": map[string]any{
										"signerName":           "podidentity.podcert.ate.dev/identity",
										"keyType":              "ECDSAP256",
										"credentialBundlePath": "credential-bundle.pem",
									},
								}},
							},
						},
						{
							"name": "tmp",
							"emptyDir": map[string]any{
								"sizeLimit": "128Mi",
							},
						},
					},
				},
			},
		},
	}
}

// WriteActorTemplate marshals tmpl as protojson-shaped YAML (without status) to w.
func WriteActorTemplate(w io.Writer, tmpl *atepb.ActorTemplate) error {
	jsonBytes, err := protojson.MarshalOptions{}.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("encoding actor template json: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return fmt.Errorf("decoding actor template json: %w", err)
	}
	delete(m, "status")

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding actor template yaml: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// WriteManifests writes objs as a multi-document Kubernetes YAML stream to w.
func WriteManifests(w io.Writer, objs []map[string]any) error {
	for i, obj := range objs {
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		data, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("encoding manifest %d: %w", i, err)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

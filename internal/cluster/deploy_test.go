package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestCreateFleetSendsTheDeployment(t *testing.T) {
	var sent map[string]any
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/apis/agones.dev/v1/namespaces/games/fleets" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if calls == 2 {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"message":"already exists"}`)
			return
		}
		json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)
	})
	spec := FleetSpec{Name: "billiards-7", Deployment: 3, Build: 7, Game: "billiards",
		Image: "fleet.example.com/billiards@sha256:abc", Port: 7770, Protocol: "UDP", Replicas: 2,
		CPURequest: 250, CPULimit: 1500, MemoryRequest: 256, MemoryLimit: 512}
	ctx := context.Background()
	if err := client.CreateFleet(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateFleet(ctx, spec); err != nil {
		t.Fatalf("an existing Fleet must count as created: %v", err)
	}

	var fleet struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Replicas int `json:"replicas"`
			Template struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					Ports []struct {
						ContainerPort int    `json:"containerPort"`
						Protocol      string `json:"protocol"`
					} `json:"ports"`
					Template struct {
						Spec struct {
							NodeSelector     map[string]string       `json:"nodeSelector"`
							ImagePullSecrets []struct{ Name string } `json:"imagePullSecrets"`
							Containers       []struct {
								Name    string `json:"name"`
								Image   string `json:"image"`
								EnvFrom []struct {
									SecretRef struct{ Name string } `json:"secretRef"`
								} `json:"envFrom"`
								Resources struct {
									Requests map[string]string `json:"requests"`
									Limits   map[string]string `json:"limits"`
								} `json:"resources"`
							} `json:"containers"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	data, _ := json.Marshal(sent)
	if err := json.Unmarshal(data, &fleet); err != nil {
		t.Fatal(err)
	}
	pod := fleet.Spec.Template.Spec.Template.Spec
	c := pod.Containers[0]
	switch {
	case fleet.Metadata.Name != "billiards-7" || fleet.Metadata.Labels[LabelDeployment] != "3" || fleet.Spec.Replicas != 2:
		t.Errorf("fleet metadata = %+v, replicas %d", fleet.Metadata, fleet.Spec.Replicas)
	case fleet.Spec.Template.Metadata.Labels[LabelBuild] != "7":
		t.Errorf("game servers are not labelled with their build: %v", fleet.Spec.Template.Metadata.Labels)
	case fleet.Spec.Template.Spec.Ports[0].ContainerPort != 7770 || fleet.Spec.Template.Spec.Ports[0].Protocol != "UDP":
		t.Errorf("ports = %+v", fleet.Spec.Template.Spec.Ports)
	case c.Name != "billiards" || c.Image != spec.Image || c.EnvFrom[0].SecretRef.Name != "billiards-runtime":
		t.Errorf("container = %+v", c)
	case pod.ImagePullSecrets[0].Name != PullSecretName:
		t.Errorf("pull secrets = %+v", pod.ImagePullSecrets)
	case pod.NodeSelector[LabelPool] != "game":
		t.Errorf("game servers may go to any machine: node selector %v", pod.NodeSelector)
	case c.Resources.Requests["cpu"] != "250m" || c.Resources.Limits["memory"] != "512Mi":
		t.Errorf("resources = %+v", c.Resources)
	}
}

func TestAutoscalerIsAppliedServerSide(t *testing.T) {
	var method, path, contentType string
	var sent map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&sent)
		io.WriteString(w, `{}`)
	})
	if err := client.ApplyAutoscaler(context.Background(), "billiards-7", 3, 2, 10); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || contentType != "application/apply-patch+yaml" ||
		path != "/apis/autoscaling.agones.dev/v1/namespaces/games/fleetautoscalers/billiards-7?fieldManager=gamefleet&force=true" {
		t.Errorf("request = %s %s (%s)", method, path, contentType)
	}
	buffer := sent["spec"].(map[string]any)["policy"].(map[string]any)["buffer"].(map[string]any)
	if buffer["bufferSize"] != float64(2) || buffer["maxReplicas"] != float64(10) || buffer["minReplicas"] != nil {
		t.Errorf("buffer = %v", buffer)
	}
	sync := sent["spec"].(map[string]any)["sync"].(map[string]any)
	if sync["type"] != "FixedInterval" || sync["fixedInterval"].(map[string]any)["seconds"] != float64(AutoscalerSyncSeconds) {
		t.Errorf("sync = %v", sync)
	}
}

func TestScaleAndDeleteTreatMissingObjectsAsGone(t *testing.T) {
	var requests []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type"))
		if r.Method == http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	if err := client.ScaleFleet(ctx, "billiards-7", 0); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteAutoscaler(ctx, "billiards-7"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteFleet(ctx, "billiards-7"); err != nil {
		t.Fatal(err)
	}
	if requests[0] != "PATCH /apis/agones.dev/v1/namespaces/games/fleets/billiards-7 application/merge-patch+json" {
		t.Errorf("scale = %s", requests[0])
	}
}

func TestManagedFleetsReadsTheDeploymentLabel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labelSelector") != LabelDeployment {
			t.Errorf("selector = %q", r.URL.Query().Get("labelSelector"))
		}
		io.WriteString(w, `{"items":[
			{"metadata":{"name":"billiards-7","labels":{"gamefleet.io/deployment":"3"}},"spec":{"replicas":0},
			 "status":{"replicas":1,"readyReplicas":0,"allocatedReplicas":1}},
			{"metadata":{"name":"odd","labels":{"gamefleet.io/deployment":"x"}}}]}`)
	})
	fleets, err := client.ManagedFleets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fleets) != 1 || fleets[0].Deployment != 3 || fleets[0].Allocated != 1 || fleets[0].Replicas != 0 || fleets[0].Current != 1 {
		t.Fatalf("fleets = %+v", fleets)
	}
}

func TestRetireKeepAndDeleteNodes(t *testing.T) {
	var requests []string
	var bodies []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if r.Method == http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	at := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	if err := client.RetireNode(ctx, "worker", at); err != nil {
		t.Fatal(err)
	}
	if err := client.KeepNode(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteNode(ctx, "worker"); err != nil {
		t.Fatalf("deleting a node that is gone: %v", err)
	}
	if requests[0] != "PATCH /api/v1/nodes/worker application/merge-patch+json" ||
		bodies[0] != `{"metadata":{"annotations":{"gamefleet.io/retiring":"2026-10-21T08:00:00Z"}},"spec":{"unschedulable":true}}` {
		t.Errorf("retire = %s %s", requests[0], bodies[0])
	}
	if bodies[1] != `{"metadata":{"annotations":{"gamefleet.io/retiring":null}},"spec":{"unschedulable":false}}` {
		t.Errorf("keep = %s", bodies[1])
	}
}

func TestApplySecretWritesWithoutReading(t *testing.T) {
	var method, path, contentType string
	var sent map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&sent)
		io.WriteString(w, `{}`)
	})
	if err := client.ApplySecret(context.Background(), PullSecret("fleet.example.com", "k")); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPatch || contentType != "application/apply-patch+yaml" ||
		path != "/api/v1/namespaces/games/secrets/gamefleet-registry?fieldManager=gamefleet&force=true" {
		t.Errorf("request = %s %s (%s)", method, path, contentType)
	}
	data := sent["data"].(map[string]any)[".dockerconfigjson"].(string)
	raw, _ := base64.StdEncoding.DecodeString(data)
	if sent["type"] != "kubernetes.io/dockerconfigjson" ||
		string(raw) != `{"auths":{"fleet.example.com":{"auth":"Y2x1c3Rlcjpr","password":"k","username":"cluster"}}}` {
		t.Errorf("secret = %v / %s", sent, raw)
	}
}

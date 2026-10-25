package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// Labels on the Fleets GameFleet creates, and on their game servers.
const (
	LabelDeployment = "gamefleet.io/deployment"
	// LabelPool marks the machines game servers may run on, which is every
	// machine but a cluster's control plane when it is kept for that alone.
	LabelPool  = "gamefleet.io/pool"
	LabelBuild = "gamefleet.io/build"
)

// Conventions every game follows in the game namespace.
const (
	// PullSecretName is the docker-registry Secret for the GameFleet
	// registry. GameFleet writes it when a cluster joins.
	PullSecretName = "gamefleet-registry"
	// RuntimeSecretSuffix names a game's runtime settings: <game>-runtime,
	// for example billiards-runtime, loaded into the container's environment.
	RuntimeSecretSuffix = "-runtime"
)

// FleetSpec is the Fleet of one deployment.
type FleetSpec struct {
	Name       string
	Deployment int64
	Build      int64
	Game       string
	Image      string // with its digest
	Port       int
	Protocol   string // UDP or TCP
	Replicas   int
	// Rooms is how many matches one game server runs at once: the capacity
	// of its rooms list.
	Rooms int
	// CPU in millicores, memory in MiB.
	CPURequest, CPULimit       int
	MemoryRequest, MemoryLimit int
}

// ManagedFleet is a Fleet that GameFleet created, as it is now.
type ManagedFleet struct {
	Name       string
	Deployment int64
	Replicas   int // what the Fleet asks for
	Current    int // game servers that exist
	Ready      int
	Allocated  int
	Reserved   int
	Deleting   bool
}

// ManagedFleets lists the Fleets GameFleet created in the game namespace.
func (c *Client) ManagedFleets(ctx context.Context) ([]ManagedFleet, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Replicas int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				Replicas          int `json:"replicas"`
				ReadyReplicas     int `json:"readyReplicas"`
				AllocatedReplicas int `json:"allocatedReplicas"`
				ReservedReplicas  int `json:"reservedReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	query := url.Values{"labelSelector": {LabelDeployment}}
	if err := c.get(ctx, apiPath("agones.dev", c.namespace, "fleets", ""), query, &list); err != nil {
		return nil, err
	}
	fleets := []ManagedFleet{}
	for _, item := range list.Items {
		id, err := strconv.ParseInt(item.Metadata.Labels[LabelDeployment], 10, 64)
		if err != nil {
			continue
		}
		fleets = append(fleets, ManagedFleet{
			Name:       item.Metadata.Name,
			Deployment: id,
			Replicas:   item.Spec.Replicas,
			Current:    item.Status.Replicas,
			Ready:      item.Status.ReadyReplicas,
			Allocated:  item.Status.AllocatedReplicas,
			Reserved:   item.Status.ReservedReplicas,
			Deleting:   item.Metadata.DeletionTimestamp != nil,
		})
	}
	sort.Slice(fleets, func(i, j int) bool { return fleets[i].Name < fleets[j].Name })
	return fleets, nil
}

// CreateFleet creates the Fleet. A Fleet of that name that already exists is
// left as it is: a deployment's Fleet never changes after it is created.
func (c *Client) CreateFleet(ctx context.Context, f FleetSpec) error {
	labels := map[string]string{
		LabelDeployment: strconv.FormatInt(f.Deployment, 10),
		LabelBuild:      strconv.FormatInt(f.Build, 10),
		LabelGame:       f.Game,
	}
	resources := func(cpu, memory int) map[string]string {
		return map[string]string{"cpu": fmt.Sprintf("%dm", cpu), "memory": fmt.Sprintf("%dMi", memory)}
	}
	fleet := map[string]any{
		"apiVersion": "agones.dev/v1",
		"kind":       "Fleet",
		"metadata":   map[string]any{"name": f.Name, "namespace": c.namespace, "labels": labels},
		"spec": map[string]any{
			"replicas":   f.Replicas,
			"scheduling": "Packed",
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"ports": []any{map[string]any{
						"name": "game", "portPolicy": "Dynamic", "containerPort": f.Port, "protocol": f.Protocol,
					}},
					// Unity servers take a while to load.
					"health": map[string]any{"initialDelaySeconds": 30},
					// The match IDs this game server runs; allocation adds one,
					// the game server removes it when the match is over.
					"lists": map[string]any{ListRooms: map[string]any{"capacity": f.Rooms}},
					"template": map[string]any{
						"spec": map[string]any{
							"nodeSelector":     map[string]string{LabelPool: "game"},
							"imagePullSecrets": []any{map[string]string{"name": PullSecretName}},
							"containers": []any{map[string]any{
								"name":  f.Game,
								"image": f.Image,
								// Optional: a game with no runtime settings has no Secret.
								"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": f.Game + RuntimeSecretSuffix, "optional": true}}},
								"resources": map[string]any{
									"requests": resources(f.CPURequest, f.MemoryRequest),
									"limits":   resources(f.CPULimit, f.MemoryLimit),
								},
							}},
						},
					},
				},
			},
		},
	}
	err := c.do(ctx, http.MethodPost, apiPath("agones.dev", c.namespace, "fleets", ""), fleet, nil)
	if errors.Is(err, errConflict) {
		return nil
	}
	return err
}

// ScaleFleet sets how many game servers the Fleet asks for. Agones never
// removes allocated game servers to get there; they leave when their match ends.
func (c *Client) ScaleFleet(ctx context.Context, name string, replicas int) error {
	patch := map[string]any{"spec": map[string]any{"replicas": replicas}}
	return c.send(ctx, http.MethodPatch, apiPath("agones.dev", c.namespace, "fleets", name), "application/merge-patch+json", patch, nil)
}

// DeleteFleet deletes the Fleet and with it every game server it still has.
// Deleting one that is already gone succeeds.
func (c *Client) DeleteFleet(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, apiPath("agones.dev", c.namespace, "fleets", name), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// AutoscalerSyncSeconds is how often Agones tops up a Fleet's idle game servers.
const AutoscalerSyncSeconds = 5

// ApplyAutoscaler makes the Fleet keep buffer idle game servers, up to
// maxReplicas in total. It creates or updates the Fleet's autoscaler, which
// has the Fleet's name.
func (c *Client) ApplyAutoscaler(ctx context.Context, fleet string, deployment int64, buffer, maxReplicas int) error {
	autoscaler := map[string]any{
		"apiVersion": "autoscaling.agones.dev/v1",
		"kind":       "FleetAutoscaler",
		"metadata": map[string]any{
			"name": fleet, "namespace": c.namespace,
			"labels": map[string]string{LabelDeployment: strconv.FormatInt(deployment, 10)},
		},
		"spec": map[string]any{
			"fleetName": fleet,
			"policy": map[string]any{
				"type":   "Buffer",
				"buffer": map[string]any{"bufferSize": buffer, "maxReplicas": maxReplicas},
			},
			// Agones tops up every 30 seconds by default, so a burst of matches
			// used up the idle game servers and players were told none was free.
			// A game server starts in about 3 seconds.
			"sync": map[string]any{"type": "FixedInterval", "fixedInterval": map[string]any{"seconds": AutoscalerSyncSeconds}},
		},
	}
	path := apiPath("autoscaling.agones.dev", c.namespace, "fleetautoscalers", fleet) + "?fieldManager=gamefleet&force=true"
	return c.send(ctx, http.MethodPatch, path, "application/apply-patch+yaml", autoscaler, nil)
}

// DeleteAutoscaler stops the Fleet's autoscaler. Deleting one that is
// already gone succeeds.
func (c *Client) DeleteAutoscaler(ctx context.Context, fleet string) error {
	err := c.do(ctx, http.MethodDelete, apiPath("autoscaling.agones.dev", c.namespace, "fleetautoscalers", fleet), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// AnnotationRetiring marks a node GameFleet is taking out of the cluster; the
// value is when that was asked for.
const AnnotationRetiring = "gamefleet.io/retiring"

// AnnotationName holds the name people gave a machine in the console. The
// node's own name comes from the host name and cannot change.
const AnnotationName = "gamefleet.io/name"

// API is everything GameFleet does with one cluster. *Client is the real
// one; clustertest.Fake stands in for it in tests.
type API interface {
	Name() string
	Namespace() string
	Check(ctx context.Context) error

	Nodes(ctx context.Context) ([]Node, error)
	Pods(ctx context.Context, node string) ([]Pod, error)
	Pod(ctx context.Context, namespace, name string) (*Pod, error)
	Fleets(ctx context.Context) ([]Fleet, error)
	Rooms(ctx context.Context) ([]Room, error)
	Events(ctx context.Context, namespace, object string) ([]Event, error)
	Log(ctx context.Context, namespace, pod, container string, lines int) (string, error)
	Usage(ctx context.Context) (Usage, error)

	Allocate(ctx context.Context, a Allocation) (Room, error)
	FindByMatch(ctx context.Context, matchID string) (*Room, error)
	Get(ctx context.Context, name string) (*Room, error)
	Shutdown(ctx context.Context, name string) error
	ForgetMatches(ctx context.Context, name string, matchIDs []string) error
	EndMatch(ctx context.Context, name, matchID string) error
	MarkRetiring(ctx context.Context, name string, retiring bool) error

	ManagedFleets(ctx context.Context) ([]ManagedFleet, error)
	CreateFleet(ctx context.Context, f FleetSpec) error
	ScaleFleet(ctx context.Context, name string, replicas int) error
	DeleteFleet(ctx context.Context, name string) error
	ApplyAutoscaler(ctx context.Context, fleet string, deployment int64, buffer, maxReplicas int) error
	DeleteAutoscaler(ctx context.Context, fleet string) error

	RetireNode(ctx context.Context, name string, at time.Time) error
	KeepNode(ctx context.Context, name string) error
	NameNode(ctx context.Context, name, title string) error
	DeleteNode(ctx context.Context, name string) error
	ApplySecret(ctx context.Context, s Secret) error
}

var _ API = (*Client)(nil)

// Name is the cluster's name in GameFleet.
func (c *Client) Name() string { return c.name }

// Check makes sure GameFleet can work with the cluster: it reaches the API
// server, Agones is installed, and the ServiceAccount may list nodes and
// Fleets in the game namespace.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.Nodes(ctx); err != nil {
		return err
	}
	if _, err := c.Fleets(ctx); err != nil {
		if errors.Is(err, errNotFound) {
			return fmt.Errorf("cluster %s: Agones is not installed", c.name)
		}
		return err
	}
	return nil
}

// RetireNode stops new pods from being placed on the node and marks it, so
// that GameFleet removes it once its matches have ended.
func (c *Client) RetireNode(ctx context.Context, name string, at time.Time) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{AnnotationRetiring: at.UTC().Format(time.RFC3339)}},
		"spec":     map[string]any{"unschedulable": true},
	}
	return c.send(ctx, http.MethodPatch, apiPath("", "", "nodes", name), "application/merge-patch+json", patch, nil)
}

// KeepNode undoes RetireNode.
func (c *Client) KeepNode(ctx context.Context, name string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationRetiring: nil}},
		"spec":     map[string]any{"unschedulable": false},
	}
	return c.send(ctx, http.MethodPatch, apiPath("", "", "nodes", name), "application/merge-patch+json", patch, nil)
}

// NameNode sets the name the console shows for a node; an empty title
// takes it away.
func (c *Client) NameNode(ctx context.Context, name, title string) error {
	var value any = title
	if title == "" {
		value = nil
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{AnnotationName: value}}}
	return c.send(ctx, http.MethodPatch, apiPath("", "", "nodes", name), "application/merge-patch+json", patch, nil)
}

// DeleteNode removes the node from the cluster. K3s on that machine keeps
// running until someone uninstalls it or the machine is gone.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, apiPath("", "", "nodes", name), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// Secret is a Secret GameFleet writes in the game namespace.
type Secret struct {
	Name string
	// Type is the Kubernetes Secret type, Opaque when empty.
	Type string
	Data map[string][]byte
}

// PullSecret is the docker-registry Secret that lets the cluster pull from
// the registry at host with a pull key.
func PullSecret(host, key string) Secret {
	auth := base64.StdEncoding.EncodeToString([]byte("cluster:" + key))
	config, _ := json.Marshal(map[string]any{
		"auths": map[string]any{host: map[string]string{"username": "cluster", "password": key, "auth": auth}},
	})
	return Secret{Name: PullSecretName, Type: "kubernetes.io/dockerconfigjson", Data: map[string][]byte{".dockerconfigjson": config}}
}

// ApplySecret creates or replaces the keys GameFleet owns in the Secret.
// GameFleet may write Secrets but not read them.
func (c *Client) ApplySecret(ctx context.Context, s Secret) error {
	typ := s.Type
	if typ == "" {
		typ = "Opaque"
	}
	data := map[string]string{}
	for k, v := range s.Data {
		data[k] = base64.StdEncoding.EncodeToString(v)
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": s.Name, "namespace": c.namespace, "labels": map[string]string{"gamefleet.io/managed": "true"}},
		"type":       typ,
		"data":       data,
	}
	path := apiPath("", c.namespace, "secrets", s.Name) + "?fieldManager=gamefleet&force=true"
	return c.send(ctx, http.MethodPatch, path, "application/apply-patch+yaml", secret, nil)
}

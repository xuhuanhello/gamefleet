package cluster

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Node is one machine of the cluster.
type Node struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Roles         []string
	InternalIP    string
	ExternalIP    string
	Kubelet       string
	OS            string
	// Capacity is the whole machine; Allocatable is what is left for pods
	// once K3s and the system have their share.
	Capacity    Resources
	Allocatable Resources
	Taints      []string
	// Pressure lists the node conditions other than Ready that are true, such
	// as MemoryPressure.
	Pressure []string
	Labels   map[string]string
	Created  time.Time
	// Retiring is when someone asked GameFleet to take the node out of the
	// cluster; zero when nobody did.
	Retiring time.Time
	// Given is the name people gave the machine in the console, if any.
	Given string
}

// Title is what the console calls the machine: the name people gave it, or
// the node's own name.
func (n Node) Title() string {
	if n.Given != "" {
		return n.Given
	}
	return n.Name
}

// Pod is one pod on some node.
type Pod struct {
	Namespace string
	Name      string
	Node      string
	// Owner is the kind of the object that controls the pod, for example
	// GameServer or ReplicaSet.
	Owner string
	// Status is what kubectl shows: the phase, or why a container is not running.
	Status string
	Ready  int
	// Containers lists the pod's containers, then its sidecars.
	Containers []string
	Restarts   int
	Requests   Resources
	Created    time.Time
}

// Fleet is an Agones Fleet and how many of its game servers are in each state.
type Fleet struct {
	Name      string
	Image     string
	Replicas  int
	Ready     int
	Allocated int
	Reserved  int
	Created   time.Time
}

// Event is a Kubernetes event about one object.
type Event struct {
	Namespace string
	Kind      string
	Name      string
	Type      string
	Reason    string
	Message   string
	Count     int
	Last      time.Time
}

// Usage is what the metrics server last measured, per node and per pod
// ("namespace/name").
type Usage struct {
	Nodes map[string]Resources
	Pods  map[string]Resources
}

const maxEvents = 200

// Namespace is where the cluster runs game servers.
func (c *Client) Namespace() string { return c.namespace }

// Nodes lists the cluster's nodes by name.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Unschedulable bool `json:"unschedulable"`
				Taints        []struct {
					Key    string `json:"key"`
					Value  string `json:"value"`
					Effect string `json:"effect"`
				} `json:"taints"`
			} `json:"spec"`
			Status struct {
				Capacity    map[string]string `json:"capacity"`
				Allocatable map[string]string `json:"allocatable"`
				Addresses   []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
					OSImage        string `json:"osImage"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.get(ctx, apiPath("", "", "nodes", ""), nil, &list); err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(list.Items))
	for _, item := range list.Items {
		n := Node{
			Name:          item.Metadata.Name,
			Unschedulable: item.Spec.Unschedulable,
			Kubelet:       item.Status.NodeInfo.KubeletVersion,
			OS:            item.Status.NodeInfo.OSImage,
			Capacity:      resources(item.Status.Capacity["cpu"], item.Status.Capacity["memory"]),
			Allocatable:   resources(item.Status.Allocatable["cpu"], item.Status.Allocatable["memory"]),
			Labels:        item.Metadata.Labels,
			Created:       item.Metadata.CreationTimestamp,
		}
		if at, err := time.Parse(time.RFC3339, item.Metadata.Annotations[AnnotationRetiring]); err == nil {
			n.Retiring = at
		}
		n.Given = item.Metadata.Annotations[AnnotationName]
		for label := range item.Metadata.Labels {
			if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok {
				n.Roles = append(n.Roles, role)
			}
		}
		sort.Strings(n.Roles)
		for _, a := range item.Status.Addresses {
			switch a.Type {
			case "InternalIP":
				n.InternalIP = a.Address
			case "ExternalIP":
				n.ExternalIP = a.Address
			}
		}
		for _, cond := range item.Status.Conditions {
			switch {
			case cond.Type == "Ready":
				n.Ready = cond.Status == "True"
			case cond.Status == "True":
				n.Pressure = append(n.Pressure, cond.Type)
			}
		}
		for _, t := range item.Spec.Taints {
			taint := t.Key
			if t.Value != "" {
				taint += "=" + t.Value
			}
			n.Taints = append(n.Taints, taint+":"+t.Effect)
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}

type podObject struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName       string         `json:"nodeName"`
		Containers     []podContainer `json:"containers"`
		InitContainers []podContainer `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type podContainer struct {
	Name          string `json:"name"`
	RestartPolicy string `json:"restartPolicy"`
	Resources     struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}

type containerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
		Terminated *struct {
			Reason string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}

// Pods lists the pods of every namespace, or only those on one node.
func (c *Client) Pods(ctx context.Context, node string) ([]Pod, error) {
	var query url.Values
	if node != "" {
		query = url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	}
	var list struct {
		Items []podObject `json:"items"`
	}
	if err := c.get(ctx, apiPath("", "", "pods", ""), query, &list); err != nil {
		return nil, err
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, item := range list.Items {
		pods = append(pods, item.pod())
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	return pods, nil
}

// Pod returns one pod, or nil when it does not exist.
func (c *Client) Pod(ctx context.Context, namespace, name string) (*Pod, error) {
	var item podObject
	err := c.get(ctx, apiPath("", namespace, "pods", name), nil, &item)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pod := item.pod()
	return &pod, nil
}

func (p podObject) pod() Pod {
	pod := Pod{
		Namespace: p.Metadata.Namespace,
		Name:      p.Metadata.Name,
		Node:      p.Spec.NodeName,
		Status:    p.Status.Phase,
		Created:   p.Metadata.CreationTimestamp,
	}
	if p.Status.Reason != "" {
		pod.Status = p.Status.Reason
	}
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controller {
			pod.Owner = o.Kind
		}
	}
	// Native sidecars are init containers that keep running, like the Agones
	// SDK server. Other init containers have finished once the pod runs.
	add := func(ct podContainer) {
		pod.Containers = append(pod.Containers, ct.Name)
		pod.Requests = pod.Requests.Add(resources(ct.Resources.Requests["cpu"], ct.Resources.Requests["memory"]))
	}
	for _, ct := range p.Spec.Containers {
		add(ct)
	}
	sidecars := map[string]bool{}
	for _, ct := range p.Spec.InitContainers {
		if ct.RestartPolicy == "Always" {
			sidecars[ct.Name] = true
			add(ct)
		}
	}
	statuses := p.Status.ContainerStatuses
	for _, s := range p.Status.InitContainerStatuses {
		if sidecars[s.Name] {
			statuses = append(statuses, s)
		}
	}
	for _, s := range statuses {
		pod.Restarts += s.RestartCount
		switch {
		case s.Ready:
			pod.Ready++
		case s.State.Waiting != nil && s.State.Waiting.Reason != "":
			pod.Status = s.State.Waiting.Reason
		case s.State.Terminated != nil && s.State.Terminated.Reason != "" && pod.Status == "Running":
			pod.Status = s.State.Terminated.Reason
		}
	}
	if p.Metadata.DeletionTimestamp != nil {
		pod.Status = "Terminating"
	}
	return pod
}

// Fleets lists the Fleets in the game namespace by name.
func (c *Client) Fleets(ctx context.Context) ([]Fleet, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Template gameServer `json:"template"`
			} `json:"spec"`
			Status struct {
				Replicas          int `json:"replicas"`
				ReadyReplicas     int `json:"readyReplicas"`
				AllocatedReplicas int `json:"allocatedReplicas"`
				ReservedReplicas  int `json:"reservedReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.get(ctx, apiPath("agones.dev", c.namespace, "fleets", ""), nil, &list); err != nil {
		return nil, err
	}
	fleets := make([]Fleet, 0, len(list.Items))
	for _, item := range list.Items {
		f := Fleet{
			Name:      item.Metadata.Name,
			Replicas:  item.Status.Replicas,
			Ready:     item.Status.ReadyReplicas,
			Allocated: item.Status.AllocatedReplicas,
			Reserved:  item.Status.ReservedReplicas,
			Created:   item.Metadata.CreationTimestamp,
		}
		if containers := item.Spec.Template.Spec.Template.Spec.Containers; len(containers) > 0 {
			f.Image = containers[0].Image
		}
		fleets = append(fleets, f)
	}
	sort.Slice(fleets, func(i, j int) bool { return fleets[i].Name < fleets[j].Name })
	return fleets, nil
}

// Rooms lists every GameServer in the game namespace, oldest first.
func (c *Client) Rooms(ctx context.Context) ([]Room, error) {
	var list struct {
		Items []gameServer `json:"items"`
	}
	if err := c.get(ctx, apiPath("agones.dev", c.namespace, "gameservers", ""), nil, &list); err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(list.Items))
	for _, gs := range list.Items {
		rooms = append(rooms, c.room(gs))
	}
	sort.SliceStable(rooms, func(i, j int) bool { return rooms[i].Created.Before(rooms[j].Created) })
	return rooms, nil
}

// Events lists the newest events, newest first. An empty namespace means all
// namespaces; a non-empty object keeps only events about objects of that name.
func (c *Client) Events(ctx context.Context, namespace, object string) ([]Event, error) {
	var query url.Values
	if object != "" {
		query = url.Values{"fieldSelector": {"involvedObject.name=" + object}}
	}
	var list struct {
		Items []struct {
			Metadata       objectMeta `json:"metadata"`
			InvolvedObject struct {
				Kind      string `json:"kind"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"involvedObject"`
			Type          string     `json:"type"`
			Reason        string     `json:"reason"`
			Message       string     `json:"message"`
			Count         int        `json:"count"`
			LastTimestamp *time.Time `json:"lastTimestamp"`
			EventTime     *time.Time `json:"eventTime"`
			Series        *struct {
				Count            int        `json:"count"`
				LastObservedTime *time.Time `json:"lastObservedTime"`
			} `json:"series"`
		} `json:"items"`
	}
	if err := c.get(ctx, apiPath("", namespace, "events", ""), query, &list); err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(list.Items))
	for _, item := range list.Items {
		e := Event{
			Namespace: item.Metadata.Namespace,
			Kind:      item.InvolvedObject.Kind,
			Name:      item.InvolvedObject.Name,
			Type:      item.Type,
			Reason:    item.Reason,
			Message:   item.Message,
			Count:     max(item.Count, 1),
			Last:      item.Metadata.CreationTimestamp,
		}
		// Events from newer components fill eventTime and series instead.
		for _, t := range []*time.Time{item.EventTime, item.LastTimestamp} {
			if t != nil && t.After(e.Last) {
				e.Last = *t
			}
		}
		if s := item.Series; s != nil {
			e.Count = max(e.Count, s.Count)
			if s.LastObservedTime != nil && s.LastObservedTime.After(e.Last) {
				e.Last = *s.LastObservedTime
			}
		}
		events = append(events, e)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Last.After(events[j].Last) })
	if len(events) > maxEvents {
		events = events[:maxEvents]
	}
	return events, nil
}

// Log returns the last lines of one container's log, at most 2 MiB.
func (c *Client) Log(ctx context.Context, namespace, pod, container string, lines int) (string, error) {
	query := url.Values{
		"container":  {container},
		"tailLines":  {strconv.Itoa(lines)},
		"limitBytes": {strconv.Itoa(2 << 20)},
	}
	var log []byte
	if err := c.get(ctx, apiPath("", namespace, "pods", pod)+"/log", query, &log); err != nil {
		return "", err
	}
	return string(log), nil
}

// Usage reads the metrics server. It fails when the cluster has none.
func (c *Client) Usage(ctx context.Context) (Usage, error) {
	type sample struct {
		Metadata objectMeta        `json:"metadata"`
		Usage    map[string]string `json:"usage"`
	}
	var nodes struct {
		Items []sample `json:"items"`
	}
	var pods struct {
		Items []struct {
			Metadata   objectMeta `json:"metadata"`
			Containers []struct {
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := c.get(ctx, apiPath("metrics.k8s.io", "", "nodes", ""), nil, &nodes); err != nil {
		return Usage{}, err
	}
	if err := c.get(ctx, apiPath("metrics.k8s.io", "", "pods", ""), nil, &pods); err != nil {
		return Usage{}, err
	}
	usage := Usage{Nodes: map[string]Resources{}, Pods: map[string]Resources{}}
	for _, n := range nodes.Items {
		usage.Nodes[n.Metadata.Name] = resources(n.Usage["cpu"], n.Usage["memory"])
	}
	for _, p := range pods.Items {
		var sum Resources
		for _, ct := range p.Containers {
			sum = sum.Add(resources(ct.Usage["cpu"], ct.Usage["memory"]))
		}
		usage.Pods[p.Metadata.Namespace+"/"+p.Metadata.Name] = sum
	}
	return usage, nil
}

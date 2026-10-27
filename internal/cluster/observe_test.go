package cluster

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestParseQuantity(t *testing.T) {
	cases := map[string]float64{
		"2":         2,
		"250m":      0.25,
		"84521331n": 0.084521331,
		"1941796Ki": 1941796 * 1024,
		"512Mi":     512 << 20,
		"1G":        1e9,
		"1e3":       1000,
		"":          0,
		"lots":      0,
	}
	for in, want := range cases {
		if got := parseQuantity(in); got != want {
			t.Errorf("parseQuantity(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNodes(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		io.WriteString(w, `{"items":[
			{"metadata":{"name":"worker","labels":{"gamefleet.io/pool":"game"}},
			 "spec":{"unschedulable":true},
			 "status":{"allocatable":{"cpu":"2","memory":"1941796Ki"},
			  "addresses":[{"type":"InternalIP","address":"10.0.0.3"},{"type":"ExternalIP","address":"203.0.113.20"}],
			  "conditions":[{"type":"MemoryPressure","status":"True"},{"type":"Ready","status":"False"}],
			  "nodeInfo":{"kubeletVersion":"v1.35.8+k3s1","osImage":"Debian GNU/Linux 13 (trixie)"}}},
			{"metadata":{"name":"manager","labels":{"node-role.kubernetes.io/control-plane":"true","node-role.kubernetes.io/master":"true"}},
			 "spec":{"taints":[{"key":"CriticalAddonsOnly","value":"true","effect":"NoExecute"}]},
			 "status":{"allocatable":{"cpu":"1950m","memory":"3Gi"},"conditions":[{"type":"Ready","status":"True"}]}}
		]}`)
	})
	nodes, err := client.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].Name != "manager" || nodes[1].Name != "worker" {
		t.Fatalf("nodes = %+v", nodes)
	}
	manager, worker := nodes[0], nodes[1]
	if !manager.Ready || !reflect.DeepEqual(manager.Roles, []string{"control-plane", "master"}) ||
		!reflect.DeepEqual(manager.Taints, []string{"CriticalAddonsOnly=true:NoExecute"}) ||
		manager.Allocatable != (Resources{MilliCPU: 1950, Memory: 3 << 30}) {
		t.Errorf("manager = %+v", manager)
	}
	if worker.Ready || !worker.Unschedulable || worker.InternalIP != "10.0.0.3" || worker.ExternalIP != "203.0.113.20" ||
		!reflect.DeepEqual(worker.Pressure, []string{"MemoryPressure"}) || worker.Allocatable.MilliCPU != 2000 ||
		worker.Kubelet != "v1.35.8+k3s1" {
		t.Errorf("worker = %+v", worker)
	}
}

func TestPodsOfOneNode(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" || r.URL.Query().Get("fieldSelector") != "spec.nodeName=worker" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		io.WriteString(w, `{"items":[
			{"metadata":{"name":"billiards-x7k2p","namespace":"games","creationTimestamp":"2026-10-20T07:00:00Z",
			  "ownerReferences":[{"kind":"GameServer","name":"billiards-x7k2p","controller":true}]},
			 "spec":{"nodeName":"worker",
			  "initContainers":[{"name":"setup"},{"name":"agones-gameserver-sidecar","restartPolicy":"Always","resources":{"requests":{"cpu":"30m"}}}],
			  "containers":[{"name":"billiards","resources":{"requests":{"cpu":"250m","memory":"256Mi"}}}]},
			 "status":{"phase":"Running",
			  "initContainerStatuses":[{"name":"setup","state":{"terminated":{"reason":"Completed"}}},
			   {"name":"agones-gameserver-sidecar","ready":true,"restartCount":1}],
			  "containerStatuses":[{"name":"billiards","ready":true}]}},
			{"metadata":{"name":"broken","namespace":"default"},
			 "spec":{"nodeName":"worker","containers":[{"name":"app"}]},
			 "status":{"phase":"Running","containerStatuses":[{"name":"app","restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}
		]}`)
	})
	pods, err := client.Pods(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatalf("pods = %+v", pods)
	}
	broken, game := pods[0], pods[1]
	if broken.Status != "CrashLoopBackOff" || broken.Restarts != 7 || broken.Ready != 0 {
		t.Errorf("broken = %+v", broken)
	}
	want := Pod{
		Namespace: "games", Name: "billiards-x7k2p", Node: "worker", Owner: "GameServer", Status: "Running",
		Ready: 2, Containers: []string{"billiards", "agones-gameserver-sidecar"}, Restarts: 1,
		Requests: Resources{MilliCPU: 280, Memory: 256 << 20}, Created: time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(game, want) {
		t.Errorf("game pod = %+v\nwant       %+v", game, want)
	}
}

func TestFleetsAndRooms(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/agones.dev/v1/namespaces/games/fleets":
			io.WriteString(w, `{"items":[{"metadata":{"name":"billiards"},
				"spec":{"template":{"spec":{"template":{"spec":{"containers":[{"image":"fleet.example.com/billiards:3"}]}}}}},
				"status":{"replicas":3,"readyReplicas":2,"allocatedReplicas":1}}]}`)
		case gameServers:
			io.WriteString(w, `{"items":[
				{"metadata":{"name":"late","creationTimestamp":"2026-10-20T07:10:00Z"},"status":{"state":"Ready"}},
				{"metadata":{"name":"early","creationTimestamp":"2026-10-20T07:00:00Z",
				  "labels":{"agones.dev/fleet":"billiards","gamefleet.io/game":"billiards"},
				  "annotations":{"gamefleet.io/match.m-1":"{\"channel\":\"stable\",\"compat\":\"dm-v7\",\"allocated\":\"2026-10-20T07:05:00.123456789Z\"}"}},
				 "spec":{"template":{"spec":{"containers":[{"image":"fleet.example.com/billiards:3"}]}}},
				 "status":{"state":"Allocated","nodeName":"worker","address":"203.0.113.10","ports":[{"port":7001}],
				  "lists":{"rooms":{"capacity":50,"values":["m-1"]}}}}
			]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	fleets, err := client.Fleets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleets) != 1 || fleets[0] != (Fleet{Name: "billiards", Image: "fleet.example.com/billiards:3", Replicas: 3, Ready: 2, Allocated: 1}) {
		t.Errorf("fleets = %+v", fleets)
	}
	rooms, err := client.Rooms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 || rooms[0].ID != "early" || rooms[1].ID != "late" {
		t.Fatalf("rooms = %+v", rooms)
	}
	room := rooms[0]
	m, ok := room.Match("m-1")
	if room.Node != "worker" || room.Image != "fleet.example.com/billiards:3" || room.Game != "billiards" || room.Capacity != 50 ||
		!ok || m.Channel != "stable" || m.Compat != "dm-v7" || !m.Allocated.Equal(time.Date(2026, 10, 20, 7, 5, 0, 123456789, time.UTC)) {
		t.Errorf("room = %+v", room)
	}
}

func TestEventsNewestFirst(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/games/events" || r.URL.Query().Get("fieldSelector") != "involvedObject.name=gs-1" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		io.WriteString(w, `{"items":[
			{"metadata":{"namespace":"games","creationTimestamp":"2026-10-20T07:00:00Z"},"involvedObject":{"kind":"Pod","name":"gs-1"},
			 "type":"Warning","reason":"Unhealthy","message":"Liveness probe failed","count":3,"lastTimestamp":"2026-10-20T07:02:00Z"},
			{"metadata":{"namespace":"games","creationTimestamp":"2026-10-20T07:00:00Z"},"involvedObject":{"kind":"GameServer","name":"gs-1"},
			 "type":"Normal","reason":"Allocated","eventTime":"2026-10-20T07:01:00Z","series":{"count":2,"lastObservedTime":"2026-10-20T07:03:00Z"}}
		]}`)
	})
	events, err := client.Events(context.Background(), "games", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Reason != "Allocated" || events[0].Count != 2 ||
		!events[0].Last.Equal(time.Date(2026, 10, 20, 7, 3, 0, 0, time.UTC)) ||
		events[1].Kind != "Pod" || events[1].Count != 3 || events[1].Type != "Warning" {
		t.Fatalf("events = %+v", events)
	}
}

func TestLogReturnsTheRawText(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/namespaces/games/pods/gs-1/log" || q.Get("container") != "billiards" || q.Get("tailLines") != "200" ||
			q.Get("sinceSeconds") != "1800" || q.Get("previous") != "true" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "DM_SERVER_READY\nDM_ALLOCATED match=m-1\n")
	})
	log, err := client.Log(context.Background(), "games", "gs-1",
		LogQuery{Container: "billiards", Lines: 200, Since: 30 * time.Minute, Previous: true})
	if err != nil || log != "DM_SERVER_READY\nDM_ALLOCATED match=m-1\n" {
		t.Fatalf("log = %q, err = %v", log, err)
	}
}

func TestUsage(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			io.WriteString(w, `{"items":[{"metadata":{"name":"worker"},"usage":{"cpu":"84521331n","memory":"960512Ki"}}]}`)
		case "/apis/metrics.k8s.io/v1beta1/pods":
			io.WriteString(w, `{"items":[{"metadata":{"name":"gs-1","namespace":"games"},
				"containers":[{"usage":{"cpu":"20m","memory":"160Mi"}},{"usage":{"cpu":"1m","memory":"10Mi"}}]}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	usage, err := client.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Nodes["worker"] != (Resources{MilliCPU: 85, Memory: 960512 << 10}) ||
		usage.Pods["games/gs-1"] != (Resources{MilliCPU: 21, Memory: 170 << 20}) {
		t.Fatalf("usage = %+v", usage)
	}
}

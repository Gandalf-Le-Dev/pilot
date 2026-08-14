// Command dashdemo serves the real dashboard with example-fleet data, for
// the README's screenshot.
//
// Same contract as the terminal captures in ../demo: the actual rendering
// code — server, views, components, charts — fed with the example fleet's
// vocabulary, so the picture is pixel-faithful to the product without
// photographing anyone's infrastructure. Being part of the module, it breaks
// the build when the dashboard's shapes change. The wobble in the series
// comes from a fixed seed, so a re-capture diffs only where reality changed.
//
// Capture (see ../README.md):
//
//	go run ./docs/readme/dashdemo &
//	"Google Chrome" --headless=new --screenshot=docs/readme/dashboard.png \
//	    --window-size=1240,1520 --hide-scrollbars \
//	    --virtual-time-budget=6000 http://127.0.0.1:5481/
package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/Gandalf-Le-Dev/pilot/internal/dashboard"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
	"github.com/Gandalf-Le-Dev/pilot/internal/runtime"
	"github.com/Gandalf-Le-Dev/pilot/internal/transport/proto"
)

var rng = rand.New(rand.NewSource(42))

func samples(n int, base, wobble float64, mem uint64) []proto.MetricSample {
	out := make([]proto.MetricSample, 0, n)
	start := time.Now().Add(-time.Duration(n) * 30 * time.Second)
	for i := range n {
		v := base + wobble*math.Sin(float64(i)/9) + rng.Float64()*wobble/2
		out = append(out, proto.MetricSample{
			At:       start.Add(time.Duration(i) * 30 * time.Second),
			CPUPct:   v,
			MemBytes: mem + uint64(float64(mem)*0.08*math.Sin(float64(i)/14)),
		})
	}
	return out
}

func obs(state, rel string) runtime.Observation {
	return runtime.Observation{State: runtime.State(state), Release: rel}
}

type fake struct{ host string }

func (f fake) Dashboard(ctx context.Context, since time.Time) (*proto.DashboardResponse, error) {
	if f.host == "server-2" {
		return &proto.DashboardResponse{
			Host:     f.host,
			Capacity: proto.Capacity{Cores: 2, MemTotal: 4 << 30},
			Status: proto.StatusResponse{
				Host: f.host,
				Disk: &proto.DiskUsage{Path: "/opt/pilot", UsedPercent: 21},
				Services: []proto.ServiceStatus{
					{Name: "api", Runtime: "compose", Manage: "deploy", Obs: obs("running", "0042-9f3ac1b")},
					{Name: "site", Runtime: "static", Manage: "deploy", Obs: obs("running", "0040-b52ffa1")},
				},
			},
			ServiceSamples: map[string][]proto.MetricSample{
				"api": samples(300, 9, 4, 700<<20),
			},
			HostSamples: samples(300, 17, 6, 1600<<20),
		}, nil
	}
	return &proto.DashboardResponse{
		Host:     f.host,
		Capacity: proto.Capacity{Cores: 4, MemTotal: 8 << 30},
		Status: proto.StatusResponse{
			Host: f.host,
			Disk: &proto.DiskUsage{Path: "/opt/pilot", UsedPercent: 34},
			Services: []proto.ServiceStatus{
				{Name: "api", Runtime: "compose", Manage: "deploy", Obs: obs("running", "0042-9f3ac1b")},
				{Name: "db", Runtime: "compose", Manage: "observe", Obs: obs("running", "0007-1fe22a1")},
				{Name: "site", Runtime: "static", Manage: "deploy", Obs: obs("running", "0040-b52ffa1")},
				{Name: "backup", Runtime: "systemd", Manage: "deploy", Obs: obs("degraded", "0019-8eec9e0"),
					Drift: &proto.Drift{Config: true, DetectedAt: time.Now().Add(-2 * time.Hour)}},
			},
		},
		ServiceSamples: map[string][]proto.MetricSample{
			"api":    samples(300, 12, 6, 900<<20),
			"db":     samples(300, 6, 2, 2<<30),
			"backup": samples(300, 1, 1, 64<<20),
		},
		HostSamples: samples(300, 28, 9, 5<<30),
		AlertEvents: []proto.AlertEvent{
			{Rule: "service.degraded", Subject: "backup", FiredAt: time.Now().Add(-40 * time.Minute)},
			{Rule: "host.disk.used_pct > 85", FiredAt: time.Now().Add(-26 * time.Hour),
				ResolvedAt: time.Now().Add(-25 * time.Hour), DeliveryFailed: true},
		},
		Deploys: map[string][]release.DeployRecord{
			"api": {
				{Release: "0042-9f3ac1b", By: "you", Outcome: "ok", FinishedAt: time.Now().Add(-3 * time.Hour)},
				{Release: "0041-c96fe11", By: "ci", Outcome: "rolled-back", FinishedAt: time.Now().Add(-30 * time.Hour)},
			},
			"site": {
				{Release: "0040-b52ffa1", By: "ci", Outcome: "ok", FinishedAt: time.Now().Add(-26 * time.Hour)},
			},
		},
	}, nil
}

func main() {
	srv := dashboard.New(dashboard.Source{
		Hosts:  []string{"server-1", "server-2"},
		Manage: map[string]string{"db": "observe"},
		Connect: func(ctx context.Context, host string) (dashboard.HostClient, string) {
			return fake{host: host}, ""
		},
	})
	url, err := srv.Serve(context.Background(), 5481)
	if err != nil {
		panic(err)
	}
	fmt.Println("demo dashboard on", url)
	srv.Run(context.Background())
}

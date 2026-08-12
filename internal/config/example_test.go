package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// exampleDir locates example/ from this test's own file, so it works regardless
// of the working directory a test runner chooses.
func exampleDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own path")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", "example")
}

// TestExampleFleetIsValid is the reason the example is worth having.
//
// A configuration example in a README is prose, and prose drifts silently — the
// first person to find out is someone copying a field that no longer exists.
// Loading it here makes the documentation fail the build instead.
func TestExampleFleetIsValid(t *testing.T) {
	f, ds, err := Load(exampleDir(t))
	if err != nil {
		t.Fatalf("the example fleet does not load: %v", err)
	}

	for _, d := range ds.Sorted() {
		t.Errorf("%s: %s: %s", d.File, d.Field, d.Message)
	}
	if ds.HasErrors() {
		t.FailNow()
	}

	if len(f.Hosts) < 2 {
		t.Errorf("got %d hosts; the example should show more than one, since "+
			"single-host is the case that hides multi-host mistakes", len(f.Hosts))
	}
}

// TestExampleCoversEveryFeature guards the claim the example makes about itself.
//
// An example that quietly stops demonstrating a feature is how a feature ends up
// undocumented, so the coverage is asserted rather than trusted.
func TestExampleCoversEveryFeature(t *testing.T) {
	f, _, err := Load(exampleDir(t))
	if err != nil {
		t.Fatal(err)
	}

	var (
		compose, static, observe, blueGreen bool
		restricted, rawRoute, spa, overlay  bool
		gitSource, impliedSource            bool
		health, svcAlerts, secretRefs       bool
		systemdSvc, systemdOneshot          bool
		unitLinks, unitPrecheck             bool
	)

	for _, s := range f.Services {
		switch s.Runtime {
		case RuntimeCompose:
			compose = true
		case RuntimeStatic:
			static = true
		case RuntimeSystemd:
			if s.Unit != nil && s.Unit.IsOneshot() {
				systemdOneshot = true
			} else {
				systemdSvc = true
			}
		}
		if s.Unit != nil {
			unitLinks = unitLinks || len(s.Unit.Links) > 0
			unitPrecheck = unitPrecheck || len(s.Unit.Precheck) > 0
		}
		if !s.Deployable() {
			observe = true
		}
		if s.Rollout != nil && s.Rollout.Strategy == StrategyBlueGreen {
			blueGreen = true
		}
		if s.Health != nil {
			health = true
		}
		if len(s.Alerts) > 0 {
			svcAlerts = true
		}
		if s.Source != nil {
			if s.Source.Repo != "" {
				gitSource = true
			}
			if strings.HasPrefix(s.Source.Path, ServicesDir+"/") {
				impliedSource = true
			}
		}
		for _, v := range s.Env {
			if strings.Contains(v, "${") {
				secretRefs = true
			}
		}
		if e := s.Expose; e != nil {
			if len(e.Allow) > 0 {
				restricted = true
			}
			if e.Raw != "" {
				rawRoute = true
			}
			if e.Static != nil {
				spa = spa || e.Static.SPA
				overlay = overlay || len(e.Static.Overlay) > 0
			}
		}
	}

	for _, c := range []struct {
		got  bool
		what string
	}{
		{compose, "the compose runtime"},
		{static, "the static runtime"},

		// Both systemd shapes, and neither modelled on the service that
		// prompted the runtime. A daemon alone would have let a daemon-only
		// adapter pass for a general one — a oneshot has no "running" state to
		// observe, is restarted through its timer rather than directly, and is
		// judged on when it last succeeded. If the schema ever stops being able
		// to express the second, this stops compiling a service that does.
		{systemdSvc, "a systemd daemon"},
		{systemdOneshot, "a systemd oneshot behind a timer"},
		{unitLinks, "unit links into the live release"},
		{unitPrecheck, "a unit precheck"},

		{observe, "manage: observe"},
		{blueGreen, "a blue-green rollout"},
		{health, "a health check"},
		{svcAlerts, "a per-service alert rule"},
		{secretRefs, "secret references"},
		{restricted, "expose.allow"},
		{rawRoute, "expose.raw"},
		{spa, "a static SPA route"},
		{overlay, "a static overlay"},
		{gitSource, "an explicit git source"},
		{impliedSource, "a source implied by the service's own directory"},
	} {
		if !c.got {
			t.Errorf("the example no longer demonstrates %s", c.what)
		}
	}

	if len(f.Notifiers) < 2 {
		t.Errorf("got %d notifiers; the example should show more than one type", len(f.Notifiers))
	}
	if len(f.Alerts) == 0 {
		t.Error("the example no longer shows a host-wide alert rule")
	}
	if f.NotifyDeploys == nil {
		t.Error("the example no longer shows notify_deploys")
	}
}

// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

//nolint:testpackage // Setup tests share the fakes with the step tests.
package enginefault

import (
	"flag"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type stubEngine struct{ flag string }

func (stub *stubEngine) BindFlags(flags *flag.FlagSet) {
	flags.StringVar(&stub.flag, "stub-engine", "", "")
}

func (*stubEngine) New() (engine.Engine, error) { return &fakeEngine{}, nil }

type stubWorkload struct{}

func (*stubWorkload) BindFlags(*flag.FlagSet) {}

func (*stubWorkload) New(client.Reader, client.Client) (workload.Workload, journal.Journal, error) {
	return &fakeWorkload{}, &fakeJournal{}, nil
}

func options() *Options {
	return &Options{
		Enabled: true, Engine: "stub", Workload: "stub", Config: Defaults(),
		Engines: map[string]EngineFactory{"stub": &stubEngine{}}, Workloads: map[string]WorkloadFactory{"stub": &stubWorkload{}},
	}
}

func TestSetupRejects(t *testing.T) {
	t.Parallel()
	if err := (&Options{}).Setup(nil, &fakeRecorder{}); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	for name, test := range map[string]struct {
		mutate func(*Options)
		want   string
	}{
		"unknown engine":   {func(options *Options) { options.Engine = "vllm" }, "unknown --engine"},
		"unknown workload": {func(options *Options) { options.Workload = "jobset" }, "unknown --workload"},
		"zero timeout":     {func(options *Options) { options.Config.ApplyTimeout = 0 }, "timeouts"},
		"negative verify":  {func(options *Options) { options.Config.VerifyTimeout = -time.Second }, "timeouts"},
		"negative unseen":  {func(options *Options) { options.Config.UnseenTimeout = -time.Second }, "timeouts"},
		"zero poll":        {func(options *Options) { options.Config.PollInterval = 0 }, "timeouts"},
		"restore mode":     {func(options *Options) { options.Config.CapacityRestore = "grow" }, "--ft-capacity-restore"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := options()
			test.mutate(opts)
			if err := opts.Setup(nil, &fakeRecorder{}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func TestBindFlags(t *testing.T) {
	t.Parallel()
	opts := options()
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	opts.BindFlags(flags)
	if opts.Config != Defaults() || opts.Workload != "lws" {
		t.Fatalf("defaults: %+v", opts)
	}
	args := []string{
		"--enable-engine-ft", "--engine=stub", "--ft-capacity-restore=release", "--ft-verify-timeout=0", "--ft-unseen-timeout=0",
		"--stub-engine=x",
	}
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	stub, ok := opts.Engines["stub"].(*stubEngine)
	if !opts.Enabled || opts.Engine != "stub" || opts.Config.CapacityRestore != RestoreRelease ||
		opts.Config.VerifyTimeout != 0 || opts.Config.UnseenTimeout != 0 || !ok || stub.flag != "x" {
		t.Fatalf("parsed: %+v", opts)
	}
}

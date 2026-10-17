// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
	"github.com/llm-d/llm-d-resiliency-operator/internal/journal"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload"
)

type EngineFactory interface {
	BindFlags(flags *flag.FlagSet)
	New() (engine.Engine, error)
}

type WorkloadFactory interface {
	BindFlags(flags *flag.FlagSet)
	New(reader client.Reader, writer client.Client) (workload.Workload, journal.Journal, error)
}

type Options struct {
	Enabled   bool
	Engine    string
	Workload  string
	Config    Config
	Engines   map[string]EngineFactory
	Workloads map[string]WorkloadFactory
}

func Defaults() Config {
	return Config{
		PollInterval: time.Second, DiagnosisTimeout: 75 * time.Second, ApplyTimeout: 90 * time.Second,
		VerifyTimeout: 2 * time.Minute, UnseenTimeout: 10 * time.Minute, FlapWindow: 2 * time.Minute,
		CapacityRestore: RestoreResetPod,
	}
}

func (options *Options) BindFlags(flags *flag.FlagSet) {
	options.Config = Defaults()
	config := &options.Config
	flags.BoolVar(&options.Enabled, "enable-engine-ft", false, "recover inference engine faults (needs --leader-elect)")
	flags.StringVar(&options.Engine, "engine", "", "inference engine implementation")
	flags.StringVar(&options.Workload, "workload", "lws", "workload implementation")
	flags.DurationVar(&config.PollInterval, "ft-poll-interval", config.PollInterval, "how often the leader reads the group and acts")
	flags.DurationVar(&config.DiagnosisTimeout, "ft-diagnosis-timeout", config.DiagnosisTimeout,
		"longest time recovering without a reconfig")
	flags.DurationVar(&config.ApplyTimeout, "ft-apply-timeout", config.ApplyTimeout, "longest time a reconfig may stay in flight")
	flags.DurationVar(&config.VerifyTimeout, "ft-verify-timeout", config.VerifyTimeout,
		"how long a failed check after a reconfig is retried (0 resets on the first failure)")
	flags.DurationVar(&config.UnseenTimeout, "ft-unseen-timeout", config.UnseenTimeout,
		"how long no admitted member may be readable before the group is reset (0 waits forever)")
	flags.StringVar((*string)(&config.CapacityRestore), "ft-capacity-restore", string(config.CapacityRestore),
		"how capacity comes back after a shrink: reset-pod, reset-all or release")
	for _, name := range slices.Sorted(maps.Keys(options.Engines)) {
		options.Engines[name].BindFlags(flags)
	}
	for _, name := range slices.Sorted(maps.Keys(options.Workloads)) {
		options.Workloads[name].BindFlags(flags)
	}
}

func (options *Options) Setup(mgr ctrl.Manager, recorder Recorder) error {
	if !options.Enabled {
		return nil
	}
	engineFactory, ok := options.Engines[options.Engine]
	if !ok {
		return fmt.Errorf("unknown --engine %q; available: %v", options.Engine, slices.Sorted(maps.Keys(options.Engines)))
	}
	workloadFactory, ok := options.Workloads[options.Workload]
	if !ok {
		return fmt.Errorf("unknown --workload %q; available: %v", options.Workload, slices.Sorted(maps.Keys(options.Workloads)))
	}
	if err := options.Config.validate(); err != nil {
		return err
	}
	eng, err := engineFactory.New()
	if err != nil {
		return err
	}
	wl, store, err := workloadFactory.New(mgr.GetAPIReader(), mgr.GetClient())
	if err != nil {
		return err
	}
	return mgr.Add(&Runner{Workload: wl, Engine: eng, Journal: store, Recorder: recorder, Config: options.Config})
}

func (config *Config) validate() error {
	if config.PollInterval <= 0 || config.DiagnosisTimeout <= 0 || config.ApplyTimeout <= 0 ||
		config.FlapWindow <= 0 || config.VerifyTimeout < 0 || config.UnseenTimeout < 0 {
		return errors.New("engine FT timeouts must be positive (--ft-verify-timeout and --ft-unseen-timeout may be 0)")
	}
	if !slices.Contains([]CapacityRestore{RestoreResetPod, RestoreResetAll, RestoreRelease}, config.CapacityRestore) {
		return fmt.Errorf("unknown --ft-capacity-restore %q", config.CapacityRestore)
	}
	return nil
}

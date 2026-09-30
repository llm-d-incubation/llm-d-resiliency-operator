// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

package enginefault

import (
	"flag"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine/vllm"
	"github.com/llm-d/llm-d-resiliency-operator/internal/workload/lws"
)

type Options struct {
	Enabled    bool
	Model      string
	Workload   lws.Config
	Controller Config
}

func (options *Options) BindFlags(flags *flag.FlagSet) {
	options.Controller = Defaults()
	flags.BoolVar(&options.Enabled, "enable-vllm-ft", false, "enable vLLM engine fault handling")
	flags.StringVar(&options.Workload.Namespace, "vllm-namespace", "default", "LWS namespace")
	flags.StringVar(&options.Workload.Name, "vllm-lws", "", "LWS name")
	flags.StringVar(&options.Model, "vllm-model", "", "served model name")
	flags.IntVar(&options.Workload.LocalRanks, "vllm-local-ranks", 2, "original DP ranks per Pod")
	flags.IntVar(&options.Workload.GroupPods, "vllm-group-pods", 2, "Pods in the LWS group")
	flags.IntVar(&options.Workload.BasePort, "vllm-api-port", 8000, "first per-rank API port")
	flags.IntVar(&options.Workload.ObserverPort, "vllm-observer-port", 9257, "process observer port")
	flags.IntVar(&options.Controller.MinSurvivors, "vllm-min-survivors", 0, "validated minimum surviving DP ranks")
	flags.IntVar(&options.Controller.StorePort, "vllm-ft-store-port", 29600, "replacement FT store port")
	flags.DurationVar(&options.Controller.DiagnosisTimeout, "vllm-diagnosis-timeout",
		options.Controller.DiagnosisTimeout, "engine diagnosis deadline")
	flags.DurationVar(&options.Controller.ApplyTimeout, "vllm-apply-timeout",
		options.Controller.ApplyTimeout, "FT completion deadline")
}

func (options *Options) Setup(mgr ctrl.Manager) error {
	if !options.Enabled {
		return nil
	}
	if options.Model == "" {
		return fmt.Errorf("vLLM FT requires --vllm-model")
	}
	if options.Controller.MinSurvivors < 1 ||
		options.Controller.MinSurvivors > options.Workload.LocalRanks*options.Workload.GroupPods ||
		options.Controller.DiagnosisTimeout <= 0 || options.Controller.ApplyTimeout <= 0 ||
		options.Controller.StorePort < 1 || options.Controller.StorePort > 65535 {
		return fmt.Errorf("invalid vLLM capacity, deadline or FT store port")
	}
	workload, err := lws.New(mgr.GetAPIReader(), mgr.GetClient(), options.Workload)
	if err != nil {
		return err
	}
	runner := &Runner{
		Adapter: vllm.New(options.Model), Workload: workload,
		Config: options.Controller,
	}
	return mgr.Add(runner)
}

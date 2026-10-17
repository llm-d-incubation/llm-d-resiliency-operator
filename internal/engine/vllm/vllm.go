// Copyright 2026 The llm-d Authors.
// SPDX-License-Identifier: Apache-2.0

// Package vllm is the vLLM engine. See docs/components/engines/vllm.md.
package vllm

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-resiliency-operator/internal/engine"
)

type Options struct {
	APIPort     int
	RanksPerPod int
	StorePort   int
	Timeout     time.Duration
}

func (options *Options) BindFlags(flags *flag.FlagSet) {
	flags.IntVar(&options.APIPort, "vllm-api-port", 8000, "API port of a Pod's first rank; rank i listens on this port plus i")
	flags.IntVar(&options.RanksPerPod, "vllm-ranks-per-pod", 1, "data parallel ranks in each Pod")
	flags.IntVar(&options.StorePort, "vllm-store-port", 29600,
		"base port for a new FT store when a reconfig removes the rank that hosts it; rank r listens on base + r")
	flags.DurationVar(&options.Timeout, "vllm-timeout", 5*time.Second, "timeout of one status, health or apply request")
}

func (options *Options) New() (engine.Engine, error) {
	if options.RanksPerPod < 1 || options.Timeout <= 0 || !validPort(options.APIPort) || !validPort(options.StorePort) {
		return nil, errors.New("invalid --vllm-* flags")
	}
	return &Engine{options: *options, client: &http.Client{}}, nil
}

func validPort(port int) bool { return port > 0 && port < 65536 }

type Engine struct {
	options Options
	client  *http.Client
}

func (*Engine) Capabilities() engine.Capabilities { return engine.Capabilities{} }

type rank struct {
	Rank      int
	Address   string
	PodIP     string
	Reached   bool
	Health    int
	Status    string
	FaultInfo string
	Mask      []int
	FTState   string
	FTError   string
	RequestID string
	Error     string
}

func (r *rank) dead() bool {
	return r.Reached && (r.Status == "dead" || r.Health == http.StatusServiceUnavailable)
}

func (r *rank) serving() bool { return r.Reached && r.Status == "healthy" && r.Health == http.StatusOK }

func (r *rank) detail() string {
	switch {
	case !r.Reached:
		return "unreachable: " + r.Error
	case r.Health == http.StatusServiceUnavailable:
		return "engine process exited"
	case r.Status == "unhealthy":
		return fmt.Sprintf("unhealthy: %s mask=%v", r.FaultInfo, r.Mask)
	case r.FTState != "":
		return fmt.Sprintf("%s, %s request %s %s", r.Status, r.FTState, r.RequestID, r.FTError)
	}
	return r.Status
}

type ranks map[engine.MemberID]*rank

func ranksOf(observation engine.Observation) ranks {
	if found, ok := observation.Engine.(ranks); ok {
		return found
	}
	return ranks{}
}

func rankNumber(id engine.MemberID) (int, error) {
	name, _, _ := strings.Cut(string(id), "@")
	number, err := strconv.Atoi(strings.TrimPrefix(name, "dp"))
	if err != nil || !strings.HasPrefix(name, "dp") {
		return 0, fmt.Errorf("member %s is not a vLLM rank", id)
	}
	return number, nil
}

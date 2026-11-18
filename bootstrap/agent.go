// Package bootstrap assembles the default LuckyAgent application runtime.
package bootstrap

import (
	"context"

	"github.com/yurika0211/lucky-agent/agent"
	"github.com/yurika0211/luckyagent-runtime/bootstrap"
	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-tools/memory"
)

func New(cfg *config.Manager) (*agent.Agent, error) {
	return NewWithMemory(cfg, nil)
}

// NewWithMemory accepts an optional complete memory runtime. On success the
// returned Agent owns its application resources, including the supplied memory.
func NewWithMemory(cfg *config.Manager, injected *memory.Runtime) (*agent.Agent, error) {
	components, err := bootstrap.Open(cfg, injected)
	if err != nil {
		return nil, err
	}
	inputs := agent.InputsFromComponents(components)
	instance, err := agent.NewWithInputs(agent.Options{TakeOwnership: true}, inputs)
	if err != nil {
		_ = components.Close()
		return nil, err
	}
	if err := components.Start(context.Background(), instance.StartupHooks()); err != nil {
		_ = instance.Close()
		return nil, err
	}
	return instance, nil
}

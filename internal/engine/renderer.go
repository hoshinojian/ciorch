// CloudInitRenderer 把 cirunner 仓 pkg/cloudinit 接到 engine 的 CloudRenderer
// （REDESIGN §5.1：ciorch 经 go module 依赖公开仓，钉 tag 版本）。
package engine

import (
	"github.com/hoshinojian/cirunner/pkg/cloudinit"

	"github.com/hoshinojian/ciorch/internal/config"
)

// CloudInitRenderer 是真渲染器：Agent 版本/哈希来自 config 的 machine_agent。
type CloudInitRenderer struct {
	Agent config.MachineAgentCfg
}

// Render 实现 CloudRenderer。
func (r CloudInitRenderer) Render(o RenderParams) ([]byte, error) {
	return cloudinit.Render(cloudinit.Params{
		RepoURL:           o.RepoURL,
		ServiceRepo:       o.ServiceRepo,
		RunnerName:        o.RunnerName,
		Labels:            o.Labels,
		Token:             o.Token,
		PublicKeys:        o.PublicKeys,
		AgentVersion:      o.AgentVersion,
		AgentSHA256:       o.AgentSHA256,
		ExtraPackages:     o.ExtraPackages,
		RunnersPerDroplet: o.RunnersPerDroplet,
	})
}

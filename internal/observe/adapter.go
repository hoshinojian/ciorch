// vpsctlapi → observe 的适配器（VPSSource 实现）。
package observe

import (
	"context"

	"github.com/hoshinojian/ciorch/internal/vpsctlapi"
)

// VPSAdapter 把 vpsctlapi.Client 适配成 observe.VPSSource。
type VPSAdapter struct {
	Client *vpsctlapi.Client
}

// ListDroplets 按 tag+账号列机并转成 observe.Droplet。
func (a *VPSAdapter) ListDroplets(ctx context.Context, tag, account string) ([]Droplet, error) {
	ds, err := a.Client.ListByTag(ctx, tag, account)
	if err != nil {
		return nil, err
	}
	out := make([]Droplet, 0, len(ds))
	for _, d := range ds {
		out = append(out, Droplet{
			ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt,
			PrivateIP: d.IPv4Private, PublicIP: d.IPv4Public,
			PriceMonthly: d.PriceMonthly,
		})
	}
	return out, nil
}

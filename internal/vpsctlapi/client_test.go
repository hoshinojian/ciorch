package vpsctlapi

import (
	"context"
	"strings"
	"testing"
)

func fakeRun(out string, captured *[]string) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if captured != nil {
			*captured = append(*captured, strings.Join(args, " "))
		}
		return []byte(out), nil
	}
}

// create 不带 -wait；受理即返回、公网 IP 为空不是错误（§3.8/§4 改造点）。
func TestCreateNoWaitEmptyIPOK(t *testing.T) {
	var cmds []string
	c := NewClient("/x/vpsctl", "/x/accounts.json")
	c.Run = fakeRun(`{"batch":"b1","created":[{"account":"team2-ci","id":"123","name":"ci-nms-team2-ci-sgp1-04"}],"errors":[]}`, &cmds)
	d, err := c.Create(context.Background(), CreateOpts{
		Account: "team2-ci", Region: "sgp1", Size: "c2-4vcpu-8gb", Image: "ubuntu-24-04-x64",
		NamePrefix: "ci-nms", StartIndex: 4, Tags: []string{"ciorch", "repo"}, UserDataFile: "/tmp/ci.yaml",
	})
	if err != nil {
		t.Fatalf("空 IP 应成功: %v", err)
	}
	if d.ID != "123" {
		t.Fatalf("解析不对: %+v", d)
	}
	last := cmds[len(cmds)-1]
	if strings.Contains(last, "-wait") {
		t.Fatalf("create 不应带 -wait：%s", last)
	}
	for _, want := range []string{"-only team2-ci", "-start-index 4", "-user-data /tmp/ci.yaml", "-accounts /x/accounts.json"} {
		if !strings.Contains(last, want) {
			t.Fatalf("create 参数缺 %q：%s", want, last)
		}
	}
}

func TestCreateErrorsSurface(t *testing.T) {
	c := NewClient("/x/vpsctl", "")
	c.Run = fakeRun(`{"batch":"b1","created":[],"errors":[{"account":"team2-ci","error":"quota exceeded"}]}`, nil)
	if _, err := c.Create(context.Background(), CreateOpts{Account: "team2-ci", UserDataFile: "/x"}); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("errors[] 应透传: %v", err)
	}
}

// list：ipv4_private / price_monthly 解析；errors[] 非空即失败（§4）。
func TestListParsesNewFieldsAndErrors(t *testing.T) {
	c := NewClient("/x/vpsctl", "")
	c.Run = fakeRun(`{"droplets":[{"account":"a","id":"1","name":"n1","ipv4_public":"1.2.3.4","ipv4_private":"10.0.0.1","price_monthly":141,"tags":["ciorch"]}],"errors":[]}`, nil)
	ds, err := c.ListByTag(context.Background(), "ciorch", "a")
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].IPv4Private != "10.0.0.1" || ds[0].PriceMonthly != 141 {
		t.Fatalf("新字段解析不对: %+v", ds[0])
	}
	c.Run = fakeRun(`{"droplets":[],"errors":[{"account":"a","error":"boom"}]}`, nil)
	if _, err := c.ListByTag(context.Background(), "ciorch", "a"); err == nil {
		t.Fatal("errors[] 非空应报错")
	}
}

func TestDeleteConfirmGate(t *testing.T) {
	var cmds []string
	c := NewClient("/x/vpsctl", "")
	c.Run = fakeRun(`[{"account":"a","id":"1","ok":true}]`, &cmds)
	if err := c.Delete(context.Background(), "a", "1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmds[0], "-ids a/1 -confirm 1") {
		t.Fatalf("精确删除安全闸参数不对：%s", cmds[0])
	}
	c.Run = fakeRun(`[{"account":"a","id":"1","ok":false,"error":"not found"}]`, nil)
	if err := c.Delete(context.Background(), "a", "1"); err == nil {
		t.Fatal("ok=false 应报错")
	}
}

package decide

import (
	"errors"
	"testing"
)

// RecordProviderError：≤10 条滚动保留；cloneState 深拷贝不共享底层
// （RecentProviderErrors 是诊断材料 recent_vpsctl_errors 的来源，§3.10.3）。
func TestRecordProviderErrorRingAndClone(t *testing.T) {
	st := RepoState{}
	for i := 0; i < 12; i++ {
		RecordProviderError(&st, t0, "create", errors.New("boom"))
	}
	if len(st.RecentProviderErrors) != providerErrorKeep {
		t.Fatalf("应截到 %d 条，got %d", providerErrorKeep, len(st.RecentProviderErrors))
	}
	RecordProviderError(&st, t0, "delete", nil) // nil 不记
	if len(st.RecentProviderErrors) != providerErrorKeep {
		t.Fatal("nil 错误不应记录")
	}
	c := cloneState(st)
	c.RecentProviderErrors[0].Err = "mutated"
	if st.RecentProviderErrors[0].Err == "mutated" {
		t.Fatal("clone 应深拷贝，不应共享底层")
	}
}

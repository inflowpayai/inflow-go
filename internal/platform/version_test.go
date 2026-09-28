package platform

import (
	"runtime/debug"
	"testing"
)

func TestSDKUserAgent(t *testing.T) {
	const path = "github.com/inflowpayai/inflow-go"
	for _, tc := range []struct {
		info *debug.BuildInfo
		want string
	}{
		{nil, "devel"},
		{&debug.BuildInfo{}, "devel"},
		{&debug.BuildInfo{Main: debug.Module{Path: path, Version: "(devel)"}}, "devel"},
		{&debug.BuildInfo{Main: debug.Module{Path: path, Version: "v1.2.3"}}, "v1.2.3"},
		{&debug.BuildInfo{Deps: []*debug.Module{{Path: path, Version: "v1.2.3"}}}, "v1.2.3"},
		{&debug.BuildInfo{Deps: []*debug.Module{{Path: path, Version: "v1.2.3", Replace: &debug.Module{Path: "../inflow-go"}}}}, "devel"},
		{&debug.BuildInfo{Deps: []*debug.Module{{Path: path, Version: "v1.2.3", Replace: &debug.Module{Path: path, Version: "v1.2.4"}}}}, "v1.2.4"},
	} {
		if got := sdkUserAgent(tc.info); got != "inflow-go/"+tc.want+" (go)" {
			t.Fatal(got)
		}
	}
}

package platform

import "runtime/debug"

// Read the installed module version rather than maintaining a second release
// version in source. Local replacements identify themselves as development builds.
func sdkUserAgent(info *debug.BuildInfo) string {
	const module = "github.com/inflowpayai/inflow-go"
	if info != nil {
		modules := append([]*debug.Module{&info.Main}, info.Deps...)
		for _, dependency := range modules {
			if dependency.Path == module {
				if dependency.Replace != nil {
					dependency = dependency.Replace
				}
				if dependency.Version != "" && dependency.Version != "(devel)" {
					return "inflow-go/" + dependency.Version + " (go)"
				}
			}
		}
	}
	return "inflow-go/devel (go)"
}

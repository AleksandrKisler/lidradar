// Package buildinfo предоставляет безопасные сведения о сборке для диагностики.
package buildinfo

import "runtime/debug"

// Значения могут быть заменены через -ldflags при выпуске контейнера или версии.
var (
	Version  = "development"
	Revision = "unknown"
)

type Info struct {
	Version  string
	Revision string
	Modified bool
}

// Current дополняет заданные значения сведениями о Git, встроенными Go.
func Current() Info {
	result := Info{Version: Version, Revision: Revision}
	data, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	if result.Version == "development" && data.Main.Version != "" && data.Main.Version != "(devel)" {
		result.Version = data.Main.Version
	}
	for _, setting := range data.Settings {
		switch setting.Key {
		case "vcs.revision":
			if result.Revision == "unknown" && setting.Value != "" {
				result.Revision = setting.Value
			}
		case "vcs.modified":
			result.Modified = setting.Value == "true"
		}
	}
	return result
}

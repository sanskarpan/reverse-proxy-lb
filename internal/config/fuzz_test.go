package config

import (
	"os"
	"testing"
)

// FuzzLoad ensures config parsing/validation never panics on arbitrary YAML input
// (it may return an error, which is fine).
func FuzzLoad(f *testing.F) {
	f.Add("server:\n  port: 8080\nbackends:\n  - url: \"http://x:1\"\n")
	f.Add("")
	f.Add("::: not yaml :::")
	f.Add("server: {port: -1}")
	f.Fuzz(func(t *testing.T, data string) {
		tmp, err := os.CreateTemp("", "fuzz-*.yaml")
		if err != nil {
			t.Skip()
		}
		defer os.Remove(tmp.Name())
		tmp.WriteString(data)
		tmp.Close()
		_, _ = Load(tmp.Name()) // must not panic
	})
}

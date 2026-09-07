package agent

import (
	"reflect"
	"testing"
)

func TestRuntimeEnvironmentRespectsPlatformIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		base      []string
		overrides map[string]string
		want      []string
	}{
		{"windows", []string{"anthropic_api_key=inherited", "Path=old", `=C:=C:\work`, `=D:=D:\work`}, map[string]string{"ANTHROPIC_API_KEY": "selected", "PATH": "new"}, []string{`=C:=C:\work`, `=D:=D:\work`, "ANTHROPIC_API_KEY=selected", "PATH=new"}},
		{"linux", []string{"token=lower", "TOKEN=old"}, map[string]string{"TOKEN": "selected"}, []string{"TOKEN=selected", "token=lower"}},
		{"darwin", []string{"TOKEN=old", "TOKEN=last", "EMPTY=old"}, map[string]string{"TOKEN": "selected", "EMPTY": ""}, []string{"EMPTY=", "TOKEN=selected"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.base...)
			got := mergeRuntimeEnvForOS(test.base, test.overrides, test.name)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("environment = %q; want %q", got, test.want)
			}
			if !reflect.DeepEqual(original, test.base) {
				t.Fatal("mutated inherited environment")
			}
		})
	}
	base := []string{"A=value"}
	if got := mergeRuntimeEnv(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("native inheritance changed: %v", got)
	}
}

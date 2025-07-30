package spec

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/jianlinjiang/trusted-launcher/internal/logging"
)

func TestLaunchSpecUnmarshalJSONHappyCases(t *testing.T) {
	s, err := GetLaunchSpec(context.Background(), logging.SimpleLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal(s.Hardened, false) {
		t.Errorf("LaunchSpec UnmarshalJSON got %+v, want %+v", s.Hardened, false)
	}

	if !cmp.Equal(s.Cmd, []string{"ls", "-l", "/home"}) {
		t.Errorf("LaunchSpec UnmarshalJSON got %+v, want %+v", s.Cmd, []string{"ls", "-l", "/home"})
	}

	if !cmp.Equal(s.ImageRef, "ubuntu:22.04") {
		t.Errorf("LaunchSpec UnmarshalJSON got %+v, want %+v", s.ImageRef, "ubuntu:22.04")
	}

	for _, env := range s.Envs {
		if !cmp.Equal(env.Name, "APPNAME") {
			t.Errorf("LaunchSpec UnmarshalJSON got %+v, want %+v", env.Name, "APPNAME")
		}

		if !cmp.Equal(env.Value, "trusted-launcher") {
			t.Errorf("LaunchSpec UnmarshalJSON got %+v, want %+v", env.Value, "trusted-launcher")
		}
	}
}

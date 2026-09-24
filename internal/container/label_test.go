package container

import "testing"

func TestKeepsSandbox(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  string
		want bool
	}{
		{name: "label set", out: "enabled\n", want: true},
		{name: "label absent", out: "\n", want: false},
		{name: "Go template's missing-key output", out: "<no value>\n", want: false},
		{name: "any other value", out: "disabled\n", want: false},
		{name: "a runtime warning before the value", out: "WARN[0000] some podman warning\nenabled\n", want: true},
		{name: "a runtime warning and no label", out: "WARN[0000] some podman warning\n\n", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := keepsSandbox(tc.out); got != tc.want {
				t.Errorf("keepsSandbox(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

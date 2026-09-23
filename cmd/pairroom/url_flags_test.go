package main

import "testing"

func TestBrowserURLAddressForms(t *testing.T) {
	for _, tc := range []struct{ address, token, want string }{
		{":7332", "", "http://127.0.0.1:7332/"},
		{"0.0.0.0:7332", "", "http://127.0.0.1:7332/"},
		{"[::]:7332", "", "http://127.0.0.1:7332/"},
		{"[::1]:7332", "", "http://[::1]:7332/"},
		{"127.0.0.2:7332", "", "http://127.0.0.2:7332/"},
		// SplitHostPort errors retain the original display address, never a
		// partially parsed host or an accidental wildcard substitution.
		{"localhost", "", "http://localhost/"},
		{"::1", "", "http://::1/"},
		{"127.0.0.1:7332", "a+b &?", "http://127.0.0.1:7332/#token=a%252Bb+%2526%253F"},
	} {
		t.Run(tc.address+tc.token, func(t *testing.T) {
			if got := browserURL(tc.address, tc.token); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoopbackListenAddressBoundaries(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"127.255.255.254:7332", true}, {"[::ffff:127.0.0.1]:7332", true},
		{"[::1]:0", true}, {"127.0.0.1", false}, {"[::1]", false},
		{"[::1%lo]:7332", false}, {"[fe80::1]:7332", false},
		{"127.0.0.1.evil.example:7332", false}, {"localhost:7332", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			if got := isLoopbackListen(tc.address); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreparseFlagValueForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing", nil, ""}, {"separate", []string{"--listen", "127.0.0.1:7332"}, "127.0.0.1:7332"},
		{"equals", []string{"--listen=[::1]:7332"}, "[::1]:7332"},
		{"trailing", []string{"--listen"}, ""}, {"empty", []string{"--listen="}, ""},
		{"other", []string{"--listening=wrong", "--other", "x"}, ""},
		{"first wins", []string{"--listen=first", "--listen=second"}, "first"},
		{"retains equals", []string{"--listen=contains=equals"}, "contains=equals"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preparseValue(tc.args, "--listen"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

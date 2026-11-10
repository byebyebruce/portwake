package proxy

import (
	"net"
	"testing"

	"portwake/internal/config"
)

func TestListenNetwork(t *testing.T) {
	tests := []struct {
		bind string
		want string
	}{
		{"", "tcp4"},
		{"0.0.0.0", "tcp4"},
		{"127.0.0.1", "tcp4"},
		{"::", "tcp6"},
		{"::1", "tcp6"},
	}
	for _, tt := range tests {
		if got := listenNetwork(tt.bind); got != tt.want {
			t.Errorf("listenNetwork(%q) = %q, want %q", tt.bind, got, tt.want)
		}
	}
}

func TestOpenWildcardBindIsIPv4(t *testing.T) {
	s := New(nil, nil)
	e, err := s.open(target{route: config.Route{Listen: 0, Bind: "0.0.0.0", Mode: config.ModeTCP}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.ln.Close()

	addr, ok := e.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address type = %T", e.ln.Addr())
	}
	if addr.IP.To4() == nil {
		t.Fatalf("listener bound to %s, want an IPv4 wildcard", addr)
	}
}

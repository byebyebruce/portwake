package app_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func httpProject(name string, entry, up int, extra string) string {
	return fmt.Sprintf(`projects:
  - name: %s
    dir: /tmp
%s    routes:
      - listen: %d
        bind: 127.0.0.1
        mode: http
        upstream: 127.0.0.1:%d
`, name, extra, entry, up)
}

func TestHTTPRequestStartsProjectAndIsLogged(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, httpProject("site", entry, up, ""))

	resp, err := http.Get(fmt.Sprintf("http://%s/some/path", addr(entry)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello site /some/path" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	eventually(t, "http_request event", func() bool {
		return h.hasEvent("project=site&type=http_request", func(e eventRow) bool {
			return e.Method == "GET" && e.Path == "/some/path" && e.Status == 200 && e.BytesOut == int64(len(body)) && e.Port == entry
		})
	})
}

func TestHTTPStartFailureReturns503(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, httpProject("site", entry, up, ""))
	h.fake.SetUpError("site", errors.New("boom"))

	resp, err := http.Get(fmt.Sprintf("http://%s/", addr(entry)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "boom") {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestHTTPUpgradeIsPassedThrough(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	start(t, httpProject("site", entry, up, ""))

	c, err := net.Dial("tcp", addr(entry))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	io.WriteString(c, "frame\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "frame\n" {
		t.Fatalf("echo over upgraded conn = %q %v", line, err)
	}
}

func TestHTTPReadinessIgnoresPortProxyThatClosesConnections(t *testing.T) {
	entry, up := freePort(t), freePort(t)
	h := start(t, httpProject("site", entry, up, ""))
	h.fake.SetWarmup("site", 300*time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://%s/", addr(entry)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first request status = %d", resp.StatusCode)
	}
}

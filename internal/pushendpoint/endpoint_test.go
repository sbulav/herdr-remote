package pushendpoint

import "testing"

func TestParseAcceptsOnlyDNSHTTPSPort443Endpoints(t *testing.T) {
	for _, endpoint := range []string{"https://push.example/path", "https://push.example:443/path?token=opaque"} {
		if _, err := Parse(endpoint); err != nil {
			t.Fatalf("valid endpoint %q rejected: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://push.example/path",
		"https://user@push.example/path",
		"https://127.0.0.1/path",
		"https://[::1]/path",
		"https://[fe80::1%25eth0]/path",
		"https://999.999.999.999/path",
		"https://bad_host.example/path",
		"https://push.example:8443/path",
		"https://push.example/path#fragment",
	} {
		if _, err := Parse(endpoint); err == nil {
			t.Fatalf("unsafe endpoint %q accepted", endpoint)
		}
	}
}

package pushendpoint

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

func Parse(raw string) (*url.URL, error) {
	if len(raw) < len("https://a") || len(raw) > 2048 {
		return nil, errors.New("invalid push endpoint length")
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Opaque != "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.New("push endpoint must be an absolute HTTPS URL without userinfo or fragment")
	}
	hostname := endpoint.Hostname()
	if hostname == "" || strings.ContainsAny(hostname, ":%") || net.ParseIP(hostname) != nil || !validDNSHostname(hostname) {
		return nil, errors.New("push endpoint must use a DNS hostname")
	}
	if port := endpoint.Port(); port != "" && port != "443" {
		return nil, errors.New("push endpoint must use HTTPS port 443")
	}
	return endpoint, nil
}

func validDNSHostname(hostname string) bool {
	hostname = strings.TrimSuffix(hostname, ".")
	if len(hostname) < 1 || len(hostname) > 253 {
		return false
	}
	hasLetter := false
	for _, label := range strings.Split(hostname, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' {
				hasLetter = true
				continue
			}
			if character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
	}
	return hasLetter
}

//go:build linux

package reviewer

import (
	"strings"
	"testing"
)

// Captured from /proc/<inert-peer>/net/ipv6_route on mini, October 21,
// 2026. The actual namespace fixture checks the current kernel separately.
const gitPrivateIPv6RouteCapture = "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n" +
	"00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000002 00000000 80200001       lo\n" +
	"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n"

func TestGitProcessNetworkClassifiesActualLoopbackAndRejectsEgressForms(t *testing.T) {
	var observed gitProcessNetwork
	if gitObserveIPv6Routes([]byte(gitPrivateIPv6RouteCapture), &observed) != nil || observed.IPv6Routes != 0 || observed.IPv6Rejects != 2 || observed.IPv6Loopback != 1 {
		t.Fatal("actual captured namespace routes", observed)
	}
	for _, kind := range []string{"reject-flag", "interface", "gateway", "destination", "loopback-prefix", "source-prefix", "unknown-flag", "truncated", "invalid-hex", "invalid-prefix", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			raw := gitPrivateIPv6RouteCapture
			switch kind {
			case "reject-flag":
				raw = strings.Replace(raw, "00200200", "00200001", 1)
			case "interface":
				raw = strings.Replace(raw, "lo", "eth0", 1)
			case "gateway":
				raw = strings.Replace(raw, "00 00000000000000000000000000000000 ffffffff", "00 20010db8000000000000000000000001 ffffffff", 1)
			case "destination":
				raw = strings.Replace(raw, "00000000000000000000000000000001 80", "20010db8000000000000000000000001 80", 1)
			case "loopback-prefix":
				raw = strings.Replace(raw, "00000000000000000000000000000001 80", "00000000000000000000000000000001 40", 1)
			case "source-prefix":
				raw = strings.Replace(raw, "00 00000000000000000000000000000000 00", "00 00000000000000000000000000000000 40", 1)
			case "unknown-flag":
				raw = strings.Replace(raw, "80200001", "80200003", 1)
			case "truncated":
				raw = raw[:len(raw)-5]
			case "invalid-hex":
				raw = "g" + raw[1:]
			case "invalid-prefix":
				raw = strings.Replace(raw, " 80 ", " ff ", 1)
			case "oversize":
				raw = strings.Repeat(raw, 256)
			}
			if raw == gitPrivateIPv6RouteCapture {
				t.Fatal("fixture mutation did not apply")
			}
			var got gitProcessNetwork
			err := gitObserveIPv6Routes([]byte(raw), &got)
			if err == nil && got.IPv6Routes == 0 {
				t.Fatal("changed route was silently accepted as non-egress", kind, got)
			}
		})
	}
}

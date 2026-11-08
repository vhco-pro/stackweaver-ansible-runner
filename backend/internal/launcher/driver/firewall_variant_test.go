// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The firewall writer runs `iptables` from whatever image it is given, and an image's `iptables`
// may be the legacy variant on a host whose Docker writes its chains with iptables-nft (or the
// other way round), where DOCKER-USER does not exist for it. The script uses the variant whose
// tables hold Docker's DOCKER-USER chain, and the plain name when neither does.
func TestIPTablesVariantPrelude(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tc := range []struct{ name, withChain, want string }{
		{"nft holds DOCKER-USER", "nft", "iptables-nft"},
		{"legacy holds DOCKER-USER", "legacy", "iptables-legacy"},
		{"neither holds it", "", "iptables"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			for _, v := range []string{"nft", "legacy"} {
				for _, tool := range []string{"iptables", "ip6tables"} {
					exit := "1"
					if v == tc.withChain {
						exit = "0"
					}
					// -S DOCKER-USER answers only for the variant holding the chain; any other call
					// prints the variant's name.
					body := "#!/bin/sh\ncase \"$*\" in \"-S DOCKER-USER\") exit " + exit + ";; esac\necho " + tool + "-" + v + "\n"
					if err := os.WriteFile(filepath.Join(bin, tool+"-"+v), []byte(body), 0o755); err != nil { //nolint:gosec // G306: a test stub that must be executable
						t.Fatal(err)
					}
				}
			}
			for _, tool := range []string{"iptables", "ip6tables"} {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho "+tool+"\n"), 0o755); err != nil { //nolint:gosec // G306: a test stub that must be executable
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), sh, "-c", iptablesVariantPrelude+"iptables -A X\nip6tables -A X\n") //nolint:gosec // G204: the test's own script
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("prelude: %v\n%s", err, out)
			}
			want6 := strings.Replace(tc.want, "iptables", "ip6tables", 1)
			if got := strings.Fields(string(out)); len(got) != 2 || got[0] != tc.want || got[1] != want6 {
				t.Errorf("the script ran %v, want %s and %s", got, tc.want, want6)
			}
		})
	}
}

package cmd

import (
	"strings"
	"testing"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

func TestPrintDetails(t *testing.T) {
	plain := func(s string) []meshcentral.DetailPart { return []meshcentral.DetailPart{{Text: s}} }
	sections := []meshcentral.DetailSection{
		{Title: "Security", Groups: []meshcentral.DetailGroup{{Rows: []meshcentral.DetailRow{
			{Label: "Linux Security", Value: []meshcentral.DetailPart{{Text: "AV - "}, {Text: "OK", State: meshcentral.Good},
				{Text: ", Firewall - "}, {Text: "BAD", State: meshcentral.Bad}}},
			{Label: "Pending Reboot", Value: []meshcentral.DetailPart{{Text: "Kernel", State: meshcentral.Attention}}},
		}}}},
		{Title: "Networking", Groups: []meshcentral.DetailGroup{{Title: "eth0", Rows: []meshcentral.DetailRow{
			{Label: "IPv4 Layer", Value: plain("IP: 192.0.2.10")},
		}}}},
	}
	var b strings.Builder
	printDetails(&b, sections, false)
	want := `Security
  Linux Security  AV - OK, Firewall - BAD
  Pending Reboot  Kernel

Networking
  eth0
    IPv4 Layer    IP: 192.0.2.10
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}

	b.Reset()
	printDetails(&b, sections, true)
	for _, s := range []string{"AV - \x1b[32mOK\x1b[0m, Firewall - \x1b[31mBAD\x1b[0m", "\x1b[33mKernel\x1b[0m", "IP: 192.0.2.10\n"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("colored output lacks %q:\n%s", s, b.String())
		}
	}
}

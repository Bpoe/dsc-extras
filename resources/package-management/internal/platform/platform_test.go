package platform

import (
	"strings"
	"testing"
)

func TestOSDetection(t *testing.T) {
	for _, test := range []struct{ input, backend string }{
		{"ID=debian\nVERSION_ID=\"12\"", "apt"},
		{"ID='ubuntu'\nID_LIKE=debian", "apt"},
		{"ID=rhel\nVERSION_ID=\"8.10\"", "dnf"},
		{"ID=rocky\nVERSION_ID=\"9.7\"\nID_LIKE=\"rhel centos fedora\"", "dnf"},
		{"ID=almalinux\nVERSION_ID=10", "dnf"},
		{"ID=rhel\nVERSION_ID=10.1", "dnf"},
		{"ID=fedora\nVERSION_ID=43\nID_LIKE=rhel", ""},
		{"ID=unknown\nID_LIKE=debian", ""},
		{"ID=rhel\nVERSION_ID=7", ""},
		{"ID=rocky\nVERSION_ID=11", ""},
		{"ID=rhel", ""},
		{"ID=\"rhel\nVERSION_ID=9", ""},
		{"ID=debian\nID=rhel\nVERSION_ID=9", ""},
		{"ID=$(echo debian)", ""},
		{"", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			actual, err := Parse(strings.NewReader(test.input))
			if actual != test.backend || (err != nil) != (test.backend == "") {
				t.Fatalf("Parse = %q, %v; want %q", actual, err, test.backend)
			}
		})
	}
}

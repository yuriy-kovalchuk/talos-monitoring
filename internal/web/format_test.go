package web_test

import (
	"testing"

	"github.com/yuriy-kovalchuk/talos-monitoring/internal/web"
)

func TestShortCPUModel(t *testing.T) {
	// Names observed on the reference cluster, plus pass-through cases.
	for _, tc := range []struct{ in, want string }{
		{"AMD Ryzen 7 6800H with Radeon Graphics", "Ryzen 7 6800H"},
		{"AMD Ryzen 7 255 w/ Radeon 780M Graphics", "Ryzen 7 255"},
		{"Intel(R) Core(TM) i5-6400T CPU @ 2.20GHz", "Core i5-6400T"},
		{"Intel(R) Core(TM) i5-6500T CPU @ 2.50GHz", "Core i5-6500T"},
		{"Intel(R) Xeon(R) Silver 4310 CPU @ 2.10GHz", "Xeon Silver 4310"},
		{"Neoverse-N1", "Neoverse-N1"},
		{"", ""},
	} {
		if got := web.ShortCPUModelForTest(tc.in); got != tc.want {
			t.Errorf("shortCPUModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDMIValueBlanksPlaceholders(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Unknown", ""},
		{"unknown", ""},
		{"Not Specified", ""},
		{"To Be Filled By O.E.M.", ""},
		{"Default string", ""},
		{"None", ""},
		{"Unknown - [0x9B05]", ""}, // unresolved JEDEC vendor id, not a name
		{"Unknown - [0x0000]", ""},
		{"  Unknown  ", ""},
		{"", ""},
		// Real values must survive untouched.
		{"Samsung", "Samsung"},
		{"B68005KF91685", "B68005KF91685"},
		{"AMD Ryzen 7 6800H", "AMD Ryzen 7 6800H"},
		{"Unknown Vendor Ltd", "Unknown Vendor Ltd"},
	} {
		if got := web.DMIValueForTest(tc.in); got != tc.want {
			t.Errorf("dmiValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

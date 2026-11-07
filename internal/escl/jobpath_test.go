package escl

import "testing"

func TestJobPath(t *testing.T) {
	ok := map[string]string{
		"http://printer.local:80/eSCL/ScanJobs/abc":   "/eSCL/ScanJobs/abc",
		"https://192.168.1.60/eSCL/ScanJobs/12/":      "/eSCL/ScanJobs/12",
		"/eSCL/ScanJobs/f0e1d2c3-0000-1111-2222-3333": "/eSCL/ScanJobs/f0e1d2c3-0000-1111-2222-3333",
	}
	for in, want := range ok {
		if got, err := jobPath(in); err != nil || got != want {
			t.Errorf("jobPath(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/eSCL/ScanJobs/", "/other/1", "/eSCL/ScanJobs/a/../../x", "/eSCL/ScanJobs/a/b"} {
		if _, err := jobPath(bad); err == nil {
			t.Errorf("jobPath(%q) accepted", bad)
		}
	}
}

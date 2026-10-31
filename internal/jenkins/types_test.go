package jenkins

import "testing"

func TestIsKnownResult(t *testing.T) {
	for _, r := range []string{ResultSuccess, ResultFailure, ResultAborted, ResultUnstable} {
		if !IsKnownResult(r) {
			t.Errorf("%q 应为已知 result", r)
		}
	}
	for _, r := range []string{"", "NOT_BUILT", "queued"} {
		if IsKnownResult(r) {
			t.Errorf("%q 应为未知 result", r)
		}
	}
}

func TestIsFolderClass(t *testing.T) {
	cases := map[string]bool{
		"com.cloudbees.hudson.plugins.folder.Folder":     true,
		"hudson.model.Folder":                            true,
		"jenkins.branch.OrganizationFolder":              true,
		"hudson.model.FreeStyleProject":                  false,
		"org.jenkinsci.plugins.workflow.job.WorkflowJob": false,
		"": false,
	}
	for class, want := range cases {
		if got := IsFolderClass(class); got != want {
			t.Errorf("IsFolderClass(%q) = %v, want %v", class, got, want)
		}
	}
}

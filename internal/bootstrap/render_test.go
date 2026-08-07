package bootstrap

import (
	"strings"
	"testing"
)

func testValues() Values {
	return Values{
		CellName:        "inference-0",
		PoolName:        "inference",
		NodeLabels:      "cells.kubeswift.io/cell=inference-0,gpu=on",
		NodeIPInterface: "node",
		ExpectedGPUs:    1,
	}
}

func TestRenderSubstitutes(t *testing.T) {
	tmpl := []byte(`#cloud-config
hostname: {{ cellName }}
runcmd:
  - k0s install worker --token-file /root/token \
      --kubelet-extra-args="--node-labels={{ nodeLabels }} --node-ip=$(ip -4 -o addr show dev {{nodeIPInterface}} | awk '{split($4,a,"/"); print a[1]}')"
  - test "$(nvidia-smi -L | grep -c '^GPU')" = "{{ expectedGPUs }}" || echo "preflight: GPU missing"
  - echo pool={{poolName}}
`)
	got, err := Render(tmpl, testValues())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)

	for _, want := range []string{
		"hostname: inference-0",
		"--node-labels=cells.kubeswift.io/cell=inference-0,gpu=on",
		"dev node |",
		`= "1"`,
		"pool=inference",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "{{") {
		t.Errorf("unsubstituted token left behind:\n%s", s)
	}
}

func TestRenderRejectsUnknownTokens(t *testing.T) {
	// A typo must be an error. Leaving it in place would produce a node that
	// joins with a broken label and no explanation.
	_, err := Render([]byte("hostname: {{ cellname }}\n"), testValues())
	if err == nil {
		t.Fatal("accepted an unknown token")
	}
	if !strings.Contains(err.Error(), "cellname") {
		t.Errorf("error does not name the offending token: %v", err)
	}
	// And it should tell the operator what IS supported.
	if !strings.Contains(err.Error(), "cellName") {
		t.Errorf("error does not list the supported keys: %v", err)
	}
}

func TestRenderRejectsAnEmptyTemplate(t *testing.T) {
	if _, err := Render(nil, testValues()); err == nil {
		t.Error("accepted an empty template: a cell would boot with no cloud-init and never join")
	}
}

func TestRenderIsStable(t *testing.T) {
	// An unstable render would rewrite the per-cell Secret on every reconcile.
	tmpl := []byte("labels: {{ nodeLabels }}\n")
	a, err := Render(tmpl, testValues())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(tmpl, testValues())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("render is not deterministic")
	}
}

func TestRenderLeavesUnrelatedBracesAlone(t *testing.T) {
	// Shell and cloud-init are full of braces; only {{ token }} is ours.
	tmpl := []byte(`runcmd:
  - awk '{print $1}' /etc/hosts
  - echo ${HOME} && echo {}
`)
	got, err := Render(tmpl, testValues())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != string(tmpl) {
		t.Errorf("modified non-token braces:\n%s", got)
	}
}

func TestNodeLabelArgIsSorted(t *testing.T) {
	got := NodeLabelArg(map[string]string{"z": "1", "a": "2", "m": "3"})
	if got != "a=2,m=3,z=1" {
		t.Errorf("NodeLabelArg = %q, want sorted", got)
	}
	if NodeLabelArg(nil) != "" {
		t.Errorf("NodeLabelArg(nil) = %q", NodeLabelArg(nil))
	}
}

func TestTemplateWithNoTokensIsUsedVerbatim(t *testing.T) {
	// The common case: a cloud-init that needs no per-cell values at all.
	tmpl := []byte("#cloud-config\nruncmd: [[ k0s, install, worker ]]\n")
	got, err := Render(tmpl, testValues())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != string(tmpl) {
		t.Error("a token-free template was modified")
	}
}

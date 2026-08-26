package triple

import (
	"slices"
	"testing"
)

// A proven host must also be a proven target: building canadian toolchains
// hosted on H requires cross_H, which is a target build. Promoting a host
// without its target is the way that dependency goes missing.
func TestProvenHostsAreProvenTargets(t *testing.T) {
	for _, h := range ProvenHosts {
		if !slices.Contains(ProvenTargets, h) {
			t.Errorf("%s is a proven host but not a proven target", h)
		}
	}
}

func TestProvenAreKnown(t *testing.T) {
	for _, list := range [][]string{ProvenHosts, ProvenTargets} {
		for _, p := range list {
			if !slices.Contains(Known, p) {
				t.Errorf("proven triple %s is not in Known", p)
			}
		}
	}
}

package core

import (
	"context"
	"strings"
	"testing"
)

// A key must move when anything that could change the output moves, and must
// not move for anything that cannot — notably Go map iteration order.
func TestKeySensitivity(t *testing.T) {
	clearKeyCache()

	base := newJob("sensitive")
	base.inputs = map[string]string{"flag": "--enable-foo", "sha": "abc"}
	k0 := freshKey(t, base)

	t.Run("input value", func(t *testing.T) {
		j := newJob("sensitive")
		j.inputs = map[string]string{"flag": "--enable-bar", "sha": "abc"}
		if freshKey(t, j) == k0 {
			t.Fatal("changing an input value did not change the key")
		}
	})

	t.Run("insertion order", func(t *testing.T) {
		j := newJob("sensitive")
		j.inputs = map[string]string{}
		j.inputs["sha"] = "abc"
		j.inputs["flag"] = "--enable-foo"
		if got := freshKey(t, j); got != k0 {
			t.Fatalf("insertion order changed the key: %s != %s", got, k0)
		}
	})

	t.Run("name", func(t *testing.T) {
		j := newJob("sensitive")
		j.inputs = base.inputs
		j.name = "other"
		if freshKey(t, j) == k0 {
			t.Fatal("changing the recipe name did not change the key")
		}
	})

	t.Run("dep key", func(t *testing.T) {
		dep1, dep2 := newJob("dep"), newJob("dep")
		dep2.inputs = map[string]string{"v": "2"}

		p1, p2 := newJob("parent"), newJob("parent")
		p1.deps = []Job{dep1}
		p2.deps = []Job{dep2}
		if freshKey(t, p1) == freshKey(t, p2) {
			t.Fatal("changing a dependency's inputs did not change the parent key")
		}
	})

	t.Run("added dep", func(t *testing.T) {
		p1 := newJob("parent")
		p2 := newJob("parent")
		p2.deps = []Job{newJob("dep")}
		if freshKey(t, p1) == freshKey(t, p2) {
			t.Fatal("adding a dependency did not change the parent key")
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			j := newJob("sensitive")
			j.inputs = map[string]string{"flag": "--enable-foo", "sha": "abc"}
			if got := freshKey(t, j); got != k0 {
				t.Fatalf("key not deterministic on iteration %d: %s != %s", i, got, k0)
			}
		}
	})
}

// A diamond must not double-count, and the shared node must have one key.
func TestKeyDiamond(t *testing.T) {
	clearKeyCache()
	leaf := newJob("leaf")
	a, b := newJob("a"), newJob("b")
	a.deps = []Job{leaf}
	b.deps = []Job{leaf}
	top := newJob("top")
	top.deps = []Job{a, b}

	nodes, err := resolve([]Job{top})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("expected 4 deduped nodes, got %d", len(nodes))
	}
	pos := map[string]int{}
	for i, n := range nodes {
		pos[n.slug] = i
	}
	for _, pair := range [][2]string{{"leaf", "a"}, {"leaf", "b"}, {"a", "top"}, {"b", "top"}} {
		if pos[pair[0]] > pos[pair[1]] {
			t.Fatalf("%s must come before %s in topological order", pair[0], pair[1])
		}
	}
}

func TestCycleDetection(t *testing.T) {
	clearKeyCache()
	a, b := newJob("cyc-a"), newJob("cyc-b")
	a.deps = []Job{b}
	b.deps = []Job{a}

	_, err := resolve([]Job{a})
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "cyc-a") {
		t.Fatalf("cycle error should name the cycle, got: %v", err)
	}

	e := testEnv(t)
	if _, err := Plan(e, []Job{a}); err == nil {
		t.Fatal("Plan should refuse a cyclic DAG")
	}
	if err := Run(context.Background(), e, []Job{a}); err == nil {
		t.Fatal("Run should refuse a cyclic DAG")
	}
}

// Two unrelated recipes claiming one slug would silently share an artifact
// directory; that must be caught loudly.
func TestSlugCollision(t *testing.T) {
	clearKeyCache()
	a := newJob("dup")
	b := newJob("dup")
	b.inputs = map[string]string{"v": "different"}
	top := newJob("top")
	top.deps = []Job{a, b}

	_, err := resolve([]Job{top})
	if err == nil || !strings.Contains(err.Error(), "share slug") {
		t.Fatalf("expected a slug collision error, got: %v", err)
	}
}

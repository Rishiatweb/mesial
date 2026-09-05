package falkorstore

import (
	"context"
	"testing"
)

// TestAddEntity_ParentNameDisambiguatesSameNamedMethods is the regression
// test for the collision a naive "just drop src_start/src_end from the MERGE
// key" fix would introduce: two different classes in one file, each
// defining a method with the same name, must produce two distinct nodes,
// not one collapsed node.
func TestAddEntity_ParentNameDisambiguatesSameNamedMethods(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	idA, err := store.AddEntity(ctx, "Method", "foo", "", "/repo/src/file.ts", "ClassA", 10, 12, "")
	if err != nil {
		t.Fatalf("AddEntity for ClassA.foo: %v", err)
	}
	idB, err := store.AddEntity(ctx, "Method", "foo", "", "/repo/src/file.ts", "ClassB", 20, 22, "")
	if err != nil {
		t.Fatalf("AddEntity for ClassB.foo: %v", err)
	}

	if idA == idB {
		t.Fatalf("expected two distinct nodes for ClassA.foo and ClassB.foo (same name, different parent), both got ID %d — parent_name is not disambiguating", idA)
	}
}

// TestAddEntity_LineShiftDoesNotChangeIdentity is the actual line-shift-
// fragility fix: re-calling AddEntity for the same entity (same label, name,
// path, parent_name) with different src_start/src_end — simulating an
// unrelated earlier edit in the file shifting every subsequent line number —
// must return the SAME node ID with the position updated in place, not
// create a second node.
func TestAddEntity_LineShiftDoesNotChangeIdentity(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	firstID, err := store.AddEntity(ctx, "Function", "doThing", "", "/repo/src/file.ts", "", 10, 15, "")
	if err != nil {
		t.Fatalf("AddEntity (first call): %v", err)
	}

	secondID, err := store.AddEntity(ctx, "Function", "doThing", "", "/repo/src/file.ts", "", 30, 35, "")
	if err != nil {
		t.Fatalf("AddEntity (second call, shifted lines): %v", err)
	}

	if firstID != secondID {
		t.Fatalf("expected same node ID across a line-shift re-add (identity should not depend on src_start/src_end), got %d then %d", firstID, secondID)
	}

	// Confirm the position was actually updated in place, not left stale.
	res, err := store.graph.Query(
		"MATCH (e) WHERE ID(e) = $id RETURN e.src_start, e.src_end",
		map[string]interface{}{"id": firstID}, nil,
	)
	if err != nil {
		t.Fatalf("querying updated position: %v", err)
	}
	if !res.Next() {
		t.Fatal("expected a row back for the entity")
	}
	r := res.Record()
	startVal, _ := r.GetByIndex(0)
	endVal, _ := r.GetByIndex(1)
	if toInt64(startVal) != 30 || toInt64(endVal) != 35 {
		t.Errorf("expected src_start/src_end updated to 30/35, got %v/%v", startVal, endVal)
	}
}

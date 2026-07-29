package query

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Loxstomper/software-factory/internal/controlroom/dag"
	"github.com/Loxstomper/software-factory/internal/core"
)

func TestDAGMapsDependsOnToEdges(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-2", Title: "child", Status: "open", DependsOn: []string{"h-1"}},
		{ID: "h-1", Title: "root", Status: "closed"},
		{ID: "h-3", Title: "leaf", Status: "open", DependsOn: []string{"h-1", "h-2"}},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	g, err := r.DAG(context.Background())
	if err != nil {
		t.Fatalf("DAG: %v", err)
	}

	wantNodes := []dag.Node{
		{ID: "h-1", Title: "root", Status: "closed"},
		{ID: "h-2", Title: "child", Status: "open"},
		{ID: "h-3", Title: "leaf", Status: "open"},
	}
	if !reflect.DeepEqual(g.Nodes, wantNodes) {
		t.Errorf("nodes =\n%+v\nwant\n%+v", g.Nodes, wantNodes)
	}

	// Edges run blocker->dependent and are sorted by (From,To).
	wantEdges := []dag.Edge{
		{From: "h-1", To: "h-2"},
		{From: "h-1", To: "h-3"},
		{From: "h-2", To: "h-3"},
	}
	if !reflect.DeepEqual(g.Edges, wantEdges) {
		t.Errorf("edges =\n%+v\nwant\n%+v", g.Edges, wantEdges)
	}
}

func TestDAGDropsDanglingEdges(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Status: "open", DependsOn: []string{"ghost", ""}},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	g, err := r.DAG(context.Background())
	if err != nil {
		t.Fatalf("DAG: %v", err)
	}
	if len(g.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(g.Nodes))
	}
	if len(g.Edges) != 0 {
		t.Errorf("edges = %+v, want none (blocker outside the issue set is dropped)", g.Edges)
	}
}

func TestDAGDeterministicOrder(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-3", Status: "open"},
		{ID: "h-1", Status: "open"},
		{ID: "h-2", Status: "open", DependsOn: []string{"h-3", "h-1"}},
	}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{})

	first, err := r.DAG(context.Background())
	if err != nil {
		t.Fatalf("DAG: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.DAG(context.Background())
		if err != nil {
			t.Fatalf("DAG: %v", err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("DAG not deterministic:\n%+v\nvs\n%+v", again, first)
		}
	}
}

func TestDAGListAllError(t *testing.T) {
	r := NewReader(&fakeIssues{allErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.DAG(context.Background()); err == nil {
		t.Fatal("DAG swallowed a ListAll error")
	}
}

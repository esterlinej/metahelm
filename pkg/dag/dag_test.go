package dag

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type testObj struct {
	name string
	deps []string
}

func (to *testObj) Name() string {
	return to.name
}
func (to *testObj) String() string {
	return to.name
}
func (to *testObj) Dependencies() []string {
	return to.deps
}

var testobjs = []GraphObject{
	&testObj{
		name: "a",
		deps: []string{"b", "c"},
	},
	&testObj{
		name: "b",
		deps: []string{"d", "e"},
	},
	&testObj{
		name: "c",
		deps: []string{"f", "g"},
	},
	&testObj{
		name: "d",
	},
	&testObj{
		name: "e",
	},
	&testObj{
		name: "f",
	},
	&testObj{
		name: "g",
		deps: []string{"h"},
	},
	&testObj{
		name: "h",
		deps: []string{"i"},
	},
	&testObj{
		name: "i",
	},
}

var testobjsNoRoot = []GraphObject{
	&testObj{
		name: "x",
		deps: []string{"y", "z"},
	},
	&testObj{
		name: "y",
	},
	&testObj{
		name: "z",
	},
	&testObj{
		name: "a",
		deps: []string{"b", "c"},
	},
	&testObj{
		name: "b",
		deps: []string{"d", "e"},
	},
	&testObj{
		name: "c",
		deps: []string{"f", "g"},
	},
	&testObj{
		name: "d",
	},
	&testObj{
		name: "e",
	},
	&testObj{
		name: "f",
	},
	&testObj{
		name: "g",
		deps: []string{"h"},
	},
	&testObj{
		name: "h",
		deps: []string{"i"},
	},
	&testObj{
		name: "i",
	},
}

func TestDAGBuild(t *testing.T) {
	og := ObjectGraph{}
	if err := og.Build(testobjs); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	root, lvls, err := og.Info()
	if err != nil {
		t.Fatalf("info failed: %v", err)
	}
	t.Logf("root: %v\n", root)
	for i, l := range lvls {
		t.Logf("LEVEL %v: %v\n", i, l)
	}
}

func TestDAGBuildMissingName(t *testing.T) {
	objs := []GraphObject{
		&testObj{
			name: "a",
			deps: []string{"b"},
		},
		&testObj{
			name: "b",
		},
		&testObj{},
	}
	og := ObjectGraph{}
	err := og.Build(objs)
	if err == nil {
		t.Fatalf("should have failed")
	}
	t.Logf(err.Error())
}

func TestDAGUnknownDependency(t *testing.T) {
	objs := []GraphObject{
		&testObj{
			name: "a",
			deps: []string{"b", "c"},
		},
		&testObj{
			name: "b",
		},
		&testObj{
			name: "c",
			deps: []string{"invalid"},
		},
	}
	og := ObjectGraph{}
	err := og.Build(objs)
	if err == nil {
		t.Fatalf("should have failed")
	}
	t.Logf(err.Error())
}

func TestDAGSelfReference(t *testing.T) {
	objs := []GraphObject{
		&testObj{
			name: "a",
			deps: []string{"b", "c"},
		},
		&testObj{
			name: "b",
		},
		&testObj{
			name: "c",
			deps: []string{"c"},
		},
	}
	og := ObjectGraph{}
	err := og.Build(objs)
	if err == nil {
		t.Fatalf("should have failed")
	}
	t.Logf(err.Error())
}

func TestDAGLevels(t *testing.T) {
	objs := []GraphObject{
		&testObj{
			name: "a",
			deps: []string{"b", "c"},
		},
		&testObj{
			name: "b",
			deps: []string{"c"},
		},
		&testObj{
			name: "c",
		},
	}
	og := ObjectGraph{}
	err := og.Build(objs)
	if err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	root, lvls, err := og.Info()
	if err != nil {
		t.Fatalf("info failed: %v", err)
	}
	t.Logf("root: %v\n", root)
	for i, l := range lvls {
		t.Logf("LEVEL %v: %v\n", i, l)
	}
}

func TestDAGBuildNoRoot(t *testing.T) {
	og := ObjectGraph{}
	if err := og.Build(testobjsNoRoot); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	root, lvls, err := og.Info()
	if err != nil {
		t.Fatalf("info failed: %v", err)
	}
	t.Logf("root: %v\n", root)
	for i, l := range lvls {
		t.Logf("LEVEL %v: %v\n", i, l)
	}
}

func TestDAGWithCycle(t *testing.T) {
	objs := []GraphObject{
		&testObj{
			name: "a",
			deps: []string{"b", "c"},
		},
		&testObj{
			name: "b",
		},
		&testObj{
			name: "c",
			deps: []string{"a"},
		},
	}
	og := ObjectGraph{}
	err := og.Build(objs)
	if err == nil {
		t.Fatalf("should have failed with detected cycle")
	}
	t.Logf(err.Error())
}

type lockingMap struct {
	sync.Mutex
	m map[string]time.Time
}

func TestDAGWalk(t *testing.T) {
	og := ObjectGraph{}
	if err := og.Build(testobjs); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	m := lockingMap{m: make(map[string]time.Time)}
	af := func(gobj GraphObject) error {
		time.Sleep(1 * time.Millisecond)
		m.Lock()
		m.m[gobj.Name()] = time.Now().UTC()
		m.Unlock()
		return nil
	}
	if err := og.Walk(context.Background(), af); err != nil {
		t.Fatalf("error in Walk: %v", err)
	}
	if len(m.m) != len(testobjs) {
		t.Fatalf("bad results length: %v (wanted %v)", len(m.m), len(testobjs))
	}
	tsa, ok := m.m["a"]
	if !ok {
		t.Fatalf("a missing")
	}
	tsc, ok := m.m["c"]
	if !ok {
		t.Fatalf("c missing")
	}
	if !tsc.Before(tsa) {
		t.Fatalf("c not before a")
	}
	tsb, ok := m.m["b"]
	if !ok {
		t.Fatalf("b missing")
	}
	if !tsb.Before(tsa) {
		t.Fatalf("b not before a")
	}
	tsg, ok := m.m["g"]
	if !ok {
		t.Fatalf("g missing")
	}
	if !tsg.Before(tsc) {
		t.Fatalf("g not before c")
	}
	tsd, ok := m.m["d"]
	if !ok {
		t.Fatalf("d missing")
	}
	if !tsd.Before(tsb) {
		t.Fatalf("d not before b")
	}
	tsh, ok := m.m["h"]
	if !ok {
		t.Fatalf("h missing")
	}
	if !tsh.Before(tsg) {
		t.Fatalf("h not before g")
	}
	tsi, ok := m.m["i"]
	if !ok {
		t.Fatalf("i missing")
	}
	if !tsi.Before(tsh) {
		t.Fatalf("i not before h")
	}
	og.Build(testobjsNoRoot)
	m.m = map[string]time.Time{}
	if err := og.Walk(context.Background(), af); err != nil {
		t.Fatalf("no root: error in Walk: %v", err)
	}
	if len(m.m) != len(testobjsNoRoot) {
		t.Fatalf("no root: bad results length: %v (wanted %v)", len(m.m), len(testobjsNoRoot))
	}
}

func TestDAGDot(t *testing.T) {
	if os.Getenv("DISPLAY_GRAPHS") == "" {
		return
	}
	og := ObjectGraph{}
	if err := og.Build(testobjs); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	b, err := og.Dot("testcharts")
	if err != nil {
		t.Fatalf("dot failed: %v", err)
	}
	if err = os.WriteFile("./testcharts.dot", b, os.ModePerm); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err = og.Build(testobjsNoRoot); err != nil {
		t.Fatalf("should have succeeded: %v", err)
	}
	b, err = og.Dot("testchartsNoRoot")
	if err != nil {
		t.Fatalf("dot failed: %v", err)
	}
	if err := os.WriteFile("./testchartsNoRoot.dot", b, os.ModePerm); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	exec.Command("/bin/bash", "-c", "dot ./testcharts.dot -Tpng -o ./testcharts.png && open ./testcharts.png").Run()
	exec.Command("/bin/bash", "-c", "dot ./testchartsNoRoot.dot -Tpng -o ./testchartsNoRoot.png && open ./testchartsNoRoot.png").Run()
}
